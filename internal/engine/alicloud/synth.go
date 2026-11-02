package alicloud

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/gorilla/websocket"

	"github.com/qtstack/mrcpgo/internal/audio"
	"github.com/qtstack/mrcpgo/internal/config"
	"github.com/qtstack/mrcpgo/internal/model"
	"github.com/qtstack/mrcpgo/internal/wsutil"
)

// Synthesizer implements model.Synthesizer on top of the NLS WebSocket
// synthesis protocol. Audio received from the service is converted to 8 kHz
// mu-law frames, which is what the RTP transceiver sends.
type Synthesizer struct {
	cfg    config.AliCloud
	tokens *TokenSource
	log    *slog.Logger

	mu     sync.Mutex
	active *synthesis
}

func newSynthesizer(cfg config.AliCloud, tokens *TokenSource, log *slog.Logger) *Synthesizer {
	return &Synthesizer{cfg: cfg, tokens: tokens, log: log}
}

// Synthesize starts a synthesis request and returns a stream of mu-law audio.
// The service connection is established before Synthesize returns, but audio
// arrives asynchronously.
func (s *Synthesizer) Synthesize(ctx context.Context, text string) (io.ReadCloser, error) {
	sessionCtx, cancel := context.WithCancel(ctx)
	token, err := s.tokens.Token(sessionCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	raw, err := dial(sessionCtx, s.cfg, token)
	if err != nil {
		cancel()
		return nil, err
	}

	reader, writer := io.Pipe()
	syn := &synthesis{
		cfg:       s.cfg,
		conn:      wsutil.New(raw, s.cfg.Timeout.Value()),
		log:       s.log,
		ctx:       sessionCtx,
		cancel:    cancel,
		reader:    reader,
		writer:    writer,
		converter: audio.NewMuLawConverter(s.cfg.TTS.SampleRate),
	}
	if err := syn.start(text); err != nil {
		_ = syn.Close()
		return nil, err
	}

	s.mu.Lock()
	previous := s.active
	s.active = syn
	s.mu.Unlock()
	if previous != nil {
		// The channel serialises SPEAK requests, so this only happens if a
		// previous stream was abandoned; release it rather than leaking it.
		_ = previous.Close()
	}

	go func() {
		defer s.clear(syn)
		syn.readLoop()
	}()
	return syn, nil
}

// Stop aborts the active synthesis.
func (s *Synthesizer) Stop() error {
	s.mu.Lock()
	syn := s.active
	s.active = nil
	s.mu.Unlock()
	if syn != nil {
		return syn.Close()
	}
	return nil
}

func (s *Synthesizer) clear(syn *synthesis) {
	s.mu.Lock()
	if s.active == syn {
		s.active = nil
	}
	s.mu.Unlock()
}

// synthesis is one synthesis request. It implements io.ReadCloser over the
// converted mu-law audio.
type synthesis struct {
	cfg    config.AliCloud
	conn   *wsutil.Conn
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc

	reader *io.PipeReader
	writer *io.PipeWriter

	converter *audio.MuLawConverter

	closeOnce sync.Once
}

func (s *synthesis) start(text string) error {
	payload := map[string]any{
		"voice":       s.cfg.TTS.Voice,
		"format":      s.cfg.TTS.Format,
		"sample_rate": s.cfg.TTS.SampleRate,
		"volume":      s.cfg.TTS.Volume,
		"speech_rate": s.cfg.TTS.SpeechRate,
		"pitch_rate":  s.cfg.TTS.PitchRate,
		"text":        text,
	}
	for key, value := range s.cfg.TTS.Params {
		payload[key] = value
	}

	namespace := nsTTS
	if s.cfg.TTS.LongText {
		namespace = nsTTSLong
	}
	req := newRequest(namespace, nameStartSynthesis, s.cfg.AppKey, newID(), payload)
	if err := s.conn.WriteJSON(req); err != nil {
		return fmt.Errorf("alicloud: send StartSynthesis: %w", err)
	}
	return nil
}

// readLoop converts upstream PCM chunks to mu-law and writes them to the pipe,
// which applies backpressure to the service.
func (s *synthesis) readLoop() {
	defer func() { _ = s.writer.Close() }()

	// A canceled request must tear the upstream connection down even if Close
	// is never called by the consumer.
	stop := context.AfterFunc(s.ctx, func() { _ = s.conn.Close() })
	defer stop()

	for {
		messageType, data, err := s.conn.Read()
		if err != nil {
			if s.ctx.Err() == nil {
				s.log.Warn("NLS synthesis connection closed", "error", err)
				_ = s.writer.CloseWithError(fmt.Errorf("alicloud: %w", err))
			}
			return
		}

		if messageType == websocket.BinaryMessage {
			if err := s.writeAudio(data); err != nil {
				return
			}
			continue
		}

		resp, err := parseResponse(data)
		if err != nil {
			s.log.Warn("unexpected NLS control frame", "error", err)
			continue
		}
		switch resp.Header.Name {
		case nameMetaInfo:
			s.log.Debug("NLS synthesis meta info", "payload", string(data))
		case nameSynthesisCompleted:
			s.log.Debug("NLS synthesis completed")
			return
		case nameTaskFailed:
			err := resp.err()
			s.log.Warn("NLS synthesis failed", "error", err)
			_ = s.writer.CloseWithError(err)
			return
		default:
			s.log.Debug("ignoring NLS message", "name", resp.Header.Name)
		}
	}
}

// writeAudio converts one PCM chunk and writes the mu-law bytes to the pipe.
func (s *synthesis) writeAudio(data []byte) error {
	ulaw := s.converter.Convert(data)
	if len(ulaw) == 0 {
		return nil
	}
	if _, err := s.writer.Write(ulaw); err != nil {
		return err
	}
	return nil
}

// Read implements io.Reader.
func (s *synthesis) Read(p []byte) (int, error) { return s.reader.Read(p) }

// Close releases the upstream connection. It is idempotent.
func (s *synthesis) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		_ = s.conn.Close()
		_ = s.reader.Close()
	})
	return nil
}

// ensure the interfaces are satisfied at compile time.
var _ model.Synthesizer = (*Synthesizer)(nil)
var _ model.Recognizer = (*Recognizer)(nil)
var _ model.AudioConsumer = (*Recognizer)(nil)
var _ io.ReadCloser = (*synthesis)(nil)

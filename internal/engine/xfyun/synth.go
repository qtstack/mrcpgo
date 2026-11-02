package xfyun

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/qtstack/mrcpgo/internal/audio"
	"github.com/qtstack/mrcpgo/internal/config"
	"github.com/qtstack/mrcpgo/internal/model"
	"github.com/qtstack/mrcpgo/internal/wsutil"
)

// Synthesizer implements model.Synthesizer on top of the iFlytek online speech
// synthesis WebSocket API.
type Synthesizer struct {
	cfg config.Xfyun
	log *slog.Logger

	mu     sync.Mutex
	active *synthesis
}

func newSynthesizer(cfg config.Xfyun, log *slog.Logger) *Synthesizer {
	return &Synthesizer{cfg: cfg, log: log}
}

// Synthesize opens a synthesis session, sends the text and returns a stream of
// 8 kHz mu-law audio.
func (s *Synthesizer) Synthesize(ctx context.Context, text string) (io.ReadCloser, error) {
	sessionCtx, cancel := context.WithCancel(ctx)

	urlStr, err := buildTTSURL(s.cfg.TTSEndpoint, s.cfg.APIKey, s.cfg.APISecret, time.Now())
	if err != nil {
		cancel()
		return nil, err
	}
	raw, err := dialWS(sessionCtx, urlStr, s.cfg.Timeout.Value())
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
	if err := syn.send(text); err != nil {
		_ = syn.Close()
		return nil, err
	}

	s.mu.Lock()
	previous := s.active
	s.active = syn
	s.mu.Unlock()
	if previous != nil {
		// The channel serialises SPEAK requests; release an abandoned stream
		// rather than leaking its connection.
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

// synthesis is one synthesis request and implements io.ReadCloser over the
// converted mu-law audio.
type synthesis struct {
	cfg       config.Xfyun
	conn      *wsutil.Conn
	log       *slog.Logger
	ctx       context.Context
	cancel    context.CancelFunc
	reader    *io.PipeReader
	writer    *io.PipeWriter
	converter *audio.MuLawConverter

	closeOnce sync.Once
}

// send submits the whole utterance as a single (last) frame.
func (s *synthesis) send(text string) error {
	business := map[string]any{
		"aue":    "raw",
		"auf":    fmt.Sprintf("audio/L16;rate=%d", s.cfg.TTS.SampleRate),
		"vcn":    s.cfg.TTS.Voice,
		"tte":    "utf8",
		"speed":  s.cfg.TTS.Speed,
		"volume": s.cfg.TTS.Volume,
		"pitch":  s.cfg.TTS.Pitch,
	}
	for key, value := range s.cfg.TTS.Params {
		business[key] = value
	}

	req := ttsRequest{
		Common:   ttsCommon{AppID: s.cfg.AppID},
		Business: business,
		Data: ttsData{
			Status:   2,
			Text:     base64.StdEncoding.EncodeToString([]byte(text)),
			Encoding: "",
		},
	}
	if err := s.conn.WriteJSON(req); err != nil {
		return fmt.Errorf("xfyun: send synthesis request: %w", err)
	}
	return nil
}

// readLoop decodes audio until the final status frame.
func (s *synthesis) readLoop() {
	defer func() { _ = s.writer.Close() }()

	stop := context.AfterFunc(s.ctx, func() { _ = s.conn.Close() })
	defer stop()

	for {
		messageType, data, err := s.conn.Read()
		if err != nil {
			if s.ctx.Err() == nil {
				s.log.Warn("iFlytek synthesis connection closed", "error", err)
				_ = s.writer.CloseWithError(fmt.Errorf("xfyun: %w", err))
			}
			return
		}

		if messageType == websocket.BinaryMessage {
			if err := s.writeAudio(data); err != nil {
				return
			}
			continue
		}

		var resp ttsResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			s.log.Warn("unexpected iFlytek control message", "error", err)
			continue
		}
		if resp.Code != 0 {
			err := fmt.Errorf("xfyun: %s (code %d, sid %s)", resp.Message, resp.Code, resp.Sid)
			s.log.Warn("iFlytek synthesis failed", "error", err)
			_ = s.writer.CloseWithError(err)
			return
		}
		if resp.Data.Audio != "" {
			pcm, err := base64.StdEncoding.DecodeString(resp.Data.Audio)
			if err != nil {
				s.log.Warn("decode iFlytek audio", "error", err)
			} else if err := s.writeAudio(pcm); err != nil {
				return
			}
		}
		if resp.Data.Status == 2 {
			s.log.Info("iFlytek synthesis completed")
			return
		}
	}
}

// writeAudio converts PCM to mu-law and writes it to the pipe.
func (s *synthesis) writeAudio(pcm []byte) error {
	ulaw := s.converter.Convert(pcm)
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

var _ model.Synthesizer = (*Synthesizer)(nil)
var _ model.Recognizer = (*Recognizer)(nil)
var _ model.AudioConsumer = (*Recognizer)(nil)
var _ io.ReadCloser = (*synthesis)(nil)

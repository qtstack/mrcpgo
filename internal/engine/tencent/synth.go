package tencent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/qtstack/mrcpgo/internal/audio"
	"github.com/qtstack/mrcpgo/internal/config"
	"github.com/qtstack/mrcpgo/internal/model"
	"github.com/qtstack/mrcpgo/internal/wsutil"
)

// Synthesizer implements model.Synthesizer on top of the Tencent Cloud
// streaming text-to-speech WebSocket API (TextToStreamAudioWSv2).
type Synthesizer struct {
	cfg config.Tencent
	log *slog.Logger

	mu     sync.Mutex
	active *synthesis
}

func newSynthesizer(cfg config.Tencent, log *slog.Logger) *Synthesizer {
	return &Synthesizer{cfg: cfg, log: log}
}

// Synthesize opens a synthesis session and returns a stream of 8 kHz mu-law
// audio.
func (s *Synthesizer) Synthesize(ctx context.Context, text string) (io.ReadCloser, error) {
	sessionCtx, cancel := context.WithCancel(ctx)
	sessionID := newID()

	endpoint, err := buildWebSocketURL(ttsURL(s.cfg), "GET", "Signature", s.ttsParams(sessionID), s.cfg.SecretKey)
	if err != nil {
		cancel()
		return nil, err
	}
	raw, err := dialWS(sessionCtx, endpoint, s.cfg.Timeout.Value())
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
		text:      text,
		sessionID: sessionID,
		converter: audio.NewMuLawConverter(s.cfg.TTS.SampleRate),
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

func (s *Synthesizer) ttsParams(sessionID string) map[string]string {
	now := time.Now()
	params := map[string]string{
		"Action":     "TextToStreamAudioWSv2",
		"AppId":      s.cfg.AppID,
		"SecretId":   s.cfg.SecretID,
		"Timestamp":  strconv.FormatInt(now.Unix(), 10),
		"Expired":    strconv.FormatInt(now.Add(signatureExpiry).Unix(), 10),
		"SessionId":  sessionID,
		"Codec":      "pcm",
		"SampleRate": strconv.Itoa(s.cfg.TTS.SampleRate),
		"Speed":      strconv.FormatFloat(s.cfg.TTS.Speed, 'g', -1, 64),
		"Volume":     strconv.FormatFloat(s.cfg.TTS.Volume, 'g', -1, 64),
	}
	if s.cfg.TTS.VoiceType != 0 {
		params["VoiceType"] = strconv.Itoa(s.cfg.TTS.VoiceType)
	}
	if s.cfg.TTS.EmotionCategory != "" {
		params["EmotionCategory"] = s.cfg.TTS.EmotionCategory
		if s.cfg.TTS.EmotionIntensity != 0 {
			params["EmotionIntensity"] = strconv.Itoa(s.cfg.TTS.EmotionIntensity)
		}
	}
	if s.cfg.TTS.SegmentRate != 0 {
		params["SegmentRate"] = strconv.Itoa(s.cfg.TTS.SegmentRate)
	}
	for key, value := range s.cfg.TTS.Params {
		params[key] = value
	}
	return params
}

// ttsURL returns the synthesis endpoint, without query parameters.
func ttsURL(cfg config.Tencent) string {
	if cfg.TTSEndpoint != "" {
		return cfg.TTSEndpoint
	}
	return "wss://tts.cloud.tencent.com/stream_wsv2"
}

// synthesis is one synthesis request and implements io.ReadCloser over the
// converted mu-law audio.
type synthesis struct {
	cfg    config.Tencent
	conn   *wsutil.Conn
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc

	reader *io.PipeReader
	writer *io.PipeWriter

	text      string
	sessionID string

	started   bool
	converter *audio.MuLawConverter

	closeOnce sync.Once
}

// readLoop waits for the READY event, sends the text, then converts audio until
// the FINAL event.
func (s *synthesis) readLoop() {
	defer func() { _ = s.writer.Close() }()

	stop := context.AfterFunc(s.ctx, func() { _ = s.conn.Close() })
	defer stop()

	for {
		messageType, data, err := s.conn.Read()
		if err != nil {
			if s.ctx.Err() == nil {
				s.log.Warn("Tencent synthesis connection closed", "error", err)
				_ = s.writer.CloseWithError(fmt.Errorf("tencent: %w", err))
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
			s.log.Warn("unexpected Tencent control message", "error", err)
			continue
		}
		if resp.Code != 0 {
			err := resp.err()
			s.log.Warn("Tencent synthesis failed", "error", err)
			_ = s.writer.CloseWithError(err)
			return
		}
		if resp.Ready == 1 && !s.started {
			if err := s.sendText(); err != nil {
				s.log.Warn("failed to send synthesis text", "error", err)
				_ = s.writer.CloseWithError(err)
				return
			}
			s.started = true
			continue
		}
		if resp.Final == 1 {
			s.log.Info("Tencent synthesis completed")
			return
		}
	}
}

// sendText submits the whole utterance and signals that no more text follows.
func (s *synthesis) sendText() error {
	if err := s.conn.WriteJSON(actionMessage{
		SessionID: s.sessionID,
		MessageID: newID(),
		Action:    actionSynthesis,
		Data:      s.text,
	}); err != nil {
		return fmt.Errorf("tencent: send %s: %w", actionSynthesis, err)
	}
	if err := s.conn.WriteJSON(actionMessage{
		SessionID: s.sessionID,
		MessageID: newID(),
		Action:    actionComplete,
		Data:      "",
	}); err != nil {
		return fmt.Errorf("tencent: send %s: %w", actionComplete, err)
	}
	return nil
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

var _ model.Synthesizer = (*Synthesizer)(nil)
var _ model.Recognizer = (*Recognizer)(nil)
var _ model.AudioConsumer = (*Recognizer)(nil)
var _ io.ReadCloser = (*synthesis)(nil)

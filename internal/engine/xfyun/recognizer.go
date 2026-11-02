package xfyun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/qtstack/mrcpgo/internal/audio"
	"github.com/qtstack/mrcpgo/internal/config"
	"github.com/qtstack/mrcpgo/internal/model"
	"github.com/qtstack/mrcpgo/internal/nlsml"
	"github.com/qtstack/mrcpgo/internal/wsutil"
)

const (
	audioQueueChunks = 100
	stopWriteTimeout = time.Second
	// asrEndFrame is the binary end-of-audio marker required by rtasr.
	asrEndFrame = `{"end": true}`
)

// ErrBusy is returned when a recognition is started while one is running.
var ErrBusy = errors.New("xfyun: recognition already in progress")

// Recognizer implements model.Recognizer and model.AudioConsumer on top of the
// iFlytek real-time transcription (rtasr) WebSocket API.
type Recognizer struct {
	cfg config.Xfyun
	log *slog.Logger

	mu      sync.Mutex
	session *recognitionSession
}

func newRecognizer(cfg config.Xfyun, log *slog.Logger) *Recognizer {
	return &Recognizer{cfg: cfg, log: log}
}

// Start opens a transcription connection, validates the handshake and returns a
// channel that receives exactly one result.
func (r *Recognizer) Start(ctx context.Context, grammarURI string) (<-chan model.RecognizerResult, error) {
	r.mu.Lock()
	busy := r.session != nil
	r.mu.Unlock()
	if busy {
		return nil, ErrBusy
	}

	// DECISION: cloud recognition has no SRGS grammar concept; the URI is
	// logged and ignored. Language and locale selection come from
	// mrcp.engine.xfyun.asr.
	if grammarURI != "" {
		r.log.Debug("grammar URI is not used by the xfyun engine", "grammar", grammarURI)
	}

	sessionCtx, cancel := context.WithCancel(ctx)
	raw, err := dialWS(sessionCtx, asrURL(r.cfg), r.cfg.Timeout.Value())
	if err != nil {
		cancel()
		return nil, err
	}
	conn := wsutil.New(raw, r.cfg.Timeout.Value())

	// Read "started" synchronously so a bad signature fails the RECOGNIZE
	// instead of producing a late event.
	if err := r.handshake(conn); err != nil {
		_ = conn.Close()
		cancel()
		return nil, err
	}

	s := &recognitionSession{
		cfg:    r.cfg,
		conn:   conn,
		log:    r.log,
		ctx:    sessionCtx,
		cancel: cancel,
		audio:  make(chan []byte, audioQueueChunks),
	}
	results := make(chan model.RecognizerResult, 1)
	r.mu.Lock()
	if r.session != nil {
		r.mu.Unlock()
		s.close()
		return nil, ErrBusy
	}
	r.session = s
	r.mu.Unlock()

	go s.run(results, func() { r.clear(s) })
	return results, nil
}

// WriteAudio implements model.AudioConsumer.
func (r *Recognizer) WriteAudio(payload []byte) error {
	r.mu.Lock()
	s := r.session
	r.mu.Unlock()
	if s == nil || len(payload) == 0 {
		return nil
	}
	s.writeAudio(payload)
	return nil
}

// Stop aborts the current recognition.
func (r *Recognizer) Stop() error {
	r.mu.Lock()
	s := r.session
	r.session = nil
	r.mu.Unlock()
	if s != nil {
		s.stop()
	}
	return nil
}

func (r *Recognizer) clear(s *recognitionSession) {
	r.mu.Lock()
	if r.session == s {
		r.session = nil
	}
	r.mu.Unlock()
}

func (r *Recognizer) handshake(conn *wsutil.Conn) error {
	timeout := r.cfg.Timeout.Value()
	if timeout > 0 {
		if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			return err
		}
		defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	}
	messageType, data, err := conn.Read()
	if err != nil {
		return fmt.Errorf("xfyun: transcription handshake: %w", err)
	}
	if messageType != websocket.TextMessage {
		return fmt.Errorf("xfyun: transcription handshake not a text frame")
	}
	var resp asrResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("xfyun: decode transcription handshake: %w", err)
	}
	if resp.Action == "error" || resp.Code != "0" {
		return fmt.Errorf("xfyun: %s (code %s, sid %s)", resp.Desc, resp.Code, resp.Sid)
	}
	return nil
}

// recognitionSession is one recognition request.
type recognitionSession struct {
	cfg    config.Xfyun
	conn   *wsutil.Conn
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc

	audio   chan []byte
	dropped atomic.Uint64

	stopOnce sync.Once
}

// run reads transcription results until a final segment arrives or the request
// is canceled.
func (s *recognitionSession) run(results chan<- model.RecognizerResult, onDone func()) {
	defer onDone()
	defer s.cancel()
	defer s.close()
	defer close(results)

	stop := context.AfterFunc(s.ctx, func() { _ = s.conn.Close() })
	defer stop()

	go s.audioLoop()

	for {
		messageType, data, err := s.conn.Read()
		if err != nil {
			if s.ctx.Err() == nil {
				s.log.Warn("iFlytek transcription connection closed", "error", err)
				results <- model.RecognizerResult{Err: fmt.Errorf("xfyun: %w", err)}
			}
			return
		}
		if messageType != websocket.TextMessage {
			continue
		}

		var resp asrResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			s.log.Warn("unexpected iFlytek control message", "error", err)
			continue
		}
		if resp.Action == "error" || (resp.Code != "" && resp.Code != "0") {
			err := fmt.Errorf("xfyun: %s (code %s, sid %s)", resp.Desc, resp.Code, resp.Sid)
			s.log.Warn("iFlytek transcription failed", "error", err)
			results <- model.RecognizerResult{Err: err}
			return
		}
		if resp.Action != "result" {
			s.log.Debug("ignoring iFlytek message", "action", resp.Action)
			continue
		}

		var segment asrResult
		if err := json.Unmarshal([]byte(resp.Data), &segment); err != nil {
			s.log.Warn("decode iFlytek result", "error", err)
			continue
		}
		if !segment.final() {
			s.log.Debug("iFlytek interim result", "text", segment.text(), "segment", segment.SegID)
			continue
		}

		// DECISION: one RECOGNIZE maps to one utterance. The first final
		// segment (st.type == "0", produced by server side VAD) completes the
		// request, which is what MRCP clients such as mod_unimrcp expect.
		text := segment.text()
		s.log.Info("iFlytek transcription completed", "text_bytes", len(text))
		results <- recognitionResult(text)
		return
	}
}

// audioLoop forwards buffered 16 kHz PCM upstream in fixed size frames.
func (s *recognitionSession) audioLoop() {
	chunkBytes := audio.ChunkBytes(s.cfg.ASR.SampleRate, s.cfg.ASR.ChunkMS)
	if chunkBytes <= 0 {
		chunkBytes = audio.ChunkBytes(16000, 40)
	}
	buf := make([]byte, 0, chunkBytes*2)

	for {
		select {
		case <-s.ctx.Done():
			return
		case pcm := <-s.audio:
			buf = append(buf, pcm...)
			for len(buf) >= chunkBytes {
				if err := s.conn.WriteBinary(buf[:chunkBytes]); err != nil {
					s.log.Warn("failed to send audio to iFlytek", "error", err)
					return
				}
				buf = buf[chunkBytes:]
			}
		}
	}
}

// writeAudio converts one mu-law frame to 16 kHz PCM and queues it.
func (s *recognitionSession) writeAudio(payload []byte) {
	pcm := audio.DecodeMuLawFrame(payload)
	pcm = audio.Upsample2(pcm)
	select {
	case s.audio <- pcm:
	default:
		if n := s.dropped.Add(1); n%50 == 1 {
			s.log.Warn("iFlytek audio queue full, dropping frames", "dropped", n)
		}
	}
}

// stop sends the end-of-audio marker and closes the connection.
func (s *recognitionSession) stop() {
	s.stopOnce.Do(func() {
		s.cancel()
		if err := s.conn.WriteMessage(websocket.BinaryMessage, []byte(asrEndFrame), stopWriteTimeout); err != nil {
			s.log.Debug("end-of-audio frame not delivered", "error", err)
		}
		_ = s.conn.Close()
	})
}

func (s *recognitionSession) close() {
	s.cancel()
	_ = s.conn.Close()
}

// recognitionResult converts transcript text into an engine result.
func recognitionResult(text string) model.RecognizerResult {
	if strings.TrimSpace(text) == "" {
		return model.RecognizerResult{CompletionCause: model.CauseNoMatch}
	}
	// rtasr does not report a confidence score for final segments.
	return model.RecognizerResult{NLSML: nlsml.Build(nlsml.Result{Text: text, Confidence: -1})}
}

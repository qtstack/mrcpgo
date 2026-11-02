package tencent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
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
	// audioQueueChunks bounds converted audio waiting for the upstream sender.
	audioQueueChunks = 100
	// stopWriteTimeout bounds the best effort end-of-stream frame.
	stopWriteTimeout = time.Second
	// signatureExpiry is how long a signed URL stays valid.
	signatureExpiry = 24 * time.Hour
)

// ErrBusy is returned when a recognition is started while one is running.
var ErrBusy = errors.New("tencent: recognition already in progress")

// Recognizer implements model.Recognizer and model.AudioConsumer on top of the
// Tencent Cloud real-time speech recognition v2 WebSocket API.
type Recognizer struct {
	cfg config.Tencent
	log *slog.Logger

	mu      sync.Mutex
	session *recognitionSession
}

func newRecognizer(cfg config.Tencent, log *slog.Logger) *Recognizer {
	return &Recognizer{cfg: cfg, log: log}
}

// Start opens a recognition connection, validates the handshake and returns a
// channel that receives exactly one result.
func (r *Recognizer) Start(ctx context.Context, grammarURI string) (<-chan model.RecognizerResult, error) {
	r.mu.Lock()
	busy := r.session != nil
	r.mu.Unlock()
	if busy {
		return nil, ErrBusy
	}

	// DECISION: cloud recognition has no SRGS grammar concept; the URI is
	// logged and otherwise ignored. Model, hotwords and similar options come
	// from mrcp.engine.tencent.asr.params.
	if grammarURI != "" {
		r.log.Debug("grammar URI is not used by the tencent engine", "grammar", grammarURI)
	}

	sessionCtx, cancel := context.WithCancel(ctx)
	params := r.asrParams()
	endpoint, err := buildWebSocketURL(asrURL(r.cfg), "", "signature", params, r.cfg.SecretKey)
	if err != nil {
		cancel()
		return nil, err
	}
	raw, err := dialWS(sessionCtx, endpoint, r.cfg.Timeout.Value())
	if err != nil {
		cancel()
		return nil, err
	}
	conn := wsutil.New(raw, r.cfg.Timeout.Value())

	// Read the handshake synchronously so authentication and parameter errors
	// surface as a failed RECOGNIZE instead of a late event.
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
		finals: make(map[int32]string),
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

// WriteAudio implements model.AudioConsumer. It converts the 20 ms mu-law frame
// to PCM and queues it; the RTP receive loop is never blocked.
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
		return fmt.Errorf("tencent: recognition handshake: %w", err)
	}
	if messageType != websocket.TextMessage {
		return fmt.Errorf("tencent: recognition handshake not a text frame")
	}
	var resp asrResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("tencent: decode recognition handshake: %w", err)
	}
	if resp.Code != 0 {
		return resp.err()
	}
	return nil
}

func (r *Recognizer) asrParams() map[string]string {
	now := time.Now()
	params := map[string]string{
		"secretid":  r.cfg.SecretID,
		"timestamp": strconv.FormatInt(now.Unix(), 10),
		"expired":   strconv.FormatInt(now.Add(signatureExpiry).Unix(), 10),
		// The service limits nonce to 10 digits, so the Unix timestamp is used,
		// as the official SDK does.
		"nonce":             strconv.FormatInt(now.Unix(), 10),
		"engine_model_type": r.cfg.ASR.EngineModel,
		"voice_id":          newID(),
		"voice_format":      "1", // pcm
		"needvad":           boolString(r.cfg.ASR.NeedVAD),
		"convert_num_mode":  strconv.Itoa(r.cfg.ASR.ConvertNumMode),
	}
	if r.cfg.ASR.VadSilenceTime > 0 {
		params["vad_silence_time"] = strconv.Itoa(r.cfg.ASR.VadSilenceTime)
	}
	for key, value := range r.cfg.ASR.Params {
		params[key] = value
	}
	return params
}

// asrURL returns the recognition endpoint, without query parameters.
func asrURL(cfg config.Tencent) string {
	if cfg.ASREndpoint != "" {
		return cfg.ASREndpoint
	}
	return "wss://asr.cloud.tencent.com/asr/v2/" + cfg.AppID
}

// recognitionSession is one recognition request.
type recognitionSession struct {
	cfg    config.Tencent
	conn   *wsutil.Conn
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc

	audio   chan []byte
	dropped atomic.Uint64

	// finals accumulates definite sentences; the read loop is the only writer.
	finals map[int32]string
	order  []int32

	stopOnce sync.Once
}

// run reads result messages until a sentence is finalised, the stream ends or
// the request is canceled.
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
				s.log.Warn("Tencent recognition connection closed", "error", err)
				results <- model.RecognizerResult{Err: fmt.Errorf("tencent: %w", err)}
			}
			return
		}
		if messageType != websocket.TextMessage {
			continue
		}

		var resp asrResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			s.log.Warn("unexpected Tencent control message", "error", err)
			continue
		}
		if resp.Code != 0 {
			err := resp.err()
			s.log.Warn("Tencent recognition failed", "error", err)
			results <- model.RecognizerResult{Err: err}
			return
		}

		if resp.Sentences != nil {
			if text, ok := s.absorb(resp.Sentences.SentenceList); ok {
				// DECISION: one RECOGNIZE maps to one utterance. The first
				// definite sentence (sentence_type=1, produced by server side
				// VAD) completes the request, which is what MRCP clients such as
				// mod_unimrcp expect from a single recognition.
				s.log.Info("Tencent recognition completed", "text_bytes", len(text))
				results <- recognitionResult(text)
				return
			}
		}
		if resp.Final == 1 {
			results <- recognitionResult(s.text())
			return
		}
	}
}

// absorb records sentence updates and reports whether a definite sentence was
// seen.
func (s *recognitionSession) absorb(sentences []asrSentence) (string, bool) {
	definite := false
	for _, item := range sentences {
		if item.Sentence == "" {
			continue
		}
		if item.SentenceType != 1 {
			s.log.Debug("Tencent interim sentence", "sentence", item.Sentence, "id", item.SentenceID)
			continue
		}
		if _, seen := s.finals[item.SentenceID]; !seen {
			s.order = append(s.order, item.SentenceID)
		}
		s.finals[item.SentenceID] = item.Sentence
		definite = true
	}
	if !definite {
		return "", false
	}
	return s.text(), true
}

// text joins the definite sentences in arrival order.
func (s *recognitionSession) text() string {
	if len(s.order) == 0 {
		return ""
	}
	parts := make([]string, 0, len(s.order))
	for _, id := range s.order {
		parts = append(parts, s.finals[id])
	}
	return strings.Join(parts, "")
}

// audioLoop forwards buffered audio upstream in fixed size frames.
func (s *recognitionSession) audioLoop() {
	chunkBytes := audio.ChunkBytes(s.cfg.ASR.SampleRate, s.cfg.ASR.ChunkMS)
	if chunkBytes <= 0 {
		chunkBytes = audio.ChunkBytes(audio.SampleRate, 100)
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
					s.log.Warn("failed to send audio to Tencent", "error", err)
					return
				}
				buf = buf[chunkBytes:]
			}
		}
	}
}

// writeAudio converts one mu-law frame and queues it.
func (s *recognitionSession) writeAudio(payload []byte) {
	pcm := audio.DecodeMuLawFrame(payload)
	if s.cfg.ASR.SampleRate == 16000 {
		pcm = audio.Upsample2(pcm)
	}
	select {
	case s.audio <- pcm:
	default:
		if n := s.dropped.Add(1); n%50 == 1 {
			s.log.Warn("Tencent audio queue full, dropping frames", "dropped", n)
		}
	}
}

// stop tells the service that no more audio follows and closes the connection.
func (s *recognitionSession) stop() {
	s.stopOnce.Do(func() {
		s.cancel()
		if err := s.conn.WriteMessage(websocket.TextMessage, []byte(asrEndMessage), stopWriteTimeout); err != nil {
			s.log.Debug("end-of-stream frame not delivered", "error", err)
		}
		_ = s.conn.Close()
	})
}

func (s *recognitionSession) close() {
	s.cancel()
	_ = s.conn.Close()
}

// recognitionResult converts recognised text into an engine result.
func recognitionResult(text string) model.RecognizerResult {
	if strings.TrimSpace(text) == "" {
		return model.RecognizerResult{CompletionCause: model.CauseNoMatch}
	}
	// Tencent v2 does not report a confidence score, so the NLSML confidence
	// attribute is omitted rather than fabricated.
	return model.RecognizerResult{NLSML: nlsml.Build(nlsml.Result{Text: text, Confidence: -1})}
}

func boolString(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

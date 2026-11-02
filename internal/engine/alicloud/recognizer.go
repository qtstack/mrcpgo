package alicloud

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/qtstack/mrcpgo/internal/audio"
	"github.com/qtstack/mrcpgo/internal/config"
	"github.com/qtstack/mrcpgo/internal/model"
	"github.com/qtstack/mrcpgo/internal/nlsml"
	"github.com/qtstack/mrcpgo/internal/wsutil"
)

// audioQueueChunks bounds the converted audio waiting to be sent upstream. At
// 20 ms per chunk this is two seconds of audio.
const audioQueueChunks = 100

// stopWriteTimeout bounds the best effort StopRecognition frame so teardown is
// never held up by a stalled connection.
const stopWriteTimeout = time.Second

// ErrBusy is returned when a recognition is started while another one is
// already running on the same channel.
var ErrBusy = errors.New("alicloud: recognition already in progress")

// Recognizer implements model.Recognizer on top of the NLS real-time speech
// recognition WebSocket protocol. It also implements model.AudioConsumer so it
// receives the inbound RTP payload stream from the session.
type Recognizer struct {
	cfg    config.AliCloud
	tokens *TokenSource
	log    *slog.Logger

	mu      sync.Mutex
	session *recognitionSession
}

func newRecognizer(cfg config.AliCloud, tokens *TokenSource, log *slog.Logger) *Recognizer {
	return &Recognizer{cfg: cfg, tokens: tokens, log: log}
}

// Start opens the NLS connection, sends StartRecognition and returns a channel
// that receives exactly one result.
func (r *Recognizer) Start(ctx context.Context, grammarURI string) (<-chan model.RecognizerResult, error) {
	r.mu.Lock()
	busy := r.session != nil
	r.mu.Unlock()
	if busy {
		return nil, ErrBusy
	}

	// DECISION: cloud recognisers have no SRGS grammar concept. The grammar URI
	// is logged and otherwise ignored; model or domain selection is configured
	// through mrcp.engine.alicloud.asr.params.
	if grammarURI != "" {
		r.log.Debug("grammar URI is not used by the alicloud engine", "grammar", grammarURI)
	}

	sessionCtx, cancel := context.WithCancel(ctx)
	token, err := r.tokens.Token(sessionCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	raw, err := dial(sessionCtx, r.cfg, token)
	if err != nil {
		cancel()
		return nil, err
	}

	s := &recognitionSession{
		cfg:        r.cfg,
		conn:       wsutil.New(raw, r.cfg.Timeout.Value()),
		log:        r.log,
		ctx:        sessionCtx,
		cancel:     cancel,
		ready:      make(chan struct{}),
		audioQueue: make(chan []byte, audioQueueChunks),
	}
	if err := s.startRecognition(); err != nil {
		s.close()
		return nil, err
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

// WriteAudio implements model.AudioConsumer. It converts 20 ms mu-law frames to
// PCM and queues them for the upstream sender. The call never blocks the RTP
// receive loop: when the queue is full audio is dropped and counted.
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

// recognitionSession is one recognition request.
type recognitionSession struct {
	cfg    config.AliCloud
	conn   *wsutil.Conn
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc

	taskID string

	ready     chan struct{}
	readyOnce sync.Once

	audioQueue chan []byte
	dropped    atomic.Uint64

	stopOnce sync.Once
}

func (s *recognitionSession) startRecognition() error {
	s.taskID = newID()
	payload := map[string]any{
		"format":                            s.cfg.ASR.Format,
		"sample_rate":                       s.cfg.ASR.SampleRate,
		"enable_intermediate_result":        s.cfg.ASR.EnableIntermediateResult,
		"enable_punctuation_prediction":     s.cfg.ASR.EnablePunctuationPrediction,
		"enable_inverse_text_normalization": s.cfg.ASR.EnableInverseTextNormalization,
	}
	for key, value := range s.cfg.ASR.Params {
		payload[key] = value
	}
	req := newRequest(nsASR, nameStartRecognition, s.cfg.AppKey, s.taskID, payload)
	if err := s.conn.WriteJSON(req); err != nil {
		return fmt.Errorf("alicloud: send StartRecognition: %w", err)
	}
	return nil
}

// run reads control frames until the request completes, fails or is canceled.
func (s *recognitionSession) run(results chan<- model.RecognizerResult, onDone func()) {
	defer onDone()
	defer s.cancel()
	defer s.close()
	defer close(results)

	// Closing the socket is what unblocks a pending read, so cancelation must
	// always close it even when Stop is not called.
	stop := context.AfterFunc(s.ctx, func() { _ = s.conn.Close() })
	defer stop()

	go s.audioLoop()

	for {
		_, data, err := s.conn.Read()
		if err != nil {
			if s.ctx.Err() == nil {
				s.log.Warn("NLS recognition connection closed", "error", err)
				results <- model.RecognizerResult{Err: fmt.Errorf("alicloud: %w", err)}
			}
			return
		}

		resp, err := parseResponse(data)
		if err != nil {
			s.log.Warn("unexpected NLS control frame", "error", err)
			continue
		}

		switch resp.Header.Name {
		case nameRecognitionStarted:
			if !resp.ok() {
				results <- model.RecognizerResult{Err: resp.err()}
				return
			}
			s.readyOnce.Do(func() { close(s.ready) })
		case nameRecognitionResultChanged:
			s.log.Debug("NLS interim result", "text", resp.result(), "index", resp.Payload["index"])
		case nameRecognitionCompleted:
			if !resp.ok() {
				results <- model.RecognizerResult{Err: resp.err()}
				return
			}
			text := resp.result()
			result := model.RecognizerResult{}
			if text == "" {
				result.CompletionCause = model.CauseNoMatch
			} else {
				result.NLSML = nlsml.Build(nlsml.Result{
					Text:       text,
					Confidence: resp.confidence(),
					Instance:   resp.instance(),
				})
			}
			s.log.Info("NLS recognition completed", "text_bytes", len(text))
			results <- result
			return
		case nameTaskFailed:
			err := resp.err()
			s.log.Warn("NLS recognition failed", "error", err)
			results <- model.RecognizerResult{Err: err}
			return
		default:
			s.log.Debug("ignoring NLS message", "name", resp.Header.Name)
		}
	}
}

// audioLoop forwards buffered audio upstream in fixed size frames once the
// service has accepted the start request.
func (s *recognitionSession) audioLoop() {
	select {
	case <-s.ready:
	case <-s.ctx.Done():
		return
	}

	chunkBytes := audio.ChunkBytes(s.cfg.ASR.SampleRate, s.cfg.ASR.ChunkMS)
	if chunkBytes <= 0 {
		chunkBytes = audio.ChunkBytes(audio.SampleRate, 100)
	}
	buf := make([]byte, 0, chunkBytes*2)

	for {
		select {
		case <-s.ctx.Done():
			return
		case pcm := <-s.audioQueue:
			buf = append(buf, pcm...)
			for len(buf) >= chunkBytes {
				if err := s.conn.WriteBinary(buf[:chunkBytes]); err != nil {
					s.log.Warn("failed to send audio to NLS", "error", err)
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
	case s.audioQueue <- pcm:
	default:
		if n := s.dropped.Add(1); n%50 == 1 {
			s.log.Warn("NLS audio queue full, dropping frames", "dropped", n)
		}
	}
}

// stop asks the service to finish and tears the connection down. The stop frame
// is best effort: the request is complete either way once the connection
// closes.
func (s *recognitionSession) stop() {
	s.stopOnce.Do(func() {
		s.cancel()
		req := newRequest(nsASR, nameStopRecognition, s.cfg.AppKey, s.taskID, nil)
		if err := s.conn.WriteJSONTimeout(req, stopWriteTimeout); err != nil {
			s.log.Debug("StopRecognition not delivered", "error", err)
		}
		_ = s.conn.Close()
	})
}

func (s *recognitionSession) close() {
	s.cancel()
	_ = s.conn.Close()
}

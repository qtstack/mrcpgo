// Package mock provides deterministic in-process implementations of the speech
// engines. They let the server be exercised end to end without an ASR or TTS
// backend.
package mock

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/qtstack/mrcpgo/internal/model"
)

// DefaultNLSML is the recognition result reported by the mock recognizer.
const DefaultNLSML = `<?xml version="1.0"?>
<result>
  <interpretation confidence="99">
    <input mode="speech">mock input</input>
    <instance>mock-instance</instance>
  </interpretation>
</result>`

// Recognizer is a mock speech recognizer. After a configurable delay it
// reports DefaultNLSML and closes its result channel.
type Recognizer struct {
	// Delay simulates recognition latency.
	Delay time.Duration
	// Result overrides the NLSML document. Empty means DefaultNLSML.
	Result []byte

	mu     sync.Mutex
	cancel context.CancelFunc
}

// NewRecognizer returns a recognizer with a short default latency.
func NewRecognizer() *Recognizer {
	return &Recognizer{Delay: 200 * time.Millisecond}
}

// Start implements model.Recognizer.
func (r *Recognizer) Start(ctx context.Context, grammarURI string) (<-chan model.RecognizerResult, error) {
	ctx, cancel := context.WithCancel(ctx)

	r.mu.Lock()
	if r.cancel != nil {
		r.cancel()
	}
	r.cancel = cancel
	r.mu.Unlock()

	result := r.Result
	if len(result) == 0 {
		result = []byte(DefaultNLSML)
	}

	ch := make(chan model.RecognizerResult, 1)
	go func() {
		defer close(ch)
		defer cancel()

		if r.Delay > 0 {
			timer := time.NewTimer(r.Delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case ch <- model.RecognizerResult{NLSML: result}:
		}
	}()
	return ch, nil
}

// Stop implements model.Recognizer.
func (r *Recognizer) Stop() error {
	r.mu.Lock()
	cancel := r.cancel
	r.cancel = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// Synthesizer is a mock speech synthesizer. It produces DefaultFrames frames of
// G.711 mu-law silence and then reports io.EOF.
type Synthesizer struct {
	// Frames is the number of 20 ms frames to produce. Zero means
	// DefaultFrames.
	Frames int

	mu     sync.Mutex
	cancel context.CancelFunc
}

// DefaultFrames is the number of frames produced by the mock synthesizer:
// 50 frames of 20 ms is one second of audio.
const DefaultFrames = 50

// silenceByte is the G.711 mu-law encoding of a zero sample.
const silenceByte = 0xFF

// NewSynthesizer returns a synthesizer with default parameters.
func NewSynthesizer() *Synthesizer {
	return &Synthesizer{Frames: DefaultFrames}
}

// Synthesize implements model.Synthesizer.
func (s *Synthesizer) Synthesize(ctx context.Context, text string) (io.ReadCloser, error) {
	ctx, cancel := context.WithCancel(ctx)

	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.cancel = cancel
	s.mu.Unlock()

	frames := s.Frames
	if frames <= 0 {
		frames = DefaultFrames
	}
	return &frameStream{
		ctx:       ctx,
		cancel:    cancel,
		remaining: frames * 160,
	}, nil
}

// Stop implements model.Synthesizer.
func (s *Synthesizer) Stop() error {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// frameStream yields mu-law silence in frame sized chunks.
type frameStream struct {
	ctx       context.Context
	cancel    context.CancelFunc
	remaining int
	closeOnce sync.Once
}

// Read implements io.Reader.
func (f *frameStream) Read(p []byte) (int, error) {
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	if f.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), f.remaining)
	for i := range p[:n] {
		p[i] = silenceByte
	}
	f.remaining -= n
	return n, nil
}

// Close implements io.Closer.
func (f *frameStream) Close() error {
	f.closeOnce.Do(f.cancel)
	return nil
}

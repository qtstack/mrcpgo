package mock

import (
	"time"

	"github.com/qtstack/mrcpgo/internal/model"
)

// Factory creates a fresh engine per MRCP control channel, so channels never
// share mutable engine state.
type Factory struct {
	// RecognitionDelay is the simulated recognition latency. Zero keeps the
	// engine default.
	RecognitionDelay time.Duration
	// FramesPerUtterance is the number of 20 ms frames a SPEAK produces. Zero
	// keeps the engine default.
	FramesPerUtterance int
}

// NewFactory returns a factory with defaults matching the engine defaults.
func NewFactory() *Factory { return &Factory{} }

// NewRecognizer implements session.EngineFactory.
func (f *Factory) NewRecognizer() (model.Recognizer, error) {
	r := NewRecognizer()
	if f.RecognitionDelay > 0 {
		r.Delay = f.RecognitionDelay
	}
	return r, nil
}

// NewSynthesizer implements session.EngineFactory.
func (f *Factory) NewSynthesizer() (model.Synthesizer, error) {
	s := NewSynthesizer()
	if f.FramesPerUtterance > 0 {
		s.Frames = f.FramesPerUtterance
	}
	return s, nil
}

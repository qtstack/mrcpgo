package xfyun

import (
	"errors"
	"log/slog"

	"github.com/qtstack/mrcpgo/internal/config"
	"github.com/qtstack/mrcpgo/internal/model"
)

// Factory creates iFlytek engines. It is safe to share between channels; it
// holds only immutable configuration.
type Factory struct {
	cfg config.Xfyun
	log *slog.Logger
}

// NewFactory validates the iFlytek settings and creates a factory.
func NewFactory(cfg config.Xfyun, log *slog.Logger) (*Factory, error) {
	if cfg.AppID == "" || cfg.APIKey == "" || cfg.APISecret == "" {
		return nil, errors.New("xfyun: app_id, api_key and api_secret are required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Factory{cfg: cfg, log: log.With("engine", "xfyun")}, nil
}

// NewRecognizer implements engine.Factory.
func (f *Factory) NewRecognizer() (model.Recognizer, error) {
	return newRecognizer(f.cfg, f.log.With("resource", "speechrecog")), nil
}

// NewSynthesizer implements engine.Factory.
func (f *Factory) NewSynthesizer() (model.Synthesizer, error) {
	return newSynthesizer(f.cfg, f.log.With("resource", "speechsynth")), nil
}

package tencent

import (
	"errors"
	"log/slog"
	"strconv"

	"github.com/qtstack/mrcpgo/internal/config"
	"github.com/qtstack/mrcpgo/internal/model"
)

// Factory creates Tencent Cloud engines. It is safe to share between channels;
// it holds only immutable configuration.
type Factory struct {
	cfg config.Tencent
	log *slog.Logger
}

// NewFactory validates the Tencent Cloud settings and creates a factory.
func NewFactory(cfg config.Tencent, log *slog.Logger) (*Factory, error) {
	if cfg.AppID == "" {
		return nil, errors.New("tencent: app_id is required")
	}
	if _, err := strconv.ParseInt(cfg.AppID, 10, 64); err != nil {
		return nil, errors.New("tencent: app_id must be numeric")
	}
	if cfg.SecretID == "" || cfg.SecretKey == "" {
		return nil, errors.New("tencent: secret_id and secret_key are required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Factory{cfg: cfg, log: log.With("engine", "tencent")}, nil
}

// NewRecognizer implements engine.Factory.
func (f *Factory) NewRecognizer() (model.Recognizer, error) {
	return newRecognizer(f.cfg, f.log.With("resource", "speechrecog")), nil
}

// NewSynthesizer implements engine.Factory.
func (f *Factory) NewSynthesizer() (model.Synthesizer, error) {
	return newSynthesizer(f.cfg, f.log.With("resource", "speechsynth")), nil
}

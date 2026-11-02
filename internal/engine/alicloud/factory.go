package alicloud

import (
	"errors"
	"log/slog"

	"github.com/qtstack/mrcpgo/internal/config"
	"github.com/qtstack/mrcpgo/internal/model"
)

// Factory creates Alibaba Cloud engines. One factory is shared by the whole
// process so the token cache is reused across channels.
type Factory struct {
	cfg    config.AliCloud
	log    *slog.Logger
	tokens *TokenSource
}

// NewFactory validates the Alibaba Cloud settings and creates a factory.
func NewFactory(cfg config.AliCloud, log *slog.Logger) (*Factory, error) {
	if cfg.AppKey == "" {
		return nil, errors.New("alicloud: app_key is required")
	}
	if cfg.Token == "" && (cfg.AccessKeyID == "" || cfg.AccessKeySecret == "") {
		return nil, errors.New("alicloud: token or access_key_id and access_key_secret are required")
	}
	if log == nil {
		log = slog.Default()
	}
	log = log.With("engine", "alicloud")
	return &Factory{
		cfg:    cfg,
		log:    log,
		tokens: NewTokenSource(cfg, log),
	}, nil
}

// NewRecognizer implements engine.Factory.
func (f *Factory) NewRecognizer() (model.Recognizer, error) {
	return newRecognizer(f.cfg, f.tokens, f.log.With("resource", "speechrecog")), nil
}

// NewSynthesizer implements engine.Factory.
func (f *Factory) NewSynthesizer() (model.Synthesizer, error) {
	return newSynthesizer(f.cfg, f.tokens, f.log.With("resource", "speechsynth")), nil
}

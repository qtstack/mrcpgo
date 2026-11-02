// Package engine selects and constructs the speech engines described by the
// configuration.
package engine

import (
	"fmt"
	"log/slog"

	"github.com/qtstack/mrcpgo/internal/config"
	"github.com/qtstack/mrcpgo/internal/engine/alicloud"
	"github.com/qtstack/mrcpgo/internal/engine/mock"
	"github.com/qtstack/mrcpgo/internal/engine/tencent"
	"github.com/qtstack/mrcpgo/internal/engine/xfyun"
	"github.com/qtstack/mrcpgo/internal/model"
)

// Factory creates one engine set. It is satisfied by every engine
// implementation and by session.EngineFactory.
type Factory interface {
	NewRecognizer() (model.Recognizer, error)
	NewSynthesizer() (model.Synthesizer, error)
}

// New returns the factory selected by mrcp.engine.type.
func New(cfg config.Config, log *slog.Logger) (Factory, error) {
	if log == nil {
		log = slog.Default()
	}
	switch cfg.MRCP.Engine.Type {
	case "mock":
		factory := mock.NewFactory()
		factory.RecognitionDelay = cfg.MRCP.Engine.RecognitionDelay.Value()
		factory.FramesPerUtterance = cfg.MRCP.Engine.FramesPerUtterance
		return factory, nil
	case "alicloud":
		return alicloud.NewFactory(cfg.MRCP.Engine.AliCloud, log)
	case "tencent":
		return tencent.NewFactory(cfg.MRCP.Engine.Tencent, log)
	case "xfyun":
		return xfyun.NewFactory(cfg.MRCP.Engine.Xfyun, log)
	default:
		return nil, fmt.Errorf("engine: unsupported type %q", cfg.MRCP.Engine.Type)
	}
}

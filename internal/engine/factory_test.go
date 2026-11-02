package engine_test

import (
	"testing"

	"github.com/qtstack/mrcpgo/internal/config"
	"github.com/qtstack/mrcpgo/internal/engine"
)

func TestNewMock(t *testing.T) {
	cfg := config.Default()
	factory, err := engine.New(cfg, nil)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if _, err := factory.NewRecognizer(); err != nil {
		t.Fatalf("NewRecognizer: %v", err)
	}
	if _, err := factory.NewSynthesizer(); err != nil {
		t.Fatalf("NewSynthesizer: %v", err)
	}
}

func TestNewAliCloud(t *testing.T) {
	cfg := config.Default()
	cfg.MRCP.Engine.Type = "alicloud"

	if _, err := engine.New(cfg, nil); err == nil {
		t.Fatal("expected an error without credentials")
	}

	cfg.MRCP.Engine.AliCloud.AppKey = "app"
	cfg.MRCP.Engine.AliCloud.Token = "token"
	factory, err := engine.New(cfg, nil)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if _, err := factory.NewRecognizer(); err != nil {
		t.Fatalf("NewRecognizer: %v", err)
	}
	if _, err := factory.NewSynthesizer(); err != nil {
		t.Fatalf("NewSynthesizer: %v", err)
	}
}

func TestNewTencent(t *testing.T) {
	cfg := config.Default()
	cfg.MRCP.Engine.Type = "tencent"

	if _, err := engine.New(cfg, nil); err == nil {
		t.Fatal("expected an error without credentials")
	}

	cfg.MRCP.Engine.Tencent.AppID = "125922"
	cfg.MRCP.Engine.Tencent.SecretID = "id"
	cfg.MRCP.Engine.Tencent.SecretKey = "key"
	factory, err := engine.New(cfg, nil)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if _, err := factory.NewRecognizer(); err != nil {
		t.Fatalf("NewRecognizer: %v", err)
	}
	if _, err := factory.NewSynthesizer(); err != nil {
		t.Fatalf("NewSynthesizer: %v", err)
	}
}

func TestNewXfyun(t *testing.T) {
	cfg := config.Default()
	cfg.MRCP.Engine.Type = "xfyun"

	if _, err := engine.New(cfg, nil); err == nil {
		t.Fatal("expected an error without credentials")
	}

	cfg.MRCP.Engine.Xfyun.AppID = "app"
	cfg.MRCP.Engine.Xfyun.APIKey = "key"
	cfg.MRCP.Engine.Xfyun.APISecret = "secret"
	factory, err := engine.New(cfg, nil)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if _, err := factory.NewRecognizer(); err != nil {
		t.Fatalf("NewRecognizer: %v", err)
	}
	if _, err := factory.NewSynthesizer(); err != nil {
		t.Fatalf("NewSynthesizer: %v", err)
	}
}

func TestNewUnsupported(t *testing.T) {
	cfg := config.Default()
	cfg.MRCP.Engine.Type = "vosk"
	if _, err := engine.New(cfg, nil); err == nil {
		t.Fatal("expected an error for an unsupported engine")
	}
}

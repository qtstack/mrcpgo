package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SIP.Listen != "0.0.0.0:5060" {
		t.Errorf("sip.listen = %q", cfg.SIP.Listen)
	}
	if cfg.MRCP.Engine.Type != "mock" {
		t.Errorf("engine type = %q", cfg.MRCP.Engine.Type)
	}
	if got := cfg.MRCP.Engine.RecognitionDelay.Value(); got != 200*time.Millisecond {
		t.Errorf("recognition delay = %v", got)
	}
	if cfg.Log.Format != "text" {
		t.Errorf("log format = %q", cfg.Log.Format)
	}
}

func TestLoadExampleConfig(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%s): %v", path, err)
	}
	if cfg.SIP.ContactUser != "mrcpgo" {
		t.Errorf("contact user = %q", cfg.SIP.ContactUser)
	}
	if cfg.MRCP.Engine.FramesPerUtterance != 50 {
		t.Errorf("frames per utterance = %d", cfg.MRCP.Engine.FramesPerUtterance)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("sip:\n  lissten: \"127.0.0.1:5060\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for an unknown field")
	}
}

func TestLoadRejectsInvalidDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("mrcp:\n  engine:\n    recognition_delay: fast\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for an invalid duration")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"bad listen address", func(c *Config) { c.SIP.Listen = "not-an-address" }},
		{"bad log level", func(c *Config) { c.Log.Level = "verbose" }},
		{"bad log format", func(c *Config) { c.Log.Format = "xml" }},
		{"unsupported engine", func(c *Config) { c.MRCP.Engine.Type = "vosk" }},
		{"empty contact user", func(c *Config) { c.SIP.ContactUser = "" }},
		{"empty server name", func(c *Config) { c.SIP.ServerName = "" }},
		{"negative frames", func(c *Config) { c.MRCP.Engine.FramesPerUtterance = -1 }},
		{"bad http listen", func(c *Config) { c.HTTP.Listen = "8080" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

func TestNewLogger(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		cfg := Default()
		cfg.Log.Format = format
		log, err := cfg.NewLogger()
		if err != nil {
			t.Fatalf("%s: NewLogger: %v", format, err)
		}
		if log == nil {
			t.Fatalf("%s: nil logger", format)
		}
	}
}

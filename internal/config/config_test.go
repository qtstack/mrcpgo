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
	engine := cfg.MRCP.Engine.AliCloud
	if engine.Region != "cn-shanghai" {
		t.Errorf("alicloud region = %q", engine.Region)
	}
	if engine.ASR.SampleRate != 8000 || engine.ASR.Format != "pcm" {
		t.Errorf("alicloud asr defaults = %+v", engine.ASR)
	}
	if engine.TTS.SampleRate != 16000 || engine.TTS.Voice != "xiaoyun" {
		t.Errorf("alicloud tts defaults = %+v", engine.TTS)
	}
	tencent := cfg.MRCP.Engine.Tencent
	if tencent.ASR.EngineModel != "16k_zh" || tencent.ASR.SampleRate != 16000 || !tencent.ASR.NeedVAD {
		t.Errorf("tencent asr defaults = %+v", tencent.ASR)
	}
	if tencent.TTS.SampleRate != 16000 {
		t.Errorf("tencent tts defaults = %+v", tencent.TTS)
	}
	xfyun := cfg.MRCP.Engine.Xfyun
	if xfyun.ASR.SampleRate != 16000 || xfyun.ASR.Lang != "cn" || xfyun.ASR.ChunkMS != 40 {
		t.Errorf("xfyun asr defaults = %+v", xfyun.ASR)
	}
	if xfyun.TTS.Voice != "xiaoyan" || xfyun.TTS.Speed != 50 {
		t.Errorf("xfyun tts defaults = %+v", xfyun.TTS)
	}
}

func TestEnvExpansion(t *testing.T) {
	t.Setenv("MRCPGO_TEST_APPKEY", "expanded-app")
	path := filepath.Join(t.TempDir(), "env.yaml")
	content := "mrcp:\n  engine:\n    type: mock\n    alicloud:\n      app_key: ${MRCPGO_TEST_APPKEY}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.MRCP.Engine.AliCloud.AppKey; got != "expanded-app" {
		t.Fatalf("app_key = %q", got)
	}
}

func TestTencentValidation(t *testing.T) {
	valid := func() Config {
		cfg := Default()
		cfg.MRCP.Engine.Type = "tencent"
		cfg.MRCP.Engine.Tencent.AppID = "125922"
		cfg.MRCP.Engine.Tencent.SecretID = "id"
		cfg.MRCP.Engine.Tencent.SecretKey = "key"
		return cfg
	}
	base := valid()
	if err := base.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Tencent)
	}{
		{"missing app id", func(tc *Tencent) { tc.AppID = "" }},
		{"non numeric app id", func(tc *Tencent) { tc.AppID = "abc" }},
		{"missing secret id", func(tc *Tencent) { tc.SecretID = "" }},
		{"missing secret key", func(tc *Tencent) { tc.SecretKey = "" }},
		{"zero timeout", func(tc *Tencent) { tc.Timeout = 0 }},
		{"empty engine model", func(tc *Tencent) { tc.ASR.EngineModel = "" }},
		{"asr sample rate", func(tc *Tencent) { tc.ASR.SampleRate = 44100 }},
		{"asr chunk", func(tc *Tencent) { tc.ASR.ChunkMS = 5 }},
		{"asr vad silence", func(tc *Tencent) { tc.ASR.VadSilenceTime = -1 }},
		{"asr convert mode", func(tc *Tencent) { tc.ASR.ConvertNumMode = 4 }},
		{"tts sample rate", func(tc *Tencent) { tc.TTS.SampleRate = 24000 }},
		{"tts speed", func(tc *Tencent) { tc.TTS.Speed = 7 }},
		{"tts volume", func(tc *Tencent) { tc.TTS.Volume = 11 }},
		{"tts segment rate", func(tc *Tencent) { tc.TTS.SegmentRate = 3 }},
		{"tts emotion intensity", func(tc *Tencent) {
			tc.TTS.EmotionCategory = "happy"
			tc.TTS.EmotionIntensity = 10
		}},
		{"tts emotion without category", func(tc *Tencent) { tc.TTS.EmotionIntensity = 100 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid()
			tt.mutate(&cfg.MRCP.Engine.Tencent)
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

func TestXfyunValidation(t *testing.T) {
	valid := func() Config {
		cfg := Default()
		cfg.MRCP.Engine.Type = "xfyun"
		cfg.MRCP.Engine.Xfyun.AppID = "app"
		cfg.MRCP.Engine.Xfyun.APIKey = "key"
		cfg.MRCP.Engine.Xfyun.APISecret = "secret"
		return cfg
	}
	base := valid()
	if err := base.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Xfyun)
	}{
		{"missing app id", func(x *Xfyun) { x.AppID = "" }},
		{"missing api key", func(x *Xfyun) { x.APIKey = "" }},
		{"missing api secret", func(x *Xfyun) { x.APISecret = "" }},
		{"zero timeout", func(x *Xfyun) { x.Timeout = 0 }},
		{"asr sample rate", func(x *Xfyun) { x.ASR.SampleRate = 8000 }},
		{"asr chunk", func(x *Xfyun) { x.ASR.ChunkMS = 5 }},
		{"tts voice", func(x *Xfyun) { x.TTS.Voice = "" }},
		{"tts sample rate", func(x *Xfyun) { x.TTS.SampleRate = 24000 }},
		{"tts speed", func(x *Xfyun) { x.TTS.Speed = 101 }},
		{"tts volume", func(x *Xfyun) { x.TTS.Volume = -1 }},
		{"tts pitch", func(x *Xfyun) { x.TTS.Pitch = 101 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid()
			tt.mutate(&cfg.MRCP.Engine.Xfyun)
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

func TestAliCloudValidation(t *testing.T) {
	valid := func() Config {
		cfg := Default()
		cfg.MRCP.Engine.Type = "alicloud"
		cfg.MRCP.Engine.AliCloud.AppKey = "app"
		cfg.MRCP.Engine.AliCloud.Token = "token"
		return cfg
	}
	base := valid()
	if err := base.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*AliCloud)
	}{
		{"missing app key", func(a *AliCloud) { a.AppKey = "" }},
		{"missing credentials", func(a *AliCloud) { a.Token = "" }},
		{"empty region", func(a *AliCloud) { a.Region = "" }},
		{"zero timeout", func(a *AliCloud) { a.Timeout = 0 }},
		{"asr format", func(a *AliCloud) { a.ASR.Format = "opus" }},
		{"asr sample rate", func(a *AliCloud) { a.ASR.SampleRate = 44100 }},
		{"asr chunk", func(a *AliCloud) { a.ASR.ChunkMS = 5 }},
		{"tts format", func(a *AliCloud) { a.TTS.Format = "mp3" }},
		{"tts sample rate", func(a *AliCloud) { a.TTS.SampleRate = 11025 }},
		{"tts voice", func(a *AliCloud) { a.TTS.Voice = "" }},
		{"tts volume", func(a *AliCloud) { a.TTS.Volume = 101 }},
		{"tts speech rate", func(a *AliCloud) { a.TTS.SpeechRate = 501 }},
		{"tts pitch rate", func(a *AliCloud) { a.TTS.PitchRate = -501 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid()
			tt.mutate(&cfg.MRCP.Engine.AliCloud)
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected a validation error")
			}
		})
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

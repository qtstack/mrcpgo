package tencent

import (
	"net/url"
	"testing"
)

func TestCanonicalQuery(t *testing.T) {
	got := canonicalQuery(map[string]string{"b": "2", "a": "1", "c": "x y"})
	if want := "a=1&b=2&c=x y"; got != want {
		t.Fatalf("canonical query = %q, want %q", got, want)
	}
	encoded := encodedQuery(map[string]string{"b": "2", "a": "1", "c": "x y"})
	if want := "a=1&b=2&c=x+y"; encoded != want {
		t.Fatalf("encoded query = %q, want %q", encoded, want)
	}
}

func TestSignQueryASR(t *testing.T) {
	// Golden value computed with an independent HMAC-SHA1 implementation for
	// the exact documented ASR signature construction.
	params := map[string]string{
		"engine_model_type": "16k_zh",
		"expired":           "1673494772",
		"needvad":           "1",
		"nonce":             "1673408372",
		"secretid":          "testsecretid",
		"timestamp":         "1673408372",
		"voice_format":      "1",
		"voice_id":          "abc",
	}
	canonical, signature := signQuery("", "asr.cloud.tencent.com/asr/v2/125922", params, "testkey")
	if canonical == "" {
		t.Fatal("empty canonical query")
	}
	const want = "L5ZkvTr5HX8EoBUCrIbhtKLFOl0="
	if signature != want {
		t.Fatalf("signature = %q, want %q", signature, want)
	}
}

func TestSignQueryTTS(t *testing.T) {
	params := map[string]string{
		"Action":     "TextToStreamAudioWSv2",
		"AppId":      "125922",
		"Codec":      "pcm",
		"Expired":    "1688697305",
		"SampleRate": "16000",
		"SecretId":   "testsecretid",
		"SessionId":  "sess-1",
		"Speed":      "0",
		"Timestamp":  "1688610905",
		"VoiceType":  "101001",
		"Volume":     "0",
	}
	_, signature := signQuery("GET", "tts.cloud.tencent.com/stream_wsv2", params, "testkey")
	const want = "dAajab1gaYNhK8U39lbQ1F5DTcc="
	if signature != want {
		t.Fatalf("signature = %q, want %q", signature, want)
	}
}

func TestBuildWebSocketURL(t *testing.T) {
	params := map[string]string{"b": "2", "a": "x y"}
	raw, err := buildWebSocketURL("wss://asr.cloud.tencent.com/asr/v2/125922", "", "signature", params, "key")
	if err != nil {
		t.Fatalf("buildWebSocketURL: %v", err)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Scheme != "wss" || parsed.Host != "asr.cloud.tencent.com" || parsed.Path != "/asr/v2/125922" {
		t.Fatalf("unexpected URL %q", raw)
	}
	query := parsed.Query()
	if query.Get("a") != "x y" || query.Get("b") != "2" {
		t.Errorf("query = %v", query)
	}
	if query.Get("signature") == "" {
		t.Error("missing signature")
	}
}

func TestBuildWebSocketURLRejectsBadScheme(t *testing.T) {
	if _, err := buildWebSocketURL("https://example.com/ws", "", "signature", nil, "key"); err == nil {
		t.Fatal("expected an error")
	}
}

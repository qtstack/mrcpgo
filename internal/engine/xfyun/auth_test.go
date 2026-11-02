package xfyun

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/qtstack/mrcpgo/internal/config"
)

func TestASRSigna(t *testing.T) {
	// Vector from the iFlytek rtasr documentation.
	got := asrSigna("595f23df", "1512041814", "d9f4aa7ea6d94faca62cd88a28fd5234")
	const want = "IrrzsJeOFk1NGfJHW6SkHUoN9CU="
	if got != want {
		t.Fatalf("signa = %q, want %q", got, want)
	}
}

func TestASRURL(t *testing.T) {
	cfg := testConfig()
	cfg.ASREndpoint = "wss://rtasr.xfyun.cn/v1/ws"
	cfg.ASR.Params = map[string]string{"pd": "edu"}

	raw := asrURL(cfg)
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Scheme != "wss" || parsed.Host != "rtasr.xfyun.cn" || parsed.Path != "/v1/ws" {
		t.Fatalf("unexpected URL %q", raw)
	}
	query := parsed.Query()
	if query.Get("appid") != "test-app" {
		t.Errorf("appid = %q", query.Get("appid"))
	}
	if query.Get("ts") == "" {
		t.Error("missing ts")
	}
	if query.Get("signa") == "" {
		t.Error("missing signa")
	}
	if query.Get("lang") != "cn" {
		t.Errorf("lang = %q", query.Get("lang"))
	}
	if query.Get("pd") != "edu" {
		t.Errorf("pd = %q", query.Get("pd"))
	}
}

func TestTTSSignature(t *testing.T) {
	// Golden value computed independently with OpenSSL HMAC-SHA256 for the
	// documented signature_origin layout.
	date := "Thu, 01 Aug 2019 01:53:21 GMT"
	got := ttsSignature("tts-api.xfyun.cn", "/v2/tts", date, "testsecret")
	const want = "UORzD0GI3ke8Qd44BWxQo9VPFNJdlCgnez6+697qbIU="
	if got != want {
		t.Fatalf("signature = %q, want %q", got, want)
	}
}

func TestBuildTTSURL(t *testing.T) {
	now := time.Date(2019, 8, 1, 1, 53, 21, 0, time.UTC)
	raw, err := buildTTSURL("wss://tts-api.xfyun.cn/v2/tts", "test-key", "testsecret", now)
	if err != nil {
		t.Fatalf("buildTTSURL: %v", err)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	query := parsed.Query()
	if query.Get("host") != "tts-api.xfyun.cn" {
		t.Errorf("host = %q", query.Get("host"))
	}
	if got, want := query.Get("date"), "Thu, 01 Aug 2019 01:53:21 GMT"; got != want {
		t.Errorf("date = %q, want %q", got, want)
	}
	authorization, err := base64.StdEncoding.DecodeString(query.Get("authorization"))
	if err != nil {
		t.Fatalf("decode authorization: %v", err)
	}
	origin := string(authorization)
	for _, want := range []string{
		`api_key="test-key"`,
		`algorithm="hmac-sha256"`,
		`headers="host date request-line"`,
		`signature="UORzD0GI3ke8Qd44BWxQo9VPFNJdlCgnez6+697qbIU="`,
	} {
		if !strings.Contains(origin, want) {
			t.Errorf("authorization missing %q:\n%s", want, origin)
		}
	}
}

func TestBuildTTSURLRejectsBadScheme(t *testing.T) {
	if _, err := buildTTSURL("https://tts-api.xfyun.cn/v2/tts", "k", "s", time.Now()); err == nil {
		t.Fatal("expected an error")
	}
}

func testConfig() config.Xfyun {
	cfg := config.Default().MRCP.Engine.Xfyun
	cfg.AppID = "test-app"
	cfg.APIKey = "test-api-key"
	cfg.APISecret = "test-api-secret"
	cfg.Timeout = config.Duration(3 * time.Second)
	return cfg
}

package alicloud

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qtstack/mrcpgo/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testConfig() config.AliCloud {
	cfg := config.Default().MRCP.Engine.AliCloud
	cfg.AppKey = "test-app"
	cfg.Token = "test-token"
	cfg.Timeout = config.Duration(3 * time.Second)
	return cfg
}

func TestSignRPC(t *testing.T) {
	// Golden vector from the Alibaba Cloud RPC signature documentation.
	params := map[string]string{
		"AccessKeyId":      "testid",
		"Action":           "DescribeRegions",
		"Format":           "XML",
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureNonce":   "3ee8c1b8-83d3-44af-a94f-4e0ad82fd6cf",
		"SignatureVersion": "1.0",
		"Timestamp":        "2016-02-23T12:46:24Z",
		"Version":          "2014-05-26",
	}
	got := signRPC("testsecret", "GET", canonicalQuery(params))
	const want = "OLeaidS1JvxuMvnyHOwuJ+uX5qY="
	if got != want {
		t.Fatalf("signature = %q, want %q", got, want)
	}
}

func TestCanonicalQuerySortsKeys(t *testing.T) {
	got := canonicalQuery(map[string]string{"b": "2", "a": "1", "c": "x y"})
	if want := "a=1&b=2&c=x%20y"; got != want {
		t.Fatalf("canonical query = %q, want %q", got, want)
	}
}

func TestTokenSourceStaticToken(t *testing.T) {
	cfg := testConfig()
	ts := NewTokenSource(cfg, testLogger())

	token, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "test-token" {
		t.Fatalf("token = %q", token)
	}
}

func TestTokenSourceFetch(t *testing.T) {
	forms := make(chan map[string]string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		form := map[string]string{}
		for key := range r.PostForm {
			form[key] = r.PostForm.Get(key)
		}
		forms <- form
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Token": map[string]any{
				"Id":         "fetched-token",
				"ExpireTime": time.Now().Add(time.Hour).Unix(),
			},
		})
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.Token = ""
	cfg.AccessKeyID = "akid"
	cfg.AccessKeySecret = "secret"
	cfg.TokenEndpoint = srv.URL
	ts := NewTokenSource(cfg, testLogger())

	token, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "fetched-token" {
		t.Fatalf("token = %q", token)
	}
	gotForm := <-forms
	if gotForm["Action"] != "CreateToken" {
		t.Errorf("Action = %q", gotForm["Action"])
	}
	if gotForm["Version"] != tokenAPIVersion {
		t.Errorf("Version = %q", gotForm["Version"])
	}
	if gotForm["AccessKeyId"] != "akid" {
		t.Errorf("AccessKeyId = %q", gotForm["AccessKeyId"])
	}
	if gotForm["Signature"] == "" {
		t.Error("missing Signature")
	}

	// A second call must be served from the cache, so no new request is made.
	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatalf("cached Token: %v", err)
	}
	select {
	case form := <-forms:
		t.Fatalf("token was fetched twice: %v", form)
	default:
	}
}

func TestTokenSourceError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ErrMsg":"InvalidAccessKeyId.NotFound","RequestId":"abc"}`))
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.Token = ""
	cfg.AccessKeyID = "bad"
	cfg.AccessKeySecret = "bad"
	cfg.TokenEndpoint = srv.URL

	if _, err := NewTokenSource(cfg, testLogger()).Token(context.Background()); err == nil {
		t.Fatal("expected an error")
	} else if !strings.Contains(err.Error(), "InvalidAccessKeyId") {
		t.Fatalf("error = %v", err)
	}
}

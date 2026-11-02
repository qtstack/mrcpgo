package xfyun

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/qtstack/mrcpgo/internal/audio"
	"github.com/qtstack/mrcpgo/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeXfyun is a minimal iFlytek speech WebSocket server.
type fakeXfyun struct {
	srv  *httptest.Server
	errs chan error
}

func newFakeXfyun(t *testing.T, handler func(*websocket.Conn, *http.Request) error) *fakeXfyun {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	errs := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			errs <- err
			return
		}
		defer conn.Close()
		errs <- handler(conn, r)
	}))
	t.Cleanup(srv.Close)
	return &fakeXfyun{srv: srv, errs: errs}
}

func (f *fakeXfyun) wsURL(path string) string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http") + path
}

func (f *fakeXfyun) wait(t *testing.T) {
	t.Helper()
	select {
	case err := <-f.errs:
		if err != nil {
			t.Fatalf("fake iFlytek server: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fake iFlytek server did not finish")
	}
}

func resultFrame(text, resultType string) map[string]any {
	inner := map[string]any{
		"cn": map[string]any{
			"st": map[string]any{
				"rt": []any{
					map[string]any{
						"ws": []any{
							map[string]any{"cw": []any{map[string]any{"w": text}}},
						},
					},
				},
				"type": resultType,
			},
		},
		"seg_id": 0,
	}
	encoded, _ := json.Marshal(inner)
	return map[string]any{
		"action": "result",
		"code":   "0",
		"data":   string(encoded),
		"desc":   "success",
		"sid":    "rta-test",
	}
}

func TestRecognizerAgainstXfyun(t *testing.T) {
	f := newFakeXfyun(t, func(conn *websocket.Conn, r *http.Request) error {
		query := r.URL.Query()
		switch {
		case query.Get("appid") != "test-app":
			return fmt.Errorf("appid = %q", query.Get("appid"))
		case query.Get("ts") == "":
			return fmt.Errorf("missing ts")
		case query.Get("signa") == "":
			return fmt.Errorf("missing signa")
		}
		if err := conn.WriteJSON(map[string]any{"action": "started", "code": "0", "desc": "success"}); err != nil {
			return err
		}

		// 40 ms at 16 kHz is 1280 bytes.
		wantChunk := audio.ChunkBytes(16000, 40)
		for {
			messageType, data, err := conn.ReadMessage()
			if err != nil {
				return err
			}
			if messageType == websocket.BinaryMessage {
				if len(data) != wantChunk {
					return fmt.Errorf("audio frame = %d bytes, want %d", len(data), wantChunk)
				}
				break
			}
		}
		return conn.WriteJSON(resultFrame("打开空调", "0"))
	})

	cfg := testConfig()
	cfg.ASREndpoint = f.wsURL("/v1/ws")
	log := testLogger()
	recognizer := newRecognizer(cfg, log)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	results, err := recognizer.Start(ctx, "builtin:grammar/hello")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Two 20 ms frames make one 40 ms chunk after upsampling to 16 kHz.
	for range 2 {
		if err := recognizer.WriteAudio(bytes.Repeat([]byte{audio.MuLawSilence}, audio.FrameBytes)); err != nil {
			t.Fatalf("WriteAudio: %v", err)
		}
	}

	select {
	case result, ok := <-results:
		if !ok {
			t.Fatal("results channel closed without a result")
		}
		if result.Err != nil {
			t.Fatalf("result error: %v", result.Err)
		}
		doc := string(result.NLSML)
		if !strings.Contains(doc, "打开空调") {
			t.Errorf("NLSML missing text:\n%s", doc)
		}
		if strings.Contains(doc, "confidence=") {
			t.Errorf("iFlytek has no confidence score, attribute should be omitted:\n%s", doc)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a result")
	}
	f.wait(t)
}

func TestRecognizerIgnoresInterimResults(t *testing.T) {
	f := newFakeXfyun(t, func(conn *websocket.Conn, r *http.Request) error {
		if err := conn.WriteJSON(map[string]any{"action": "started", "code": "0"}); err != nil {
			return err
		}
		if err := conn.WriteJSON(resultFrame("打开", "1")); err != nil {
			return err
		}
		if err := conn.WriteJSON(resultFrame("打开空调", "0")); err != nil {
			return err
		}
		return nil
	})

	cfg := testConfig()
	cfg.ASREndpoint = f.wsURL("/v1/ws")
	recognizer := newRecognizer(cfg, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	results, err := recognizer.Start(ctx, "")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	result := <-results
	if result.Err != nil {
		t.Fatalf("result error: %v", result.Err)
	}
	if !strings.Contains(string(result.NLSML), "打开空调") {
		t.Fatalf("NLSML = %s", result.NLSML)
	}
	f.wait(t)
}

// holdUntilClientCloses keeps the connection open after a terminal frame so a
// client that is still reading is not raced by an abrupt TCP close.
func holdUntilClientCloses(conn *websocket.Conn) error {
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return nil
		}
	}
}

func TestRecognizerHandshakeFailure(t *testing.T) {
	f := newFakeXfyun(t, func(conn *websocket.Conn, r *http.Request) error {
		if err := conn.WriteJSON(map[string]any{
			"action": "error",
			"code":   "10110",
			"desc":   "invalid authorization|illegal signa",
		}); err != nil {
			return err
		}
		return holdUntilClientCloses(conn)
	})

	cfg := testConfig()
	cfg.ASREndpoint = f.wsURL("/v1/ws")
	recognizer := newRecognizer(cfg, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := recognizer.Start(ctx, ""); err == nil {
		t.Fatal("expected a handshake error")
	} else if !strings.Contains(err.Error(), "illegal signa") {
		t.Fatalf("error = %v", err)
	}
	f.wait(t)
}

func TestRecognizerContextCancelClosesResults(t *testing.T) {
	f := newFakeXfyun(t, func(conn *websocket.Conn, r *http.Request) error {
		if err := conn.WriteJSON(map[string]any{"action": "started", "code": "0"}); err != nil {
			return err
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return nil
			}
		}
	})

	cfg := testConfig()
	cfg.ASREndpoint = f.wsURL("/v1/ws")
	recognizer := newRecognizer(cfg, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	results, err := recognizer.Start(ctx, "")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()

	select {
	case _, ok := <-results:
		if ok {
			t.Fatal("unexpected result after cancel")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("results channel was not closed after cancel")
	}
	f.wait(t)
}

func TestSynthesizerAgainstXfyun(t *testing.T) {
	const text = "hello from xfyun"
	f := newFakeXfyun(t, func(conn *websocket.Conn, r *http.Request) error {
		query := r.URL.Query()
		if query.Get("authorization") == "" {
			return fmt.Errorf("missing authorization")
		}
		if query.Get("date") == "" {
			return fmt.Errorf("missing date")
		}
		if got := query.Get("host"); got != r.Host {
			return fmt.Errorf("host = %q, request host = %q", got, r.Host)
		}

		messageType, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		if messageType != websocket.TextMessage {
			return fmt.Errorf("request frame type = %d", messageType)
		}
		var req ttsRequest
		if err := json.Unmarshal(data, &req); err != nil {
			return err
		}
		if req.Common.AppID != "test-app" {
			return fmt.Errorf("app_id = %q", req.Common.AppID)
		}
		if req.Business["aue"] != "raw" || req.Business["vcn"] != "xiaoyan" || req.Business["tte"] != "utf8" {
			return fmt.Errorf("business = %v", req.Business)
		}
		if req.Business["auf"] != "audio/L16;rate=16000" {
			return fmt.Errorf("auf = %v", req.Business["auf"])
		}
		if req.Data.Status != 2 {
			return fmt.Errorf("status = %d", req.Data.Status)
		}
		decoded, err := base64.StdEncoding.DecodeString(req.Data.Text)
		if err != nil {
			return err
		}
		if string(decoded) != text {
			return fmt.Errorf("text = %q", decoded)
		}

		return conn.WriteJSON(map[string]any{
			"code": 0,
			"sid":  "tts-test",
			"data": map[string]any{
				"audio":  base64.StdEncoding.EncodeToString(make([]byte, 3200)),
				"status": 2,
			},
		})
	})

	cfg := testConfig()
	cfg.TTSEndpoint = f.wsURL("/v2/tts")
	synthesizer := newSynthesizer(cfg, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := synthesizer.Synthesize(ctx, text)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	defer stream.Close()

	data, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if want := 800; len(data) != want {
		t.Fatalf("audio bytes = %d, want %d", len(data), want)
	}
	for i, b := range data {
		if b != audio.MuLawSilence {
			t.Fatalf("byte %d = %#02x, want %#02x", i, b, audio.MuLawSilence)
		}
	}
	f.wait(t)
}

func TestSynthesizerFailure(t *testing.T) {
	f := newFakeXfyun(t, func(conn *websocket.Conn, r *http.Request) error {
		if err := conn.WriteJSON(map[string]any{"code": 10105, "message": "invalid access", "sid": "s"}); err != nil {
			return err
		}
		return holdUntilClientCloses(conn)
	})

	cfg := testConfig()
	cfg.TTSEndpoint = f.wsURL("/v2/tts")
	synthesizer := newSynthesizer(cfg, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := synthesizer.Synthesize(ctx, "hello")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	defer stream.Close()

	if _, err := io.ReadAll(stream); err == nil {
		t.Fatal("expected a read error")
	} else if !strings.Contains(err.Error(), "invalid access") {
		t.Fatalf("error = %v", err)
	}
	// Close before waiting so the fake server, which holds the connection open
	// for the client, can observe the teardown.
	_ = stream.Close()
	f.wait(t)
}

func TestSynthesizerStopAbortsStream(t *testing.T) {
	f := newFakeXfyun(t, func(conn *websocket.Conn, r *http.Request) error {
		// Consume the request, then stream audio forever.
		if _, _, err := conn.ReadMessage(); err != nil {
			return err
		}
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		audioChunk := base64.StdEncoding.EncodeToString(make([]byte, 1600))
		for {
			if err := conn.WriteJSON(map[string]any{
				"code": 0,
				"data": map[string]any{"audio": audioChunk, "status": 1},
			}); err != nil {
				return nil
			}
		}
	})

	cfg := testConfig()
	cfg.TTSEndpoint = f.wsURL("/v2/tts")
	cfg.TTS.SampleRate = 8000
	synthesizer := newSynthesizer(cfg, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := synthesizer.Synthesize(ctx, "hello")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}

	buf := make([]byte, 160)
	if _, err := io.ReadFull(stream, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := synthesizer.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := stream.Read(buf); err != nil {
			return
		}
	}
	t.Fatal("stream still readable after Stop")
}

func TestFactoryValidation(t *testing.T) {
	if _, err := NewFactory(config.Xfyun{}, testLogger()); err == nil {
		t.Fatal("expected an error for missing credentials")
	}
	cfg := testConfig()
	factory, err := NewFactory(cfg, testLogger())
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	if _, err := factory.NewRecognizer(); err != nil {
		t.Fatalf("NewRecognizer: %v", err)
	}
	if _, err := factory.NewSynthesizer(); err != nil {
		t.Fatalf("NewSynthesizer: %v", err)
	}
}

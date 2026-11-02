package tencent

import (
	"bytes"
	"context"
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

func testConfig() config.Tencent {
	cfg := config.Default().MRCP.Engine.Tencent
	cfg.AppID = "125922"
	cfg.SecretID = "test-secret-id"
	cfg.SecretKey = "test-secret-key"
	cfg.Timeout = config.Duration(3 * time.Second)
	return cfg
}

// fakeTencent is a minimal Tencent Cloud speech WebSocket server.
type fakeTencent struct {
	srv  *httptest.Server
	errs chan error
}

func newFakeTencent(t *testing.T, handler func(*websocket.Conn, *http.Request) error) *fakeTencent {
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
	return &fakeTencent{srv: srv, errs: errs}
}

func (f *fakeTencent) wsURL(path string) string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http") + path
}

func (f *fakeTencent) wait(t *testing.T) {
	t.Helper()
	select {
	case err := <-f.errs:
		if err != nil {
			t.Fatalf("fake Tencent server: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fake Tencent server did not finish")
	}
}

func TestRecognizerAgainstTencent(t *testing.T) {
	f := newFakeTencent(t, func(conn *websocket.Conn, r *http.Request) error {
		query := r.URL.Query()
		switch {
		case query.Get("engine_model_type") != "16k_zh":
			return fmt.Errorf("engine_model_type = %q", query.Get("engine_model_type"))
		case query.Get("voice_format") != "1":
			return fmt.Errorf("voice_format = %q", query.Get("voice_format"))
		case query.Get("needvad") != "1":
			return fmt.Errorf("needvad = %q", query.Get("needvad"))
		case query.Get("secretid") != "test-secret-id":
			return fmt.Errorf("secretid = %q", query.Get("secretid"))
		case query.Get("voice_id") == "":
			return fmt.Errorf("missing voice_id")
		case query.Get("signature") == "":
			return fmt.Errorf("missing signature")
		}

		if err := conn.WriteJSON(map[string]any{
			"code": 0, "message": "success", "voice_id": query.Get("voice_id"),
		}); err != nil {
			return err
		}

		wantChunk := audio.ChunkBytes(16000, 20)
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

		return conn.WriteJSON(map[string]any{
			"code": 0, "message": "success",
			"sentences": map[string]any{
				"sentence_list": []map[string]any{
					{"sentence": "打开空调", "sentence_type": 1, "sentence_id": 0},
				},
			},
		})
	})

	cfg := testConfig()
	cfg.ASREndpoint = f.wsURL("/asr/v2/125922")
	cfg.ASR.ChunkMS = 20
	log := testLogger()
	recognizer := newRecognizer(cfg, log)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	results, err := recognizer.Start(ctx, "builtin:grammar/hello")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := recognizer.WriteAudio(bytes.Repeat([]byte{audio.MuLawSilence}, audio.FrameBytes)); err != nil {
		t.Fatalf("WriteAudio: %v", err)
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
			t.Errorf("Tencent has no confidence score, attribute should be omitted:\n%s", doc)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a result")
	}
	f.wait(t)
}

func TestRecognizerFinalWithoutSentenceIsNoMatch(t *testing.T) {
	f := newFakeTencent(t, func(conn *websocket.Conn, r *http.Request) error {
		if err := conn.WriteJSON(map[string]any{"code": 0, "message": "success"}); err != nil {
			return err
		}
		return conn.WriteJSON(map[string]any{"code": 0, "message": "success", "final": 1})
	})

	cfg := testConfig()
	cfg.ASREndpoint = f.wsURL("/asr/v2/125922")
	log := testLogger()
	recognizer := newRecognizer(cfg, log)

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
	if result.CompletionCause != "001 no-match" {
		t.Fatalf("completion cause = %q", result.CompletionCause)
	}
	f.wait(t)
}

func TestRecognizerHandshakeFailure(t *testing.T) {
	f := newFakeTencent(t, func(conn *websocket.Conn, r *http.Request) error {
		return conn.WriteJSON(map[string]any{"code": 4001, "message": "invalid signature"})
	})

	cfg := testConfig()
	cfg.ASREndpoint = f.wsURL("/asr/v2/125922")
	recognizer := newRecognizer(cfg, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := recognizer.Start(ctx, ""); err == nil {
		t.Fatal("expected a handshake error")
	} else if !strings.Contains(err.Error(), "invalid signature") {
		t.Fatalf("error = %v", err)
	}
	f.wait(t)
}

func TestRecognizerContextCancelClosesResults(t *testing.T) {
	f := newFakeTencent(t, func(conn *websocket.Conn, r *http.Request) error {
		if err := conn.WriteJSON(map[string]any{"code": 0, "message": "success"}); err != nil {
			return err
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return nil
			}
		}
	})

	cfg := testConfig()
	cfg.ASREndpoint = f.wsURL("/asr/v2/125922")
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

func TestSynthesizerAgainstTencent(t *testing.T) {
	const text = "hello from tencent"
	f := newFakeTencent(t, func(conn *websocket.Conn, r *http.Request) error {
		query := r.URL.Query()
		switch {
		case query.Get("Action") != "TextToStreamAudioWSv2":
			return fmt.Errorf("Action = %q", query.Get("Action"))
		case query.Get("AppId") != "125922":
			return fmt.Errorf("AppId = %q", query.Get("AppId"))
		case query.Get("Codec") != "pcm":
			return fmt.Errorf("Codec = %q", query.Get("Codec"))
		case query.Get("Signature") == "":
			return fmt.Errorf("missing Signature")
		}
		sessionID := query.Get("SessionId")

		if err := conn.WriteJSON(map[string]any{
			"code": 0, "message": "success", "session_id": sessionID,
		}); err != nil {
			return err
		}
		if err := conn.WriteJSON(map[string]any{
			"code": 0, "message": "success", "session_id": sessionID, "ready": 1,
		}); err != nil {
			return err
		}

		var actions []actionMessage
		for len(actions) < 2 {
			messageType, data, err := conn.ReadMessage()
			if err != nil {
				return err
			}
			if messageType != websocket.TextMessage {
				continue
			}
			var action actionMessage
			if err := json.Unmarshal(data, &action); err != nil {
				return err
			}
			actions = append(actions, action)
		}
		if actions[0].Action != actionSynthesis || actions[0].Data != text {
			return fmt.Errorf("first action = %+v", actions[0])
		}
		if actions[1].Action != actionComplete {
			return fmt.Errorf("second action = %+v", actions[1])
		}
		if actions[0].SessionID != sessionID || actions[1].SessionID != sessionID {
			return fmt.Errorf("session id mismatch")
		}

		// 100 ms of silence at 16 kHz.
		if err := conn.WriteMessage(websocket.BinaryMessage, make([]byte, 3200)); err != nil {
			return err
		}
		return conn.WriteJSON(map[string]any{
			"code": 0, "message": "success", "session_id": sessionID, "final": 1,
		})
	})

	cfg := testConfig()
	cfg.TTSEndpoint = f.wsURL("/stream_wsv2")
	log := testLogger()
	synthesizer := newSynthesizer(cfg, log)

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
	f := newFakeTencent(t, func(conn *websocket.Conn, r *http.Request) error {
		return conn.WriteJSON(map[string]any{
			"code": 4002, "message": "text is empty",
		})
	})

	cfg := testConfig()
	cfg.TTSEndpoint = f.wsURL("/stream_wsv2")
	synthesizer := newSynthesizer(cfg, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := synthesizer.Synthesize(ctx, "")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	defer stream.Close()

	_, err = io.ReadAll(stream)
	if err == nil {
		t.Fatal("expected a read error")
	}
	if !strings.Contains(err.Error(), "text is empty") {
		t.Fatalf("error = %v", err)
	}
	f.wait(t)
}

func TestSynthesizerStopAbortsStream(t *testing.T) {
	f := newFakeTencent(t, func(conn *websocket.Conn, r *http.Request) error {
		sessionID := r.URL.Query().Get("SessionId")
		if err := conn.WriteJSON(map[string]any{"code": 0, "ready": 1, "session_id": sessionID}); err != nil {
			return err
		}
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		chunk := make([]byte, 1600)
		for {
			if err := conn.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
				return nil
			}
		}
	})

	cfg := testConfig()
	cfg.TTSEndpoint = f.wsURL("/stream_wsv2")
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
	if _, err := NewFactory(config.Tencent{}, testLogger()); err == nil {
		t.Fatal("expected an error for missing credentials")
	}
	cfg := testConfig()
	cfg.AppID = "not-a-number"
	if _, err := NewFactory(cfg, testLogger()); err == nil {
		t.Fatal("expected an error for a non numeric app id")
	}

	cfg = testConfig()
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

// ensure the import is used on all build paths.

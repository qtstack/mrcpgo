package alicloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/qtstack/mrcpgo/internal/audio"
	"github.com/qtstack/mrcpgo/internal/config"
)

// fakeNLS is a minimal NLS WebSocket server used by the engine tests. The
// handler runs on the server goroutine and must return its failures rather than
// calling t.Fatalf.
type fakeNLS struct {
	srv  *httptest.Server
	errs chan error
}

func newFakeNLS(t *testing.T, handler func(*websocket.Conn) error) *fakeNLS {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	errs := make(chan error, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get(headerToken); got != "test-token" {
			errs <- fmt.Errorf("unexpected token %q", got)
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			errs <- err
			return
		}
		defer conn.Close()
		errs <- handler(conn)
	}))
	t.Cleanup(srv.Close)
	return &fakeNLS{srv: srv, errs: errs}
}

func (f *fakeNLS) endpoint() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http")
}

func (f *fakeNLS) wait(t *testing.T) {
	t.Helper()
	select {
	case err := <-f.errs:
		if err != nil {
			t.Fatalf("fake NLS server: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fake NLS server did not finish")
	}
}

type startFrame struct {
	Header struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		AppKey    string `json:"appkey"`
		TaskID    string `json:"task_id"`
	} `json:"header"`
	Payload map[string]any `json:"payload"`
}

func readStart(conn *websocket.Conn) (startFrame, error) {
	messageType, data, err := conn.ReadMessage()
	if err != nil {
		return startFrame{}, fmt.Errorf("read start frame: %w", err)
	}
	if messageType != websocket.TextMessage {
		return startFrame{}, fmt.Errorf("start frame type = %d, want text", messageType)
	}
	var frame startFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		return startFrame{}, fmt.Errorf("decode start frame: %w", err)
	}
	return frame, nil
}

func writeJSON(conn *websocket.Conn, v any) error {
	if err := conn.WriteJSON(v); err != nil {
		return fmt.Errorf("write control frame: %w", err)
	}
	return nil
}

func controlFrame(name string, status int, payload map[string]any) map[string]any {
	frame := map[string]any{
		"header": map[string]any{"name": name, "status": status},
	}
	if payload != nil {
		frame["payload"] = payload
	}
	return frame
}

func TestRecognizerAgainstNLS(t *testing.T) {
	f := newFakeNLS(t, func(conn *websocket.Conn) error {
		start, err := readStart(conn)
		if err != nil {
			return err
		}
		switch {
		case start.Header.Name != nameStartRecognition:
			return fmt.Errorf("start name = %q", start.Header.Name)
		case start.Header.Namespace != nsASR:
			return fmt.Errorf("namespace = %q", start.Header.Namespace)
		case start.Header.AppKey != "test-app":
			return fmt.Errorf("appkey = %q", start.Header.AppKey)
		case start.Header.TaskID == "":
			return fmt.Errorf("missing task_id")
		}
		if got, _ := start.Payload["sample_rate"].(float64); got != 8000 {
			return fmt.Errorf("sample_rate = %v", start.Payload["sample_rate"])
		}
		if got, _ := start.Payload["format"].(string); got != "pcm" {
			return fmt.Errorf("format = %v", start.Payload["format"])
		}

		if err := writeJSON(conn, controlFrame(nameRecognitionStarted, statusSuccess, nil)); err != nil {
			return err
		}

		wantChunk := audio.ChunkBytes(8000, 20)
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

		return writeJSON(conn, controlFrame(nameRecognitionCompleted, statusSuccess, map[string]any{
			"result":     "你好世界",
			"confidence": 0.95,
			"index":      1,
		}))
	})

	cfg := testConfig()
	cfg.Endpoint = f.endpoint()
	cfg.ASR.ChunkMS = 20
	log := testLogger()
	recognizer := newRecognizer(cfg, NewTokenSource(cfg, log), log)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	results, err := recognizer.Start(ctx, "builtin:grammar/hello")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for range 3 {
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
		if !strings.Contains(doc, "你好世界") {
			t.Errorf("NLSML missing text:\n%s", doc)
		}
		if !strings.Contains(doc, `confidence="95"`) {
			t.Errorf("NLSML missing confidence:\n%s", doc)
		}
		if result.CompletionCause != "" {
			t.Errorf("unexpected completion cause %q", result.CompletionCause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a result")
	}
	f.wait(t)

	if err := recognizer.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestRecognizerEmptyResultReportsNoMatch(t *testing.T) {
	f := newFakeNLS(t, func(conn *websocket.Conn) error {
		if _, err := readStart(conn); err != nil {
			return err
		}
		if err := writeJSON(conn, controlFrame(nameRecognitionStarted, statusSuccess, nil)); err != nil {
			return err
		}
		return writeJSON(conn, controlFrame(nameRecognitionCompleted, statusSuccess, map[string]any{"result": ""}))
	})

	cfg := testConfig()
	cfg.Endpoint = f.endpoint()
	log := testLogger()
	recognizer := newRecognizer(cfg, NewTokenSource(cfg, log), log)

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
	if len(result.NLSML) != 0 {
		t.Fatalf("unexpected NLSML: %s", result.NLSML)
	}
	f.wait(t)
}

func TestRecognizerTaskFailed(t *testing.T) {
	f := newFakeNLS(t, func(conn *websocket.Conn) error {
		if _, err := readStart(conn); err != nil {
			return err
		}
		if err := writeJSON(conn, controlFrame(nameRecognitionStarted, statusSuccess, nil)); err != nil {
			return err
		}
		return writeJSON(conn, map[string]any{
			"header": map[string]any{
				"name":        nameTaskFailed,
				"status":      40000001,
				"status_text": "invalid appkey",
			},
		})
	})

	cfg := testConfig()
	cfg.Endpoint = f.endpoint()
	log := testLogger()
	recognizer := newRecognizer(cfg, NewTokenSource(cfg, log), log)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	results, err := recognizer.Start(ctx, "")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	result := <-results
	if result.Err == nil {
		t.Fatal("expected an error result")
	}
	if !strings.Contains(result.Err.Error(), "invalid appkey") {
		t.Fatalf("error = %v", result.Err)
	}
	f.wait(t)
}

func TestRecognizerStopClosesResults(t *testing.T) {
	f := newFakeNLS(t, func(conn *websocket.Conn) error {
		if _, err := readStart(conn); err != nil {
			return err
		}
		if err := writeJSON(conn, controlFrame(nameRecognitionStarted, statusSuccess, nil)); err != nil {
			return err
		}
		for {
			messageType, data, err := conn.ReadMessage()
			if err != nil {
				return nil
			}
			if messageType == websocket.TextMessage && strings.Contains(string(data), nameStopRecognition) {
				return nil
			}
		}
	})

	cfg := testConfig()
	cfg.Endpoint = f.endpoint()
	log := testLogger()
	recognizer := newRecognizer(cfg, NewTokenSource(cfg, log), log)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	results, err := recognizer.Start(ctx, "")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := recognizer.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	select {
	case _, ok := <-results:
		if ok {
			t.Fatal("unexpected result after Stop")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("results channel was not closed after Stop")
	}
	f.wait(t)
}

func TestRecognizerContextCancelClosesResults(t *testing.T) {
	f := newFakeNLS(t, func(conn *websocket.Conn) error {
		if _, err := readStart(conn); err != nil {
			return err
		}
		if err := writeJSON(conn, controlFrame(nameRecognitionStarted, statusSuccess, nil)); err != nil {
			return err
		}
		// Block until the client goes away.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return nil
			}
		}
	})

	cfg := testConfig()
	cfg.Endpoint = f.endpoint()
	log := testLogger()
	recognizer := newRecognizer(cfg, NewTokenSource(cfg, log), log)

	ctx, cancel := context.WithCancel(context.Background())
	results, err := recognizer.Start(ctx, "")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()

	select {
	case _, ok := <-results:
		if ok {
			t.Fatal("unexpected result after context cancel")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("results channel was not closed after context cancel")
	}
	f.wait(t)
}

func TestSynthesizerAgainstNLS(t *testing.T) {
	for _, rate := range []int{8000, 16000} {
		t.Run(fmt.Sprintf("%dHz", rate), func(t *testing.T) {
			f := newFakeNLS(t, func(conn *websocket.Conn) error {
				start, err := readStart(conn)
				if err != nil {
					return err
				}
				switch {
				case start.Header.Name != nameStartSynthesis:
					return fmt.Errorf("start name = %q", start.Header.Name)
				case start.Header.Namespace != nsTTS:
					return fmt.Errorf("namespace = %q", start.Header.Namespace)
				}
				if got, _ := start.Payload["text"].(string); got != "hello" {
					return fmt.Errorf("text = %v", start.Payload["text"])
				}
				if got, _ := start.Payload["voice"].(string); got != "xiaoyun" {
					return fmt.Errorf("voice = %v", start.Payload["voice"])
				}
				if got, _ := start.Payload["sample_rate"].(float64); int(got) != rate {
					return fmt.Errorf("sample_rate = %v", start.Payload["sample_rate"])
				}

				if err := writeJSON(conn, controlFrame(nameMetaInfo, statusSuccess, map[string]any{
					"format": "pcm", "sample_rate": rate,
				})); err != nil {
					return err
				}
				// 100 ms of silence.
				pcm := make([]byte, rate*2/10)
				if err := conn.WriteMessage(websocket.BinaryMessage, pcm); err != nil {
					return err
				}
				return writeJSON(conn, controlFrame(nameSynthesisCompleted, statusSuccess, nil))
			})

			cfg := testConfig()
			cfg.Endpoint = f.endpoint()
			cfg.TTS.SampleRate = rate
			log := testLogger()
			synthesizer := newSynthesizer(cfg, NewTokenSource(cfg, log), log)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			stream, err := synthesizer.Synthesize(ctx, "hello")
			if err != nil {
				t.Fatalf("Synthesize: %v", err)
			}
			defer stream.Close()

			data, err := io.ReadAll(stream)
			if err != nil {
				t.Fatalf("read stream: %v", err)
			}
			// 100 ms at 8 kHz is 800 samples, or 800 mu-law bytes.
			if want := 800; len(data) != want {
				t.Fatalf("audio bytes = %d, want %d", len(data), want)
			}
			for i, b := range data {
				if b != audio.MuLawSilence {
					t.Fatalf("byte %d = %#02x, want %#02x", i, b, audio.MuLawSilence)
				}
			}
			f.wait(t)
		})
	}
}

func TestSynthesizerTaskFailed(t *testing.T) {
	f := newFakeNLS(t, func(conn *websocket.Conn) error {
		if _, err := readStart(conn); err != nil {
			return err
		}
		return writeJSON(conn, map[string]any{
			"header": map[string]any{
				"name":        nameTaskFailed,
				"status":      40000002,
				"status_text": "synthesis rejected",
			},
		})
	})

	cfg := testConfig()
	cfg.Endpoint = f.endpoint()
	log := testLogger()
	synthesizer := newSynthesizer(cfg, NewTokenSource(cfg, log), log)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := synthesizer.Synthesize(ctx, "hello")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	defer stream.Close()

	_, err = io.ReadAll(stream)
	if err == nil {
		t.Fatal("expected a read error")
	}
	if !strings.Contains(err.Error(), "synthesis rejected") {
		t.Fatalf("error = %v", err)
	}
	f.wait(t)
}

func TestSynthesizerStopAbortsStream(t *testing.T) {
	f := newFakeNLS(t, func(conn *websocket.Conn) error {
		if _, err := readStart(conn); err != nil {
			return err
		}
		if err := writeJSON(conn, controlFrame(nameMetaInfo, statusSuccess, nil)); err != nil {
			return err
		}
		// Never complete; write until the client closes the connection.
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		pcm := make([]byte, 1600)
		for {
			if err := conn.WriteMessage(websocket.BinaryMessage, pcm); err != nil {
				return nil
			}
		}
	})

	cfg := testConfig()
	cfg.Endpoint = f.endpoint()
	cfg.TTS.SampleRate = 8000
	log := testLogger()
	synthesizer := newSynthesizer(cfg, NewTokenSource(cfg, log), log)

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
			// The stream reported the closure, which is the expected outcome.
			return
		}
	}
	t.Fatal("stream still readable after Stop")
}

func TestFactoryValidation(t *testing.T) {
	if _, err := NewFactory(config.AliCloud{}, testLogger()); err == nil {
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

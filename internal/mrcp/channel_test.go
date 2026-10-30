package mrcp

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qtstack/mrcpgo/internal/engine/mock"
	"github.com/qtstack/mrcpgo/internal/model"
)

// discardLogger returns a logger that drops every record, keeping test output
// readable.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// frameCounter counts frames handed to the RTP sender.
type frameCounter struct {
	frames atomic.Int64
	bytes  atomic.Int64
}

func (f *frameCounter) SendFrame(frame []byte) error {
	f.frames.Add(1)
	f.bytes.Add(int64(len(frame)))
	return nil
}

type harness struct {
	channel *Channel
	client  net.Conn
	reader  *bufio.Reader
	player  *frameCounter
}

func newHarness(t *testing.T, resource model.Resource, recog model.Recognizer, synth model.Synthesizer) *harness {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	player := &frameCounter{}

	cfg := Config{
		ID:          "test-channel@" + resource.String(),
		Resource:    resource,
		Conn:        serverConn,
		Recognizer:  recog,
		Synthesizer: synth,
		Player:      player,
		Logger:      discardLogger(),
	}
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch.Serve()

	h := &harness{channel: ch, client: clientConn, reader: bufio.NewReader(clientConn), player: player}
	t.Cleanup(func() {
		ch.Close(nil)
		_ = clientConn.Close()
	})
	return h
}

func (h *harness) send(t *testing.T, msg *Message) {
	t.Helper()
	if err := WriteMessage(h.client, msg); err != nil {
		t.Fatalf("write request: %v", err)
	}
}

func (h *harness) sendRequest(t *testing.T, method Method, id uint32, headers Headers, body []byte) {
	t.Helper()
	msg := &Message{
		Type:      TypeRequest,
		Method:    method,
		RequestID: id,
		Headers:   headers,
		Body:      body,
	}
	h.send(t, msg)
}

func (h *harness) read(t *testing.T) *Message {
	t.Helper()
	if err := h.client.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	msg, err := ReadMessage(h.reader)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return msg
}

// expectNothing asserts that no message arrives within d.
func (h *harness) expectNothing(t *testing.T, d time.Duration) {
	t.Helper()
	if err := h.client.SetReadDeadline(time.Now().Add(d)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, err := ReadMessage(h.reader)
	if err == nil {
		t.Fatal("unexpected message")
	}
	var nerr net.Error
	if !errors.As(err, &nerr) || !nerr.Timeout() {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestChannelRejectsMissingChannelIdentifier(t *testing.T) {
	h := newHarness(t, model.ResourceSpeechSynth, nil, mock.NewSynthesizer())
	h.sendRequest(t, MethodSetParams, 1, nil, nil)

	res := h.read(t)
	if res.Type != TypeResponse || res.StatusCode != StatusBadRequest {
		t.Fatalf("response = %+v", res)
	}
}

func TestChannelRejectsUnknownChannel(t *testing.T) {
	h := newHarness(t, model.ResourceSpeechSynth, nil, mock.NewSynthesizer())
	h.sendRequest(t, MethodSetParams, 1, Headers{{HeaderChannelIdentifier, "other@speechsynth"}}, nil)

	res := h.read(t)
	if res.StatusCode != StatusResourceNotAllocated {
		t.Fatalf("status = %d, want %d", res.StatusCode, StatusResourceNotAllocated)
	}
}

func TestChannelSynchronousMethods(t *testing.T) {
	h := newHarness(t, model.ResourceSpeechSynth, nil, mock.NewSynthesizer())
	id := h.channel.ID()

	for _, method := range []Method{MethodSetParams, MethodGetParams, MethodStartInputTimers} {
		h.sendRequest(t, method, 1, Headers{{HeaderChannelIdentifier, id}}, nil)
		res := h.read(t)
		if res.StatusCode != StatusSuccess || res.RequestState != StateComplete {
			t.Errorf("%s: status/state = %d/%s", method, res.StatusCode, res.RequestState)
		}
	}
}

func TestChannelRejectsUnknownMethod(t *testing.T) {
	h := newHarness(t, model.ResourceSpeechSynth, nil, mock.NewSynthesizer())
	h.sendRequest(t, Method("GET-DTMFDIGIT-COLLECTION"), 1,
		Headers{{HeaderChannelIdentifier, h.channel.ID()}}, nil)

	res := h.read(t)
	if res.StatusCode != StatusNotImplemented {
		t.Fatalf("status = %d", res.StatusCode)
	}
}

func TestChannelRejectsRecognizeOnSynthChannel(t *testing.T) {
	h := newHarness(t, model.ResourceSpeechSynth, nil, mock.NewSynthesizer())
	h.sendRequest(t, MethodRecognize, 1, Headers{{HeaderChannelIdentifier, h.channel.ID()}}, nil)

	res := h.read(t)
	if res.StatusCode != StatusMethodNotValidState {
		t.Fatalf("status = %d", res.StatusCode)
	}
}

func TestChannelRecognizeFlow(t *testing.T) {
	recog := mock.NewRecognizer()
	recog.Delay = 10 * time.Millisecond
	h := newHarness(t, model.ResourceSpeechRecog, recog, nil)

	h.sendRequest(t, MethodRecognize, 1,
		Headers{{HeaderChannelIdentifier, h.channel.ID()}},
		[]byte("builtin:grammar/hello"))

	res := h.read(t)
	if res.Type != TypeResponse || res.StatusCode != StatusSuccess || res.RequestState != StateInProgress {
		t.Fatalf("recognize response = %+v", res)
	}

	start := h.read(t)
	if start.Type != TypeEvent || start.EventName != EventStartOfInput || start.RequestState != StateInProgress {
		t.Fatalf("start of input event = %+v", start)
	}

	complete := h.read(t)
	if complete.Type != TypeEvent || complete.EventName != EventRecognitionComplete {
		t.Fatalf("completion event = %+v", complete)
	}
	if complete.RequestState != StateComplete {
		t.Errorf("state = %q", complete.RequestState)
	}
	if complete.ContentType() != ContentTypeNLSML {
		t.Errorf("content type = %q", complete.ContentType())
	}
	if len(complete.Body) == 0 {
		t.Error("empty NLSML body")
	}
	if id := complete.ChannelIdentifier(); id != h.channel.ID() {
		t.Errorf("channel identifier = %q", id)
	}
}

func TestChannelRecognizeRequiresGrammar(t *testing.T) {
	h := newHarness(t, model.ResourceSpeechRecog, mock.NewRecognizer(), nil)
	h.sendRequest(t, MethodRecognize, 1, Headers{{HeaderChannelIdentifier, h.channel.ID()}}, nil)

	res := h.read(t)
	if res.StatusCode != StatusBadRequest {
		t.Fatalf("status = %d", res.StatusCode)
	}
}

func TestChannelRecognizeUsesDefinedGrammar(t *testing.T) {
	recog := mock.NewRecognizer()
	recog.Delay = 0
	h := newHarness(t, model.ResourceSpeechRecog, recog, nil)

	h.sendRequest(t, MethodDefineGrammar, 1,
		Headers{
			{HeaderChannelIdentifier, h.channel.ID()},
			{HeaderContentType, ContentTypeURIList},
		},
		[]byte("builtin:grammar/hello"))
	res := h.read(t)
	if res.StatusCode != StatusSuccess {
		t.Fatalf("define-grammar status = %d", res.StatusCode)
	}

	h.sendRequest(t, MethodRecognize, 2, Headers{{HeaderChannelIdentifier, h.channel.ID()}}, nil)
	res = h.read(t)
	if res.StatusCode != StatusSuccess {
		t.Fatalf("recognize status = %d", res.StatusCode)
	}
	h.read(t) // START-OF-INPUT
	complete := h.read(t)
	if complete.EventName != EventRecognitionComplete {
		t.Fatalf("event = %q", complete.EventName)
	}
}

func TestChannelDefineGrammarRejectsUnsupportedType(t *testing.T) {
	h := newHarness(t, model.ResourceSpeechRecog, mock.NewRecognizer(), nil)
	h.sendRequest(t, MethodDefineGrammar, 1,
		Headers{
			{HeaderChannelIdentifier, h.channel.ID()},
			{HeaderContentType, "application/grammar+xml"},
		},
		[]byte("<grammar/>"))

	res := h.read(t)
	if res.StatusCode != StatusNotImplemented {
		t.Fatalf("status = %d", res.StatusCode)
	}
}

func TestChannelSpeakFlow(t *testing.T) {
	synth := mock.NewSynthesizer()
	synth.Frames = 2
	h := newHarness(t, model.ResourceSpeechSynth, nil, synth)

	h.sendRequest(t, MethodSpeak, 1, Headers{{HeaderChannelIdentifier, h.channel.ID()}},
		[]byte("hello world"))

	res := h.read(t)
	if res.StatusCode != StatusSuccess || res.RequestState != StateInProgress {
		t.Fatalf("speak response = %+v", res)
	}

	complete := h.read(t)
	if complete.Type != TypeEvent || complete.EventName != EventSpeakComplete {
		t.Fatalf("event = %+v", complete)
	}
	if got := h.player.frames.Load(); got != 2 {
		t.Errorf("frames sent = %d, want 2", got)
	}
	if got := h.player.bytes.Load(); got != 2*160 {
		t.Errorf("bytes sent = %d, want %d", got, 2*160)
	}
}

func TestChannelStopSuppressesCompletionEvent(t *testing.T) {
	recog := mock.NewRecognizer()
	recog.Delay = time.Second
	h := newHarness(t, model.ResourceSpeechRecog, recog, nil)

	h.sendRequest(t, MethodRecognize, 1, Headers{{HeaderChannelIdentifier, h.channel.ID()}}, []byte("grammar"))
	if res := h.read(t); res.RequestState != StateInProgress {
		t.Fatalf("recognize response = %+v", res)
	}
	if ev := h.read(t); ev.EventName != EventStartOfInput {
		t.Fatalf("event = %q", ev.EventName)
	}

	h.sendRequest(t, MethodStop, 2, Headers{{HeaderChannelIdentifier, h.channel.ID()}}, nil)
	res := h.read(t)
	if res.RequestID != 2 || res.StatusCode != StatusSuccess || res.RequestState != StateComplete {
		t.Fatalf("stop response = %+v", res)
	}

	// The recognizer would report after one second; STOP must suppress it.
	h.expectNothing(t, 1500*time.Millisecond)
}

func TestChannelCloseIsIdempotent(t *testing.T) {
	h := newHarness(t, model.ResourceSpeechSynth, nil, mock.NewSynthesizer())
	h.channel.Close(nil)
	h.channel.Close(nil)
}

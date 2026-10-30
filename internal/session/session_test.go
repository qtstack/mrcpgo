package session_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qtstack/mrcpgo/internal/engine/mock"
	"github.com/qtstack/mrcpgo/internal/media"
	"github.com/qtstack/mrcpgo/internal/model"
	"github.com/qtstack/mrcpgo/internal/mrcp"
	"github.com/qtstack/mrcpgo/internal/session"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeDialog is a stand in for sipgo's dialog session.
type fakeDialog struct {
	ctx    context.Context
	cancel context.CancelFunc
	closes atomic.Int64
}

func newFakeDialog() *fakeDialog {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeDialog{ctx: ctx, cancel: cancel}
}

func (d *fakeDialog) Close() error { d.closes.Add(1); return nil }

func (d *fakeDialog) Context() context.Context { return d.ctx }

// spyRecognizer counts Start and Stop calls while delegating to the mock.
type spyRecognizer struct {
	*mock.Recognizer
	starts atomic.Int64
	stops  atomic.Int64
}

func (s *spyRecognizer) Start(ctx context.Context, grammarURI string) (<-chan model.RecognizerResult, error) {
	s.starts.Add(1)
	return s.Recognizer.Start(ctx, grammarURI)
}

func (s *spyRecognizer) Stop() error {
	s.stops.Add(1)
	return s.Recognizer.Stop()
}

type spyFactory struct {
	mu          sync.Mutex
	recognizers []*spyRecognizer
	synths      []*mock.Synthesizer
	delay       time.Duration
}

func (f *spyFactory) NewRecognizer() (model.Recognizer, error) {
	r := &spyRecognizer{Recognizer: mock.NewRecognizer()}
	if f.delay > 0 {
		r.Delay = f.delay
	}
	f.mu.Lock()
	f.recognizers = append(f.recognizers, r)
	f.mu.Unlock()
	return r, nil
}

func (f *spyFactory) NewSynthesizer() (model.Synthesizer, error) {
	s := mock.NewSynthesizer()
	f.mu.Lock()
	f.synths = append(f.synths, s)
	f.mu.Unlock()
	return s, nil
}

func (f *spyFactory) lastRecognizer() *spyRecognizer {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.recognizers) == 0 {
		return nil
	}
	return f.recognizers[len(f.recognizers)-1]
}

// sessionFixture assembles a session backed by real loopback sockets.
type sessionFixture struct {
	session  *session.Session
	dialog   *fakeDialog
	factory  *spyFactory
	addr     string
	closed   atomic.Int64
	closedCh chan model.SessionState
}

func newFixture(t *testing.T, resource model.Resource, delay time.Duration) *sessionFixture {
	t.Helper()

	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen UDP: %v", err)
	}
	trx, err := media.New(udpConn, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}, testLogger())
	if err != nil {
		t.Fatalf("new transceiver: %v", err)
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		_ = trx.Close()
		t.Fatalf("listen TCP: %v", err)
	}

	factory := &spyFactory{delay: delay}
	dialog := newFakeDialog()
	fixture := &sessionFixture{
		dialog:   dialog,
		factory:  factory,
		addr:     listener.Addr().String(),
		closedCh: make(chan model.SessionState, 1),
	}

	channelID := "chan@" + resource.String()
	sess, err := session.New(session.Config{
		CallID:    "call-" + resource.String(),
		RemoteSDP: []byte("v=0"),
		Dialog:    dialog,
		RTP:       trx,
		Controls: []session.ControlEndpoint{{
			ID:       channelID,
			Resource: resource,
			Listener: listener,
		}},
		Engines: factory,
		Logger:  testLogger(),
		OnClosed: func(s *session.Session) {
			fixture.closed.Add(1)
			fixture.closedCh <- s.State()
		},
	})
	if err != nil {
		_ = trx.Close()
		_ = listener.Close()
		t.Fatalf("new session: %v", err)
	}
	fixture.session = sess
	t.Cleanup(func() { _ = sess.Close(errors.New("test cleanup")) })
	return fixture
}

func (f *sessionFixture) connect(t *testing.T) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", f.addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial control channel: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, bufio.NewReader(conn)
}

func writeRequest(t *testing.T, conn net.Conn, method mrcp.Method, requestID uint32, channelID string, body []byte) {
	t.Helper()
	msg := &mrcp.Message{
		Type:      mrcp.TypeRequest,
		Method:    method,
		RequestID: requestID,
		Headers:   mrcp.Headers{{Name: mrcp.HeaderChannelIdentifier, Value: channelID}},
		Body:      body,
	}
	if err := mrcp.WriteMessage(conn, msg); err != nil {
		t.Fatalf("write request: %v", err)
	}
}

func readMessage(t *testing.T, conn net.Conn, reader *bufio.Reader) *mrcp.Message {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	msg, err := mrcp.ReadMessage(reader)
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	return msg
}

func TestSessionLifecycle(t *testing.T) {
	f := newFixture(t, model.ResourceSpeechRecog, 10*time.Millisecond)
	f.session.Start()

	if got := f.session.State(); got != model.SessionCreated {
		t.Fatalf("state = %s, want created", got)
	}
	if !f.session.Activate() {
		t.Fatal("Activate returned false")
	}
	if got := f.session.State(); got != model.SessionActive {
		t.Fatalf("state = %s, want active", got)
	}
	if f.session.Activate() {
		t.Fatal("second Activate should be a no-op")
	}

	if err := f.session.Close(errors.New("test")); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := f.session.State(); got != model.SessionClosed {
		t.Fatalf("state = %s, want closed", got)
	}

	select {
	case state := <-f.closedCh:
		if state != model.SessionReleasing {
			t.Fatalf("OnClosed observed state %s, want releasing", state)
		}
	default:
		t.Fatal("OnClosed was not called")
	}
	if f.closed.Load() != 1 {
		t.Fatalf("OnClosed calls = %d", f.closed.Load())
	}
	if f.dialog.closes.Load() != 1 {
		t.Fatalf("dialog Close calls = %d", f.dialog.closes.Load())
	}
}

func TestSessionCloseIsIdempotent(t *testing.T) {
	f := newFixture(t, model.ResourceSpeechSynth, 0)
	f.session.Start()
	f.session.Activate()

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = f.session.Close(errors.New("concurrent"))
		}()
	}
	wg.Wait()
	_ = f.session.Close(nil)

	if got := f.closed.Load(); got != 1 {
		t.Fatalf("OnClosed calls = %d, want 1", got)
	}
	if got := f.session.State(); got != model.SessionClosed {
		t.Fatalf("state = %s", got)
	}
}

func TestSessionReleasesListener(t *testing.T) {
	f := newFixture(t, model.ResourceSpeechSynth, 0)
	f.session.Start()
	addr := f.addr

	if err := f.session.Close(nil); err != nil {
		t.Fatalf("Close: %v", err)
	}

	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("control listener still accepts connections after Close")
	}
}

func TestSessionGatesRequestsUntilActivate(t *testing.T) {
	f := newFixture(t, model.ResourceSpeechRecog, 0)
	f.session.Start()

	conn, reader := f.connect(t)
	writeRequest(t, conn, mrcp.MethodSetParams, 1, "chan@speechrecog", nil)

	if err := conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := mrcp.ReadMessage(reader); err == nil {
		t.Fatal("request was answered before the dialog was activated")
	}

	f.session.Activate()
	res := readMessage(t, conn, reader)
	if res.StatusCode != mrcp.StatusSuccess {
		t.Fatalf("status = %d", res.StatusCode)
	}
}

func TestSessionCallsEngineStartAndStop(t *testing.T) {
	f := newFixture(t, model.ResourceSpeechRecog, 30*time.Second)
	f.session.Start()
	f.session.Activate()

	conn, reader := f.connect(t)
	writeRequest(t, conn, mrcp.MethodRecognize, 1, "chan@speechrecog", []byte("builtin:grammar/hello"))

	res := readMessage(t, conn, reader)
	if res.RequestState != mrcp.StateInProgress {
		t.Fatalf("response state = %q", res.RequestState)
	}
	if ev := readMessage(t, conn, reader); ev.EventName != mrcp.EventStartOfInput {
		t.Fatalf("event = %q", ev.EventName)
	}

	recog := f.factory.lastRecognizer()
	if recog == nil {
		t.Fatal("no recognizer created")
	}
	if got := recog.starts.Load(); got != 1 {
		t.Fatalf("Start calls = %d, want 1", got)
	}

	// Closing the session with an in-flight request must abort the engine.
	if err := f.session.Close(errors.New("teardown")); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := recog.stops.Load(); got != 1 {
		t.Fatalf("Stop calls = %d, want 1", got)
	}
}

func TestSessionClosesOnDialogTermination(t *testing.T) {
	f := newFixture(t, model.ResourceSpeechSynth, 0)
	f.session.Start()
	f.session.Activate()

	f.dialog.cancel()

	select {
	case <-f.closedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("session did not close when the dialog terminated")
	}
	if got := f.session.State(); got != model.SessionClosed {
		t.Fatalf("state = %s", got)
	}
}

func TestManager(t *testing.T) {
	m := session.NewManager(testLogger())

	f := newFixture(t, model.ResourceSpeechSynth, 0)
	f.session.Start()

	if err := m.Add(f.session); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := m.Add(f.session); err == nil {
		t.Fatal("duplicate Add should fail")
	}
	if got, ok := m.Get(f.session.CallID()); !ok || got != f.session {
		t.Fatal("Get returned the wrong session")
	}
	if m.Len() != 1 {
		t.Fatalf("Len = %d", m.Len())
	}

	m.Remove(f.session)
	if m.Len() != 0 {
		t.Fatalf("Len = %d after Remove", m.Len())
	}
	if _, ok := m.Get(f.session.CallID()); ok {
		t.Fatal("Get returned a removed session")
	}
}

func TestManagerShutdown(t *testing.T) {
	m := session.NewManager(testLogger())

	var fixtures []*sessionFixture
	for _, resource := range []model.Resource{model.ResourceSpeechRecog, model.ResourceSpeechSynth} {
		f := newFixture(t, resource, 0)
		f.session.Start()
		f.session.Activate()
		if err := m.Add(f.session); err != nil {
			t.Fatalf("Add: %v", err)
		}
		fixtures = append(fixtures, f)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if m.Len() != 0 {
		t.Fatalf("Len = %d after shutdown", m.Len())
	}
	for _, f := range fixtures {
		if got := f.session.State(); got != model.SessionClosed {
			t.Errorf("state = %s, want closed", got)
		}
	}
}

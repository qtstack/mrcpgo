package mrcp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/qtstack/mrcpgo/internal/model"
	"github.com/qtstack/mrcpgo/internal/syncx"
)

const (
	// ptime is the RTP packetisation interval for PCMU: 20 ms, i.e. 160 bytes
	// at 8 kHz.
	ptime = 20 * time.Millisecond
	// frameSize is the number of bytes in one 20 ms G.711 mu-law frame.
	frameSize = 160
	// writeTimeout bounds a single control connection write so a stalled peer
	// cannot wedge the writer.
	writeTimeout = 5 * time.Second
	// closeTimeout bounds how long Close waits for tracked goroutines.
	closeTimeout = 5 * time.Second
)

// Content types understood by this implementation.
const (
	ContentTypeNLSML   = "application/nlsml+xml"
	ContentTypeSRGS    = "application/srgs+xml"
	ContentTypeURIList = "text/uri-list"
	ContentTypeSSML    = "application/ssml+xml"
)

// FrameSender sends one G.711 mu-law frame to the RTP peer. It is implemented
// by media.Transceiver; declaring it here keeps this package free of an
// internal/media dependency.
type FrameSender interface {
	SendFrame(frame []byte) error
}

// Config describes a single MRCPv2 control channel.
type Config struct {
	// ID is the full Channel-Identifier value, "<session-id>@<resource>".
	ID string
	// Resource is the resource type encoded in ID.
	Resource model.Resource
	// Conn is the accepted control connection. The channel takes ownership.
	Conn net.Conn
	// Recognizer serves RECOGNIZE. Required for ResourceSpeechRecog.
	Recognizer model.Recognizer
	// Synthesizer serves SPEAK. Required for ResourceSpeechSynth.
	Synthesizer model.Synthesizer
	// Player receives synthesised frames. Optional; when nil frames are
	// generated but not transmitted.
	Player FrameSender
	// Logger receives structured logs. Defaults to slog.Default().
	Logger *slog.Logger
	// Ready, when non-nil, gates request processing until it is closed. The
	// SIP layer closes it once the dialog is confirmed by an ACK.
	Ready <-chan struct{}
	// OnClose is invoked once when the channel stops, either because the peer
	// closed the connection or because the channel was closed locally. The
	// callback must not call Close synchronously.
	OnClose func(error)
}

// Channel is one MRCPv2 control connection bound to one resource.
//
// Exactly one goroutine reads from the connection and dispatches requests;
// responses and events may be written from engine goroutines, so all writes
// are serialised by writeMu.
type Channel struct {
	id       string
	resource model.Resource
	conn     net.Conn
	recog    model.Recognizer
	synth    model.Synthesizer
	player   FrameSender
	log      *slog.Logger
	ready    <-chan struct{}
	onClose  func(error)

	ctx    context.Context
	cancel context.CancelFunc

	writeMu sync.Mutex

	// mu guards the fields below. State transitions and event emission both
	// take mu, which guarantees that no completion event can be written after
	// a STOP response has cleared the active request.
	mu         sync.Mutex
	active     *activeRequest
	grammarURI string
	closing    bool

	wg         sync.WaitGroup
	closeOnce  sync.Once
	notifyOnce sync.Once
}

// activeRequest is the request currently occupying the channel.
type activeRequest struct {
	id     uint32
	method Method
	cancel context.CancelFunc
}

// New validates cfg and creates a channel. Serve must be called to start
// reading.
func New(cfg Config) (*Channel, error) {
	if cfg.ID == "" {
		return nil, errors.New("mrcp: channel requires an ID")
	}
	if cfg.Conn == nil {
		return nil, errors.New("mrcp: channel requires a connection")
	}
	if !cfg.Resource.Valid() {
		return nil, fmt.Errorf("mrcp: unsupported resource %q", cfg.Resource)
	}
	if cfg.Resource == model.ResourceSpeechRecog && cfg.Recognizer == nil {
		return nil, errors.New("mrcp: recognizer channel requires a recognizer")
	}
	if cfg.Resource == model.ResourceSpeechSynth && cfg.Synthesizer == nil {
		return nil, errors.New("mrcp: synthesizer channel requires a synthesizer")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("channel_id", cfg.ID, "resource", cfg.Resource.String())

	ctx, cancel := context.WithCancel(context.Background())
	return &Channel{
		id:       cfg.ID,
		resource: cfg.Resource,
		conn:     cfg.Conn,
		recog:    cfg.Recognizer,
		synth:    cfg.Synthesizer,
		player:   cfg.Player,
		log:      log,
		ready:    cfg.Ready,
		onClose:  cfg.OnClose,
		ctx:      ctx,
		cancel:   cancel,
	}, nil
}

// ID returns the Channel-Identifier value.
func (c *Channel) ID() string { return c.id }

// Resource returns the resource type served by this channel.
func (c *Channel) Resource() model.Resource { return c.resource }

// RemoteAddr returns the peer address of the control connection.
func (c *Channel) RemoteAddr() string {
	if addr := c.conn.RemoteAddr(); addr != nil {
		return addr.String()
	}
	return ""
}

// Serve starts the read loop. It returns immediately.
func (c *Channel) Serve() {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		if err := c.serve(); err != nil {
			c.notifyClose(err)
		}
	}()
}

func (c *Channel) serve() error {
	if c.ready != nil {
		select {
		case <-c.ready:
		case <-c.ctx.Done():
			return nil
		}
	}

	reader := bufio.NewReader(c.conn)
	for {
		msg, err := ReadMessage(reader)
		if err != nil {
			if c.ctx.Err() != nil {
				// Local shutdown; not a peer error.
				return nil
			}
			return fmt.Errorf("mrcp: read message: %w", err)
		}
		c.dispatch(msg)
	}
}

// Close stops the channel, terminating any in-flight request and closing the
// control connection. It blocks until goroutines exit or the timeout elapses.
// Close is idempotent and safe to call concurrently.
//
// DECISION: an in-flight request is terminated with a context cancellation and
// an engine Stop call instead of writing an MRCP STOP request. In RFC 6787
// STOP is a client-to-server request; the server signals teardown by closing
// the control connection, which terminates every request on it (section 4.3).
func (c *Channel) Close(reason error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closing = true
		active := c.active
		c.active = nil
		c.mu.Unlock()

		if active != nil {
			c.log.Debug("terminating in-flight request", "request_id", active.id,
				"mrcp_method", active.method, "reason", reason)
			active.cancel()
			c.stopEngine(active.method)
		}

		c.cancel()
		if err := c.conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			c.log.Debug("closing control connection", "error", err)
		}

		if !syncx.WaitTimeout(&c.wg, closeTimeout) {
			c.log.Warn("timed out waiting for channel goroutines to exit")
		}
	})
}

// dispatch validates and handles one inbound message.
func (c *Channel) dispatch(msg *Message) {
	if msg.Type != TypeRequest {
		c.log.Warn("ignoring non-request message", "mrcp_type", msg.Type.String())
		return
	}

	chID, ok := msg.Headers.Get(HeaderChannelIdentifier)
	if !ok {
		c.log.Warn("rejecting request without Channel-Identifier", "mrcp_method", msg.Method)
		c.respond(msg, StatusBadRequest, StateComplete)
		return
	}
	if chID != c.id {
		c.log.Warn("rejecting request for unknown channel",
			"mrcp_method", msg.Method, "received_channel", chID)
		c.respond(msg, StatusResourceNotAllocated, StateComplete)
		return
	}

	switch msg.Method {
	case MethodSetParams, MethodGetParams, MethodStartInputTimers:
		c.respond(msg, StatusSuccess, StateComplete)
	case MethodDefineGrammar:
		c.handleDefineGrammar(msg)
	case MethodRecognize:
		c.handleRecognize(msg)
	case MethodSpeak:
		c.handleSpeak(msg)
	case MethodStop:
		c.handleStop(msg)
	default:
		c.log.Warn("unsupported MRCP method", "mrcp_method", msg.Method)
		c.respond(msg, StatusNotImplemented, StateComplete)
	}
}

// handleDefineGrammar records the grammar reference used by a later RECOGNIZE.
// Only the minimal grammar content types are supported; synthesis of a full
// grammar store is out of scope.
func (c *Channel) handleDefineGrammar(msg *Message) {
	if c.resource != model.ResourceSpeechRecog {
		c.respond(msg, StatusMethodNotValidState, StateComplete)
		return
	}
	grammar := strings.TrimSpace(string(msg.Body))
	if grammar == "" {
		c.respond(msg, StatusBadRequest, StateComplete)
		return
	}
	switch msg.ContentType() {
	case ContentTypeSRGS, ContentTypeURIList:
	default:
		c.log.Warn("unsupported grammar content type", "content_type", msg.ContentType())
		c.respond(msg, StatusNotImplemented, StateComplete)
		return
	}

	c.mu.Lock()
	c.grammarURI = grammar
	c.mu.Unlock()
	c.respond(msg, StatusSuccess, StateComplete)
}

// handleRecognize starts asynchronous recognition and answers IN-PROGRESS.
func (c *Channel) handleRecognize(msg *Message) {
	if c.resource != model.ResourceSpeechRecog {
		c.respond(msg, StatusMethodNotValidState, StateComplete)
		return
	}

	c.mu.Lock()
	if c.active != nil {
		busy := c.active
		c.mu.Unlock()
		c.log.Debug("recognize rejected: channel busy", "active_request_id", busy.id)
		c.respond(msg, StatusMethodNotValidState, StateComplete)
		return
	}
	grammar := strings.TrimSpace(string(msg.Body))
	if grammar == "" {
		grammar = c.grammarURI
	}
	c.mu.Unlock()

	if grammar == "" {
		c.log.Warn("recognize rejected: no grammar")
		c.respond(msg, StatusBadRequest, StateComplete)
		return
	}

	ctx, cancel := context.WithCancel(c.ctx)
	results, err := c.recog.Start(ctx, grammar)
	if err != nil {
		cancel()
		c.log.Error("recognizer failed to start", "error", err, "request_id", msg.RequestID)
		c.respond(msg, StatusInternalError, StateComplete)
		return
	}

	active := &activeRequest{id: msg.RequestID, method: MethodRecognize, cancel: cancel}
	if !c.setActive(active) {
		cancel()
		c.respond(msg, StatusMethodNotValidState, StateComplete)
		return
	}

	// Write IN-PROGRESS before starting the result watcher so the first event
	// can never overtake the response it belongs to.
	c.respond(msg, StatusSuccess, StateInProgress)
	c.log.Info("recognition started", "request_id", msg.RequestID, "grammar", grammar)

	c.goTracked(func() {
		defer cancel()
		c.recognitionLoop(ctx, msg.RequestID, results)
	})
}

func (c *Channel) recognitionLoop(ctx context.Context, reqID uint32, results <-chan model.RecognizerResult) {
	// DECISION: the mock recognizer has no VAD, so the channel reports
	// START-OF-INPUT as soon as recognition begins. A real engine would emit
	// this event when it detects speech onset.
	c.emitIfActive(reqID, func() {
		c.sendEvent(reqID, EventStartOfInput, StateInProgress, "", nil)
	})

	for {
		select {
		case <-ctx.Done():
			return
		case res, ok := <-results:
			if !ok {
				// The engine stopped without producing a result, which only
				// happens on cancellation. Clear the request so the channel
				// returns to idle.
				c.clearActive(reqID)
				return
			}
			cause := res.CompletionCause
			if cause == "" {
				if res.Err != nil {
					cause = model.CauseNoInput
				} else {
					cause = model.CauseSuccess
				}
			}
			contentType := ""
			if len(res.NLSML) > 0 {
				contentType = ContentTypeNLSML
			}
			c.finishIfActive(reqID, func() {
				c.sendEventWithCause(reqID, EventRecognitionComplete, cause, contentType, res.NLSML)
			})
			return
		}
	}
}

// handleSpeak starts asynchronous synthesis and answers IN-PROGRESS.
func (c *Channel) handleSpeak(msg *Message) {
	if c.resource != model.ResourceSpeechSynth {
		c.respond(msg, StatusMethodNotValidState, StateComplete)
		return
	}

	c.mu.Lock()
	if c.active != nil {
		busy := c.active
		c.mu.Unlock()
		c.log.Debug("speak rejected: channel busy", "active_request_id", busy.id)
		c.respond(msg, StatusMethodNotValidState, StateComplete)
		return
	}
	c.mu.Unlock()

	text := string(msg.Body)
	ctx, cancel := context.WithCancel(c.ctx)
	stream, err := c.synth.Synthesize(ctx, text)
	if err != nil {
		cancel()
		c.log.Error("synthesizer failed to start", "error", err, "request_id", msg.RequestID)
		c.respond(msg, StatusInternalError, StateComplete)
		return
	}

	active := &activeRequest{id: msg.RequestID, method: MethodSpeak, cancel: cancel}
	if !c.setActive(active) {
		cancel()
		_ = stream.Close()
		c.respond(msg, StatusMethodNotValidState, StateComplete)
		return
	}

	c.respond(msg, StatusSuccess, StateInProgress)
	c.log.Info("synthesis started", "request_id", msg.RequestID, "text_bytes", len(text))

	c.goTracked(func() {
		defer cancel()
		c.speechLoop(ctx, msg.RequestID, stream)
	})
}

// speechLoop forwards one 160 byte frame every 20 ms until the stream ends.
func (c *Channel) speechLoop(ctx context.Context, reqID uint32, stream io.ReadCloser) {
	defer stream.Close()

	ticker := time.NewTicker(ptime)
	defer ticker.Stop()

	frame := make([]byte, frameSize)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		n, err := io.ReadFull(stream, frame)
		if n > 0 && c.player != nil {
			if sendErr := c.player.SendFrame(frame[:n]); sendErr != nil {
				c.log.Warn("failed to send RTP frame", "error", sendErr, "request_id", reqID)
			}
		}
		switch {
		case err == nil:
			continue
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			c.finishIfActive(reqID, func() {
				c.sendEventWithCause(reqID, EventSpeakComplete, model.CauseSuccess, "", nil)
			})
			return
		default:
			c.log.Error("synthesis stream failed", "error", err, "request_id", reqID)
			c.finishIfActive(reqID, func() {
				c.sendEventWithCause(reqID, EventSpeakComplete, model.CauseNoInput, "", nil)
			})
			return
		}
	}
}

// handleStop terminates the active request, if any, and answers COMPLETE.
func (c *Channel) handleStop(msg *Message) {
	c.mu.Lock()
	active := c.active
	c.active = nil
	c.mu.Unlock()

	if active != nil {
		active.cancel()
		c.log.Info("request stopped", "request_id", active.id, "mrcp_method", active.method)
		// Engine teardown may involve a network round trip to the speech
		// service; answer the STOP first and abort in the background.
		c.goTracked(func() { c.stopEngine(active.method) })
	}
	c.respond(msg, StatusSuccess, StateComplete)
}

// stopEngine aborts the engine serving method.
func (c *Channel) stopEngine(method Method) {
	var err error
	switch method {
	case MethodRecognize:
		if c.recog != nil {
			err = c.recog.Stop()
		}
	case MethodSpeak:
		if c.synth != nil {
			err = c.synth.Stop()
		}
	}
	if err != nil {
		c.log.Debug("engine stop failed", "error", err, "mrcp_method", method)
	}
}

// setActive claims the channel for req.
func (c *Channel) setActive(req *activeRequest) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing || c.active != nil {
		return false
	}
	c.active = req
	return true
}

// clearActive releases the channel if reqID is still the active request.
func (c *Channel) clearActive(reqID uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != nil && c.active.id == reqID {
		c.active = nil
	}
}

// emitIfActive writes the event produced by fn when reqID is still the active
// request, without releasing the channel.
func (c *Channel) emitIfActive(reqID uint32, fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil || c.active.id != reqID {
		return
	}
	fn()
}

// finishIfActive releases the active request and emits fn while holding the
// state lock. Holding the lock across the write guarantees that a concurrent
// STOP either observes the request as still active (so the event precedes the
// STOP response) or as already finished (so no event is written at all).
func (c *Channel) finishIfActive(reqID uint32, fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil || c.active.id != reqID {
		return
	}
	c.active = nil
	fn()
}

// respond writes a response to req.
func (c *Channel) respond(req *Message, status int, state RequestState) {
	c.respondWith(req, status, state, "", nil)
}

func (c *Channel) respondWith(req *Message, status int, state RequestState, contentType string, body []byte) {
	res := NewResponse(req, status, state)
	if contentType != "" {
		res.Headers.Set(HeaderContentType, contentType)
	}
	res.Body = body
	if err := c.write(res); err != nil {
		c.log.Warn("failed to write response", "status", status, "error", err)
	}
	c.log.Debug("response sent", "request_id", req.RequestID, "mrcp_method", req.Method, "status", status)
}

func (c *Channel) sendEvent(reqID uint32, name string, state RequestState, contentType string, body []byte) {
	c.sendEventWithCause(reqID, name, "", contentType, body)
}

func (c *Channel) sendEventWithCause(reqID uint32, name, cause, contentType string, body []byte) {
	ev := NewEvent(c.id, name, reqID, stateFor(name))
	if cause != "" {
		ev.Headers.Set(HeaderCompletionCause, cause)
	}
	if contentType != "" {
		ev.Headers.Set(HeaderContentType, contentType)
	}
	ev.Body = body
	if err := c.write(ev); err != nil {
		c.log.Warn("failed to write event", "event", name, "request_id", reqID, "error", err)
		return
	}
	c.log.Debug("event sent", "event", name, "request_id", reqID)
}

func stateFor(event string) RequestState {
	if event == EventStartOfInput {
		return StateInProgress
	}
	return StateComplete
}

// write serialises m onto the control connection.
func (c *Channel) write(m *Message) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}
	return WriteMessage(c.conn, m)
}

// goTracked runs fn in a goroutine tracked by the channel wait group. The
// goroutine is not started once shutdown has begun.
func (c *Channel) goTracked(fn func()) {
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return
	}
	c.wg.Add(1)
	c.mu.Unlock()

	go func() {
		defer c.wg.Done()
		fn()
	}()
}

// notifyClose reports the read loop exit exactly once.
func (c *Channel) notifyClose(err error) {
	c.notifyOnce.Do(func() {
		if c.onClose != nil {
			c.onClose(err)
		}
	})
}

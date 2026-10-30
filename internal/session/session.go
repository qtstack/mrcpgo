// Package session owns the lifecycle of one SIP dialog plus everything
// attached to it: MRCP control channels, the RTP transceiver and the speech
// engines.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/pion/rtp"

	"github.com/qtstack/mrcpgo/internal/media"
	"github.com/qtstack/mrcpgo/internal/model"
	"github.com/qtstack/mrcpgo/internal/mrcp"
	"github.com/qtstack/mrcpgo/internal/syncx"
)

// closeTimeout bounds how long Close waits for goroutines to finish.
const closeTimeout = 5 * time.Second

// Dialog is the SIP dialog the session belongs to. It is satisfied by
// *sipgo.DialogServerSession; the narrow interface keeps this package testable
// without a SIP stack.
type Dialog interface {
	// Close releases dialog bookkeeping.
	Close() error
	// Context is done when the dialog is terminated by the peer or locally.
	Context() context.Context
}

// EngineFactory creates a fresh engine for each control channel.
type EngineFactory interface {
	NewRecognizer() (model.Recognizer, error)
	NewSynthesizer() (model.Synthesizer, error)
}

// ControlEndpoint is a control channel the server has prepared to answer.
// The listener must already be bound so that its port can be advertised in the
// SDP answer.
type ControlEndpoint struct {
	// ID is the full Channel-Identifier, "<session-id>@<resource>".
	ID string
	// Resource is the resource served on this channel.
	Resource model.Resource
	// Listener accepts exactly the connections for this channel.
	Listener net.Listener
}

// Config describes a session to create.
type Config struct {
	CallID    string
	LocalSDP  []byte
	RemoteSDP []byte
	Dialog    Dialog
	RTP       *media.Transceiver
	Controls  []ControlEndpoint
	Engines   EngineFactory
	Logger    *slog.Logger
	// OnClosed is called once, after resources are released, so the owner can
	// remove the session from its registry.
	OnClosed func(*Session)
}

// consumerRef pairs an audio sink with the control channel that registered it,
// so audio routing errors can be attributed to a channel.
type consumerRef struct {
	id       string
	consumer model.AudioConsumer
}

// Session is the aggregate root for one call.
type Session struct {
	callID   string
	localSDP []byte
	remSDP   []byte
	dialog   Dialog
	rtp      *media.Transceiver
	controls []ControlEndpoint
	engines  EngineFactory
	log      *slog.Logger
	onClosed func(*Session)

	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.RWMutex
	state     model.SessionState
	channels  map[string]*mrcp.Channel
	consumers []consumerRef

	activeOnce sync.Once
	activeCh   chan struct{}

	wg        sync.WaitGroup
	closeOnce sync.Once
	startOnce sync.Once
}

// New validates cfg and returns a session in SessionCreated state. Start must
// be called before the SDP answer is sent.
func New(cfg Config) (*Session, error) {
	if cfg.CallID == "" {
		return nil, errors.New("session: call id is required")
	}
	if cfg.RTP == nil {
		return nil, errors.New("session: RTP transceiver is required")
	}
	if cfg.Engines == nil {
		return nil, errors.New("session: engine factory is required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &Session{
		callID:   cfg.CallID,
		localSDP: cfg.LocalSDP,
		remSDP:   cfg.RemoteSDP,
		dialog:   cfg.Dialog,
		rtp:      cfg.RTP,
		controls: cfg.Controls,
		engines:  cfg.Engines,
		log:      log.With("call_id", cfg.CallID),
		onClosed: cfg.OnClosed,
		ctx:      ctx,
		cancel:   cancel,
		state:    model.SessionCreated,
		channels: make(map[string]*mrcp.Channel, len(cfg.Controls)),
		activeCh: make(chan struct{}),
	}, nil
}

// Start binds the session to its allocated resources: it starts the RTP
// receive loop, the control channel accept loops and the dialog watcher.
// Control requests are not served until Activate is called.
func (s *Session) Start() {
	s.startOnce.Do(func() {
		s.wg.Add(1)
		go s.rtpLoop()

		for _, ep := range s.controls {
			s.wg.Add(1)
			go s.acceptLoop(ep)
		}

		if s.dialog != nil {
			go s.watchDialog()
		}
	})
}

// Activate moves the session to SessionActive once the dialog is confirmed.
// It reports whether the transition happened; a session that is already
// released stays released.
func (s *Session) Activate() bool {
	s.mu.Lock()
	if s.state != model.SessionCreated {
		s.mu.Unlock()
		return false
	}
	s.state = model.SessionActive
	s.mu.Unlock()

	s.activeOnce.Do(func() { close(s.activeCh) })
	return true
}

// State returns the current lifecycle state.
func (s *Session) State() model.SessionState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// CallID returns the SIP Call-ID this session belongs to.
func (s *Session) CallID() string { return s.callID }

// LocalSDP returns the SDP answer sent to the peer.
func (s *Session) LocalSDP() []byte { return s.localSDP }

// RemoteSDP returns the SDP offer received from the peer.
func (s *Session) RemoteSDP() []byte { return s.remSDP }

// RTP returns the session transceiver.
func (s *Session) RTP() *media.Transceiver { return s.rtp }

// ChannelCount returns the number of control channels currently attached.
func (s *Session) ChannelCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.channels)
}

// Close releases every resource. It is idempotent and blocks until resources
// are released or the internal timeout elapses. Concurrent callers block until
// the first close completes.
//
// Release order follows the design contract: refuse new work, stop media, stop
// control channels, close listeners, wait for goroutines, unregister, then
// detach the dialog.
func (s *Session) Close(reason error) error {
	s.closeOnce.Do(func() { s.release(reason) })
	return nil
}

func (s *Session) release(reason error) {
	s.log.Info("closing session", "reason", reasonText(reason))

	s.mu.Lock()
	if s.state == model.SessionClosed {
		s.mu.Unlock()
		return
	}
	s.state = model.SessionReleasing
	channels := make([]*mrcp.Channel, 0, len(s.channels))
	for _, ch := range s.channels {
		channels = append(channels, ch)
	}
	s.mu.Unlock()

	// 1. Refuse new work: cancel everything derived from the session context.
	s.cancel()

	// 2. Stop media.
	if s.rtp != nil {
		if err := s.rtp.Close(); err != nil {
			s.log.Debug("closing RTP socket", "error", err)
		}
	}

	// 3. Terminate in-flight control requests and close their connections.
	for _, ch := range channels {
		ch.Close(reason)
	}

	// 4. Close the control listeners so accept loops stop.
	for _, ep := range s.controls {
		if err := ep.Listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			s.log.Debug("closing control listener", "error", err, "channel_id", ep.ID)
		}
	}

	// 5. Wait for accept loops and the RTP loop.
	if !syncx.WaitTimeout(&s.wg, closeTimeout) {
		s.log.Warn("timed out waiting for session goroutines to exit")
	}

	// 6. Unregister before reporting Closed so observers never observe a closed
	// session still present in the registry.
	if s.onClosed != nil {
		s.onClosed(s)
	}

	// 7. Publish the terminal state.
	s.mu.Lock()
	s.state = model.SessionClosed
	s.mu.Unlock()

	// 8. Detach from the SIP dialog.
	if s.dialog != nil {
		if err := s.dialog.Close(); err != nil {
			s.log.Debug("closing SIP dialog", "error", err)
		}
	}

	s.log.Info("session closed")
}

// AddChannel attaches an accepted control connection. It is called by the
// accept loop once per connection.
func (s *Session) addChannel(ep ControlEndpoint, conn net.Conn) error {
	log := s.log.With("channel_id", ep.ID, "resource", ep.Resource.String(), "remote", conn.RemoteAddr().String())
	log.Info("MRCP control connection accepted")

	cfg := mrcp.Config{
		ID:       ep.ID,
		Resource: ep.Resource,
		Conn:     conn,
		Player:   s.rtp,
		Logger:   log,
		Ready:    s.activeCh,
		OnClose: func(err error) {
			s.mu.Lock()
			delete(s.channels, ep.ID)
			s.removeConsumerLocked(ep.ID)
			s.mu.Unlock()
			// Releasing from the read loop goroutine would deadlock on the
			// session wait group, so hand teardown to a fresh goroutine.
			go func() {
				s.Close(fmt.Errorf("control channel %s closed: %w", ep.ID, err))
			}()
		},
	}

	var consumer model.AudioConsumer
	switch ep.Resource {
	case model.ResourceSpeechRecog:
		recog, err := s.engines.NewRecognizer()
		if err != nil {
			return fmt.Errorf("create recognizer: %w", err)
		}
		cfg.Recognizer = recog
		consumer, _ = recog.(model.AudioConsumer)
	case model.ResourceSpeechSynth:
		synth, err := s.engines.NewSynthesizer()
		if err != nil {
			return fmt.Errorf("create synthesizer: %w", err)
		}
		cfg.Synthesizer = synth
	}

	ch, err := mrcp.New(cfg)
	if err != nil {
		return err
	}

	s.mu.Lock()
	if s.state != model.SessionCreated && s.state != model.SessionActive {
		// The session started shutting down while the connection was being
		// accepted.
		s.mu.Unlock()
		return fmt.Errorf("session is %s", s.state)
	}
	if existing, ok := s.channels[ep.ID]; ok {
		s.mu.Unlock()
		return fmt.Errorf("channel %s already connected (existing remote %s)",
			ep.ID, existing.RemoteAddr())
	}
	s.channels[ep.ID] = ch
	if consumer != nil {
		// Copy on write: dispatchAudio reads the slice header under the read
		// lock and then iterates without holding it.
		s.consumers = append(s.consumers[:len(s.consumers):len(s.consumers)],
			consumerRef{id: ep.ID, consumer: consumer})
	}
	s.mu.Unlock()

	ch.Serve()
	return nil
}

// removeConsumerLocked drops the audio sink registered for channelID. The
// caller must hold s.mu for writing.
func (s *Session) removeConsumerLocked(channelID string) {
	out := s.consumers[:0:0]
	for _, ref := range s.consumers {
		if ref.id != channelID {
			out = append(out, ref)
		}
	}
	s.consumers = out
}

// acceptLoop accepts control connections for one endpoint.
func (s *Session) acceptLoop(ep ControlEndpoint) {
	defer s.wg.Done()
	for {
		conn, err := ep.Listener.Accept()
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			s.log.Warn("control accept failed", "channel_id", ep.ID, "error", err)
			return
		}
		if err := s.addChannel(ep, conn); err != nil {
			s.log.Warn("rejecting control connection", "channel_id", ep.ID, "error", err)
			if cerr := conn.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) {
				s.log.Debug("closing rejected connection", "error", cerr)
			}
		}
	}
}

// rtpLoop drains inbound RTP and feeds audio aware recognizers.
func (s *Session) rtpLoop() {
	defer s.wg.Done()
	err := s.rtp.ReceiveLoop(s.ctx, s.dispatchAudio)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, media.ErrClosed) {
		s.log.Warn("RTP receive loop stopped", "error", err)
	}
}

func (s *Session) dispatchAudio(pkt *rtp.Packet) {
	if len(pkt.Payload) == 0 {
		return
	}
	// Snapshot the consumers before invoking them so a slow engine cannot hold
	// the session lock and delay teardown. The slice is copy-on-write, so the
	// snapshot stays valid after the lock is released.
	s.mu.RLock()
	consumers := s.consumers
	s.mu.RUnlock()

	for _, ref := range consumers {
		if err := ref.consumer.WriteAudio(pkt.Payload); err != nil {
			s.log.Debug("recognizer rejected audio", "channel_id", ref.id, "error", err)
		}
	}
}

// watchDialog closes the session as soon as the SIP dialog terminates for any
// reason: BYE, CANCEL or transport failure.
func (s *Session) watchDialog() {
	dialogCtx := s.dialog.Context()
	select {
	case <-dialogCtx.Done():
		cause := context.Cause(dialogCtx)
		if cause == nil {
			cause = errors.New("dialog terminated")
		}
		// Do not hold the watcher open while Close waits for goroutines.
		go s.Close(fmt.Errorf("dialog terminated: %w", cause))
	case <-s.ctx.Done():
	}
}

// Stats returns the RTP reception statistics.
func (s *Session) Stats() model.RTPStats { return s.rtp.Stats() }

func reasonText(err error) string {
	if err == nil {
		return "closed"
	}
	return err.Error()
}

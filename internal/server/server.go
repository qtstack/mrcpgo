// Package server implements the SIP user agent side of the media resource
// server: it answers INVITEs with an SDP answer that advertises an RTP port and
// one MRCPv2 control port per control channel, and maps dialogs onto sessions.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"github.com/qtstack/mrcpgo/internal/config"
	"github.com/qtstack/mrcpgo/internal/session"
)

// shutdownTimeout bounds the teardown of every session on shutdown.
const shutdownTimeout = 10 * time.Second

// Server is the SIP UAS that fronts the media resource server.
type Server struct {
	cfg     config.Config
	log     *slog.Logger
	engines session.EngineFactory

	ua        *sipgo.UserAgent
	dialogUA  *sipgo.DialogUA
	sipServer *sipgo.Server
	manager   *session.Manager

	// dialogs maps Call-ID onto the sipgo dialog so ACK and BYE can be
	// correlated. The SIP layer's own cache is not used: this keeps full
	// control over dialog lifetimes.
	dialogs sync.Map // string -> *sipgo.DialogServerSession

	mu       sync.Mutex
	conn     *net.UDPConn
	addr     string
	sipPort  int
	shutErr  error
	closeOne sync.Once
}

// New creates a server. engines supplies a fresh set of speech engines per
// control channel and must not be nil.
func New(cfg config.Config, log *slog.Logger, engines session.EngineFactory) (*Server, error) {
	if engines == nil {
		return nil, errors.New("server: engine factory is required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	// DECISION: sipgo's transport layer logs a few paths through a package
	// level logger rather than the configured one. Aligning it here keeps the
	// server's output consistent; it must happen before the user agent is
	// created.
	sip.SetDefaultLogger(log)

	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent(cfg.SIP.UserAgent),
		sipgo.WithUserAgentTransactionLayerOptions(sip.WithTransactionLayerLogger(log.With("component", "sip-transaction"))),
		sipgo.WithUserAgentTransportLayerOptions(sip.WithTransportLayerLogger(log.With("component", "sip-transport"))),
	)
	if err != nil {
		return nil, fmt.Errorf("server: create SIP user agent: %w", err)
	}

	client, err := sipgo.NewClient(ua, sipgo.WithClientLogger(log.With("component", "sip-client")))
	if err != nil {
		_ = ua.Close()
		return nil, fmt.Errorf("server: create SIP client: %w", err)
	}

	sipServer, err := sipgo.NewServer(ua, sipgo.WithServerLogger(log.With("component", "sip-server")))
	if err != nil {
		_ = ua.Close()
		return nil, fmt.Errorf("server: create SIP server: %w", err)
	}

	listenHost, listenPort, err := net.SplitHostPort(cfg.SIP.Listen)
	if err != nil {
		_ = ua.Close()
		return nil, fmt.Errorf("server: sip.listen %q: %w", cfg.SIP.Listen, err)
	}
	port, err := strconv.Atoi(listenPort)
	if err != nil {
		_ = ua.Close()
		return nil, fmt.Errorf("server: sip.listen port %q: %w", listenPort, err)
	}

	// The Contact header of provisional responses uses the configured
	// advertise address; confirmed responses carry a per-call Contact built
	// from the address actually used to reach the peer.
	contactHost := cfg.SIP.AdvertiseAddr
	if contactHost == "" {
		contactHost = listenHost
		if contactHost == "" || contactHost == "0.0.0.0" || contactHost == "::" {
			contactHost = "127.0.0.1"
		}
	}

	s := &Server{
		cfg:     cfg,
		log:     log,
		engines: engines,
		ua:      ua,
		dialogUA: &sipgo.DialogUA{
			Client: client,
			ContactHDR: sip.ContactHeader{
				Address: sip.Uri{User: cfg.SIP.ContactUser, Host: contactHost, Port: port},
			},
		},
		sipServer: sipServer,
		manager:   session.NewManager(log),
		sipPort:   port,
	}

	sipServer.OnInvite(s.onInvite)
	sipServer.OnAck(s.onAck)
	sipServer.OnBye(s.onBye)
	sipServer.OnCancel(s.onCancel)
	sipServer.OnOptions(s.onOptions)
	return s, nil
}

// Sessions exposes the session registry, primarily for health reporting and
// tests.
func (s *Server) Sessions() *session.Manager { return s.manager }

// Addr returns the bound SIP listen address, or an empty string before Listen.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Listen binds the SIP UDP socket. It is idempotent.
func (s *Server) Listen() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		return s.addr, nil
	}
	laddr, err := net.ResolveUDPAddr("udp4", s.cfg.SIP.Listen)
	if err != nil {
		return "", fmt.Errorf("server: resolve %q: %w", s.cfg.SIP.Listen, err)
	}
	conn, err := net.ListenUDP("udp4", laddr)
	if err != nil {
		return "", fmt.Errorf("server: listen %q: %w", s.cfg.SIP.Listen, err)
	}
	s.conn = conn
	s.addr = conn.LocalAddr().String()
	// The configured port may be zero (ephemeral); Contact headers written
	// after this point must advertise the port actually bound.
	if udpAddr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		s.sipPort = udpAddr.Port
		s.dialogUA.ContactHDR.Address.Port = udpAddr.Port
	}
	s.log.Info("SIP listening", "addr", s.addr)
	return s.addr, nil
}

// contactPort returns the SIP port to advertise in Contact headers.
func (s *Server) contactPort() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sipPort
}

// Run binds the SIP socket and serves until ctx is canceled.
func (s *Server) Run(ctx context.Context) error {
	if _, err := s.Listen(); err != nil {
		return err
	}
	return s.Serve(ctx)
}

// Serve serves SIP on an already bound socket until ctx is canceled, then
// releases every session.
func (s *Server) Serve(ctx context.Context) error {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return errors.New("server: Serve called before Listen")
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.sipServer.ServeUDP(conn) }()

	select {
	case err := <-serveErr:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		shutdownErr := s.Shutdown(shutdownCtx)
		if err == nil {
			return shutdownErr
		}
		return errors.Join(fmt.Errorf("server: SIP listener stopped: %w", err), shutdownErr)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := s.Shutdown(shutdownCtx); err != nil {
		return err
	}
	<-serveErr
	return nil
}

// Shutdown stops accepting SIP requests and releases every session. It is
// idempotent.
func (s *Server) Shutdown(ctx context.Context) error {
	s.closeOne.Do(func() {
		s.mu.Lock()
		conn := s.conn
		s.conn = nil
		s.mu.Unlock()

		if conn != nil {
			if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				s.shutErr = errors.Join(s.shutErr, fmt.Errorf("close SIP socket: %w", err))
			}
		}
		if err := s.manager.Shutdown(ctx); err != nil {
			s.shutErr = errors.Join(s.shutErr, fmt.Errorf("close sessions: %w", err))
		}
		if err := s.ua.Close(); err != nil {
			s.shutErr = errors.Join(s.shutErr, fmt.Errorf("close SIP user agent: %w", err))
		}
	})
	return s.shutErr
}

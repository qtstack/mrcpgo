package server

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"uuid"

	"github.com/qtstack/mrcpgo/internal/media"
	"github.com/qtstack/mrcpgo/internal/model"
	"github.com/qtstack/mrcpgo/internal/sdp"
	"github.com/qtstack/mrcpgo/internal/session"
)

// maxChannelIDLength is the RFC 6787 limit on the session-id part of a
// Channel-Identifier.
const maxChannelIDLength = 49

// negotiate validates the SDP offer, allocates local resources and builds the
// answer. On failure it returns an SDP status code and leaves nothing
// allocated.
func (s *Server) negotiate(
	req *sip.Request,
	dlg *sipgo.DialogServerSession,
	callID string,
	log *slog.Logger,
) (answer []byte, sess *session.Session, host string, code int, err error) {
	if ct := req.ContentType(); ct != nil && !isSDPContentType(*ct) {
		return nil, nil, "", sip.StatusUnsupportedMediaType,
			fmt.Errorf("unsupported content type %q", string(*ct))
	}
	if len(req.Body()) == 0 {
		return nil, nil, "", sip.StatusNotAcceptableHere, errors.New("empty SDP offer")
	}

	offer, err := sdp.Parse(req.Body())
	if err != nil {
		return nil, nil, "", sip.StatusNotAcceptableHere, fmt.Errorf("parse SDP offer: %w", err)
	}

	remoteAudio := offer.FirstAudio()
	if remoteAudio == nil || remoteAudio.Port == 0 {
		return nil, nil, "", sip.StatusNotAcceptableHere, errors.New("offer has no audio stream")
	}
	if !remoteAudio.AcceptsPayloadType(sdp.PCMUPayloadType) {
		return nil, nil, "", sip.StatusNotAcceptableHere, errors.New("offer does not include PCMU")
	}
	if len(offer.MRCPMedia()) == 0 {
		return nil, nil, "", sip.StatusNotAcceptableHere,
			errors.New("offer has no MRCPv2 control channel")
	}

	// Allocate one RTP socket and one control listener per accepted MRCP media
	// section. Entries stay aligned with the offer so the answer mirrors it.
	entries := make([]sdp.MediaEntry, len(offer.Media))
	controls := make([]session.ControlEndpoint, 0, len(offer.MRCPMedia()))
	var (
		rtpConn      *net.UDPConn
		audioHandled bool
	)
	release := func() {
		if rtpConn != nil {
			_ = rtpConn.Close()
		}
		for _, ep := range controls {
			_ = ep.Listener.Close()
		}
	}

	for i := range offer.Media {
		m := &offer.Media[i]
		switch {
		case m.IsAudio() && !audioHandled:
			if m.Port == 0 || !m.AcceptsPayloadType(sdp.PCMUPayloadType) {
				continue
			}
			conn, lerr := s.listenRTP()
			if lerr != nil {
				release()
				return nil, nil, "", sip.StatusInternalServerError, lerr
			}
			rtpConn = conn
			entries[i] = sdp.MediaEntry{Port: localPort(conn.LocalAddr())}
			audioHandled = true
		case m.IsMRCP():
			entry, ep, perr := s.prepareControl(m)
			if perr != nil {
				// Reject only this control channel; the offer may contain
				// further channels that are usable.
				log.Warn("rejecting MRCP control channel", "error", perr)
				continue
			}
			controls = append(controls, ep)
			entries[i] = entry
		}
	}

	if rtpConn == nil {
		release()
		return nil, nil, "", sip.StatusNotAcceptableHere, errors.New("no usable audio stream")
	}
	if len(controls) == 0 {
		release()
		return nil, nil, "", sip.StatusNotAcceptableHere, errors.New("no usable MRCP control channel")
	}

	remoteHost := offer.RemoteAddress(remoteAudio)
	if remoteHost == "" {
		release()
		return nil, nil, "", sip.StatusNotAcceptableHere, errors.New("offer has no c= address")
	}
	remoteAddr, err := net.ResolveUDPAddr("udp4",
		net.JoinHostPort(remoteHost, strconv.Itoa(remoteAudio.Port)))
	if err != nil {
		release()
		return nil, nil, "", sip.StatusNotAcceptableHere,
			fmt.Errorf("resolve remote RTP address %s: %w", remoteHost, err)
	}

	host, err = s.advertiseHost(remoteAddr)
	if err != nil {
		release()
		return nil, nil, "", sip.StatusInternalServerError, err
	}

	answer, err = sdp.BuildAnswer(offer, sdp.AnswerOptions{
		User:           s.cfg.SIP.ContactUser,
		SessionID:      newSessionID(),
		SessionVersion: 1,
		Host:           host,
		Entries:        entries,
	})
	if err != nil {
		release()
		return nil, nil, "", sip.StatusInternalServerError, err
	}

	tx, err := media.New(rtpConn, remoteAddr, log)
	if err != nil {
		release()
		return nil, nil, "", sip.StatusInternalServerError,
			fmt.Errorf("create RTP transceiver: %w", err)
	}

	sess, err = session.New(session.Config{
		CallID:    callID,
		LocalSDP:  answer,
		RemoteSDP: req.Body(),
		Dialog:    dlg,
		RTP:       tx,
		Controls:  controls,
		Engines:   s.engines,
		Logger:    log,
		OnClosed: func(closed *session.Session) {
			s.manager.Remove(closed)
		},
	})
	if err != nil {
		_ = tx.Close()
		for _, ep := range controls {
			_ = ep.Listener.Close()
		}
		return nil, nil, "", sip.StatusInternalServerError,
			fmt.Errorf("create session: %w", err)
	}

	// Register before answering: a shutdown between allocation and answer must
	// be able to find and release the pre-allocated resources.
	if err := s.manager.Add(sess); err != nil {
		_ = sess.Close(err)
		return nil, nil, "", sip.StatusInternalServerError, err
	}
	// Accept loops must run before the 200 OK: FreeSWITCH may connect as soon
	// as it receives the answer.
	sess.Start()

	return answer, sess, host, 0, nil
}

// isSDPContentType reports whether ct is application/sdp, ignoring media type
// parameters such as "; charset=utf-8".
func isSDPContentType(ct sip.ContentTypeHeader) bool {
	value := strings.TrimSpace(string(ct))
	if i := strings.IndexByte(value, ';'); i >= 0 {
		value = strings.TrimSpace(value[:i])
	}
	return strings.EqualFold(value, "application/sdp")
}

// prepareControl validates one MRCP media section and binds its listener.
func (s *Server) prepareControl(m *sdp.Media) (sdp.MediaEntry, session.ControlEndpoint, error) {
	var zero sdp.MediaEntry
	var empty session.ControlEndpoint

	id, resource, ok := m.ChannelIdentifier()
	if !ok {
		return zero, empty, errors.New("missing or malformed a=channel attribute")
	}
	res := model.Resource(resource)
	if !res.Valid() {
		return zero, empty, fmt.Errorf("unsupported resource %q", resource)
	}
	switch setup := strings.ToLower(m.Setup()); setup {
	case "", "passive", "actpass":
		// The server is the passive side; actpass lets us choose passive.
	default:
		return zero, empty, fmt.Errorf("unsupported a=setup:%s", setup)
	}

	if id == "" {
		// DECISION: RFC 6787 allows the server to allocate the session-id
		// part of the Channel-Identifier when the client proposes none.
		id = newChannelID()
	}
	if len(id) > maxChannelIDLength {
		return zero, empty, fmt.Errorf("channel id %q exceeds %d characters", id, maxChannelIDLength)
	}

	listener, err := net.Listen("tcp4", net.JoinHostPort(s.cfg.MRCP.ListenHost, "0"))
	if err != nil {
		return zero, empty, fmt.Errorf("listen for control channel: %w", err)
	}

	fullID := id + "@" + resource
	return sdp.MediaEntry{Port: localPort(listener.Addr()), ChannelID: id, Resource: resource},
		session.ControlEndpoint{ID: fullID, Resource: res, Listener: listener},
		nil
}

// listenRTP binds an ephemeral UDP socket for the audio stream.
func (s *Server) listenRTP() (*net.UDPConn, error) {
	addr, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(s.cfg.RTP.ListenHost, "0"))
	if err != nil {
		return nil, fmt.Errorf("resolve RTP listen address: %w", err)
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return nil, fmt.Errorf("listen for RTP: %w", err)
	}
	return conn, nil
}

// advertiseHost resolves the address written into Contact and SDP.
func (s *Server) advertiseHost(remote *net.UDPAddr) (string, error) {
	if host := s.cfg.SIP.AdvertiseAddr; host != "" {
		return host, nil
	}
	if host := s.cfg.RTP.ListenHost; host != "" && host != "0.0.0.0" && host != "::" {
		return host, nil
	}
	if remote == nil {
		return "", errors.New("no remote address to derive a local address from")
	}
	conn, err := net.DialUDP("udp4", nil, remote)
	if err != nil {
		return "", fmt.Errorf("determine local address: %w", err)
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr.IP == nil {
		return "", errors.New("determine local address: unexpected address type")
	}
	return addr.IP.String(), nil
}

// localPort extracts a port from a UDP or TCP address.
func localPort(addr net.Addr) int {
	switch a := addr.(type) {
	case *net.UDPAddr:
		return a.Port
	case *net.TCPAddr:
		return a.Port
	default:
		_, port, err := net.SplitHostPort(addr.String())
		if err != nil {
			return 0
		}
		p, _ := strconv.Atoi(port)
		return p
	}
}

// newChannelID returns a fresh 32 character channel session-id. hex is used
// because RFC 6787 restricts the identifier to alphanumerics and underscores.
func newChannelID() string {
	return strings.ReplaceAll(uuid.New().String(), "-", "")
}

// newSessionID returns a random numeric session identifier for the SDP o=
// line.
func newSessionID() uint64 {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return uint64(time.Now().UnixNano())
	}
	if v := binary.BigEndian.Uint64(buf[:]); v != 0 {
		return v
	}
	return 1
}

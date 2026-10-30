// Package media implements the RTP half of a media session: sending and
// receiving G.711 mu-law frames over UDP.
//
// The implementation is deliberately small. It maintains sequence number,
// timestamp and SSRC for the outbound stream, and records receive statistics,
// but performs no jitter buffering or reordering.
package media

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"

	"github.com/qtstack/mrcpgo/internal/model"
)

const (
	// PCMUPayloadType is the RTP payload type for G.711 mu-law.
	PCMUPayloadType = 0
	// FrameSize is the number of bytes in a 20 ms 8 kHz mu-law frame.
	FrameSize = 160
	// ClockRate is the RTP clock rate for PCMU.
	ClockRate = 8000
	// SamplesPerFrame is the number of samples carried by one frame, which
	// equals the timestamp increment per packet.
	SamplesPerFrame = FrameSize
	// readBufferSize is large enough for any reasonable RTP packet.
	readBufferSize = 2048
)

// ErrClosed is returned when the transceiver is used after Close.
var ErrClosed = errors.New("media: transceiver closed")

// ErrNoRemote is returned when sending before a remote address is known.
var ErrNoRemote = errors.New("media: no remote RTP address")

// Transceiver sends and receives RTP on a single UDP socket.
//
// SendFrame is safe for concurrent use: the RTP ticker and the teardown path
// may both call it.
type Transceiver struct {
	conn   *net.UDPConn
	remote atomic.Pointer[net.UDPAddr]
	ssrc   uint32
	log    *slog.Logger

	mu        sync.Mutex
	seq       uint16
	timestamp uint32
	closed    bool

	// Receive statistics. The counters are atomic so Stats may be read while
	// the receive loop is running; lastSeq is only touched by ReceiveLoop.
	received   atomic.Uint64
	lost       atomic.Uint64
	outOfOrder atomic.Uint64
	lastSeq    uint16

	closeOnce sync.Once
}

// New creates a transceiver that owns conn. remote may be nil if it is not
// known yet; SetRemote must then be called before the first SendFrame.
func New(conn *net.UDPConn, remote *net.UDPAddr, log *slog.Logger) (*Transceiver, error) {
	if conn == nil {
		return nil, errors.New("media: nil UDP connection")
	}
	ssrc, err := randomSSRC()
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	t := &Transceiver{
		conn: conn,
		ssrc: ssrc,
		log:  log.With("ssrc", ssrc),
		// DECISION: the initial sequence number and timestamp are random, as
		// RFC 3550 recommends, to make streams harder to predict and to avoid
		// collisions with a previous session reusing the port.
		seq:       uint16(ssrc),
		timestamp: ssrc,
	}
	t.SetRemote(remote)
	return t, nil
}

func randomSSRC() (uint32, error) {
	var buf [4]byte
	for attempt := 0; attempt < 8; attempt++ {
		if _, err := rand.Read(buf[:]); err != nil {
			return 0, err
		}
		if v := binary.BigEndian.Uint32(buf[:]); v != 0 {
			return v, nil
		}
	}
	return 0, errors.New("media: failed to generate a non-zero SSRC")
}

// LocalAddr returns the local UDP address, or nil after Close.
func (t *Transceiver) LocalAddr() net.Addr { return t.conn.LocalAddr() }

// SSRC returns the synchronisation source identifier of the outbound stream.
func (t *Transceiver) SSRC() uint32 { return t.ssrc }

// SetRemote updates the destination RTP address. Passing nil clears it.
func (t *Transceiver) SetRemote(addr *net.UDPAddr) {
	t.remote.Store(addr)
}

// Remote returns the current destination address, or nil.
func (t *Transceiver) Remote() *net.UDPAddr { return t.remote.Load() }

// SendFrame packetises one PCMU frame and transmits it.
func (t *Transceiver) SendFrame(frame []byte) error {
	remote := t.remote.Load()
	if remote == nil {
		return ErrNoRemote
	}

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return ErrClosed
	}
	pkt := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    PCMUPayloadType,
			SequenceNumber: t.seq,
			Timestamp:      t.timestamp,
			SSRC:           t.ssrc,
		},
		Payload: frame,
	}
	t.seq++
	t.timestamp += SamplesPerFrame
	t.mu.Unlock()

	buf, err := pkt.Marshal()
	if err != nil {
		return err
	}
	if _, err := t.conn.WriteToUDP(buf, remote); err != nil {
		return err
	}
	return nil
}

// ReceiveLoop reads RTP packets until ctx is canceled or the socket is closed,
// invoking handle for each valid packet. Invalid packets are logged and
// skipped. The loop returns ctx.Err() on cancellation and ErrClosed after
// Close.
func (t *Transceiver) ReceiveLoop(ctx context.Context, handle func(*rtp.Packet)) error {
	stop := context.AfterFunc(ctx, func() {
		// Unblock the read. The deadline only affects this receiver.
		_ = t.conn.SetReadDeadline(time.Now())
	})
	defer stop()

	buf := make([]byte, readBufferSize)
	for {
		n, _, err := t.conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, net.ErrClosed) || t.isClosed() {
				return ErrClosed
			}
			return err
		}
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			t.log.Debug("dropping malformed RTP packet", "error", err, "bytes", n)
			continue
		}
		t.record(pkt)
		if handle != nil {
			handle(pkt)
		}
	}
}

// record updates reception statistics and logs sequence anomalies.
func (t *Transceiver) record(pkt *rtp.Packet) {
	if t.received.Add(1) == 1 {
		t.lastSeq = pkt.SequenceNumber
		return
	}
	// Sequence numbers wrap at 16 bits; interpret the delta as signed.
	delta := int16(pkt.SequenceNumber - t.lastSeq)
	switch {
	case delta == 1:
		// In order.
	case delta == 0:
		t.outOfOrder.Add(1)
		t.log.Debug("duplicate RTP sequence number", "seq", pkt.SequenceNumber)
	case delta > 1:
		gap := uint64(delta - 1)
		t.lost.Add(gap)
		t.log.Debug("RTP packet loss detected", "seq", pkt.SequenceNumber, "lost", gap)
	default:
		t.outOfOrder.Add(1)
		t.log.Debug("out of order RTP packet", "seq", pkt.SequenceNumber)
	}
	t.lastSeq = pkt.SequenceNumber
}

// Stats returns a snapshot of the reception statistics.
func (t *Transceiver) Stats() model.RTPStats {
	return model.RTPStats{
		Received:   t.received.Load(),
		Lost:       t.lost.Load(),
		OutOfOrder: t.outOfOrder.Load(),
	}
}

// Close stops the transceiver and releases the UDP socket.
func (t *Transceiver) Close() error {
	var err error
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		t.mu.Unlock()
		err = t.conn.Close()
	})
	return err
}

func (t *Transceiver) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

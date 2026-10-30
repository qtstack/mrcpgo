package media

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"
)

// newPair returns a transceiver bound to one loopback socket and a peer socket
// that can send and receive RTP.
func newPair(t *testing.T) (*Transceiver, *net.UDPConn) {
	t.Helper()

	trxConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen transceiver: %v", err)
	}
	peerConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		_ = trxConn.Close()
		t.Fatalf("listen peer: %v", err)
	}

	trx, err := New(trxConn, peerConn.LocalAddr().(*net.UDPAddr), nil)
	if err != nil {
		t.Fatalf("new transceiver: %v", err)
	}
	t.Cleanup(func() {
		_ = trx.Close()
		_ = peerConn.Close()
	})
	return trx, peerConn
}

func readPacket(t *testing.T, conn *net.UDPConn) *rtp.Packet {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	buf := make([]byte, 1500)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read RTP: %v", err)
	}
	pkt := &rtp.Packet{}
	if err := pkt.Unmarshal(buf[:n]); err != nil {
		t.Fatalf("unmarshal RTP: %v", err)
	}
	return pkt
}

func TestSendFrame(t *testing.T) {
	trx, peer := newPair(t)

	if trx.SSRC() == 0 {
		t.Fatal("SSRC must not be zero")
	}

	frame := make([]byte, FrameSize)
	for i := range frame {
		frame[i] = 0xFF
	}

	if err := trx.SendFrame(frame); err != nil {
		t.Fatalf("send first frame: %v", err)
	}
	first := readPacket(t, peer)

	if err := trx.SendFrame(frame); err != nil {
		t.Fatalf("send second frame: %v", err)
	}
	second := readPacket(t, peer)

	if first.Version != 2 {
		t.Errorf("version = %d", first.Version)
	}
	if first.PayloadType != PCMUPayloadType {
		t.Errorf("payload type = %d", first.PayloadType)
	}
	if first.SSRC != trx.SSRC() || second.SSRC != trx.SSRC() {
		t.Errorf("SSRC mismatch: %d %d %d", first.SSRC, second.SSRC, trx.SSRC())
	}
	if len(first.Payload) != FrameSize {
		t.Errorf("payload size = %d", len(first.Payload))
	}
	if second.SequenceNumber != first.SequenceNumber+1 {
		t.Errorf("sequence numbers = %d, %d", first.SequenceNumber, second.SequenceNumber)
	}
	if second.Timestamp != first.Timestamp+SamplesPerFrame {
		t.Errorf("timestamps = %d, %d (want +%d)", first.Timestamp, second.Timestamp, SamplesPerFrame)
	}
}

func TestSendFrameWithoutRemote(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	trx, err := New(conn, nil, nil)
	if err != nil {
		t.Fatalf("new transceiver: %v", err)
	}
	defer trx.Close()

	if err := trx.SendFrame(make([]byte, FrameSize)); !errors.Is(err, ErrNoRemote) {
		t.Fatalf("error = %v, want ErrNoRemote", err)
	}
}

func TestSendFrameAfterClose(t *testing.T) {
	trx, _ := newPair(t)
	if err := trx.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := trx.SendFrame(make([]byte, FrameSize)); !errors.Is(err, ErrClosed) {
		t.Fatalf("error = %v, want ErrClosed", err)
	}
}

func TestReceiveLoop(t *testing.T) {
	trx, peer := newPair(t)
	trxAddr := trx.LocalAddr().(*net.UDPAddr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan *rtp.Packet, 8)
	done := make(chan error, 1)
	go func() {
		done <- trx.ReceiveLoop(ctx, func(pkt *rtp.Packet) { received <- pkt })
	}()

	send := func(seq uint16, ts uint32) {
		pkt := &rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: PCMUPayloadType, SequenceNumber: seq, Timestamp: ts, SSRC: 1},
			Payload: []byte{0xFF},
		}
		buf, err := pkt.Marshal()
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := peer.WriteToUDP(buf, trxAddr); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	send(100, 0)
	send(102, 320) // 101 lost
	send(101, 160) // out of order

	for i := 0; i < 3; i++ {
		select {
		case <-received:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for RTP")
		}
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("loop error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("receive loop did not stop")
	}

	stats := trx.Stats()
	if stats.Received != 3 {
		t.Errorf("received = %d", stats.Received)
	}
	if stats.Lost != 1 {
		t.Errorf("lost = %d, want 1", stats.Lost)
	}
	if stats.OutOfOrder != 1 {
		t.Errorf("out of order = %d, want 1", stats.OutOfOrder)
	}
}

func TestReceiveLoopStopsOnClose(t *testing.T) {
	trx, _ := newPair(t)

	done := make(chan error, 1)
	go func() {
		done <- trx.ReceiveLoop(context.Background(), nil)
	}()

	if err := trx.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("loop error = %v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("receive loop did not stop after close")
	}
}

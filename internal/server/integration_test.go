//go:build integration

package server_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime"
	"runtime/pprof"
	"strconv"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"

	"github.com/qtstack/mrcpgo/internal/config"
	"github.com/qtstack/mrcpgo/internal/engine/mock"
	"github.com/qtstack/mrcpgo/internal/mrcp"
	"github.com/qtstack/mrcpgo/internal/sdp"
	"github.com/qtstack/mrcpgo/internal/server"
)

const (
	testRecogID = "32a30720ab3e9d4d"
	testSynthID = "a1b2c3d4e5f60718"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// waitFor polls cond until it is true or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", msg)
}

// checkGoroutineLeaks uses the Go 1.27 runtime goroutineleak profile to assert
// that teardown left no unreachable, permanently blocked goroutines behind.
func checkGoroutineLeaks(t *testing.T) {
	t.Helper()
	profile := pprof.Lookup("goroutineleak")
	if profile == nil {
		t.Log("goroutineleak profile is not available in this toolchain")
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		if profile.Count() == 0 {
			return
		}
		if time.Now().After(deadline) {
			var buf bytes.Buffer
			_ = profile.WriteTo(&buf, 2)
			t.Fatalf("goroutine leak detected:\n%s", buf.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func writeMRCP(t *testing.T, conn net.Conn, msg *mrcp.Message) {
	t.Helper()
	if err := mrcp.WriteMessage(conn, msg); err != nil {
		t.Fatalf("write MRCP message: %v", err)
	}
}

func readMRCP(t *testing.T, conn net.Conn, reader *bufio.Reader) *mrcp.Message {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	msg, err := mrcp.ReadMessage(reader)
	if err != nil {
		t.Fatalf("read MRCP message: %v", err)
	}
	return msg
}

// TestCallLifecycle drives a full call against the server using a real SIP UAC
// and a real MRCPv2 control connection.
func TestCallLifecycle(t *testing.T) {
	cfg := config.Default()
	cfg.SIP.Listen = "127.0.0.1:0"
	cfg.SIP.AdvertiseAddr = "127.0.0.1"
	cfg.RTP.ListenHost = "127.0.0.1"
	cfg.MRCP.ListenHost = "127.0.0.1"
	cfg.MRCP.Engine.RecognitionDelay = config.Duration(20 * time.Millisecond)

	log := discardLogger()
	srv, err := server.New(cfg, log, mock.NewFactory())
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	addr, err := srv.Listen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ctx) }()

	stopped := false
	defer func() {
		if !stopped {
			cancel()
			select {
			case <-serveDone:
			case <-time.After(10 * time.Second):
				t.Error("server did not stop")
			}
		}
	}()

	ua, err := sipgo.NewUA(sipgo.WithUserAgent("mrcpgo-test-uac"))
	if err != nil {
		t.Fatalf("create UAC: %v", err)
	}
	defer ua.Close()
	client, err := sipgo.NewClient(ua)
	if err != nil {
		t.Fatalf("create UAC client: %v", err)
	}
	dialogUA := &sipgo.DialogUA{
		Client:     client,
		ContactHDR: sip.ContactHeader{Address: sip.Uri{User: "fs"}},
	}

	rtpPeer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen UAC RTP: %v", err)
	}
	defer rtpPeer.Close()

	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split server address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse server port: %v", err)
	}

	offer := fmt.Sprintf(`v=0
o=FreeSWITCH 1687513427 1687513428 IN IP4 127.0.0.1
s=FreeSWITCH
c=IN IP4 127.0.0.1
t=0 0
m=audio %d RTP/AVP 0 101
a=rtpmap:0 PCMU/8000
a=rtpmap:101 telephone-event/8000
a=ptime:20
a=sendrecv
m=application 9 TCP/MRCPv2 1
a=setup:passive
a=connection:new
a=channel:%s@speechrecog
a=cmid:1
m=application 9 TCP/MRCPv2 1
a=setup:passive
a=connection:new
a=channel:%s@speechsynth
a=cmid:1
`, rtpPeer.LocalAddr().(*net.UDPAddr).Port, testRecogID, testSynthID)

	inviteCtx, cancelInvite := context.WithTimeout(ctx, 15*time.Second)
	defer cancelInvite()

	contentType := sip.ContentTypeHeader("application/sdp")
	dlg, err := dialogUA.Invite(inviteCtx,
		sip.Uri{User: "mrcpgo", Host: host, Port: port}, []byte(offer), &contentType)
	if err != nil {
		t.Fatalf("send INVITE: %v", err)
	}
	if err := dlg.WaitAnswer(inviteCtx, sipgo.AnswerOptions{
		OnResponse: func(res *sip.Response) error {
			t.Logf("received %d %s", res.StatusCode, res.Reason)
			return nil
		},
	}); err != nil {
		t.Fatalf("wait for answer: %v", err)
	}
	if dlg.InviteResponse.StatusCode != sip.StatusOK {
		t.Fatalf("INVITE status = %d", dlg.InviteResponse.StatusCode)
	}

	answer, err := sdp.Parse(dlg.InviteResponse.Body())
	if err != nil {
		t.Fatalf("parse answer: %v\n%s", err, dlg.InviteResponse.Body())
	}
	if len(answer.Media) != 3 {
		t.Fatalf("answer media sections = %d, want 3", len(answer.Media))
	}
	audio := answer.Media[0]
	recogMedia := answer.Media[1]
	synthMedia := answer.Media[2]
	if audio.Type != "audio" || audio.Port == 0 {
		t.Fatalf("answer audio = %+v", audio)
	}
	if !recogMedia.IsMRCP() || !synthMedia.IsMRCP() {
		t.Fatalf("answer control sections are not MRCP: %+v %+v", recogMedia, synthMedia)
	}
	if recogMedia.Port == 0 || synthMedia.Port == 0 {
		t.Fatalf("answer control ports = %d/%d", recogMedia.Port, synthMedia.Port)
	}

	// Confirm the dialog. This unblocks the server's 200 OK and activates the
	// session.
	if err := dlg.Ack(inviteCtx); err != nil {
		t.Fatalf("send ACK: %v", err)
	}
	waitFor(t, 3*time.Second, "session registration", func() bool {
		return srv.Sessions().Len() == 1
	})

	// --- Recognizer channel ---
	recogAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(recogMedia.Port))
	recogConn, err := net.DialTimeout("tcp", recogAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("connect recognizer channel: %v", err)
	}
	defer recogConn.Close()
	recogReader := bufio.NewReader(recogConn)

	writeMRCP(t, recogConn, &mrcp.Message{
		Type:      mrcp.TypeRequest,
		Method:    mrcp.MethodRecognize,
		RequestID: 1,
		Headers: mrcp.Headers{
			{Name: mrcp.HeaderChannelIdentifier, Value: testRecogID + "@speechrecog"},
		},
		Body: []byte("builtin:grammar/hello"),
	})

	res := readMRCP(t, recogConn, recogReader)
	if res.Type != mrcp.TypeResponse || res.StatusCode != mrcp.StatusSuccess {
		t.Fatalf("RECOGNIZE response = %+v", res)
	}
	if res.RequestState != mrcp.StateInProgress {
		t.Fatalf("RECOGNIZE state = %q", res.RequestState)
	}

	var recognition *mrcp.Message
	for range 3 {
		msg := readMRCP(t, recogConn, recogReader)
		if msg.Type == mrcp.TypeEvent && msg.EventName == mrcp.EventRecognitionComplete {
			recognition = msg
			break
		}
		if msg.Type == mrcp.TypeEvent && msg.EventName == mrcp.EventStartOfInput {
			continue
		}
		t.Fatalf("unexpected message %+v", msg)
	}
	if recognition == nil {
		t.Fatal("no RECOGNITION-COMPLETE event")
	}
	if len(recognition.Body) == 0 {
		t.Fatal("RECOGNITION-COMPLETE without NLSML body")
	}
	if recognition.ContentType() != mrcp.ContentTypeNLSML {
		t.Fatalf("NLSML content type = %q", recognition.ContentType())
	}

	// --- Synthesizer channel ---
	synthAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(synthMedia.Port))
	synthConn, err := net.DialTimeout("tcp", synthAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("connect synthesizer channel: %v", err)
	}
	defer synthConn.Close()
	synthReader := bufio.NewReader(synthConn)

	writeMRCP(t, synthConn, &mrcp.Message{
		Type:      mrcp.TypeRequest,
		Method:    mrcp.MethodSpeak,
		RequestID: 2,
		Headers: mrcp.Headers{
			{Name: mrcp.HeaderChannelIdentifier, Value: testSynthID + "@speechsynth"},
			{Name: mrcp.HeaderContentType, Value: mrcp.ContentTypeSSML},
		},
		Body: []byte("<speak>hello</speak>"),
	})
	res = readMRCP(t, synthConn, synthReader)
	if res.StatusCode != mrcp.StatusSuccess || res.RequestState != mrcp.StateInProgress {
		t.Fatalf("SPEAK response = %+v", res)
	}

	// The server must be streaming 20 ms PCMU frames to the negotiated port.
	if err := rtpPeer.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set RTP deadline: %v", err)
	}
	buf := make([]byte, 1500)
	n, _, err := rtpPeer.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read RTP: %v", err)
	}
	pkt := &rtp.Packet{}
	if err := pkt.Unmarshal(buf[:n]); err != nil {
		t.Fatalf("unmarshal RTP: %v", err)
	}
	if pkt.PayloadType != 0 {
		t.Fatalf("RTP payload type = %d", pkt.PayloadType)
	}
	if len(pkt.Payload) != 160 {
		t.Fatalf("RTP payload size = %d", len(pkt.Payload))
	}

	msg := readMRCP(t, synthConn, synthReader)
	if msg.Type != mrcp.TypeEvent || msg.EventName != mrcp.EventSpeakComplete {
		t.Fatalf("SPEAK event = %+v", msg)
	}
	if msg.RequestState != mrcp.StateComplete {
		t.Fatalf("SPEAK-COMPLETE state = %q", msg.RequestState)
	}

	// --- Teardown ---
	if err := dlg.Bye(inviteCtx); err != nil {
		t.Fatalf("send BYE: %v", err)
	}
	waitFor(t, 5*time.Second, "session removal", func() bool {
		return srv.Sessions().Len() == 0
	})

	// Control listeners must be closed.
	waitFor(t, 3*time.Second, "control listener release", func() bool {
		conn, err := net.DialTimeout("tcp", recogAddr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return false
		}
		return true
	})

	// The RTP socket must be released, so the port must be bindable again.
	waitFor(t, 3*time.Second, "RTP socket release", func() bool {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: audio.Port})
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	})
	if stats := srv.Sessions().Len(); stats != 0 {
		t.Fatalf("sessions after teardown = %d", stats)
	}

	_ = recogConn.Close()
	_ = synthConn.Close()
	_ = dlg.Close()
	_ = client.Close()
	if err := ua.Close(); err != nil {
		t.Errorf("close UAC: %v", err)
	}

	stopped = true
	cancel()
	if err := <-serveDone; err != nil {
		t.Fatalf("server stopped with error: %v", err)
	}

	checkGoroutineLeaks(t)
}

// TestInviteWithoutMRCPIsRejected covers the 488 path.
func TestInviteWithoutMRCPIsRejected(t *testing.T) {
	cfg := config.Default()
	cfg.SIP.Listen = "127.0.0.1:0"
	cfg.SIP.AdvertiseAddr = "127.0.0.1"
	cfg.RTP.ListenHost = "127.0.0.1"
	cfg.MRCP.ListenHost = "127.0.0.1"

	srv, err := server.New(cfg, discardLogger(), mock.NewFactory())
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	addr, err := srv.Listen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ctx) }()
	defer func() {
		cancel()
		if err := <-serveDone; err != nil {
			t.Errorf("server stopped with error: %v", err)
		}
	}()

	ua, err := sipgo.NewUA(sipgo.WithUserAgent("mrcpgo-test-uac"))
	if err != nil {
		t.Fatalf("create UAC: %v", err)
	}
	defer ua.Close()
	client, err := sipgo.NewClient(ua)
	if err != nil {
		t.Fatalf("create UAC client: %v", err)
	}
	dialogUA := &sipgo.DialogUA{Client: client, ContactHDR: sip.ContactHeader{Address: sip.Uri{User: "fs"}}}

	host, portText, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portText)

	offer := "v=0\r\no=fs 1 1 IN IP4 127.0.0.1\r\ns=fs\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\n" +
		"m=audio 40000 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n"

	inviteCtx, cancelInvite := context.WithTimeout(ctx, 10*time.Second)
	defer cancelInvite()
	contentType := sip.ContentTypeHeader("application/sdp")
	dlg, err := dialogUA.Invite(inviteCtx, sip.Uri{User: "mrcpgo", Host: host, Port: port},
		[]byte(offer), &contentType)
	if err != nil {
		t.Fatalf("send INVITE: %v", err)
	}

	err = dlg.WaitAnswer(inviteCtx, sipgo.AnswerOptions{})
	var resErr *sipgo.ErrDialogResponse
	if err == nil || !errors.As(err, &resErr) {
		t.Fatalf("expected a failure response, got %v", err)
	}
	if resErr.Res.StatusCode != sip.StatusNotAcceptableHere {
		t.Fatalf("status = %d, want %d", resErr.Res.StatusCode, sip.StatusNotAcceptableHere)
	}

	_ = dlg.Close()
	_ = client.Close()
}

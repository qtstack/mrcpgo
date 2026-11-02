//go:build integration

package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/gorilla/websocket"
	"github.com/pion/rtp"

	"github.com/qtstack/mrcpgo/internal/config"
	"github.com/qtstack/mrcpgo/internal/engine/xfyun"
	"github.com/qtstack/mrcpgo/internal/mrcp"
	"github.com/qtstack/mrcpgo/internal/sdp"
	"github.com/qtstack/mrcpgo/internal/server"
)

// fakeXfyunSpeech emulates the rtasr and online TTS WebSocket endpoints.
func fakeXfyunSpeech(t *testing.T) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		switch r.URL.Path {
		case "/v1/ws":
			_ = conn.WriteJSON(map[string]any{"action": "started", "code": "0", "desc": "success"})
			for {
				messageType, _, err := conn.ReadMessage()
				if err != nil {
					return
				}
				if messageType == websocket.BinaryMessage {
					break
				}
			}
			inner := map[string]any{
				"cn": map[string]any{
					"st": map[string]any{
						"rt": []any{
							map[string]any{"ws": []any{
								map[string]any{"cw": []any{map[string]any{"w": "打开空调"}}},
							}},
						},
						"type": "0",
					},
				},
				"seg_id": 0,
			}
			encoded, _ := json.Marshal(inner)
			_ = conn.WriteJSON(map[string]any{
				"action": "result", "code": "0", "data": string(encoded), "desc": "success",
			})
		case "/v2/tts":
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			_ = conn.WriteJSON(map[string]any{
				"code": 0,
				"data": map[string]any{
					"audio":  base64.StdEncoding.EncodeToString(make([]byte, 1600)),
					"status": 2,
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestCallWithXfyunEngine drives a full call where both engines talk to a fake
// iFlytek WebSocket service.
func TestCallWithXfyunEngine(t *testing.T) {
	cloud := fakeXfyunSpeech(t)
	base := wsEndpoint(cloud)

	cfg := config.Default()
	cfg.SIP.Listen = "127.0.0.1:0"
	cfg.SIP.AdvertiseAddr = "127.0.0.1"
	cfg.RTP.ListenHost = "127.0.0.1"
	cfg.MRCP.ListenHost = "127.0.0.1"
	cfg.MRCP.Engine.Type = "xfyun"
	cfg.MRCP.Engine.Xfyun.AppID = "test-app"
	cfg.MRCP.Engine.Xfyun.APIKey = "test-api-key"
	cfg.MRCP.Engine.Xfyun.APISecret = "test-api-secret"
	cfg.MRCP.Engine.Xfyun.ASREndpoint = base + "/v1/ws"
	cfg.MRCP.Engine.Xfyun.TTSEndpoint = base + "/v2/tts"
	cfg.MRCP.Engine.Xfyun.Timeout = config.Duration(5 * time.Second)
	cfg.MRCP.Engine.Xfyun.ASR.ChunkMS = 40
	cfg.MRCP.Engine.Xfyun.TTS.SampleRate = 8000

	log := discardLogger()
	engines, err := xfyun.NewFactory(cfg.MRCP.Engine.Xfyun, log)
	if err != nil {
		t.Fatalf("create engine factory: %v", err)
	}
	srv, err := server.New(cfg, log, engines)
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
			<-serveDone
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

	host, portText, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portText)

	offer := fmt.Sprintf(`v=0
o=FreeSWITCH 1687513427 1687513428 IN IP4 127.0.0.1
s=FreeSWITCH
c=IN IP4 127.0.0.1
t=0 0
m=audio %d RTP/AVP 0
a=rtpmap:0 PCMU/8000
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

	inviteCtx, cancelInvite := context.WithTimeout(ctx, 20*time.Second)
	defer cancelInvite()
	contentType := sip.ContentTypeHeader("application/sdp")

	dlg, err := dialogUA.Invite(inviteCtx, sip.Uri{User: "mrcpgo", Host: host, Port: port},
		[]byte(offer), &contentType)
	if err != nil {
		t.Fatalf("send INVITE: %v", err)
	}
	if err := dlg.WaitAnswer(inviteCtx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("wait for answer: %v", err)
	}
	if dlg.InviteResponse.StatusCode != sip.StatusOK {
		t.Fatalf("INVITE status = %d", dlg.InviteResponse.StatusCode)
	}
	answer, err := sdp.Parse(dlg.InviteResponse.Body())
	if err != nil {
		t.Fatalf("parse answer: %v", err)
	}
	if len(answer.Media) != 3 {
		t.Fatalf("answer media sections = %d", len(answer.Media))
	}
	audioPort := answer.Media[0].Port
	recogPort := answer.Media[1].Port
	synthPort := answer.Media[2].Port

	if err := dlg.Ack(inviteCtx); err != nil {
		t.Fatalf("send ACK: %v", err)
	}
	waitFor(t, 3*time.Second, "session registration", func() bool {
		return srv.Sessions().Len() == 1
	})

	// --- Recognizer ---
	recogConn, err := net.DialTimeout("tcp",
		net.JoinHostPort("127.0.0.1", strconv.Itoa(recogPort)), 3*time.Second)
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
	if res := readMRCP(t, recogConn, recogReader); res.RequestState != mrcp.StateInProgress {
		t.Fatalf("RECOGNIZE response = %+v", res)
	}

	audioAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: audioPort}
	// rtasr needs 40 ms (two 20 ms frames) before the first upstream chunk.
	for i := range 4 {
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    0,
				SequenceNumber: uint16(1000 + i),
				Timestamp:      uint32(i) * 160,
				SSRC:           0x99AABBCC,
			},
			Payload: bytes.Repeat([]byte{0xFF}, 160),
		}
		raw, err := pkt.Marshal()
		if err != nil {
			t.Fatalf("marshal RTP: %v", err)
		}
		if _, err := rtpPeer.WriteToUDP(raw, audioAddr); err != nil {
			t.Fatalf("send RTP: %v", err)
		}
	}

	var recognition *mrcp.Message
	for range 3 {
		msg := readMRCP(t, recogConn, recogReader)
		if msg.Type == mrcp.TypeEvent && msg.EventName == mrcp.EventRecognitionComplete {
			recognition = msg
			break
		}
	}
	if recognition == nil {
		t.Fatal("no RECOGNITION-COMPLETE event")
	}
	if !strings.Contains(string(recognition.Body), "打开空调") {
		t.Fatalf("NLSML = %s", recognition.Body)
	}

	// --- Synthesizer ---
	synthConn, err := net.DialTimeout("tcp",
		net.JoinHostPort("127.0.0.1", strconv.Itoa(synthPort)), 3*time.Second)
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
		},
		Body: []byte("hello from xfyun"),
	})
	if res := readMRCP(t, synthConn, synthReader); res.RequestState != mrcp.StateInProgress {
		t.Fatalf("SPEAK response = %+v", res)
	}

	if err := rtpPeer.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set RTP deadline: %v", err)
	}
	buf := make([]byte, 1500)
	n, _, err := rtpPeer.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read RTP: %v", err)
	}
	frame := &rtp.Packet{}
	if err := frame.Unmarshal(buf[:n]); err != nil {
		t.Fatalf("unmarshal RTP: %v", err)
	}
	if frame.PayloadType != 0 || len(frame.Payload) != 160 {
		t.Fatalf("RTP frame = %+v", frame)
	}

	ev := readMRCP(t, synthConn, synthReader)
	if ev.Type != mrcp.TypeEvent || ev.EventName != mrcp.EventSpeakComplete {
		t.Fatalf("SPEAK event = %+v", ev)
	}

	// --- Teardown ---
	if err := dlg.Bye(inviteCtx); err != nil {
		t.Fatalf("send BYE: %v", err)
	}
	waitFor(t, 5*time.Second, "session removal", func() bool {
		return srv.Sessions().Len() == 0
	})

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

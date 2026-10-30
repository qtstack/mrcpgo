package sdp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readOffer(t *testing.T) *Session {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "freeswitch_offer.sdp")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read offer fixture: %v", err)
	}
	offer, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse offer: %v", err)
	}
	return offer
}

func TestParseFreeSwitchOffer(t *testing.T) {
	offer := readOffer(t)

	if got, want := offer.Connection, "192.168.1.20"; got != want {
		t.Errorf("session connection = %q, want %q", got, want)
	}
	if len(offer.Media) != 3 {
		t.Fatalf("media sections = %d, want 3", len(offer.Media))
	}

	audio := offer.FirstAudio()
	if audio == nil {
		t.Fatal("no audio section")
	}
	if audio.Port != 23984 {
		t.Errorf("audio port = %d", audio.Port)
	}
	if !audio.AcceptsPayloadType(0) {
		t.Error("audio does not offer PCMU")
	}
	if !audio.AcceptsPayloadType(101) {
		t.Error("audio does not offer telephone-event")
	}
	if v, _ := audio.Attribute("rtpmap"); !strings.HasPrefix(v, "0 PCMU/8000") {
		t.Errorf("rtpmap = %q", v)
	}

	controls := offer.MRCPMedia()
	if len(controls) != 2 {
		t.Fatalf("MRCP sections = %d, want 2", len(controls))
	}

	wantResources := []string{"speechrecog", "speechsynth"}
	for i, want := range wantResources {
		id, resource, ok := controls[i].ChannelIdentifier()
		if !ok {
			t.Fatalf("control %d: missing channel identifier", i)
		}
		if id != "32a30720ab3e9d4d" {
			t.Errorf("control %d: id = %q", i, id)
		}
		if resource != want {
			t.Errorf("control %d: resource = %q, want %q", i, resource, want)
		}
		if cmid := controls[i].Cmid(); cmid != "1" {
			t.Errorf("control %d: cmid = %q", i, cmid)
		}
		if setup := controls[i].Setup(); setup != "passive" {
			t.Errorf("control %d: setup = %q", i, setup)
		}
	}
}

func TestBuildAnswer(t *testing.T) {
	offer := readOffer(t)

	answer, err := BuildAnswer(offer, AnswerOptions{
		User:           "mrcpgo",
		SessionID:      1234,
		SessionVersion: 1,
		Host:           "127.0.0.1",
		Entries: []MediaEntry{
			{Port: 40000},
			{Port: 50000, ChannelID: "32a30720ab3e9d4d", Resource: "speechrecog"},
			{Port: 50001, ChannelID: "32a30720ab3e9d4d", Resource: "speechsynth"},
		},
	})
	if err != nil {
		t.Fatalf("BuildAnswer: %v", err)
	}

	parsed, err := Parse(answer)
	if err != nil {
		t.Fatalf("parse answer: %v\n%s", err, answer)
	}
	if parsed.Connection != "127.0.0.1" {
		t.Errorf("answer connection = %q", parsed.Connection)
	}
	if !strings.HasPrefix(parsed.Origin, "mrcpgo 1234 1 IN IP4 127.0.0.1") {
		t.Errorf("origin = %q", parsed.Origin)
	}
	if len(parsed.Media) != len(offer.Media) {
		t.Fatalf("answer media sections = %d, want %d", len(parsed.Media), len(offer.Media))
	}

	audio := parsed.Media[0]
	if audio.Type != "audio" || audio.Port != 40000 {
		t.Fatalf("audio answer = %+v", audio)
	}
	if audio.Protocol != "RTP/AVP" || len(audio.Formats) != 1 || audio.Formats[0] != "0" {
		t.Errorf("audio formats = %v %v", audio.Protocol, audio.Formats)
	}
	if v, ok := audio.Attribute("rtpmap"); !ok || v != "0 PCMU/8000" {
		t.Errorf("audio rtpmap = %q", v)
	}
	if v, ok := audio.Attribute("sendrecv"); !ok || v != "" {
		t.Errorf("audio sendrecv attribute missing: %q", v)
	}

	for i, wantPort := range []int{50000, 50001} {
		m := parsed.Media[i+1]
		if !m.IsMRCP() {
			t.Fatalf("media %d is not MRCP", i+1)
		}
		if m.Port != wantPort {
			t.Errorf("control %d port = %d, want %d", i, m.Port, wantPort)
		}
		if got := m.Setup(); got != "passive" {
			t.Errorf("control %d setup = %q", i, got)
		}
		if v, ok := m.Attribute("connection"); !ok || v != "new" {
			t.Errorf("control %d connection = %q", i, v)
		}
		if got := m.Cmid(); got != "1" {
			t.Errorf("control %d cmid = %q", i, got)
		}
		id, resource, ok := m.ChannelIdentifier()
		if !ok || id != "32a30720ab3e9d4d" {
			t.Errorf("control %d channel = %q %v", i, id, ok)
		}
		if resource != []string{"speechrecog", "speechsynth"}[i] {
			t.Errorf("control %d resource = %q", i, resource)
		}
	}
}

func TestBuildAnswerRejectsUnsupportedMedia(t *testing.T) {
	offer, err := Parse([]byte("v=0\r\nc=IN IP4 10.0.0.1\r\n" +
		"m=video 9000 RTP/AVP 96\r\n" +
		"m=audio 9002 RTP/AVP 0\r\n" +
		"m=application 9 TCP/MRCPv2 1\r\na=setup:passive\r\na=channel:1@speechrecog\r\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	answer, err := BuildAnswer(offer, AnswerOptions{
		Host: "10.0.0.2",
		Entries: []MediaEntry{
			{}, // video rejected
			{Port: 4000},
			{Port: 5000, ChannelID: "1", Resource: "speechrecog"},
		},
	})
	if err != nil {
		t.Fatalf("BuildAnswer: %v", err)
	}
	parsed, err := Parse(answer)
	if err != nil {
		t.Fatalf("parse answer: %v", err)
	}
	if len(parsed.Media) != 3 {
		t.Fatalf("media sections = %d", len(parsed.Media))
	}
	if parsed.Media[0].Type != "video" || parsed.Media[0].Port != 0 {
		t.Errorf("rejected video = %+v", parsed.Media[0])
	}
	if parsed.Media[0].Protocol != "RTP/AVP" || len(parsed.Media[0].Formats) != 1 {
		t.Errorf("rejected video formats = %v", parsed.Media[0].Formats)
	}
}

func TestBuildAnswerValidation(t *testing.T) {
	if _, err := BuildAnswer(nil, AnswerOptions{Host: "127.0.0.1"}); err == nil {
		t.Error("expected error for nil offer")
	}
	offer, _ := Parse([]byte("v=0\r\nm=application 9 TCP/MRCPv2 1\r\na=channel:1@speechrecog\r\n"))
	if _, err := BuildAnswer(offer, AnswerOptions{}); err == nil {
		t.Error("expected error for missing host")
	}
	if _, err := BuildAnswer(offer, AnswerOptions{Host: "127.0.0.1", Entries: []MediaEntry{{Port: 5000}}}); err == nil {
		t.Error("expected error for control answer without channel id")
	}
}

func TestParseChannelIdentifier(t *testing.T) {
	tests := []struct {
		value    string
		id       string
		resource string
		ok       bool
	}{
		{"abc@speechrecog", "abc", "speechrecog", true},
		{"@speechsynth", "", "speechsynth", true},
		{"no-at-sign", "", "", false},
		{"abc@", "", "", false},
	}
	for _, tt := range tests {
		id, resource, ok := ParseChannelIdentifier(tt.value)
		if id != tt.id || resource != tt.resource || ok != tt.ok {
			t.Errorf("%q = (%q, %q, %v), want (%q, %q, %v)",
				tt.value, id, resource, ok, tt.id, tt.resource, tt.ok)
		}
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"no version", "o=- 1 1 IN IP4 127.0.0.1\r\n"},
		{"bad media line", "v=0\r\nm=audio\r\n"},
		{"bad port", "v=0\r\nm=audio abc RTP/AVP 0\r\n"},
		{"bad connection", "v=0\r\nc=IN IP4\r\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.body)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestParseToleratesUnknownLines(t *testing.T) {
	offer, err := Parse([]byte("v=0\r\n" +
		"o=- 1 1 IN IP4 127.0.0.1\r\n" +
		"s=-\r\n" +
		"i=unknown session line\r\n" +
		"u=http://example.com\r\n" +
		"e=test@example.com\r\n" +
		"p=+1 555 0100\r\n" +
		"b=AS:64\r\n" +
		"t=0 0\r\n" +
		"z=2882844526 -1h\r\n" +
		"k=clear:secret\r\n" +
		"a=unknown:value\r\n" +
		"m=audio 9002 RTP/AVP 0\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(offer.Media) != 1 || offer.Media[0].Port != 9002 {
		t.Fatalf("media = %+v", offer.Media)
	}
}

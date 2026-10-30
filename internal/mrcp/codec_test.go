package mrcp

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readOne(t *testing.T, raw string) (*Message, error) {
	t.Helper()
	return ReadMessage(bufio.NewReader(strings.NewReader(raw)))
}

func mustReadOne(t *testing.T, raw string) *Message {
	t.Helper()
	msg, err := readOne(t, raw)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	return msg
}

func TestReadMessage(t *testing.T) {
	t.Run("request without body", func(t *testing.T) {
		raw := "MRCP/2.0 112 RECOGNIZE 7\r\n" +
			"Channel-Identifier: 32a30720ab3e9d4d@speechrecog\r\n" +
			"Content-Type: application/srgs+xml\r\n"
		msg := mustReadOne(t, raw)
		if msg.Type != TypeRequest {
			t.Fatalf("type = %v, want request", msg.Type)
		}
		if msg.Method != MethodRecognize {
			t.Errorf("method = %q", msg.Method)
		}
		if msg.RequestID != 7 {
			t.Errorf("request id = %d", msg.RequestID)
		}
		if got := msg.ChannelIdentifier(); got != "32a30720ab3e9d4d@speechrecog" {
			t.Errorf("channel identifier = %q", got)
		}
		if msg.ContentType() != "application/srgs+xml" {
			t.Errorf("content type = %q", msg.ContentType())
		}
		if msg.Body != nil {
			t.Errorf("body = %q, want nil", msg.Body)
		}
		if msg.Length != len(raw) {
			t.Errorf("length = %d, want %d", msg.Length, len(raw))
		}
	})

	t.Run("response", func(t *testing.T) {
		raw := "MRCP/2.0 66 7 200 IN-PROGRESS\r\n" +
			"Channel-Identifier: a@speechsynth\r\n"
		msg := mustReadOne(t, raw)
		if msg.Type != TypeResponse {
			t.Fatalf("type = %v", msg.Type)
		}
		if msg.RequestID != 7 || msg.StatusCode != StatusSuccess {
			t.Errorf("id/status = %d/%d", msg.RequestID, msg.StatusCode)
		}
		if msg.RequestState != StateInProgress {
			t.Errorf("state = %q", msg.RequestState)
		}
	})

	t.Run("event with body", func(t *testing.T) {
		body := "<?xml version=\"1.0\"?>\n<result/>"
		raw := "MRCP/2.0 171 RECOGNITION-COMPLETE 7 COMPLETE\r\n" +
			"Channel-Identifier: a@speechrecog\r\n" +
			"Content-Type: application/nlsml+xml\r\n" +
			"Content-Length: 31\r\n" +
			"\r\n" + body
		msg := mustReadOne(t, raw)
		if msg.Type != TypeEvent {
			t.Fatalf("type = %v", msg.Type)
		}
		if msg.EventName != EventRecognitionComplete {
			t.Errorf("event = %q", msg.EventName)
		}
		if msg.RequestState != StateComplete {
			t.Errorf("state = %q", msg.RequestState)
		}
		if string(msg.Body) != body {
			t.Errorf("body = %q", msg.Body)
		}
	})

	t.Run("body spanning the read buffer", func(t *testing.T) {
		body := strings.Repeat("x", 10000)
		raw := "MRCP/2.0 10084 SPEAK 1\r\n" +
			"Channel-Identifier: a@speechsynth\r\n" +
			"Content-Length: 10000\r\n" +
			"\r\n" + body
		msg := mustReadOne(t, raw)
		if len(msg.Body) != len(body) {
			t.Fatalf("body length = %d", len(msg.Body))
		}
	})

	t.Run("no headers and no body", func(t *testing.T) {
		raw := "MRCP/2.0 20 STOP 1\r\n"
		msg := mustReadOne(t, raw)
		if msg.Method != MethodStop {
			t.Errorf("method = %q", msg.Method)
		}
		if len(msg.Headers) != 0 {
			t.Errorf("headers = %v", msg.Headers)
		}
	})

	t.Run("two messages on one stream", func(t *testing.T) {
		first := "MRCP/2.0 55 STOP 1\r\nChannel-Identifier: a@speechsynth\r\n"
		second := "MRCP/2.0 55 STOP 2\r\nChannel-Identifier: a@speechsynth\r\n"
		r := bufio.NewReader(strings.NewReader(first + second))
		m1, err := ReadMessage(r)
		if err != nil {
			t.Fatalf("first: %v", err)
		}
		m2, err := ReadMessage(r)
		if err != nil {
			t.Fatalf("second: %v", err)
		}
		if m1.RequestID != 1 || m2.RequestID != 2 {
			t.Errorf("ids = %d, %d", m1.RequestID, m2.RequestID)
		}
	})
}

func TestReadMessageErrors(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want error
	}{
		{
			name: "bad version",
			raw:  "MRCP/1.0 23 STOP 1\r\n",
			want: ErrMalformedStartLine,
		},
		{
			name: "not mrcp",
			raw:  "SIP/2.0 23 STOP 1\r\n",
			want: ErrMalformedStartLine,
		},
		{
			name: "non numeric length",
			raw:  "MRCP/2.0 abc RECOGNIZE 1\r\n",
			want: ErrMalformedStartLine,
		},
		{
			name: "length shorter than start line",
			raw:  "MRCP/2.0 5 RECOGNIZE 1\r\n",
			want: ErrMalformedStartLine,
		},
		{
			name: "start line without CRLF",
			raw:  "MRCP/2.0 23 STOP 1\n",
			want: ErrMalformedStartLine,
		},
		{
			name: "content length mismatch",
			raw: "MRCP/2.0 80 SPEAK 1\r\n" +
				"Channel-Identifier: a@speechsynth\r\n" +
				"Content-Length: 4\r\n\r\nabc",
			want: ErrContentLengthMismatch,
		},
		{
			name: "body without content length",
			raw: "MRCP/2.0 61 SPEAK 1\r\n" +
				"Channel-Identifier: a@speechsynth\r\n\r\nabc",
			want: ErrMalformedHeaders,
		},
		{
			name: "invalid request state",
			raw:  "MRCP/2.0 59 7 200 DONE\r\nChannel-Identifier: a@speechsynth\r\n",
			want: ErrMalformedStartLine,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readOne(t, tt.raw)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestReadMessageTooLarge(t *testing.T) {
	raw := "MRCP/2.0 999999 SPEAK 1\r\nChannel-Identifier: a@speechsynth\r\n"
	if _, err := ReadMessageLimit(bufio.NewReader(strings.NewReader(raw)), 1024); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("error = %v", err)
	}
}

func TestReadMessageTruncated(t *testing.T) {
	raw := "MRCP/2.0 60 RECOGNIZE 7\r\nChannel-Identifier: a@speechrecog\r\n"
	raw = raw[:len(raw)-10]
	_, err := readOne(t, raw)
	if !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		t.Fatalf("error = %v", err)
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		msg  *Message
	}{
		{
			name: "request",
			msg: &Message{
				Type:      TypeRequest,
				RequestID: 42,
				Method:    MethodRecognize,
				Headers:   Headers{{HeaderChannelIdentifier, "abc@speechrecog"}},
			},
		},
		{
			name: "request with body",
			msg: &Message{
				Type:      TypeRequest,
				RequestID: 42,
				Method:    MethodSpeak,
				Headers: Headers{
					{HeaderChannelIdentifier, "abc@speechsynth"},
					{HeaderContentType, ContentTypeSSML},
				},
				Body: []byte("<speak>hello</speak>"),
			},
		},
		{
			name: "response",
			msg:  NewResponse(&Message{RequestID: 3, Headers: Headers{{HeaderChannelIdentifier, "abc@speechsynth"}}}, StatusSuccess, StateInProgress),
		},
		{
			name: "event",
			msg: func() *Message {
				ev := NewEvent("abc@speechrecog", EventRecognitionComplete, 9, StateComplete)
				ev.Headers.Set(HeaderCompletionCause, "000 success")
				ev.Headers.Set(HeaderContentType, ContentTypeNLSML)
				ev.Body = []byte("<result/>")
				return ev
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := tt.msg.MarshalBinary()
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			decoded, err := ReadMessage(bufio.NewReader(bytes.NewReader(encoded)))
			if err != nil {
				t.Fatalf("read: %v\nmessage:\n%s", err, encoded)
			}
			if decoded.Type != tt.msg.Type || decoded.RequestID != tt.msg.RequestID {
				t.Fatalf("type/id = %v/%d", decoded.Type, decoded.RequestID)
			}
			if decoded.Method != tt.msg.Method {
				t.Errorf("method = %q, want %q", decoded.Method, tt.msg.Method)
			}
			if decoded.EventName != tt.msg.EventName {
				t.Errorf("event = %q, want %q", decoded.EventName, tt.msg.EventName)
			}
			if decoded.RequestState != tt.msg.RequestState {
				t.Errorf("state = %q, want %q", decoded.RequestState, tt.msg.RequestState)
			}
			if !bytes.Equal(decoded.Body, tt.msg.Body) {
				t.Errorf("body = %q, want %q", decoded.Body, tt.msg.Body)
			}
			if decoded.Length != len(encoded) {
				t.Errorf("message-length = %d, actual = %d", decoded.Length, len(encoded))
			}
			// Re-encoding must be byte identical.
			reencoded, err := decoded.MarshalBinary()
			if err != nil {
				t.Fatalf("re-marshal: %v", err)
			}
			if !bytes.Equal(encoded, reencoded) {
				t.Errorf("re-encoded message differs:\n%q\n%q", encoded, reencoded)
			}
		})
	}
}

func TestMarshalAddsContentLength(t *testing.T) {
	msg := &Message{
		Type:         TypeEvent,
		EventName:    EventSpeakComplete,
		RequestID:    1,
		RequestState: StateComplete,
		Headers:      Headers{{HeaderChannelIdentifier, "a@speechsynth"}},
		Body:         []byte("payload"),
	}
	encoded, err := msg.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(encoded, []byte("Content-Length: 7\r\n")) {
		t.Fatalf("missing content-length:\n%q", encoded)
	}
}

func TestMarshalRejectsInconsistentContentLength(t *testing.T) {
	msg := &Message{
		Type:      TypeRequest,
		RequestID: 1,
		Method:    MethodSpeak,
		Headers:   Headers{{HeaderContentLength, "99"}},
		Body:      []byte("payload"),
	}
	if _, err := msg.MarshalBinary(); !errors.Is(err, ErrContentLengthMismatch) {
		t.Fatalf("error = %v", err)
	}
}

func TestHeaders(t *testing.T) {
	h := Headers{{"Channel-Identifier", "a@b"}}
	if v, ok := h.Get("channel-identifier"); !ok || v != "a@b" {
		t.Fatalf("get = %q, %v", v, ok)
	}
	h.Set("CHANNEL-IDENTIFIER", "c@d")
	if len(h) != 1 || h[0].Value != "c@d" {
		t.Fatalf("set = %v", h)
	}
	h.Add("Content-Type", "text/plain")
	h.Add("content-type", "text/html")
	if got := h.Values("Content-Type"); len(got) != 2 {
		t.Fatalf("values = %v", got)
	}
	h.Del("content-type")
	if _, ok := h.Get("Content-Type"); ok {
		t.Fatal("delete failed")
	}
}

func TestFixtureFiles(t *testing.T) {
	cases := []struct {
		file string
		typ  Type
	}{
		{"mrcp_recognize.txt", TypeRequest},
		{"mrcp_recognition_complete.txt", TypeEvent},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			path := filepath.Join("..", "..", "testdata", tc.file)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			msg, err := ReadMessage(bufio.NewReader(bytes.NewReader(raw)))
			if err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			if msg.Type != tc.typ {
				t.Fatalf("type = %v, want %v", msg.Type, tc.typ)
			}
			encoded, err := msg.MarshalBinary()
			if err != nil {
				t.Fatalf("marshal fixture: %v", err)
			}
			if !bytes.Equal(encoded, raw) {
				t.Fatalf("round trip differs:\n got: %q\nwant: %q", encoded, raw)
			}
		})
	}
}

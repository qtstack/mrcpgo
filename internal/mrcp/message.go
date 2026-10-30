// Package mrcp implements the MRCPv2 (RFC 6787) text protocol: the wire
// codec, the channel state machine and request dispatch for the speech
// recognizer and synthesizer resources.
package mrcp

import (
	"fmt"
	"strings"
)

// Type is the MRCP message type encoded in the start line.
type Type uint8

const (
	// TypeRequest is a client to server request.
	TypeRequest Type = iota + 1
	// TypeResponse is a server to client response to a request.
	TypeResponse
	// TypeEvent is an asynchronous server to client event.
	TypeEvent
)

// String implements fmt.Stringer.
func (t Type) String() string {
	switch t {
	case TypeRequest:
		return "request"
	case TypeResponse:
		return "response"
	case TypeEvent:
		return "event"
	default:
		return fmt.Sprintf("Type(%d)", uint8(t))
	}
}

// Version is the only MRCP version this implementation speaks.
const Version = "2.0"

// Method is an MRCP method name. Method names are case sensitive on the wire
// and MUST be upper case (RFC 6787 section 6).
type Method string

// Methods supported by this implementation. Methods outside this list are
// answered with 501 Not Implemented.
const (
	MethodSetParams        Method = "SET-PARAMS"
	MethodGetParams        Method = "GET-PARAMS"
	MethodDefineGrammar    Method = "DEFINE-GRAMMAR"
	MethodRecognize        Method = "RECOGNIZE"
	MethodStartInputTimers Method = "START-INPUT-TIMERS"
	MethodStop             Method = "STOP"
	MethodSpeak            Method = "SPEAK"
)

// Event names emitted by this implementation.
const (
	EventRecognitionComplete = "RECOGNITION-COMPLETE"
	EventStartOfInput        = "START-OF-INPUT"
	EventSpeakComplete       = "SPEAK-COMPLETE"
)

// RequestState is the state of the request as reported in responses and
// events (RFC 6787 section 5.1).
type RequestState string

const (
	// StateComplete means the request has completed.
	StateComplete RequestState = "COMPLETE"
	// StateInProgress means the request is being processed.
	StateInProgress RequestState = "IN-PROGRESS"
	// StatePending means the request is queued behind another request.
	StatePending RequestState = "PENDING"
)

// Status codes used by this implementation (RFC 6787 section 5.2).
const (
	StatusSuccess              = 200
	StatusBadRequest           = 400
	StatusMethodNotValidState  = 402
	StatusUnsupportedHeader    = 403
	StatusResourceNotAllocated = 406
	StatusResourceInUse        = 407
	StatusNotImplemented       = 501
	StatusInternalError        = 500
)

// Well known header names.
const (
	HeaderChannelIdentifier = "Channel-Identifier"
	HeaderContentType       = "Content-Type"
	HeaderContentLength     = "Content-Length"
	HeaderCompletionCause   = "Completion-Cause"
)

// Header is a single MRCP header field. MRCP header names are case
// insensitive.
type Header struct {
	Name  string
	Value string
}

// String renders the header in wire format without the trailing CRLF.
func (h Header) String() string { return h.Name + ": " + h.Value }

// Headers is an ordered list of header fields. Ordering is preserved because
// some clients compare header order in traces, and because it keeps golden
// file tests deterministic.
type Headers []Header

// Get returns the value of the first header whose name matches name.
func (h Headers) Get(name string) (string, bool) {
	for _, hdr := range h {
		if strings.EqualFold(hdr.Name, name) {
			return hdr.Value, true
		}
	}
	return "", false
}

// Values returns the values of every header whose name matches name.
func (h Headers) Values(name string) []string {
	var out []string
	for _, hdr := range h {
		if strings.EqualFold(hdr.Name, name) {
			out = append(out, hdr.Value)
		}
	}
	return out
}

// Set replaces the first header matching name, or appends it when absent.
func (h *Headers) Set(name, value string) {
	for i := range *h {
		if strings.EqualFold((*h)[i].Name, name) {
			(*h)[i].Value = value
			return
		}
	}
	*h = append(*h, Header{Name: name, Value: value})
}

// Add appends a header without looking for an existing one.
func (h *Headers) Add(name, value string) {
	*h = append(*h, Header{Name: name, Value: value})
}

// Del removes every header matching name.
func (h *Headers) Del(name string) {
	out := (*h)[:0]
	for _, hdr := range *h {
		if !strings.EqualFold(hdr.Name, name) {
			out = append(out, hdr)
		}
	}
	*h = out
}

// Message is a decoded MRCP message. Which fields are meaningful depends on
// Type:
//
//	TypeRequest  : Method, RequestID
//	TypeResponse : RequestID, StatusCode
//	TypeEvent    : EventName, RequestID, RequestState
type Message struct {
	Type Type

	// Length is the message-length value taken from the start line.
	Length int

	// RequestID identifies the request this message belongs to.
	RequestID uint32

	// Method is set for requests.
	Method Method

	// EventName is set for events.
	EventName string

	// StatusCode is set for responses.
	StatusCode int

	// RequestState is set for responses and events.
	RequestState RequestState

	// Headers holds the message header fields in wire order.
	Headers Headers

	// Body holds the message body, or nil when there is none.
	Body []byte
}

// ChannelIdentifier returns the Channel-Identifier header value.
func (m *Message) ChannelIdentifier() string {
	v, _ := m.Headers.Get(HeaderChannelIdentifier)
	return v
}

// ContentType returns the Content-Type header value without parameters.
func (m *Message) ContentType() string {
	v, _ := m.Headers.Get(HeaderContentType)
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// NewResponse builds a response to req carrying the given status and state.
// The Channel-Identifier of the request is echoed so clients can correlate the
// response even when the identifier is unknown to the server.
func NewResponse(req *Message, status int, state RequestState) *Message {
	m := &Message{
		Type:         TypeResponse,
		RequestID:    req.RequestID,
		StatusCode:   status,
		RequestState: state,
		Headers:      make(Headers, 0, 2),
	}
	if id := req.ChannelIdentifier(); id != "" {
		m.Headers.Add(HeaderChannelIdentifier, id)
	}
	return m
}

// NewEvent builds an asynchronous event for the request identified by
// requestID.
func NewEvent(channelID, name string, requestID uint32, state RequestState) *Message {
	m := &Message{
		Type:         TypeEvent,
		RequestID:    requestID,
		EventName:    name,
		RequestState: state,
		Headers:      make(Headers, 0, 3),
	}
	if channelID != "" {
		m.Headers.Add(HeaderChannelIdentifier, channelID)
	}
	return m
}

// Package sdp implements the small subset of RFC 4566 needed to negotiate an
// audio stream and one or more MRCPv2 control channels.
//
// Unknown attributes and session lines are ignored rather than rejected, so a
// peer that adds extensions does not break negotiation.
package sdp

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrEmpty is returned when a session description contains no lines.
var ErrEmpty = errors.New("sdp: empty session description")

// Attribute is one a= line. Value is empty for attributes without a colon.
type Attribute struct {
	Key   string
	Value string
}

// Media is one m= section together with the attributes and media level
// connection that belong to it.
type Media struct {
	// Type is the media type, for example "audio" or "application".
	Type string
	// Port is the transport port. A zero port rejects the stream.
	Port int
	// Protocol is the transport protocol, for example "RTP/AVP" or
	// "TCP/MRCPv2".
	Protocol string
	// Formats holds the payload type or format list from the m= line.
	Formats []string
	// Connection is the media level c= address, empty when the session level
	// address applies.
	Connection string
	// Attributes holds the a= lines in order.
	Attributes []Attribute
}

// Session is a parsed session description.
type Session struct {
	Origin     string
	Name       string
	Connection string
	Attributes []Attribute
	Media      []Media
}

// Parse decodes the minimal SDP subset used by this server.
func Parse(body []byte) (*Session, error) {
	lines := splitLines(string(body))
	if len(lines) == 0 {
		return nil, ErrEmpty
	}
	if !strings.HasPrefix(lines[0], "v=") {
		return nil, fmt.Errorf("sdp: missing version line, got %q", lines[0])
	}

	s := &Session{}
	var current *Media
	for _, line := range lines {
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "m="):
			m, err := parseMedia(line)
			if err != nil {
				return nil, err
			}
			s.Media = append(s.Media, *m)
			current = &s.Media[len(s.Media)-1]
		case strings.HasPrefix(line, "a="):
			attr := parseAttribute(strings.TrimPrefix(line, "a="))
			if current != nil {
				current.Attributes = append(current.Attributes, attr)
			} else {
				s.Attributes = append(s.Attributes, attr)
			}
		case strings.HasPrefix(line, "c="):
			addr, err := parseConnection(line)
			if err != nil {
				return nil, err
			}
			if current != nil {
				current.Connection = addr
			} else {
				s.Connection = addr
			}
		case strings.HasPrefix(line, "o="):
			s.Origin = strings.TrimPrefix(line, "o=")
		case strings.HasPrefix(line, "s="):
			s.Name = strings.TrimPrefix(line, "s=")
		default:
			// v=, t=, b=, i=, u=, e=, p=, z=, k= and extensions: not needed.
		}
	}
	return s, nil
}

func splitLines(body string) []string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	lines := strings.Split(body, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func parseMedia(line string) (*Media, error) {
	fields := strings.Fields(strings.TrimPrefix(line, "m="))
	if len(fields) < 3 {
		return nil, fmt.Errorf("sdp: malformed media line %q", line)
	}
	portField := fields[1]
	if i := strings.IndexByte(portField, '/'); i >= 0 {
		portField = portField[:i]
	}
	port, err := strconv.Atoi(portField)
	if err != nil || port < 0 || port > 65535 {
		return nil, fmt.Errorf("sdp: invalid port in media line %q", line)
	}
	m := &Media{
		Type:     strings.ToLower(fields[0]),
		Port:     port,
		Protocol: fields[2],
	}
	if len(fields) > 3 {
		m.Formats = append(m.Formats, fields[3:]...)
	}
	return m, nil
}

func parseConnection(line string) (string, error) {
	fields := strings.Fields(strings.TrimPrefix(line, "c="))
	if len(fields) < 3 {
		return "", fmt.Errorf("sdp: malformed connection line %q", line)
	}
	addr := fields[2]
	if i := strings.IndexByte(addr, '/'); i >= 0 {
		addr = addr[:i]
	}
	if addr == "" {
		return "", fmt.Errorf("sdp: empty connection address in %q", line)
	}
	return addr, nil
}

func parseAttribute(value string) Attribute {
	key, val, ok := strings.Cut(value, ":")
	if !ok {
		return Attribute{Key: key}
	}
	return Attribute{Key: key, Value: val}
}

// Attribute returns the value of the first attribute named key.
func (m *Media) Attribute(key string) (string, bool) {
	for _, a := range m.Attributes {
		if a.Key == key {
			return a.Value, true
		}
	}
	return "", false
}

// AttributeValues returns every value of attributes named key.
func (m *Media) AttributeValues(key string) []string {
	var out []string
	for _, a := range m.Attributes {
		if a.Key == key {
			out = append(out, a.Value)
		}
	}
	return out
}

// MID returns the RFC 5888 media identification, or an empty string.
func (m *Media) MID() string {
	v, _ := m.Attribute("mid")
	return v
}

// Cmid returns the RFC 5888 control media identification, or an empty string.
func (m *Media) Cmid() string {
	v, _ := m.Attribute("cmid")
	return v
}

// Setup returns the RFC 4145 setup attribute, or an empty string.
func (m *Media) Setup() string {
	v, _ := m.Attribute("setup")
	return v
}

// ChannelIdentifier returns the parsed a=channel value of an MRCP media
// section.
func (m *Media) ChannelIdentifier() (id, resource string, ok bool) {
	v, found := m.Attribute("channel")
	if !found {
		return "", "", false
	}
	return ParseChannelIdentifier(v)
}

// IsMRCP reports whether m describes an MRCPv2 control channel.
func (m *Media) IsMRCP() bool {
	return m.Type == "application" && strings.EqualFold(m.Protocol, "TCP/MRCPv2")
}

// IsAudio reports whether m describes an audio stream.
func (m *Media) IsAudio() bool { return m.Type == "audio" }

// AcceptsPayloadType reports whether the m= line offers pt.
func (m *Media) AcceptsPayloadType(pt int) bool {
	want := strconv.Itoa(pt)
	for _, f := range m.Formats {
		if f == want {
			return true
		}
	}
	return false
}

// RemoteAddress returns the address that applies to m, falling back to the
// session level connection when the media section has none.
func (s *Session) RemoteAddress(m *Media) string {
	if m != nil && m.Connection != "" {
		return m.Connection
	}
	return s.Connection
}

// FirstAudio returns the first audio media section, or nil.
func (s *Session) FirstAudio() *Media {
	for i := range s.Media {
		if s.Media[i].IsAudio() {
			return &s.Media[i]
		}
	}
	return nil
}

// MRCPMedia returns every MRCPv2 control channel section in offer order.
func (s *Session) MRCPMedia() []*Media {
	var out []*Media
	for i := range s.Media {
		if s.Media[i].IsMRCP() {
			out = append(out, &s.Media[i])
		}
	}
	return out
}

// ParseChannelIdentifier splits "<session-id>@<resource>". The session-id may be
// empty, in which case the server allocates one and returns it in the answer.
func ParseChannelIdentifier(v string) (id, resource string, ok bool) {
	i := strings.LastIndexByte(v, '@')
	if i < 0 || i == len(v)-1 {
		return "", "", false
	}
	return v[:i], v[i+1:], true
}

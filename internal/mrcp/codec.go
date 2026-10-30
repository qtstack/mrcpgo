package mrcp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// DefaultMaxMessageSize bounds the size of a single MRCP message. The
// message-length field is attacker controlled, so it must never be trusted to
// allocate memory directly.
const DefaultMaxMessageSize = 1 << 20 // 1 MiB

// maxStartLineLength bounds the start line, which is read before
// message-length is known.
const maxStartLineLength = 1024

// Errors returned by the codec.
var (
	// ErrMessageTooLarge means the peer announced a message larger than the
	// configured limit.
	ErrMessageTooLarge = errors.New("mrcp: message exceeds size limit")
	// ErrMalformedStartLine means the start line does not follow RFC 6787.
	ErrMalformedStartLine = errors.New("mrcp: malformed start line")
	// ErrMalformedHeaders means the header block could not be parsed.
	ErrMalformedHeaders = errors.New("mrcp: malformed headers")
	// ErrContentLengthMismatch means the body does not match Content-Length.
	ErrContentLengthMismatch = errors.New("mrcp: content-length mismatch")
	// ErrChannelIdentifierMissing means the mandatory header is absent.
	ErrChannelIdentifierMissing = errors.New("mrcp: missing Channel-Identifier header")
)

// ReadMessage reads one MRCP message from r.
func ReadMessage(r *bufio.Reader) (*Message, error) {
	return ReadMessageLimit(r, DefaultMaxMessageSize)
}

// ReadMessageLimit reads one MRCP message from r, rejecting messages whose
// announced length exceeds maxSize.
//
// MRCPv2 framing is length prefixed: the first token after the version on the
// start line is message-length, the total number of bytes from "MRCP/2.0" to
// the last byte of the message. A TCP stream carries no message boundaries, so
// the decoder reads the start line, extracts message-length, and then reads
// exactly the remaining bytes. See RFC 6787 section 4.1.
func ReadMessageLimit(r *bufio.Reader, maxSize int) (*Message, error) {
	startLine, err := readStartLine(r)
	if err != nil {
		return nil, err
	}

	msgLen, err := parseMessageLength(startLine)
	if err != nil {
		return nil, err
	}
	if msgLen > maxSize {
		return nil, fmt.Errorf("%w: announced %d bytes, limit %d", ErrMessageTooLarge, msgLen, maxSize)
	}

	// The start line includes its trailing CRLF, and message-length counts it.
	consumed := len(startLine) + len("\r\n")
	if msgLen < consumed {
		return nil, fmt.Errorf("%w: message-length %d shorter than start line (%d bytes)",
			ErrMalformedStartLine, msgLen, consumed)
	}

	rest := make([]byte, msgLen-consumed)
	if _, err := io.ReadFull(r, rest); err != nil {
		return nil, fmt.Errorf("mrcp: read message body: %w", err)
	}

	msg, err := parseMessage(startLine, rest)
	if err != nil {
		return nil, err
	}
	msg.Length = msgLen
	return msg, nil
}

// readStartLine reads a single CRLF terminated line without reading past it.
func readStartLine(r *bufio.Reader) (string, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > maxStartLineLength {
			return "", fmt.Errorf("%w: start line longer than %d bytes", ErrMalformedStartLine, maxStartLineLength)
		}
		if err == nil {
			break
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return "", err
		}
		if len(buf) > maxStartLineLength {
			return "", fmt.Errorf("%w: start line longer than %d bytes", ErrMalformedStartLine, maxStartLineLength)
		}
	}
	if !bytes.HasSuffix(buf, []byte("\r\n")) {
		return "", fmt.Errorf("%w: start line must end with CRLF", ErrMalformedStartLine)
	}
	// Strip the trailing CRLF: the caller accounts for it separately.
	return string(buf[:len(buf)-2]), nil
}

// parseMessageLength extracts message-length from the start line.
func parseMessageLength(startLine string) (int, error) {
	fields := strings.Fields(startLine)
	if len(fields) < 2 {
		return 0, fmt.Errorf("%w: %q", ErrMalformedStartLine, startLine)
	}
	version, ok := strings.CutPrefix(fields[0], "MRCP/")
	if !ok {
		return 0, fmt.Errorf("%w: unsupported protocol %q", ErrMalformedStartLine, fields[0])
	}
	if version != Version {
		return 0, fmt.Errorf("%w: unsupported version %q", ErrMalformedStartLine, version)
	}
	n, err := strconv.Atoi(fields[1])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%w: invalid message-length %q", ErrMalformedStartLine, fields[1])
	}
	return n, nil
}

// parseMessage decodes the start line plus the bytes that follow it.
func parseMessage(startLine string, rest []byte) (*Message, error) {
	fields := strings.Fields(startLine)
	// fields[0] is the version, fields[1] is message-length.
	tokens := fields[2:]

	msg := &Message{}
	switch len(tokens) {
	case 2:
		// request: <method-name> <request-id>
		method, err := parseMethod(tokens[0])
		if err != nil {
			return nil, err
		}
		id, err := parseRequestID(tokens[1])
		if err != nil {
			return nil, err
		}
		msg.Type = TypeRequest
		msg.Method = method
		msg.RequestID = id
	case 3:
		if id, err := parseRequestID(tokens[0]); err == nil {
			// response: <request-id> <status-code>
			code, err := strconv.Atoi(tokens[1])
			if err != nil || code < 100 || code > 699 {
				return nil, fmt.Errorf("%w: invalid status code %q", ErrMalformedStartLine, tokens[1])
			}
			state, err := parseRequestState(tokens[2])
			if err != nil {
				return nil, err
			}
			msg.Type = TypeResponse
			msg.RequestID = id
			msg.StatusCode = code
			msg.RequestState = state
			break
		}
		// event: <event-name> <request-id> <request-state>
		id, err := parseRequestID(tokens[1])
		if err != nil {
			return nil, err
		}
		state, err := parseRequestState(tokens[2])
		if err != nil {
			return nil, err
		}
		msg.Type = TypeEvent
		msg.EventName = tokens[0]
		msg.RequestID = id
		msg.RequestState = state
	default:
		return nil, fmt.Errorf("%w: expected 2 or 3 fields after message-length, got %d",
			ErrMalformedStartLine, len(tokens))
	}

	if err := parseBody(msg, rest); err != nil {
		return nil, err
	}
	return msg, nil
}

// parseBody splits the header block from the body and validates
// Content-Length. rest starts immediately after the start line CRLF.
func parseBody(msg *Message, rest []byte) error {
	var headerBlock, body []byte

	switch {
	case bytes.HasPrefix(rest, []byte("\r\n")):
		// Start line CRLF is immediately followed by the blank line that
		// separates headers from body: the message has no headers.
		body = rest[2:]
	default:
		idx := bytes.Index(rest, []byte("\r\n\r\n"))
		if idx < 0 {
			headerBlock = rest
		} else {
			headerBlock = rest[:idx]
			body = rest[idx+4:]
		}
	}

	headers, err := parseHeaders(headerBlock)
	if err != nil {
		return err
	}
	msg.Headers = headers

	if len(body) == 0 {
		msg.Body = nil
	} else {
		msg.Body = body
	}

	declared, ok := msg.Headers.Get(HeaderContentLength)
	if ok {
		n, err := strconv.Atoi(strings.TrimSpace(declared))
		if err != nil || n < 0 {
			return fmt.Errorf("%w: invalid Content-Length %q", ErrMalformedHeaders, declared)
		}
		if n != len(body) {
			return fmt.Errorf("%w: header says %d bytes, body has %d",
				ErrContentLengthMismatch, n, len(body))
		}
		return nil
	}
	if len(body) > 0 {
		return fmt.Errorf("%w: body present without Content-Length", ErrMalformedHeaders)
	}
	return nil
}

// parseHeaders decodes a CRLF separated header block.
func parseHeaders(block []byte) (Headers, error) {
	if len(block) == 0 {
		return nil, nil
	}
	lines := bytes.Split(block, []byte("\r\n"))
	headers := make(Headers, 0, len(lines))
	for _, line := range lines {
		if len(line) == 0 {
			// Tolerate a stray empty line rather than rejecting the message.
			continue
		}
		name, value, ok := strings.Cut(string(line), ":")
		if !ok {
			return nil, fmt.Errorf("%w: header line without colon: %q", ErrMalformedHeaders, line)
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("%w: empty header name", ErrMalformedHeaders)
		}
		headers.Add(name, strings.TrimSpace(value))
	}
	return headers, nil
}

func parseRequestID(s string) (uint32, error) {
	id, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%w: invalid request-id %q", ErrMalformedStartLine, s)
	}
	return uint32(id), nil
}

func parseRequestState(s string) (RequestState, error) {
	switch state := RequestState(s); state {
	case StateComplete, StateInProgress, StatePending:
		return state, nil
	default:
		return "", fmt.Errorf("%w: invalid request-state %q", ErrMalformedStartLine, s)
	}
}

func parseMethod(s string) (Method, error) {
	if s == "" || strings.ContainsAny(s, " \t\r\n") {
		return "", fmt.Errorf("%w: invalid method name %q", ErrMalformedStartLine, s)
	}
	return Method(s), nil
}

// WriteMessage encodes m and writes it to w.
func WriteMessage(w io.Writer, m *Message) error {
	if m == nil {
		return errors.New("mrcp: nil message")
	}
	b, err := m.MarshalBinary()
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// MarshalBinary encodes m into its wire representation.
//
// The encoding is start-line CRLF, zero or more header lines each terminated by
// CRLF, and then, when a body is present, a blank line followed by the body.
// message-length covers everything, including the start line itself.
func (m *Message) MarshalBinary() ([]byte, error) {
	tail, err := m.startLineTail()
	if err != nil {
		return nil, err
	}

	body := m.Body
	headers := m.Headers
	if len(body) > 0 {
		// Content-Length is mandatory when a body is present (RFC 6787 4.1).
		if v, ok := headers.Get(HeaderContentLength); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err != nil || n != len(body) {
				return nil, fmt.Errorf("%w: header %q does not match %d byte body",
					ErrContentLengthMismatch, v, len(body))
			}
		} else {
			headers = append(Headers{
				{Name: HeaderContentLength, Value: strconv.Itoa(len(body))},
			}, headers...)
		}
	}

	var suffix bytes.Buffer
	for _, h := range headers {
		if h.Name == "" {
			return nil, errors.New("mrcp: header with empty name")
		}
		suffix.WriteString(h.Name)
		suffix.WriteString(": ")
		suffix.WriteString(h.Value)
		suffix.WriteString("\r\n")
	}
	if len(body) > 0 {
		suffix.WriteString("\r\n")
		suffix.Write(body)
	}

	startLine := buildStartLine(tail, suffix.Len())
	var out bytes.Buffer
	out.Grow(len(startLine) + suffix.Len())
	out.WriteString(startLine)
	out.Write(suffix.Bytes())
	return out.Bytes(), nil
}

// startLineTail renders everything on the start line after message-length.
func (m *Message) startLineTail() (string, error) {
	switch m.Type {
	case TypeRequest:
		if m.Method == "" {
			return "", errors.New("mrcp: request without method")
		}
		return string(m.Method) + " " + strconv.FormatUint(uint64(m.RequestID), 10), nil
	case TypeResponse:
		if m.StatusCode < 100 || m.StatusCode > 699 {
			return "", fmt.Errorf("mrcp: invalid status code %d", m.StatusCode)
		}
		if m.RequestState == "" {
			return "", errors.New("mrcp: response without request-state")
		}
		return fmt.Sprintf("%d %d %s", m.RequestID, m.StatusCode, m.RequestState), nil
	case TypeEvent:
		if m.EventName == "" {
			return "", errors.New("mrcp: event without name")
		}
		if m.RequestState == "" {
			return "", errors.New("mrcp: event without request-state")
		}
		return fmt.Sprintf("%s %d %s", m.EventName, m.RequestID, m.RequestState), nil
	default:
		return "", fmt.Errorf("mrcp: cannot encode message type %s", m.Type)
	}
}

// buildStartLine renders the start line. message-length depends on its own
// width, so the value is computed iteratively until it is stable.
func buildStartLine(tail string, suffixLen int) string {
	const prefix = "MRCP/" + Version + " "
	// Total message length = start line (prefix, length field, separator,
	// tail, CRLF) + everything that follows the start line.
	total := len(prefix) + 1 + 1 + len(tail) + len("\r\n") + suffixLen
	for range 4 {
		startLine := prefix + strconv.Itoa(total) + " " + tail + "\r\n"
		if len(startLine)+suffixLen == total {
			return startLine
		}
		total = len(startLine) + suffixLen
	}
	return prefix + strconv.Itoa(total) + " " + tail + "\r\n"
}

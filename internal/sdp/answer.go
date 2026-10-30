package sdp

import (
	"errors"
	"fmt"
	"strings"
)

// PCMUPayloadType is the static RTP payload type for G.711 mu-law.
const PCMUPayloadType = 0

// MediaEntry is the answer to a single media section of the offer. Entries are
// matched to offer media sections by index.
type MediaEntry struct {
	// Port is the local port the server allocated. Zero rejects the stream,
	// which is how unsupported media sections are answered (RFC 3264 section
	// 6).
	Port int
	// ChannelID and Resource describe an MRCPv2 control channel. They are
	// required when answering an m=application TCP/MRCPv2 section.
	ChannelID string
	Resource  string
}

// AnswerOptions configures answer generation.
type AnswerOptions struct {
	// User populates the o= line, for example "mrcpgo".
	User string
	// SessionID is the numeric session identifier of the o= line.
	SessionID uint64
	// SessionVersion is the version of the o= line.
	SessionVersion uint64
	// Host is the address advertised in c= and o=.
	Host string
	// Entries answer the offer's media sections in order. Missing entries are
	// treated as rejected sections.
	Entries []MediaEntry
}

// BuildAnswer renders an answer that mirrors the offer's media sections. The
// order of m= lines is preserved, which RFC 3264 requires.
func BuildAnswer(offer *Session, opts AnswerOptions) ([]byte, error) {
	if offer == nil {
		return nil, errors.New("sdp: nil offer")
	}
	if opts.Host == "" {
		return nil, errors.New("sdp: answer requires a host")
	}
	user := opts.User
	if user == "" {
		user = "mrcpgo"
	}

	var b strings.Builder
	b.WriteString("v=0\r\n")
	fmt.Fprintf(&b, "o=%s %d %d IN IP4 %s\r\n", user, opts.SessionID, opts.SessionVersion, opts.Host)
	b.WriteString("s=mrcpgo\r\n")
	fmt.Fprintf(&b, "c=IN IP4 %s\r\n", opts.Host)
	b.WriteString("t=0 0\r\n")

	for i := range offer.Media {
		var entry MediaEntry
		if i < len(opts.Entries) {
			entry = opts.Entries[i]
		}
		media := &offer.Media[i]
		switch {
		case media.IsAudio() && entry.Port > 0:
			writeAudio(&b, media, entry.Port)
		case media.IsMRCP() && entry.Port > 0:
			if err := writeControl(&b, media, entry); err != nil {
				return nil, err
			}
		default:
			writeRejected(&b, media)
		}
	}
	return []byte(b.String()), nil
}

func writeAudio(b *strings.Builder, media *Media, port int) {
	fmt.Fprintf(b, "m=audio %d RTP/AVP %d\r\n", port, PCMUPayloadType)
	fmt.Fprintf(b, "a=rtpmap:%d PCMU/8000\r\n", PCMUPayloadType)
	b.WriteString("a=ptime:20\r\n")
	b.WriteString("a=sendrecv\r\n")
	if mid := media.MID(); mid != "" {
		fmt.Fprintf(b, "a=mid:%s\r\n", mid)
	}
}

func writeControl(b *strings.Builder, media *Media, entry MediaEntry) error {
	if entry.ChannelID == "" || entry.Resource == "" {
		return fmt.Errorf("sdp: control channel answer needs channel id and resource")
	}
	// MRCPv2 control channels are always offered as a single format.
	fmt.Fprintf(b, "m=application %d TCP/MRCPv2 1\r\n", entry.Port)
	b.WriteString("a=setup:passive\r\n")
	b.WriteString("a=connection:new\r\n")
	fmt.Fprintf(b, "a=channel:%s@%s\r\n", entry.ChannelID, entry.Resource)
	if cmid := media.Cmid(); cmid != "" {
		fmt.Fprintf(b, "a=cmid:%s\r\n", cmid)
	}
	if mid := media.MID(); mid != "" {
		fmt.Fprintf(b, "a=mid:%s\r\n", mid)
	}
	return nil
}

func writeRejected(b *strings.Builder, media *Media) {
	formats := media.Formats
	if len(formats) == 0 {
		formats = []string{"0"}
	}
	fmt.Fprintf(b, "m=%s 0 %s %s\r\n", media.Type, media.Protocol, strings.Join(formats, " "))
	// RFC 3264 section 6: a rejected section must be kept in the answer with a
	// zero port so the offer/answer line correspondence is preserved.
}

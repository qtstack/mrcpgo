// Package nlsml builds the Natural Language Semantic Markup Language documents
// returned in RECOGNITION-COMPLETE event bodies (RFC 6787 appendix B).
package nlsml

import (
	"bytes"
	"encoding/xml"
	"fmt"
)

// Result is the recognition output that should be reported to the client.
type Result struct {
	// Text is the recognised utterance.
	Text string
	// Confidence is the engine confidence in the range 0-100. A negative
	// value omits the attribute, which is what engines without a confidence
	// score should use rather than fabricating one.
	Confidence int
	// Instance is an opaque provider payload. Optional.
	Instance string
}

// Build renders a minimal but well formed NLSML document.
func Build(r Result) []byte {
	var b bytes.Buffer
	b.WriteString("<?xml version=\"1.0\"?>\n<result>\n  <interpretation")
	if r.Confidence >= 0 {
		confidence := r.Confidence
		if confidence > 100 {
			confidence = 100
		}
		fmt.Fprintf(&b, " confidence=\"%d\"", confidence)
	}
	b.WriteString(">\n")
	b.WriteString("    <input mode=\"speech\">")
	escape(&b, r.Text)
	b.WriteString("</input>\n")
	if r.Instance != "" {
		b.WriteString("    <instance>")
		escape(&b, r.Instance)
		b.WriteString("</instance>\n")
	}
	b.WriteString("  </interpretation>\n</result>")
	return b.Bytes()
}

func escape(b *bytes.Buffer, s string) {
	if err := xml.EscapeText(b, []byte(s)); err != nil {
		// bytes.Buffer never fails; fall back to a conservative replacement.
		b.WriteString(s)
	}
}

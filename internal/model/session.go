// Package model contains the protocol independent data types that are shared
// between the MRCP resource server, its sessions and its pluggable engines.
//
// The package intentionally depends on the standard library only. Keeping the
// engine contracts here avoids an import cycle between internal/session and
// internal/engine.
package model

import "fmt"

// SessionState is the lifecycle state of a media session. The zero value is
// SessionCreated so a freshly allocated Session needs no explicit
// initialisation.
type SessionState int

const (
	// SessionCreated means SIP resources are allocated but the dialog has not
	// been confirmed with an ACK yet.
	SessionCreated SessionState = iota
	// SessionActive means the dialog is confirmed and MRCP requests are served.
	SessionActive
	// SessionReleasing means teardown is in progress and new requests must be
	// rejected.
	SessionReleasing
	// SessionClosed means every resource has been released.
	SessionClosed
)

// String implements fmt.Stringer.
func (s SessionState) String() string {
	switch s {
	case SessionCreated:
		return "created"
	case SessionActive:
		return "active"
	case SessionReleasing:
		return "releasing"
	case SessionClosed:
		return "closed"
	default:
		return fmt.Sprintf("SessionState(%d)", int(s))
	}
}

// Resource identifies an MRCP resource type as it appears in the
// Channel-Identifier and in the SDP a=channel attribute.
type Resource string

// The two resource types implemented by this server.
const (
	ResourceSpeechRecog Resource = "speechrecog"
	ResourceSpeechSynth Resource = "speechsynth"
)

// Valid reports whether r is a resource type this server can serve.
func (r Resource) Valid() bool {
	switch r {
	case ResourceSpeechRecog, ResourceSpeechSynth:
		return true
	default:
		return false
	}
}

// String implements fmt.Stringer.
func (r Resource) String() string { return string(r) }

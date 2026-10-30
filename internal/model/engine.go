package model

import (
	"context"
	"io"
)

// RecognizerResult carries the outcome of a single recognition request.
type RecognizerResult struct {
	// NLSML is the raw NLSML document reported in the RECOGNITION-COMPLETE
	// event body. It is ignored when Err is non-nil.
	NLSML []byte
	// Err reports a recognition failure. A non-nil Err is surfaced to the
	// client as a RECOGNITION-COMPLETE event with a Completion-Cause header.
	Err error
}

// Recognizer is the speech recognition engine contract.
type Recognizer interface {
	// Start begins recognition for grammarURI and returns a channel that
	// delivers at most one result and is then closed. The channel must be
	// closed when ctx is canceled, which allows the MRCP layer to stop a
	// request without leaking a goroutine.
	Start(ctx context.Context, grammarURI string) (<-chan RecognizerResult, error)
	// Stop aborts any recognition currently in progress.
	Stop() error
}

// Synthesizer is the speech synthesis engine contract.
type Synthesizer interface {
	// Synthesize returns a stream of 160 byte G.711 mu-law frames sampled at
	// 8 kHz. Returning io.EOF from Read signals that synthesis is complete.
	Synthesize(ctx context.Context, text string) (io.ReadCloser, error)
	// Stop aborts any synthesis currently in progress.
	Stop() error
}

// AudioConsumer is an optional interface a Recognizer may implement to receive
// the inbound RTP payload stream. Engines that do not implement it (such as the
// bundled mock) simply ignore received audio.
type AudioConsumer interface {
	WriteAudio(payload []byte) error
}

// RTPStats is a snapshot of RTP reception statistics.
type RTPStats struct {
	Received   uint64
	Lost       uint64
	OutOfOrder uint64
}

package mock

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestRecognizerDeliversResult(t *testing.T) {
	r := NewRecognizer()
	r.Delay = 0

	results, err := r.Start(context.Background(), "builtin:grammar/hello")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case res, ok := <-results:
		if !ok {
			t.Fatal("channel closed without a result")
		}
		if res.Err != nil {
			t.Fatalf("result error: %v", res.Err)
		}
		if len(res.NLSML) == 0 {
			t.Fatal("empty NLSML")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a result")
	}

	if _, ok := <-results; ok {
		t.Fatal("result channel should be closed after one result")
	}
}

func TestRecognizerClosesChannelOnCancel(t *testing.T) {
	r := NewRecognizer()
	r.Delay = 5 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	results, err := r.Start(ctx, "builtin:grammar/hello")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()

	select {
	case _, ok := <-results:
		if ok {
			t.Fatal("unexpected result after cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("result channel was not closed after cancellation")
	}
}

func TestRecognizerStop(t *testing.T) {
	r := NewRecognizer()
	r.Delay = 5 * time.Second

	results, err := r.Start(context.Background(), "builtin:grammar/hello")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := r.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	select {
	case _, ok := <-results:
		if ok {
			t.Fatal("unexpected result after Stop")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("result channel was not closed after Stop")
	}
}

func TestSynthesizerProducesFrames(t *testing.T) {
	s := NewSynthesizer()
	s.Frames = 3

	stream, err := s.Synthesize(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	defer stream.Close()

	frame := make([]byte, 160)
	total := 0
	for {
		n, err := stream.Read(frame)
		total += n
		for _, b := range frame[:n] {
			if b != silenceByte {
				t.Fatalf("byte = %#x, want %#x", b, silenceByte)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if want := 3 * 160; total != want {
		t.Fatalf("total bytes = %d, want %d", total, want)
	}
}

func TestSynthesizerDefaultLength(t *testing.T) {
	s := NewSynthesizer()
	stream, err := s.Synthesize(context.Background(), "")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	defer stream.Close()

	data, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	if want := DefaultFrames * 160; len(data) != want {
		t.Fatalf("bytes = %d, want %d", len(data), want)
	}
}

func TestSynthesizerStop(t *testing.T) {
	s := NewSynthesizer()
	stream, err := s.Synthesize(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	defer stream.Close()

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := stream.Read(make([]byte, 160)); !errors.Is(err, context.Canceled) {
		t.Fatalf("read error = %v, want context.Canceled", err)
	}
}

func TestFactoryOverridesDefaults(t *testing.T) {
	f := &Factory{
		RecognitionDelay:   10 * time.Millisecond,
		FramesPerUtterance: 2,
	}

	recog, err := f.NewRecognizer()
	if err != nil {
		t.Fatalf("NewRecognizer: %v", err)
	}
	if got := recog.(*Recognizer).Delay; got != 10*time.Millisecond {
		t.Errorf("delay = %v", got)
	}

	synth, err := f.NewSynthesizer()
	if err != nil {
		t.Fatalf("NewSynthesizer: %v", err)
	}
	if got := synth.(*Synthesizer).Frames; got != 2 {
		t.Errorf("frames = %d", got)
	}
}

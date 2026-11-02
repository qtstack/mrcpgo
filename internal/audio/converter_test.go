package audio

import (
	"encoding/binary"
	"testing"
)

func TestMuLawConverter8k(t *testing.T) {
	converter := NewMuLawConverter(8000)
	out := converter.Convert(make([]byte, 320)) // 20 ms of silence
	if len(out) != 160 {
		t.Fatalf("length = %d, want 160", len(out))
	}
	for i, b := range out {
		if b != MuLawSilence {
			t.Fatalf("byte %d = %#02x", i, b)
		}
	}
}

func TestMuLawConverter16kDownsamples(t *testing.T) {
	converter := NewMuLawConverter(16000)
	pcm := make([]byte, 0, 8)
	for _, sample := range []int16{0, 0, 1000, 1000} {
		var buf [2]byte
		binary.LittleEndian.PutUint16(buf[:], uint16(sample))
		pcm = append(pcm, buf[:]...)
	}
	out := converter.Convert(pcm)
	if len(out) != 2 {
		t.Fatalf("length = %d, want 2", len(out))
	}
	if out[0] != MuLawSilence {
		t.Fatalf("silent sample = %#02x", out[0])
	}
	if want := EncodeMuLaw(1000); out[1] != want {
		t.Fatalf("second sample = %#02x, want %#02x", out[1], want)
	}
}

func TestMuLawConverterCarriesSplitSample(t *testing.T) {
	converter := NewMuLawConverter(8000)
	var buf [2]byte
	sample := int16(-1000)
	binary.LittleEndian.PutUint16(buf[:], uint16(sample))

	if out := converter.Convert(buf[:1]); len(out) != 0 {
		t.Fatalf("half sample produced %d bytes", len(out))
	}
	out := converter.Convert(buf[1:])
	if len(out) != 1 {
		t.Fatalf("completed sample produced %d bytes", len(out))
	}
	if want := EncodeMuLaw(-1000); out[0] != want {
		t.Fatalf("sample = %#02x, want %#02x", out[0], want)
	}
}

func TestMuLawConverterEmpty(t *testing.T) {
	converter := NewMuLawConverter(8000)
	if out := converter.Convert(nil); len(out) != 0 {
		t.Fatalf("length = %d", len(out))
	}
}

package audio

import (
	"encoding/binary"
	"testing"
)

func TestMuLawKnownValues(t *testing.T) {
	tests := []struct {
		sample int16
		ulaw   byte
	}{
		{0, 0xFF},
		{-1, 0x7F},
		{32767, 0x80},
		{-32768, 0x00},
	}
	for _, tt := range tests {
		if got := EncodeMuLaw(tt.sample); got != tt.ulaw {
			t.Errorf("EncodeMuLaw(%d) = %#02x, want %#02x", tt.sample, got, tt.ulaw)
		}
	}
}

func TestMuLawRoundTrip(t *testing.T) {
	// Every mu-law code must decode and re-encode to itself. The single
	// exception is 0x7F, the negative zero code, which decodes to the same
	// sample as 0xFF and therefore re-encodes as positive zero.
	for code := 0; code < 256; code++ {
		b := byte(code)
		got := EncodeMuLaw(DecodeMuLaw(b))
		if b == 0x7F {
			if got != 0xFF {
				t.Fatalf("negative zero re-encoded as %#02x, want 0xff", got)
			}
			continue
		}
		if got != b {
			t.Fatalf("code %#02x: re-encoded as %#02x", b, got)
		}
	}
}

func TestMuLawFrameRoundTrip(t *testing.T) {
	frame := make([]byte, FrameBytes)
	for i := range frame {
		frame[i] = byte(i)
	}
	pcm := DecodeMuLawFrame(frame)
	if len(pcm) != FrameBytes*2 {
		t.Fatalf("pcm length = %d", len(pcm))
	}
	back := EncodeMuLawFrame(pcm)
	if len(back) != len(frame) {
		t.Fatalf("re-encoded length = %d", len(back))
	}
	for i := range frame {
		want := frame[i]
		if want == 0x7F {
			want = 0xFF // negative zero collapses to positive zero
		}
		if back[i] != want {
			t.Fatalf("byte %d: got %#02x want %#02x", i, back[i], want)
		}
	}
}

func TestEncodeMuLawFrameIgnoresOddByte(t *testing.T) {
	pcm := []byte{0x00, 0x00, 0x00} // one sample plus a stray byte
	if got := EncodeMuLawFrame(pcm); len(got) != 1 {
		t.Fatalf("length = %d, want 1", len(got))
	}
}

func TestUpsample2(t *testing.T) {
	pcm := make([]byte, 0, 8)
	for _, s := range []int16{0, 100, 200, 300} {
		var buf [2]byte
		binary.LittleEndian.PutUint16(buf[:], uint16(s))
		pcm = append(pcm, buf[:]...)
	}
	out := Upsample2(pcm)
	if len(out) != len(pcm)*2 {
		t.Fatalf("length = %d, want %d", len(out), len(pcm)*2)
	}
	got := make([]int16, 0, 8)
	for i := 0; i+1 < len(out); i += 2 {
		got = append(got, int16(binary.LittleEndian.Uint16(out[i:])))
	}
	want := []int16{0, 50, 100, 150, 200, 250, 300, 300}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sample %d = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestDownsample2(t *testing.T) {
	pcm := make([]byte, 0, 8)
	for _, s := range []int16{0, 100, 200, 300, 400, 500} {
		var buf [2]byte
		binary.LittleEndian.PutUint16(buf[:], uint16(s))
		pcm = append(pcm, buf[:]...)
	}
	out := Downsample2(pcm)
	if len(out) != len(pcm)/2 {
		t.Fatalf("length = %d, want %d", len(out), len(pcm)/2)
	}
	got := make([]int16, 0, 3)
	for i := 0; i+1 < len(out); i += 2 {
		got = append(got, int16(binary.LittleEndian.Uint16(out[i:])))
	}
	want := []int16{50, 250, 450}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sample %d = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestChunkBytes(t *testing.T) {
	if got := ChunkBytes(8000, 100); got != 1600 {
		t.Errorf("8 kHz 100 ms = %d, want 1600", got)
	}
	if got := ChunkBytes(16000, 100); got != 3200 {
		t.Errorf("16 kHz 100 ms = %d, want 3200", got)
	}
	if got := ChunkBytes(0, 100); got != 0 {
		t.Errorf("invalid rate = %d, want 0", got)
	}
}

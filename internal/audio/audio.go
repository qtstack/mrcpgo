// Package audio provides the small amount of signal processing the server
// needs to bridge telephony audio (8 kHz G.711 mu-law) with speech engines that
// consume or produce linear PCM.
package audio

import "encoding/binary"

const (
	// MuLawSilence is the G.711 mu-law encoding of a zero sample.
	MuLawSilence byte = 0xFF
	// FrameBytes is the size of a 20 ms mu-law frame at 8 kHz.
	FrameBytes = 160
	// SampleRate is the telephony sample rate used by PCMU.
	SampleRate = 8000
	// BytesPerSample is the size of one 16 bit linear PCM sample.
	BytesPerSample = 2
)

// DecodeMuLaw converts one mu-law byte to a 16 bit linear sample.
func DecodeMuLaw(u byte) int16 {
	u = ^u
	sign := u & 0x80
	exponent := (u >> 4) & 0x07
	mantissa := u & 0x0F

	sample := int32(mantissa)<<3 + 0x84
	sample <<= exponent
	sample -= 0x84
	if sign != 0 {
		sample = -sample
	}
	switch {
	case sample > 32767:
		return 32767
	case sample < -32768:
		return -32768
	default:
		return int16(sample)
	}
}

// EncodeMuLaw converts one 16 bit linear sample to mu-law.
func EncodeMuLaw(sample int16) byte {
	const (
		bias = 0x84
		clip = 32635
	)
	sign := byte(0)
	value := int32(sample)
	if value < 0 {
		value = -value
		sign = 0x80
	}
	if value > clip {
		value = clip
	}
	value += bias

	exponent := int32(7)
	for mask := int32(0x4000); exponent > 0 && value&mask == 0; mask >>= 1 {
		exponent--
	}
	mantissa := (value >> (exponent + 3)) & 0x0F
	return ^(sign | byte(exponent<<4) | byte(mantissa))
}

// DecodeMuLawFrame converts a mu-law payload to little endian 16 bit PCM.
func DecodeMuLawFrame(frame []byte) []byte {
	out := make([]byte, len(frame)*BytesPerSample)
	for i, b := range frame {
		binary.LittleEndian.PutUint16(out[i*BytesPerSample:], uint16(DecodeMuLaw(b)))
	}
	return out
}

// EncodeMuLawFrame converts little endian 16 bit PCM to mu-law. A trailing odd
// byte is ignored.
func EncodeMuLawFrame(pcm []byte) []byte {
	out := make([]byte, len(pcm)/BytesPerSample)
	for i := range out {
		out[i] = EncodeMuLaw(int16(binary.LittleEndian.Uint16(pcm[i*BytesPerSample:])))
	}
	return out
}

// Upsample2 doubles the sample rate with linear interpolation. It is used to
// feed engines that only accept 16 kHz input; it cannot add information that a
// narrowband source never contained.
func Upsample2(pcm []byte) []byte {
	samples := len(pcm) / BytesPerSample
	if samples == 0 {
		return nil
	}
	out := make([]byte, 0, len(pcm)*2)
	var buf [2]byte
	for i := 0; i < samples; i++ {
		cur := int32(int16(binary.LittleEndian.Uint16(pcm[i*BytesPerSample:])))
		next := cur
		if i+1 < samples {
			next = int32(int16(binary.LittleEndian.Uint16(pcm[(i+1)*BytesPerSample:])))
		}
		binary.LittleEndian.PutUint16(buf[:], uint16(int16(cur)))
		out = append(out, buf[:]...)
		binary.LittleEndian.PutUint16(buf[:], uint16(int16((cur+next)/2)))
		out = append(out, buf[:]...)
	}
	return out
}

// Downsample2 halves the sample rate by averaging adjacent sample pairs, a mild
// low pass that avoids the aliasing of naive decimation.
func Downsample2(pcm []byte) []byte {
	samples := len(pcm) / BytesPerSample
	if samples == 0 {
		return nil
	}
	out := make([]byte, 0, len(pcm))
	var buf [2]byte
	for i := 0; i < samples; i += 2 {
		a := int32(int16(binary.LittleEndian.Uint16(pcm[i*BytesPerSample:])))
		b := a
		if i+1 < samples {
			b = int32(int16(binary.LittleEndian.Uint16(pcm[(i+1)*BytesPerSample:])))
		}
		binary.LittleEndian.PutUint16(buf[:], uint16(int16((a+b)/2)))
		out = append(out, buf[:]...)
	}
	return out
}

// ChunkBytes returns the number of PCM bytes covering ms milliseconds.
func ChunkBytes(sampleRate, ms int) int {
	if sampleRate <= 0 || ms <= 0 {
		return 0
	}
	return sampleRate * BytesPerSample * ms / 1000
}

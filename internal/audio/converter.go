package audio

// MuLawConverter converts a stream of little endian 16 bit PCM chunks to 8 kHz
// G.711 mu-law bytes. It tolerates chunk boundaries that split a sample and
// downsamples 16 kHz input by averaging sample pairs.
type MuLawConverter struct {
	sampleRate int
	carry      byte
	hasCarry   bool
}

// NewMuLawConverter creates a converter for PCM at sampleRate (8000 or 16000).
func NewMuLawConverter(sampleRate int) *MuLawConverter {
	return &MuLawConverter{sampleRate: sampleRate}
}

// Convert returns the mu-law bytes for one PCM chunk. The result may be empty
// when the chunk does not complete a sample.
func (c *MuLawConverter) Convert(pcm []byte) []byte {
	if c.hasCarry {
		pcm = append([]byte{c.carry}, pcm...)
		c.hasCarry = false
	}
	if len(pcm)%2 == 1 {
		c.carry = pcm[len(pcm)-1]
		c.hasCarry = true
		pcm = pcm[:len(pcm)-1]
	}
	if c.sampleRate == 16000 {
		pcm = Downsample2(pcm)
	}
	return EncodeMuLawFrame(pcm)
}

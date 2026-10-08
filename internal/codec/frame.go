package codec

import "time"

// Telephony framing constants. A 20 ms packetization interval at 8 kHz is the
// near-universal default for SIP/RTP G.711, and gives 160 samples -> 160 bytes.
const (
	SampleRate8k  = 8000
	SampleRate16k = 16000

	FrameDuration   = 20 * time.Millisecond
	SamplesPerFrame = SampleRate8k / 50 // 160
	G711FrameBytes  = SamplesPerFrame   // 1 byte per sample
)

// PayloadType is the static RTP payload type for a G.711 variant (RFC 3551).
type PayloadType uint8

const (
	PayloadTypePCMU PayloadType = 0 // G.711 u-law
	PayloadTypePCMA PayloadType = 8 // G.711 A-law
)

// Codec identifies which companding law a call is using.
type Codec string

const (
	PCMU Codec = "PCMU"
	PCMA Codec = "PCMA"
)

// PayloadType maps the codec to its static RTP payload type.
func (c Codec) PayloadType() PayloadType {
	if c == PCMA {
		return PayloadTypePCMA
	}
	return PayloadTypePCMU
}

// Encode compands a block of linear PCM into the codec's wire bytes.
func (c Codec) Encode(pcm []int16) []byte {
	if c == PCMA {
		return EncodeALaw(pcm)
	}
	return EncodeULaw(pcm)
}

// Decode expands the codec's wire bytes back to linear PCM.
func (c Codec) Decode(b []byte) []int16 {
	if c == PCMA {
		return DecodeALaw(b)
	}
	return DecodeULaw(b)
}

// CodecFromPayloadType resolves a static RTP payload type to a codec. The second
// return is false for payload types this demo does not handle.
func CodecFromPayloadType(pt PayloadType) (Codec, bool) {
	switch pt {
	case PayloadTypePCMU:
		return PCMU, true
	case PayloadTypePCMA:
		return PCMA, true
	default:
		return "", false
	}
}

// FramePCM splits linear PCM into fixed 160-sample frames. A short final frame is
// zero-padded to full length so every RTP packet on the wire carries a constant
// payload size, as a real gateway would emit.
func FramePCM(pcm []int16) [][]int16 {
	if len(pcm) == 0 {
		return nil
	}
	n := (len(pcm) + SamplesPerFrame - 1) / SamplesPerFrame
	frames := make([][]int16, 0, n)
	for off := 0; off < len(pcm); off += SamplesPerFrame {
		end := off + SamplesPerFrame
		if end <= len(pcm) {
			// Copy rather than subslice: callers encode frames concurrently and
			// aliasing the source buffer invites surprises.
			f := make([]int16, SamplesPerFrame)
			copy(f, pcm[off:end])
			frames = append(frames, f)
			continue
		}
		f := make([]int16, SamplesPerFrame) // zero-padded tail
		copy(f, pcm[off:])
		frames = append(frames, f)
	}
	return frames
}

// FrameCount reports how many 160-sample frames a PCM buffer will produce.
func FrameCount(samples int) int {
	if samples <= 0 {
		return 0
	}
	return (samples + SamplesPerFrame - 1) / SamplesPerFrame
}

// Duration8k reports the wall-clock duration of a sample count at 8 kHz.
func Duration8k(samples int) time.Duration {
	return time.Duration(samples) * time.Second / SampleRate8k
}

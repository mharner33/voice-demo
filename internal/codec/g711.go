// Package codec implements the G.711 companding codecs, PCM framing, WAV file I/O,
// and 2x resampling needed to move telephony audio between the RTP wire format
// (8 kHz G.711) and the linear PCM that speech providers expect.
package codec

// G.711 constants, named as in ITU-T G.711 and the Sun reference implementation.
const (
	signBit   = 0x80 // sign bit for a u-law byte
	quantMask = 0x0F // quantization field
	segShift  = 4    // left shift for the segment number
	segMask   = 0x70 // segment field
	bias      = 0x84 // bias for linear code
	uLawClip  = 8159 // max 14-bit magnitude u-law can represent
	aLawClip  = 4095 // max 13-bit magnitude A-law can represent
)

// Segment end points. A linear magnitude is classified by the first segment it
// fits inside; the index of that segment becomes the exponent field.
var (
	segUEnd = [8]int32{0x3F, 0x7F, 0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF, 0x1FFF}
	segAEnd = [8]int32{0x1F, 0x3F, 0x7F, 0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF}
)

// uLawDecodeTable and aLawDecodeTable are built once at init. Decoding is on the
// hot path for every inbound RTP packet, and 256 entries is 512 bytes.
var (
	uLawDecodeTable [256]int16
	aLawDecodeTable [256]int16
)

func init() {
	for i := 0; i < 256; i++ {
		uLawDecodeTable[i] = ulawToLinear(uint8(i))
		aLawDecodeTable[i] = alawToLinear(uint8(i))
	}
}

// search returns the index of the first segment end that val fits within, or
// len(ends) if val exceeds them all (meaning the value must be clipped).
func search(val int32, ends *[8]int32) int32 {
	for i := int32(0); i < 8; i++ {
		if val <= ends[i] {
			return i
		}
	}
	return 8
}

// LinearToULaw encodes one 16-bit linear PCM sample as a u-law byte (G.711 PCMU).
func LinearToULaw(sample int16) uint8 {
	// Work in int32: negating math.MinInt16 overflows int16.
	v := int32(sample) >> 2 // to 14-bit

	var mask int32
	if v < 0 {
		v = -v
		mask = 0x7F
	} else {
		mask = 0xFF
	}
	if v > uLawClip {
		v = uLawClip
	}
	v += bias >> 2

	seg := search(v, &segUEnd)
	if seg >= 8 {
		return uint8(0x7F ^ mask)
	}
	uval := (seg << segShift) | ((v >> (seg + 1)) & quantMask)
	return uint8(uval ^ mask)
}

// ULawToLinear decodes one u-law byte to a 16-bit linear PCM sample.
func ULawToLinear(u uint8) int16 { return uLawDecodeTable[u] }

func ulawToLinear(u uint8) int16 {
	u = ^u
	t := ((int32(u) & quantMask) << 3) + bias
	t <<= (int32(u) & segMask) >> segShift
	if int32(u)&signBit != 0 {
		return int16(bias - t)
	}
	return int16(t - bias)
}

// LinearToALaw encodes one 16-bit linear PCM sample as an A-law byte (G.711 PCMA).
func LinearToALaw(sample int16) uint8 {
	v := int32(sample) >> 3 // to 13-bit

	var mask int32
	if v >= 0 {
		mask = 0xD5 // sign bit set, plus the A-law even-bit inversion
	} else {
		mask = 0x55
		v = -v - 1
	}

	seg := search(v, &segAEnd)
	if seg >= 8 {
		return uint8(0x7F ^ mask)
	}
	aval := seg << segShift
	if seg < 2 {
		aval |= (v >> 1) & quantMask
	} else {
		aval |= (v >> seg) & quantMask
	}
	return uint8(aval ^ mask)
}

// ALawToLinear decodes one A-law byte to a 16-bit linear PCM sample.
func ALawToLinear(a uint8) int16 { return aLawDecodeTable[a] }

func alawToLinear(a uint8) int16 {
	a ^= 0x55
	t := (int32(a) & quantMask) << 4
	seg := (int32(a) & segMask) >> segShift
	switch seg {
	case 0:
		t += 8
	case 1:
		t += 0x108
	default:
		t += 0x108
		t <<= seg - 1
	}
	if int32(a)&signBit != 0 {
		return int16(t)
	}
	return int16(-t)
}

// EncodeULaw encodes a block of linear PCM samples to u-law bytes.
func EncodeULaw(pcm []int16) []byte {
	out := make([]byte, len(pcm))
	for i, s := range pcm {
		out[i] = LinearToULaw(s)
	}
	return out
}

// DecodeULaw decodes a block of u-law bytes to linear PCM samples.
func DecodeULaw(b []byte) []int16 {
	out := make([]int16, len(b))
	for i, u := range b {
		out[i] = uLawDecodeTable[u]
	}
	return out
}

// EncodeALaw encodes a block of linear PCM samples to A-law bytes.
func EncodeALaw(pcm []int16) []byte {
	out := make([]byte, len(pcm))
	for i, s := range pcm {
		out[i] = LinearToALaw(s)
	}
	return out
}

// DecodeALaw decodes a block of A-law bytes to linear PCM samples.
func DecodeALaw(b []byte) []int16 {
	out := make([]int16, len(b))
	for i, a := range b {
		out[i] = aLawDecodeTable[a]
	}
	return out
}

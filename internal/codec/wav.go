package codec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// WAV format tags we understand (from the WAVEFORMATEX registry).
const (
	wavFormatPCM        = 0x0001
	wavFormatALaw       = 0x0006
	wavFormatULaw       = 0x0007
	wavFormatExtensible = 0xFFFE
)

var (
	// ErrNotWAV means the file is not RIFF/WAVE at all.
	ErrNotWAV = errors.New("codec: not a RIFF/WAVE file")
	// ErrUnsupportedWAV means the file is valid WAV but in a form we don't decode.
	ErrUnsupportedWAV = errors.New("codec: unsupported WAV encoding")
)

// Audio is linear PCM with its sample rate. Always mono: telephony is mono, and
// multi-channel sources are downmixed on read.
type Audio struct {
	PCM        []int16
	SampleRate int
}

// Duration reports the wall-clock length of the audio.
func (a Audio) Duration() float64 {
	if a.SampleRate == 0 {
		return 0
	}
	return float64(len(a.PCM)) / float64(a.SampleRate)
}

// ReadWAVFile loads a WAV file as mono linear PCM.
func ReadWAVFile(path string) (Audio, error) {
	f, err := os.Open(path)
	if err != nil {
		return Audio{}, err
	}
	defer f.Close()
	return ReadWAV(f)
}

// ReadWAV decodes a RIFF/WAVE stream into mono linear PCM. It accepts 16-bit PCM,
// 8-bit u-law, and 8-bit A-law, mono or multi-channel, and skips unknown chunks.
func ReadWAV(r io.Reader) (Audio, error) {
	var riff [12]byte
	if _, err := io.ReadFull(r, riff[:]); err != nil {
		return Audio{}, fmt.Errorf("%w: short header", ErrNotWAV)
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return Audio{}, ErrNotWAV
	}

	var (
		format     uint16
		channels   int
		sampleRate int
		bits       int
		haveFmt    bool
	)

	for {
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return Audio{}, fmt.Errorf("%w: no data chunk", ErrNotWAV)
			}
			return Audio{}, err
		}
		id := string(hdr[0:4])
		size := binary.LittleEndian.Uint32(hdr[4:8])

		switch id {
		case "fmt ":
			if size < 16 {
				return Audio{}, fmt.Errorf("%w: fmt chunk too small", ErrNotWAV)
			}
			buf := make([]byte, size)
			if _, err := io.ReadFull(r, buf); err != nil {
				return Audio{}, err
			}
			format = binary.LittleEndian.Uint16(buf[0:2])
			channels = int(binary.LittleEndian.Uint16(buf[2:4]))
			sampleRate = int(binary.LittleEndian.Uint32(buf[4:8]))
			bits = int(binary.LittleEndian.Uint16(buf[14:16]))
			// WAVE_FORMAT_EXTENSIBLE hides the real tag in the first two bytes
			// of the GUID in the extension block.
			if format == wavFormatExtensible && size >= 26 {
				format = binary.LittleEndian.Uint16(buf[24:26])
			}
			haveFmt = true

		case "data":
			if !haveFmt {
				return Audio{}, fmt.Errorf("%w: data chunk before fmt", ErrNotWAV)
			}
			if channels < 1 {
				return Audio{}, fmt.Errorf("%w: %d channels", ErrUnsupportedWAV, channels)
			}
			data := make([]byte, size)
			if _, err := io.ReadFull(r, data); err != nil {
				// Tolerate a truncated final chunk: some generators write a
				// size that overshoots the actual payload.
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					return Audio{}, err
				}
			}
			pcm, err := decodeSamples(data, format, bits)
			if err != nil {
				return Audio{}, err
			}
			return Audio{PCM: downmix(pcm, channels), SampleRate: sampleRate}, nil

		default:
			if _, err := io.CopyN(io.Discard, r, int64(size)); err != nil {
				return Audio{}, fmt.Errorf("%w: truncated at chunk %q", ErrNotWAV, id)
			}
		}

		// RIFF chunks are word-aligned: an odd size is followed by a pad byte.
		if size%2 == 1 {
			if _, err := io.CopyN(io.Discard, r, 1); err != nil && !errors.Is(err, io.EOF) {
				return Audio{}, err
			}
		}
	}
}

func decodeSamples(data []byte, format uint16, bits int) ([]int16, error) {
	switch {
	case format == wavFormatPCM && bits == 16:
		pcm := make([]int16, len(data)/2)
		for i := range pcm {
			pcm[i] = int16(binary.LittleEndian.Uint16(data[i*2:]))
		}
		return pcm, nil
	case format == wavFormatPCM && bits == 8:
		// 8-bit PCM WAV is unsigned, centered on 128.
		pcm := make([]int16, len(data))
		for i, b := range data {
			pcm[i] = int16(int(b)-128) << 8
		}
		return pcm, nil
	case format == wavFormatULaw && bits == 8:
		return DecodeULaw(data), nil
	case format == wavFormatALaw && bits == 8:
		return DecodeALaw(data), nil
	default:
		return nil, fmt.Errorf("%w: format 0x%04X, %d-bit", ErrUnsupportedWAV, format, bits)
	}
}

// downmix averages interleaved channels into mono.
func downmix(pcm []int16, channels int) []int16 {
	if channels == 1 {
		return pcm
	}
	n := len(pcm) / channels
	out := make([]int16, n)
	for i := 0; i < n; i++ {
		sum := 0
		for c := 0; c < channels; c++ {
			sum += int(pcm[i*channels+c])
		}
		out[i] = int16(sum / channels)
	}
	return out
}

// WriteWAVFile writes mono linear PCM to a 16-bit PCM WAV file.
func WriteWAVFile(path string, a Audio) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := WriteWAV(f, a); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// WriteWAV writes mono linear PCM as a 16-bit PCM RIFF/WAVE stream.
func WriteWAV(w io.Writer, a Audio) error {
	if a.SampleRate <= 0 {
		return fmt.Errorf("codec: invalid sample rate %d", a.SampleRate)
	}
	const (
		channels = 1
		bits     = 16
	)
	dataLen := len(a.PCM) * 2
	byteRate := a.SampleRate * channels * bits / 8

	hdr := make([]byte, 0, 44)
	le16 := func(v uint16) { hdr = binary.LittleEndian.AppendUint16(hdr, v) }
	le32 := func(v uint32) { hdr = binary.LittleEndian.AppendUint32(hdr, v) }

	hdr = append(hdr, "RIFF"...)
	le32(uint32(36 + dataLen))
	hdr = append(hdr, "WAVE"...)
	hdr = append(hdr, "fmt "...)
	le32(16)
	le16(wavFormatPCM)
	le16(channels)
	le32(uint32(a.SampleRate))
	le32(uint32(byteRate))
	le16(channels * bits / 8) // block align
	le16(bits)
	hdr = append(hdr, "data"...)
	le32(uint32(dataLen))

	if _, err := w.Write(hdr); err != nil {
		return err
	}

	body := make([]byte, dataLen)
	for i, s := range a.PCM {
		binary.LittleEndian.PutUint16(body[i*2:], uint16(s))
	}
	_, err := w.Write(body)
	return err
}

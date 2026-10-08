package codec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestWAVRoundTripPreservesPCMExactly(t *testing.T) {
	in := Tone(440, SampleRate8k, 100*time.Millisecond, 0.6)

	var buf bytes.Buffer
	if err := WriteWAV(&buf, in); err != nil {
		t.Fatalf("WriteWAV: %v", err)
	}

	out, err := ReadWAV(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadWAV: %v", err)
	}
	if out.SampleRate != in.SampleRate {
		t.Errorf("sample rate %d, want %d", out.SampleRate, in.SampleRate)
	}
	// 16-bit PCM is lossless, so this must be exact — not merely close.
	if !equalPCM(out.PCM, in.PCM) {
		t.Error("PCM changed across a WAV round trip")
	}
}

func TestWAVFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tone.wav")
	in := Tone(1000, SampleRate8k, 50*time.Millisecond, 0.4)

	if err := WriteWAVFile(path, in); err != nil {
		t.Fatalf("WriteWAVFile: %v", err)
	}
	out, err := ReadWAVFile(path)
	if err != nil {
		t.Fatalf("ReadWAVFile: %v", err)
	}
	if !equalPCM(out.PCM, in.PCM) {
		t.Error("PCM changed across a WAV file round trip")
	}
	if d := out.Duration(); d < 0.049 || d > 0.051 {
		t.Errorf("Duration() = %.4fs, want ~0.05s", d)
	}
}

// TestReadWAVCompandedPayloads covers the telephony case: WAV files whose data
// chunk is already G.711 rather than linear PCM.
func TestReadWAVCompandedPayloads(t *testing.T) {
	pcm := Tone(500, SampleRate8k, 60*time.Millisecond, 0.5).PCM

	tests := []struct {
		name   string
		format uint16
		data   []byte
		want   []int16
	}{
		{"ulaw", wavFormatULaw, EncodeULaw(pcm), DecodeULaw(EncodeULaw(pcm))},
		{"alaw", wavFormatALaw, EncodeALaw(pcm), DecodeALaw(EncodeALaw(pcm))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := buildWAV(tc.format, 1, SampleRate8k, 8, tc.data)
			got, err := ReadWAV(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("ReadWAV: %v", err)
			}
			if !equalPCM(got.PCM, tc.want) {
				t.Error("companded WAV did not decode to the expected PCM")
			}
		})
	}
}

func TestReadWAVDownmixesStereo(t *testing.T) {
	// Left is constant +1000, right constant -1000; the mono average is 0.
	const n = 160
	left, right := int16(1000), int16(-1000)
	data := make([]byte, n*2*2)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint16(data[i*4:], uint16(left))
		binary.LittleEndian.PutUint16(data[i*4+2:], uint16(right))
	}
	raw := buildWAV(wavFormatPCM, 2, SampleRate8k, 16, data)

	got, err := ReadWAV(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadWAV: %v", err)
	}
	if len(got.PCM) != n {
		t.Fatalf("got %d mono samples, want %d", len(got.PCM), n)
	}
	for i, s := range got.PCM {
		if s != 0 {
			t.Fatalf("sample %d = %d, want 0 after downmix", i, s)
		}
	}
}

// TestReadWAVSkipsUnknownChunks guards against the real-world case of recorders
// that insert LIST/fact chunks before the data chunk.
func TestReadWAVSkipsUnknownChunks(t *testing.T) {
	pcm := []int16{100, -100, 200, -200}
	data := make([]byte, len(pcm)*2)
	for i, s := range pcm {
		binary.LittleEndian.PutUint16(data[i*2:], uint16(s))
	}

	body := new(bytes.Buffer)
	writeChunk(body, "fmt ", fmtChunk(wavFormatPCM, 1, SampleRate8k, 16))
	// An odd-sized chunk also exercises RIFF word-alignment padding.
	writeChunk(body, "LIST", []byte("INFOxyz"))
	writeChunk(body, "data", data)

	got, err := ReadWAV(bytes.NewReader(riffWrap(body.Bytes())))
	if err != nil {
		t.Fatalf("ReadWAV: %v", err)
	}
	if !equalPCM(got.PCM, pcm) {
		t.Errorf("PCM = %v, want %v", got.PCM, pcm)
	}
}

func TestReadWAVExtensibleFormat(t *testing.T) {
	pcm := []int16{1, 2, 3, 4}
	data := make([]byte, len(pcm)*2)
	for i, s := range pcm {
		binary.LittleEndian.PutUint16(data[i*2:], uint16(s))
	}

	// WAVE_FORMAT_EXTENSIBLE: the real tag lives in the extension GUID.
	f := fmtChunk(wavFormatExtensible, 1, SampleRate8k, 16)
	f = binary.LittleEndian.AppendUint16(f, 22) // cbSize
	f = binary.LittleEndian.AppendUint16(f, 16) // valid bits
	f = binary.LittleEndian.AppendUint32(f, 0)  // channel mask
	f = binary.LittleEndian.AppendUint16(f, wavFormatPCM)
	f = append(f, make([]byte, 14)...) // rest of the GUID

	body := new(bytes.Buffer)
	writeChunk(body, "fmt ", f)
	writeChunk(body, "data", data)

	got, err := ReadWAV(bytes.NewReader(riffWrap(body.Bytes())))
	if err != nil {
		t.Fatalf("ReadWAV: %v", err)
	}
	if !equalPCM(got.PCM, pcm) {
		t.Errorf("PCM = %v, want %v", got.PCM, pcm)
	}
}

func TestReadWAVRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
		want error
	}{
		{"empty", nil, ErrNotWAV},
		{"short header", []byte("RIFF"), ErrNotWAV},
		{"not wave", append([]byte("RIFF"), append(make([]byte, 4), []byte("AVI ")...)...), ErrNotWAV},
		{"no data chunk", riffWrap(chunk("fmt ", fmtChunk(wavFormatPCM, 1, SampleRate8k, 16))), ErrNotWAV},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ReadWAV(bytes.NewReader(tc.raw)); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}

	// Valid WAV, but an encoding we deliberately do not support (32-bit float).
	raw := buildWAV(0x0003, 1, SampleRate8k, 32, make([]byte, 64))
	if _, err := ReadWAV(bytes.NewReader(raw)); !errors.Is(err, ErrUnsupportedWAV) {
		t.Errorf("32-bit float: err = %v, want ErrUnsupportedWAV", err)
	}
}

func TestWriteWAVRejectsBadSampleRate(t *testing.T) {
	if err := WriteWAV(&bytes.Buffer{}, Audio{PCM: []int16{1}, SampleRate: 0}); err == nil {
		t.Error("WriteWAV accepted a zero sample rate")
	}
}

// --- helpers for building WAV bytes by hand ---

func fmtChunk(format uint16, channels, sampleRate, bits int) []byte {
	b := make([]byte, 0, 16)
	b = binary.LittleEndian.AppendUint16(b, format)
	b = binary.LittleEndian.AppendUint16(b, uint16(channels))
	b = binary.LittleEndian.AppendUint32(b, uint32(sampleRate))
	b = binary.LittleEndian.AppendUint32(b, uint32(sampleRate*channels*bits/8))
	b = binary.LittleEndian.AppendUint16(b, uint16(channels*bits/8))
	b = binary.LittleEndian.AppendUint16(b, uint16(bits))
	return b
}

func chunk(id string, payload []byte) []byte {
	buf := new(bytes.Buffer)
	writeChunk(buf, id, payload)
	return buf.Bytes()
}

func writeChunk(w *bytes.Buffer, id string, payload []byte) {
	w.WriteString(id)
	binary.Write(w, binary.LittleEndian, uint32(len(payload)))
	w.Write(payload)
	if len(payload)%2 == 1 {
		w.WriteByte(0) // RIFF word alignment
	}
}

func riffWrap(body []byte) []byte {
	out := make([]byte, 0, 12+len(body))
	out = append(out, "RIFF"...)
	out = binary.LittleEndian.AppendUint32(out, uint32(4+len(body)))
	out = append(out, "WAVE"...)
	return append(out, body...)
}

func buildWAV(format uint16, channels, sampleRate, bits int, data []byte) []byte {
	body := new(bytes.Buffer)
	writeChunk(body, "fmt ", fmtChunk(format, channels, sampleRate, bits))
	writeChunk(body, "data", data)
	return riffWrap(body.Bytes())
}

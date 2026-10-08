package rtp

import (
	"math"
	"testing"

	"github.com/mharner33/voice-demo/internal/codec"
)

func TestPacketizerAdvancesSequenceAndTimestamp(t *testing.T) {
	p := NewPacketizerAt(0xABCD, codec.PCMU, 100, 5000)
	payload := make([]byte, codec.G711FrameBytes)

	for i := 0; i < 10; i++ {
		pkt := p.Packetize(payload)

		if want := uint16(100 + i); pkt.SequenceNumber != want {
			t.Errorf("packet %d: seq = %d, want %d", i, pkt.SequenceNumber, want)
		}
		// Timestamps advance by the sample count, not by one.
		if want := uint32(5000 + i*codec.SamplesPerFrame); pkt.Timestamp != want {
			t.Errorf("packet %d: timestamp = %d, want %d", i, pkt.Timestamp, want)
		}
		if pkt.SSRC != 0xABCD {
			t.Errorf("packet %d: SSRC = %#x, want 0xabcd", i, pkt.SSRC)
		}
		if pkt.Version != 2 {
			t.Errorf("packet %d: version = %d, want 2", i, pkt.Version)
		}
		if want := uint8(codec.PayloadTypePCMU); pkt.PayloadType != want {
			t.Errorf("packet %d: payload type = %d, want %d", i, pkt.PayloadType, want)
		}
	}
}

// TestPacketizerMarksOnlyFirstPacket checks the marker bit, which signals the
// start of a talkspurt to the receiver.
func TestPacketizerMarksOnlyFirstPacket(t *testing.T) {
	p := NewPacketizerAt(1, codec.PCMU, 0, 0)
	payload := make([]byte, codec.G711FrameBytes)

	if !p.Packetize(payload).Marker {
		t.Error("first packet has no marker bit")
	}
	for i := 1; i < 5; i++ {
		if p.Packetize(payload).Marker {
			t.Errorf("packet %d has the marker bit set", i)
		}
	}
}

// TestPacketizerWrapsSequence covers the 16-bit rollover. A long call crosses it,
// and wrapping must be a plain increment rather than a panic or a reset.
func TestPacketizerWrapsSequence(t *testing.T) {
	p := NewPacketizerAt(1, codec.PCMU, math.MaxUint16, 0)
	payload := make([]byte, codec.G711FrameBytes)

	if got := p.Packetize(payload).SequenceNumber; got != math.MaxUint16 {
		t.Fatalf("seq = %d, want %d", got, math.MaxUint16)
	}
	if got := p.Packetize(payload).SequenceNumber; got != 0 {
		t.Errorf("seq after wrap = %d, want 0", got)
	}
	if got := p.Packetize(payload).SequenceNumber; got != 1 {
		t.Errorf("seq = %d, want 1", got)
	}
}

func TestPacketizerWrapsTimestamp(t *testing.T) {
	start := uint32(math.MaxUint32 - codec.SamplesPerFrame/2)
	p := NewPacketizerAt(1, codec.PCMU, 0, start)
	payload := make([]byte, codec.G711FrameBytes)

	if got := p.Packetize(payload).Timestamp; got != start {
		t.Fatalf("timestamp = %d, want %d", got, start)
	}
	// Wraps modulo 2^32 rather than saturating.
	want := start + codec.SamplesPerFrame
	if got := p.Packetize(payload).Timestamp; got != want {
		t.Errorf("timestamp after wrap = %d, want %d", got, want)
	}
}

func TestPacketizerUsesCodecPayloadType(t *testing.T) {
	payload := make([]byte, codec.G711FrameBytes)

	if got := NewPacketizerAt(1, codec.PCMA, 0, 0).Packetize(payload).PayloadType; got != 8 {
		t.Errorf("PCMA payload type = %d, want 8", got)
	}
	if got := NewPacketizerAt(1, codec.PCMU, 0, 0).Packetize(payload).PayloadType; got != 0 {
		t.Errorf("PCMU payload type = %d, want 0", got)
	}
}

func TestPacketizerAccessors(t *testing.T) {
	p := NewPacketizerAt(0x1234, codec.PCMU, 77, 0)
	if p.SSRC() != 0x1234 {
		t.Errorf("SSRC() = %#x, want 0x1234", p.SSRC())
	}
	if p.NextSeq() != 77 {
		t.Errorf("NextSeq() = %d, want 77", p.NextSeq())
	}
	p.Packetize(make([]byte, codec.G711FrameBytes))
	if p.NextSeq() != 78 {
		t.Errorf("NextSeq() after one packet = %d, want 78", p.NextSeq())
	}
}

// TestMarshalUnmarshalRoundTrip verifies the wire encoding survives a round trip
// with every header field and the payload intact.
func TestMarshalUnmarshalRoundTrip(t *testing.T) {
	pcm := codec.Tone(440, codec.SampleRate8k, codec.FrameDuration, 0.5).PCM
	payload := codec.PCMU.Encode(pcm)
	if len(payload) != codec.G711FrameBytes {
		t.Fatalf("payload is %d bytes, want %d", len(payload), codec.G711FrameBytes)
	}

	p := NewPacketizerAt(0xCAFEBABE, codec.PCMU, 9999, 123456)
	want := p.Packetize(payload)

	buf, err := Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// 12-byte header plus the payload, with no extensions or padding.
	if len(buf) != 12+codec.G711FrameBytes {
		t.Errorf("marshaled to %d bytes, want %d", len(buf), 12+codec.G711FrameBytes)
	}

	got, err := Unmarshal(buf)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.SequenceNumber != want.SequenceNumber {
		t.Errorf("seq = %d, want %d", got.SequenceNumber, want.SequenceNumber)
	}
	if got.Timestamp != want.Timestamp {
		t.Errorf("timestamp = %d, want %d", got.Timestamp, want.Timestamp)
	}
	if got.SSRC != want.SSRC {
		t.Errorf("SSRC = %#x, want %#x", got.SSRC, want.SSRC)
	}
	if got.PayloadType != want.PayloadType {
		t.Errorf("payload type = %d, want %d", got.PayloadType, want.PayloadType)
	}
	if got.Marker != want.Marker {
		t.Errorf("marker = %v, want %v", got.Marker, want.Marker)
	}
	if string(got.Payload) != string(payload) {
		t.Error("payload changed across the round trip")
	}

	// The audio must still decode to the same samples after the wire trip.
	if decoded := codec.PCMU.Decode(got.Payload); len(decoded) != len(pcm) {
		t.Errorf("decoded %d samples, want %d", len(decoded), len(pcm))
	}
}

func TestUnmarshalRejectsGarbage(t *testing.T) {
	for _, tc := range []struct {
		name string
		buf  []byte
	}{
		{"empty", nil},
		{"truncated header", []byte{0x80, 0x00, 0x00}},
		{"zeros", make([]byte, 4)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Unmarshal(tc.buf); err == nil {
				t.Error("Unmarshal accepted a malformed packet")
			}
		})
	}
}

// TestRandomIdentifiersDiffer checks the random SSRC and starting sequence
// required by RFC 3550 are actually random, so two concurrent calls do not
// collide in the receiver's SSRC demultiplexing.
func TestRandomIdentifiersDiffer(t *testing.T) {
	seen := make(map[uint32]bool)
	for i := 0; i < 100; i++ {
		ssrc, err := NewSSRC()
		if err != nil {
			t.Fatalf("NewSSRC: %v", err)
		}
		if seen[ssrc] {
			t.Fatalf("NewSSRC returned a duplicate value %#x", ssrc)
		}
		seen[ssrc] = true
	}

	p1, err := NewPacketizer(1, codec.PCMU)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := NewPacketizer(1, codec.PCMU)
	if err != nil {
		t.Fatal(err)
	}
	// A collision is possible but overwhelmingly unlikely; a constant start is not.
	if p1.NextSeq() == p2.NextSeq() {
		t.Log("two packetizers started at the same sequence number — " +
			"possible by chance, suspicious if it repeats")
	}
}

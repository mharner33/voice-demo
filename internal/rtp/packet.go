// Package rtp carries G.711 audio over RTP and measures what the network did to
// it: packet loss, interarrival jitter, reordering, and duplication.
//
// The measurement side is deliberately independent of the sending side. The
// receiver reconstructs loss from sequence-number gaps alone, exactly as a real
// gateway must, so its numbers can be checked against what the sender knows it
// dropped. A demo that reported the sender's own drop count would prove nothing.
package rtp

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"

	pionrtp "github.com/pion/rtp"

	"github.com/mharner33/voice-demo/internal/codec"
)

// MaxPacketSize bounds a read buffer. G.711 at 20 ms is a 12-byte header plus a
// 160-byte payload, so this is generous even with header extensions.
const MaxPacketSize = 1500

// Packetizer turns encoded audio frames into RTP packets, advancing the sequence
// number and timestamp per RFC 3550.
type Packetizer struct {
	ssrc        uint32
	payloadType uint8
	seq         uint16
	timestamp   uint32
	samples     uint32 // samples per frame, the timestamp increment
	first       bool
}

// NewPacketizer creates a packetizer for a codec. Sequence number and timestamp
// start at random values, as RFC 3550 requires.
func NewPacketizer(ssrc uint32, c codec.Codec) (*Packetizer, error) {
	seq, err := randUint16()
	if err != nil {
		return nil, err
	}
	ts, err := randUint32()
	if err != nil {
		return nil, err
	}
	return NewPacketizerAt(ssrc, c, seq, ts), nil
}

// NewPacketizerAt creates a packetizer with explicit starting sequence and
// timestamp, so tests can pin the wire values — including near the wrap point.
func NewPacketizerAt(ssrc uint32, c codec.Codec, startSeq uint16, startTS uint32) *Packetizer {
	return &Packetizer{
		ssrc:        ssrc,
		payloadType: uint8(c.PayloadType()),
		seq:         startSeq,
		timestamp:   startTS,
		samples:     codec.SamplesPerFrame,
		first:       true,
	}
}

// Packetize wraps one encoded frame in an RTP packet. The marker bit is set on
// the first packet of the stream, which is how a receiver recognizes the start
// of a talkspurt.
func (p *Packetizer) Packetize(payload []byte) *pionrtp.Packet {
	pkt := &pionrtp.Packet{
		Header: pionrtp.Header{
			Version:        2,
			PayloadType:    p.payloadType,
			SequenceNumber: p.seq,
			Timestamp:      p.timestamp,
			SSRC:           p.ssrc,
			Marker:         p.first,
		},
		Payload: payload,
	}
	p.seq++                  // intentionally wraps at 65535
	p.timestamp += p.samples // intentionally wraps at 2^32
	p.first = false
	return pkt
}

// SSRC returns the synchronization source identifier of this stream.
func (p *Packetizer) SSRC() uint32 { return p.ssrc }

// NextSeq returns the sequence number the next packet will carry.
func (p *Packetizer) NextSeq() uint16 { return p.seq }

// Marshal serializes a packet for the wire.
func Marshal(pkt *pionrtp.Packet) ([]byte, error) { return pkt.Marshal() }

// Unmarshal parses a packet off the wire.
func Unmarshal(b []byte) (*pionrtp.Packet, error) {
	pkt := &pionrtp.Packet{}
	if err := pkt.Unmarshal(b); err != nil {
		return nil, fmt.Errorf("rtp: malformed packet (%d bytes): %w", len(b), err)
	}
	return pkt, nil
}

// NewSSRC generates a random synchronization source identifier.
func NewSSRC() (uint32, error) { return randUint32() }

func randUint32() (uint32, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, fmt.Errorf("rtp: generating random value: %w", err)
	}
	return binary.BigEndian.Uint32(b[:]), nil
}

func randUint16() (uint16, error) {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, fmt.Errorf("rtp: generating random value: %w", err)
	}
	return binary.BigEndian.Uint16(b[:]), nil
}

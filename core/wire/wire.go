// Package wire defines the bond header that wraps every WireGuard datagram
// on the way across a path, and the small control payloads that ride next to
// the data.
//
//	 0      1        2       3       4          8          12          16         20        28
//	+------+--------+-------+-------+----------+----------+-----------+----------+---------+---------
//	|ver|ty| pathID | flags | class | session  | path seq | global seq| send ts  |   tag   | payload
//	+------+--------+-------+-------+----------+----------+-----------+----------+---------+---------
//
// The tag is SipHash-2-4 over the whole datagram (tag field zeroed), keyed by
// the per-session key handed out at join time. It authenticates the path
// metadata so nobody off the session can move a path's address, poison the
// reorder window or forge feedback. Confidentiality and integrity of user
// traffic are WireGuard's job, one layer up.
package wire

import (
	"encoding/binary"
	"errors"
)

const (
	Version   = 1
	HeaderLen = 28
	tagOff    = 20
	TagLen    = 8
)

type Type uint8

const (
	TypeData  Type = 1 // payload is one WireGuard message
	TypeProbe Type = 2 // payload is a Feedback
	TypeHello Type = 3 // payload is JSON path metadata (hub → aggregator)
)

const (
	FlagFromServer uint8 = 1 << 0 // direction bit; stops reflection of a peer's own packets
	FlagDup        uint8 = 1 << 1 // another copy of this global seq went out on a different path
	FlagRetx       uint8 = 1 << 2 // resent after the original path died
)

// Class is the scheduling class of a datagram.
type Class uint8

const (
	ClassBulk     Class = 0 // one path, reordered on receive
	ClassRealtime Class = 1 // duplicated on the best paths, never held for reordering
	ClassControl  Class = 2 // handshakes and keepalives: sprayed on every live path
)

func (c Class) String() string {
	switch c {
	case ClassRealtime:
		return "realtime"
	case ClassControl:
		return "control"
	}
	return "bulk"
}

type Header struct {
	Type      Type
	PathID    uint8
	Flags     uint8
	Class     Class
	Session   uint32
	PathSeq   uint32
	GlobalSeq uint32
	SendTS    uint32 // sender clock, milliseconds
}

// Key is the 128-bit per-session tag key.
type Key struct{ k0, k1 uint64 }

func NewKey(b [16]byte) Key {
	return Key{binary.LittleEndian.Uint64(b[0:8]), binary.LittleEndian.Uint64(b[8:16])}
}

var (
	ErrShort   = errors.New("wire: short packet")
	ErrVersion = errors.New("wire: unknown version")
)

// Put writes h into the first HeaderLen bytes of b, leaving the tag zeroed.
func (h *Header) Put(b []byte) {
	_ = b[HeaderLen-1]
	b[0] = Version<<4 | uint8(h.Type)&0x0f
	b[1] = h.PathID
	b[2] = h.Flags
	b[3] = uint8(h.Class)
	binary.BigEndian.PutUint32(b[4:], h.Session)
	binary.BigEndian.PutUint32(b[8:], h.PathSeq)
	binary.BigEndian.PutUint32(b[12:], h.GlobalSeq)
	binary.BigEndian.PutUint32(b[16:], h.SendTS)
	clear(b[tagOff:HeaderLen])
}

func Parse(b []byte) (Header, error) {
	if len(b) < HeaderLen {
		return Header{}, ErrShort
	}
	if b[0]>>4 != Version {
		return Header{}, ErrVersion
	}
	return Header{
		Type:      Type(b[0] & 0x0f),
		PathID:    b[1],
		Flags:     b[2],
		Class:     Class(b[3]),
		Session:   binary.BigEndian.Uint32(b[4:]),
		PathSeq:   binary.BigEndian.Uint32(b[8:]),
		GlobalSeq: binary.BigEndian.Uint32(b[12:]),
		SendTS:    binary.BigEndian.Uint32(b[16:]),
	}, nil
}

// SessionOf peeks the session id without a full parse.
func SessionOf(b []byte) (uint32, bool) {
	if len(b) < HeaderLen || b[0]>>4 != Version {
		return 0, false
	}
	return binary.BigEndian.Uint32(b[4:]), true
}

// Seal computes the tag of pkt (header already written) and stores it.
func Seal(k *Key, pkt []byte) {
	clear(pkt[tagOff:HeaderLen])
	binary.LittleEndian.PutUint64(pkt[tagOff:], sipHash24(k.k0, k.k1, pkt))
}

// Verify checks pkt's tag. It leaves pkt unchanged.
func Verify(k *Key, pkt []byte) bool {
	if len(pkt) < HeaderLen {
		return false
	}
	got := binary.LittleEndian.Uint64(pkt[tagOff:])
	clear(pkt[tagOff:HeaderLen])
	want := sipHash24(k.k0, k.k1, pkt)
	binary.LittleEndian.PutUint64(pkt[tagOff:], got)
	return got == want
}

// SeqLess reports whether a comes before b in 32-bit serial-number order.
func SeqLess(a, b uint32) bool { return int32(a-b) < 0 }

// FeedbackLen is the size of an encoded Feedback.
const FeedbackLen = 28

// Feedback is the receiver's report about one path, carried by every probe.
// All counters are cumulative so a lost probe costs nothing.
type Feedback struct {
	EchoTS    uint32 // SendTS of the newest packet seen on this path
	EchoDelay uint32 // milliseconds that packet sat here before this probe left; ^0 = nothing to echo
	RxHighest uint32 // highest path seq seen
	RxPackets uint32 // packets seen on this path
	RxBytes   uint64 // bytes seen on this path
	RxClock   uint32 // receiver clock (ms) when this was built, for delivery-rate maths
}

const NoEcho = ^uint32(0)

func (f *Feedback) Put(b []byte) {
	_ = b[FeedbackLen-1]
	binary.BigEndian.PutUint32(b[0:], f.EchoTS)
	binary.BigEndian.PutUint32(b[4:], f.EchoDelay)
	binary.BigEndian.PutUint32(b[8:], f.RxHighest)
	binary.BigEndian.PutUint32(b[12:], f.RxPackets)
	binary.BigEndian.PutUint64(b[16:], f.RxBytes)
	binary.BigEndian.PutUint32(b[24:], f.RxClock)
}

func ParseFeedback(b []byte) (Feedback, error) {
	if len(b) < FeedbackLen {
		return Feedback{}, ErrShort
	}
	return Feedback{
		EchoTS:    binary.BigEndian.Uint32(b[0:]),
		EchoDelay: binary.BigEndian.Uint32(b[4:]),
		RxHighest: binary.BigEndian.Uint32(b[8:]),
		RxPackets: binary.BigEndian.Uint32(b[12:]),
		RxBytes:   binary.BigEndian.Uint64(b[16:]),
		RxClock:   binary.BigEndian.Uint32(b[24:]),
	}, nil
}

package wire

import (
	"bytes"
	"testing"
)

func refKey() (uint64, uint64) {
	// Key 00 01 .. 0f from the SipHash paper.
	return 0x0706050403020100, 0x0f0e0d0c0b0a0908
}

func TestSipHashVectors(t *testing.T) {
	k0, k1 := refKey()
	if got := sipHash24(k0, k1, nil); got != 0x726fdb47dd0e0e31 {
		t.Fatalf("empty: %#x", got)
	}
	msg := make([]byte, 15)
	for i := range msg {
		msg[i] = byte(i)
	}
	if got := sipHash24(k0, k1, msg); got != 0xa129ca6149be45e5 {
		t.Fatalf("15 bytes: %#x", got)
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	h := Header{Type: TypeData, PathID: 7, Flags: FlagFromServer | FlagDup, Class: ClassRealtime,
		Session: 0xdeadbeef, PathSeq: 42, GlobalSeq: 0xffffffff, SendTS: 123456}
	pkt := make([]byte, HeaderLen+5)
	h.Put(pkt)
	copy(pkt[HeaderLen:], "hello")
	got, err := Parse(pkt)
	if err != nil || got != h {
		t.Fatalf("got %+v err %v", got, err)
	}
	if id, ok := SessionOf(pkt); !ok || id != h.Session {
		t.Fatalf("SessionOf = %x %v", id, ok)
	}
}

func TestSealVerify(t *testing.T) {
	k := NewKey([16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	other := NewKey([16]byte{9})
	h := Header{Type: TypeData, Session: 1, PathSeq: 1, GlobalSeq: 1}
	pkt := make([]byte, HeaderLen+64)
	h.Put(pkt)
	for i := HeaderLen; i < len(pkt); i++ {
		pkt[i] = byte(i)
	}
	Seal(&k, pkt)
	orig := bytes.Clone(pkt)
	if !Verify(&k, pkt) {
		t.Fatal("valid tag rejected")
	}
	if !bytes.Equal(orig, pkt) {
		t.Fatal("Verify mutated the packet")
	}
	if Verify(&other, pkt) {
		t.Fatal("wrong key accepted")
	}
	// Any flipped bit, in header or payload, must break the tag.
	for _, i := range []int{1, 2, 5, 9, 13, 17, 21, HeaderLen, len(pkt) - 1} {
		pkt[i] ^= 0x40
		if Verify(&k, pkt) {
			t.Fatalf("tampering at byte %d accepted", i)
		}
		pkt[i] ^= 0x40
	}
}

func TestParseRejects(t *testing.T) {
	if _, err := Parse(make([]byte, HeaderLen-1)); err != ErrShort {
		t.Fatalf("short: %v", err)
	}
	b := make([]byte, HeaderLen)
	b[0] = 0xf1
	if _, err := Parse(b); err != ErrVersion {
		t.Fatalf("version: %v", err)
	}
}

func TestFeedbackRoundTrip(t *testing.T) {
	f := Feedback{EchoTS: 9, EchoDelay: NoEcho, RxHighest: 77, RxPackets: 70, RxBytes: 1 << 40, RxClock: 5}
	b := make([]byte, FeedbackLen)
	f.Put(b)
	got, err := ParseFeedback(b)
	if err != nil || got != f {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestSeqLessWraps(t *testing.T) {
	if !SeqLess(0xfffffff0, 5) || SeqLess(5, 0xfffffff0) || SeqLess(3, 3) {
		t.Fatal("serial arithmetic broken")
	}
}

func FuzzParse(f *testing.F) {
	f.Add(make([]byte, HeaderLen))
	f.Add([]byte{0x11, 0, 0, 0})
	k := NewKey([16]byte{1})
	f.Fuzz(func(t *testing.T, b []byte) {
		h, err := Parse(b)
		if err != nil {
			return
		}
		Verify(&k, b)
		if h.Type == TypeProbe {
			ParseFeedback(b[HeaderLen:])
		}
	})
}

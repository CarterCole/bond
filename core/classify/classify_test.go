package classify

import (
	"encoding/binary"
	"net/netip"
	"testing"

	"github.com/CarterCole/bond/core/wire"
)

// ip4 builds a minimal IPv4 packet with a valid header checksum.
func ip4(src, dst string, proto uint8, sport, dport uint16, dscp uint8) []byte {
	b := make([]byte, 28)
	b[0] = 0x45
	b[1] = dscp<<2 | 0x01 // keep an ECN bit set to prove it survives
	binary.BigEndian.PutUint16(b[2:], 28)
	b[8] = 64
	b[9] = proto
	copy(b[12:], netip.MustParseAddr(src).AsSlice())
	copy(b[16:], netip.MustParseAddr(dst).AsSlice())
	binary.BigEndian.PutUint16(b[10:], ^sum16(b[:20]))
	binary.BigEndian.PutUint16(b[20:], sport)
	binary.BigEndian.PutUint16(b[22:], dport)
	return b
}

func ip6(src, dst string, proto uint8, sport, dport uint16) []byte {
	b := make([]byte, 48)
	b[0] = 0x60
	b[3] = 0x77 // flow label bits that must not be disturbed
	b[6] = proto
	copy(b[8:], netip.MustParseAddr(src).AsSlice())
	copy(b[24:], netip.MustParseAddr(dst).AsSlice())
	binary.BigEndian.PutUint16(b[40:], sport)
	binary.BigEndian.PutUint16(b[42:], dport)
	return b
}

func sum16(b []byte) uint16 {
	var s uint32
	for i := 0; i+1 < len(b); i += 2 {
		s += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	for s>>16 != 0 {
		s = s&0xffff + s>>16
	}
	return uint16(s)
}

func mustNew(t *testing.T, r Rules) *Classifier {
	t.Helper()
	c, err := New(r)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRules(t *testing.T) {
	c := mustNew(t, Rules{Devices: []string{"192.168.1.50", "10.9.0.0/24"}, Ports: []string{"1935", "udp/9000-9010"}, DSCP: []uint8{34}})
	cases := []struct {
		name string
		pkt  []byte
		want wire.Class
	}{
		{"priority device as source", ip4("192.168.1.50", "8.8.8.8", protoTCP, 5000, 443, 0), wire.ClassRealtime},
		{"priority device as destination", ip4("8.8.8.8", "192.168.1.50", protoTCP, 443, 5000, 0), wire.ClassRealtime},
		{"priority prefix", ip4("10.9.0.77", "8.8.8.8", protoUDP, 1, 2, 0), wire.ClassRealtime},
		{"other device", ip4("192.168.1.51", "8.8.8.8", protoTCP, 5000, 443, 0), wire.ClassBulk},
		{"rtmp port any proto", ip4("192.168.1.9", "8.8.8.8", protoTCP, 5000, 1935, 0), wire.ClassRealtime},
		{"udp range", ip4("192.168.1.9", "8.8.8.8", protoUDP, 9005, 4000, 0), wire.ClassRealtime},
		{"udp range does not match tcp", ip4("192.168.1.9", "8.8.8.8", protoTCP, 9005, 4000, 0), wire.ClassBulk},
		{"already marked EF", ip4("192.168.1.9", "8.8.8.8", protoUDP, 1, 2, EF), wire.ClassRealtime},
		{"extra dscp AF41", ip4("192.168.1.9", "8.8.8.8", protoUDP, 1, 2, 34), wire.ClassRealtime},
		{"icmp from priority device", ip4("192.168.1.50", "8.8.8.8", 1, 0, 0, 0), wire.ClassRealtime},
		{"ipv6 port", ip6("fd00::1", "2001:db8::1", protoTCP, 40000, 1935), wire.ClassRealtime},
		{"ipv6 other", ip6("fd00::1", "2001:db8::1", protoTCP, 40000, 443), wire.ClassBulk},
		{"garbage", []byte{0x45, 0, 0}, wire.ClassBulk},
	}
	for _, tc := range cases {
		if got := c.Outbound(tc.pkt); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAllMakesEverythingRealtime(t *testing.T) {
	c := mustNew(t, Rules{All: true})
	if c.Outbound(ip4("1.2.3.4", "5.6.7.8", protoTCP, 1, 2, 0)) != wire.ClassRealtime {
		t.Fatal("All did not apply")
	}
}

func TestStampSetsEFAndKeepsChecksumValid(t *testing.T) {
	c := mustNew(t, Rules{Devices: []string{"192.168.1.50"}})
	c.Stamp = true
	p := ip4("192.168.1.50", "8.8.8.8", protoUDP, 1, 2, 0)
	c.Outbound(p)
	if p[1]>>2 != EF || p[1]&3 != 1 {
		t.Fatalf("tos byte %#x: want DSCP EF with ECN untouched", p[1])
	}
	if sum16(p[:20]) != 0xffff {
		t.Fatal("IPv4 header checksum broken by restamp")
	}
	// Unmatched packets are left alone.
	q := ip4("192.168.1.51", "8.8.8.8", protoUDP, 1, 2, 0)
	c.Outbound(q)
	if q[1]>>2 != 0 {
		t.Fatal("stamped a bulk packet")
	}

	v6 := ip6("fd00::1", "2001:db8::1", protoUDP, 1, 2)
	c6 := mustNew(t, Rules{All: true})
	c6.Stamp = true
	c6.Outbound(v6)
	if pk, _ := parse(v6); pk.dscp != EF || v6[3] != 0x77 || v6[0]>>4 != 6 {
		t.Fatalf("ipv6 restamp wrong: dscp=%d bytes=% x", pk.dscp, v6[:4])
	}
}

// The aggregator has no rules: it learns from marked uplink packets and
// applies the class to the replies.
func TestAggregatorLearnsReplyFlows(t *testing.T) {
	hub := mustNew(t, Rules{Devices: []string{"10.207.0.5"}})
	hub.Stamp = true
	agg := mustNew(t, Rules{})
	agg.Learn = true

	up := ip4("10.207.0.5", "93.184.216.34", protoTCP, 51000, 443, 0)
	down := ip4("93.184.216.34", "10.207.0.5", protoTCP, 443, 51000, 0)
	otherDown := ip4("93.184.216.34", "10.207.0.5", protoTCP, 443, 51001, 0)

	if agg.Outbound(down) != wire.ClassBulk {
		t.Fatal("reply was realtime before anything was learned")
	}
	if hub.Outbound(up) != wire.ClassRealtime {
		t.Fatal("hub rule did not match")
	}
	agg.Inbound(up) // comes out of the tunnel stamped EF
	if agg.Outbound(down) != wire.ClassRealtime {
		t.Fatal("reply to a realtime flow was not realtime")
	}
	if agg.Outbound(otherDown) != wire.ClassBulk {
		t.Fatal("a different flow to the same device was promoted")
	}
}

func TestBadRulesRejected(t *testing.T) {
	for _, r := range []Rules{
		{Devices: []string{"not-an-ip"}},
		{Ports: []string{"sctp/5"}},
		{Ports: []string{"0"}},
		{Ports: []string{"9-3"}},
		{DSCP: []uint8{64}},
	} {
		if Validate(r) == nil {
			t.Errorf("accepted %+v", r)
		}
	}
}

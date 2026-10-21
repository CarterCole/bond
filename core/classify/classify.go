// Package classify decides which packets are realtime ("priority") and
// which are bulk, by looking at the plaintext packet on its way into the
// tunnel.
//
// DSCP EF is the realtime mark everywhere in a bond. On a hub, packets that
// match a priority rule (a device, a port) are stamped EF before they are
// encrypted; packets an application already marked EF are honoured as they
// are. The aggregator sees the mark on what comes out of the tunnel,
// remembers the flow, and gives the replies the same treatment on the way
// back — so priority works in both directions without the two ends having to
// share rules.
package classify

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/CarterCole/bond/core/wire"
)

// EF (expedited forwarding) is the DSCP that marks realtime traffic.
const EF = 46

// Rules are the user's priority settings. The zero value prioritises nothing
// but traffic already marked EF.
type Rules struct {
	// All makes everything this node sends realtime: "this device is the
	// priority device".
	All bool `json:"all,omitempty"`
	// Devices are addresses or prefixes; packets to or from them are realtime.
	Devices []string `json:"devices,omitempty"`
	// Ports are "1935", "udp/9000", "tcp/5000-5010"; packets to or from them are realtime.
	Ports []string `json:"ports,omitempty"`
	// DSCP lists further DSCP values to treat as realtime (EF always is).
	DSCP []uint8 `json:"dscp,omitempty"`
}

func (r Rules) Empty() bool {
	return !r.All && len(r.Devices) == 0 && len(r.Ports) == 0 && len(r.DSCP) == 0
}

const (
	protoTCP = 6
	protoUDP = 17
)

type portRule struct {
	proto  uint8 // 0 = tcp and udp
	lo, hi uint16
}

type compiled struct {
	all      bool
	prefixes []netip.Prefix
	ports    []portRule
	dscp     [64]bool
}

func compile(r Rules) (*compiled, error) {
	c := &compiled{all: r.All}
	c.dscp[EF] = true
	for _, d := range r.DSCP {
		if d > 63 {
			return nil, fmt.Errorf("dscp %d out of range", d)
		}
		c.dscp[d] = true
	}
	for _, s := range r.Devices {
		if p, err := netip.ParsePrefix(s); err == nil {
			c.prefixes = append(c.prefixes, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("priority device %q is not an IP address or prefix", s)
		}
		c.prefixes = append(c.prefixes, netip.PrefixFrom(a, a.BitLen()))
	}
	for _, s := range r.Ports {
		pr, err := parsePort(s)
		if err != nil {
			return nil, err
		}
		c.ports = append(c.ports, pr)
	}
	return c, nil
}

func parsePort(s string) (portRule, error) {
	var pr portRule
	spec := s
	if proto, rest, ok := strings.Cut(s, "/"); ok {
		switch strings.ToLower(proto) {
		case "tcp":
			pr.proto = protoTCP
		case "udp":
			pr.proto = protoUDP
		default:
			return pr, fmt.Errorf("priority port %q: protocol must be tcp or udp", s)
		}
		spec = rest
	}
	lo, hi, ranged := strings.Cut(spec, "-")
	a, err := strconv.ParseUint(lo, 10, 16)
	if err != nil || a == 0 {
		return pr, fmt.Errorf("priority port %q is not a port, proto/port or port range", s)
	}
	b := a
	if ranged {
		if b, err = strconv.ParseUint(hi, 10, 16); err != nil || b < a {
			return pr, fmt.Errorf("priority port %q has a bad range", s)
		}
	}
	pr.lo, pr.hi = uint16(a), uint16(b)
	return pr, nil
}

// Validate reports whether r can be used.
func Validate(r Rules) error {
	_, err := compile(r)
	return err
}

// Classifier applies Rules to packets. All methods are safe for concurrent use.
type Classifier struct {
	rules atomic.Pointer[compiled]
	// Stamp rewrites the DSCP of rule-matched packets to EF so the far end
	// can recognise them (hub side).
	Stamp bool
	// Learn remembers flows whose packets arrive marked realtime, so the
	// replies can be matched without rules (aggregator side).
	Learn bool
	flows flowTable

	Realtime atomic.Uint64 // packets classified realtime
}

func New(r Rules) (*Classifier, error) {
	c := &Classifier{}
	if err := c.SetRules(r); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Classifier) SetRules(r Rules) error {
	cr, err := compile(r)
	if err != nil {
		return err
	}
	c.rules.Store(cr)
	return nil
}

// packet is the handful of header fields classification looks at.
type packet struct {
	v6           bool
	src, dst     netip.Addr
	proto        uint8
	sport, dport uint16
	hasPorts     bool
	dscp         uint8
}

func parse(b []byte) (p packet, ok bool) {
	if len(b) < 20 {
		return p, false
	}
	var l4 []byte
	switch b[0] >> 4 {
	case 4:
		ihl := int(b[0]&0x0f) * 4
		if ihl < 20 || len(b) < ihl {
			return p, false
		}
		p.dscp = b[1] >> 2
		p.proto = b[9]
		p.src = netip.AddrFrom4([4]byte(b[12:16]))
		p.dst = netip.AddrFrom4([4]byte(b[16:20]))
		if binary.BigEndian.Uint16(b[6:])&0x1fff == 0 { // first fragment only carries ports
			l4 = b[ihl:]
		}
	case 6:
		if len(b) < 40 {
			return p, false
		}
		p.v6 = true
		p.dscp = (b[0]&0x0f)<<2 | b[1]>>6
		p.proto = b[6]
		p.src = netip.AddrFrom16([16]byte(b[8:24]))
		p.dst = netip.AddrFrom16([16]byte(b[24:40]))
		l4 = b[40:]
	default:
		return p, false
	}
	if (p.proto == protoTCP || p.proto == protoUDP) && len(l4) >= 4 {
		p.sport = binary.BigEndian.Uint16(l4)
		p.dport = binary.BigEndian.Uint16(l4[2:])
		p.hasPorts = true
	}
	return p, true
}

func (c *compiled) match(p *packet) bool {
	if c.all {
		return true
	}
	for _, pf := range c.prefixes {
		if pf.Contains(p.src) || pf.Contains(p.dst) {
			return true
		}
	}
	if p.hasPorts {
		for _, pr := range c.ports {
			if pr.proto != 0 && pr.proto != p.proto {
				continue
			}
			if (p.sport >= pr.lo && p.sport <= pr.hi) || (p.dport >= pr.lo && p.dport <= pr.hi) {
				return true
			}
		}
	}
	return false
}

// Outbound classifies a plaintext packet that is about to enter the tunnel.
// With Stamp set it may rewrite the packet's DSCP in place.
func (c *Classifier) Outbound(b []byte) wire.Class {
	p, ok := parse(b)
	if !ok {
		return wire.ClassBulk
	}
	r := c.rules.Load()
	switch {
	case r.dscp[p.dscp]:
	case r.match(&p):
		if c.Stamp {
			setDSCP(b, p.v6, EF)
		}
	case c.Learn && c.flows.has(&p):
	default:
		return wire.ClassBulk
	}
	c.Realtime.Add(1)
	return wire.ClassRealtime
}

// Inbound looks at a plaintext packet that just came out of the tunnel. With
// Learn set, a packet marked realtime teaches the classifier its flow.
func (c *Classifier) Inbound(b []byte) {
	if !c.Learn {
		return
	}
	p, ok := parse(b)
	if !ok || !p.hasPorts {
		return
	}
	if r := c.rules.Load(); r.dscp[p.dscp] {
		c.flows.learn(&p)
	}
}

// setDSCP rewrites the DSCP field, preserving ECN and fixing the IPv4 checksum.
func setDSCP(b []byte, v6 bool, dscp uint8) {
	if v6 {
		b[0] = b[0]&0xf0 | dscp>>2
		b[1] = b[1]&0x3f | dscp<<6
		return
	}
	old := binary.BigEndian.Uint16(b[0:])
	b[1] = dscp<<2 | b[1]&0x03
	updated := binary.BigEndian.Uint16(b[0:])
	// RFC 1624: HC' = ~(~HC + ~m + m')
	sum := uint32(^binary.BigEndian.Uint16(b[10:])) + uint32(^old) + uint32(updated)
	sum = sum&0xffff + sum>>16
	sum = sum&0xffff + sum>>16
	binary.BigEndian.PutUint16(b[10:], ^uint16(sum))
}

// flowKey identifies a flow as its reply packets will present it:
// src is the far end, dst is the mesh end.
type flowKey struct {
	src, dst     netip.Addr
	sport, dport uint16
	proto        uint8
}

const (
	flowTTL = int64(60 * time.Second)
	flowMax = 16384
)

type flowTable struct {
	n     atomic.Int64 // lets the hot path skip the lock while nothing is learned
	mu    sync.RWMutex
	m     map[flowKey]int64 // last seen, unix nanos
	swept int64
}

func (t *flowTable) learn(p *packet) {
	now := time.Now().UnixNano()
	k := flowKey{src: p.dst, dst: p.src, sport: p.dport, dport: p.sport, proto: p.proto}
	// Every packet of a realtime flow lands here; only refresh once a second.
	t.mu.RLock()
	at, known := t.m[k]
	t.mu.RUnlock()
	if known && now-at < int64(time.Second) {
		return
	}
	t.mu.Lock()
	if t.m == nil {
		t.m = make(map[flowKey]int64)
	}
	if _, known := t.m[k]; known || len(t.m) < flowMax {
		t.m[k] = now
	}
	if now-t.swept > flowTTL/2 {
		t.swept = now
		for k, at := range t.m {
			if now-at > flowTTL {
				delete(t.m, k)
			}
		}
	}
	t.n.Store(int64(len(t.m)))
	t.mu.Unlock()
}

func (t *flowTable) has(p *packet) bool {
	if !p.hasPorts || t.n.Load() == 0 {
		return false
	}
	k := flowKey{src: p.src, dst: p.dst, sport: p.sport, dport: p.dport, proto: p.proto}
	t.mu.RLock()
	at, ok := t.m[k]
	t.mu.RUnlock()
	return ok && time.Now().UnixNano()-at <= flowTTL
}

package node

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/CarterCole/bond/core/control"
)

// An aggregator with no inbound port registers with a rendezvous, and hubs
// reach it there. In these tests the aggregator's link to the rendezvous goes
// through a simulated NAT that lets nothing in it did not ask for.

// nat is a UDP NAT for one inside host talking to one outside address, with
// address-and-port-dependent filtering: only that address may send back, and
// only to a mapping the inside opened. Rebind forgets every mapping, as a
// home router does when it restarts or its public address changes.
type nat struct {
	inside *net.UDPConn
	target netip.AddrPort

	mu   sync.Mutex
	maps map[netip.AddrPort]*net.UDPConn
}

func newNAT(t *testing.T, target netip.AddrPort) *nat {
	t.Helper()
	in, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	n := &nat{inside: in, target: target, maps: map[netip.AddrPort]*net.UDPConn{}}
	go n.serve()
	t.Cleanup(func() {
		in.Close()
		n.rebind()
	})
	return n
}

// addr is where the inside host sends, believing it to be the target.
func (n *nat) addr() netip.AddrPort { return n.inside.LocalAddr().(*net.UDPAddr).AddrPort() }

func (n *nat) serve() {
	b := make([]byte, 2048)
	for {
		k, from, err := n.inside.ReadFromUDPAddrPort(b)
		if err != nil {
			return
		}
		if out := n.mapping(from); out != nil {
			out.WriteToUDPAddrPort(b[:k], n.target)
		}
	}
}

func (n *nat) mapping(inside netip.AddrPort) *net.UDPConn {
	n.mu.Lock()
	defer n.mu.Unlock()
	if m := n.maps[inside]; m != nil {
		return m
	}
	out, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil
	}
	n.maps[inside] = out
	go func() {
		b := make([]byte, 2048)
		for {
			k, from, err := out.ReadFromUDPAddrPort(b)
			if err != nil {
				return
			}
			if from != n.target {
				continue // filtered: nobody else gets in
			}
			n.inside.WriteToUDPAddrPort(b[:k], inside)
		}
	}()
	return out
}

// outside lists the addresses the NAT's mappings send from.
func (n *nat) outside() map[string]bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := map[string]bool{}
	for _, m := range n.maps {
		out[m.LocalAddr().String()] = true
	}
	return out
}

func (n *nat) rebind() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for k, m := range n.maps {
		m.Close()
		delete(n.maps, k)
	}
}

func startRendezvous(t *testing.T, dir, listen string) *RendezvousNode {
	t.Helper()
	r, err := StartRendezvous(RendezvousOptions{StateDir: dir, Listen: listen, Public: "127.0.0.1", Admin: "127.0.0.1:0", Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r
}

func rvAddr(r *RendezvousNode) netip.AddrPort {
	return netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(r.Port()))
}

// startHidden starts an aggregator registered with r, its link sent through
// linkVia when that is set.
func startHidden(t *testing.T, dir string, r *RendezvousNode, linkVia netip.AddrPort) *Server {
	t.Helper()
	s, err := StartServer(ServerOptions{
		StateDir: dir, Listen: "127.0.0.1:0", Via: r.Code().String(), Userspace: true,
		Admin: "127.0.0.1:0", Log: quiet, Dial: internet(t), viaLink: linkVia,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	eventually(t, "registered with the rendezvous", 5*time.Second, registered(s))
	return s
}

func registered(s *Server) func() error {
	return func() error {
		if v := s.Status().Via; v == nil || v.State != "registered" || !v.LinkUp {
			return fmt.Errorf("rendezvous: %+v", v)
		}
		return nil
	}
}

func TestHiddenAggregatorThroughRendezvous(t *testing.T) {
	r := startRendezvous(t, t.TempDir(), "127.0.0.1:0")
	n := newNAT(t, rvAddr(r))
	s := startHidden(t, t.TempDir(), r, n.addr())

	// The join code names the rendezvous, and joining goes through it.
	code, err := s.JoinCode()
	if err != nil || code.Port != r.Port() {
		t.Fatalf("join code %v names port %d, rendezvous is on %d", err, code.Port, r.Port())
	}
	cfg, dir := join(t, s, "studio", control.KindHub)
	if cfg.Mesh.UDP[0] != rvAddr(r).String() {
		t.Fatalf("devices are told to send to %v", cfg.Mesh.UDP)
	}
	h := startHub(t, HubOptions{Config: cfg, StateDir: dir})
	eventually(t, "fetch through the rendezvous", 10*time.Second, wantPage(h))
	if up := pathsUp(h); !up["a"] || !up["b"] {
		t.Fatalf("paths: %v", up)
	}
	start := time.Now()
	body, err := fetch(h, "http://93.184.216.34/?bytes=2000000")
	if err != nil || len(body) != 2_000_000 {
		t.Fatalf("2 MB download: %d bytes, %v", len(body), err)
	}
	t.Logf("2 MB through the rendezvous in %v", time.Since(start).Round(time.Millisecond))

	// Both of the hub's paths reach the aggregator through the rendezvous,
	// and the aggregator knows it.
	ss := s.Status()
	if len(ss.Nodes) != 1 || !ss.Nodes[0].Online {
		t.Fatalf("aggregator status: %+v", ss.Nodes)
	}
	for _, p := range ss.Nodes[0].Session.Paths {
		if p.Via != r.Code().HostPort() {
			t.Fatalf("path %s via %q", p.Name, p.Via)
		}
	}
	rs := r.Status().Rendezvous
	if len(rs.Aggregators) != 1 || rs.Aggregators[0].Sources != 2 || rs.Aggregators[0].Sessions != 1 {
		t.Fatalf("rendezvous status: %+v", rs)
	}
	before := rs.Aggregators[0].Link
	if out := n.outside(); len(out) != 1 || !out[before] {
		t.Fatalf("the rendezvous sees the link from %s, not from the NAT's mapping %v", before, out)
	}

	// The router in front of the aggregator restarts and the link comes out
	// of a new port. The aggregator's next frame moves the link there.
	n.rebind()
	eventually(t, "link moved", 5*time.Second, func() error {
		if l := r.Status().Rendezvous.Aggregators[0].Link; l == before || !n.outside()[l] {
			return fmt.Errorf("link at %s, NAT mapping %v", l, n.outside())
		}
		return nil
	})
	eventually(t, "fetch after the NAT rebound", 5*time.Second, wantPage(h))
	eventually(t, "aggregator told where it is seen from", 5*time.Second, func() error {
		if seen, now := s.Status().Via.Seen, r.Status().Rendezvous.Aggregators[0].Link; seen != now {
			return fmt.Errorf("aggregator thinks %s, rendezvous sees %s", seen, now)
		}
		return nil
	})
}

func TestRendezvousCarriesWebSocketPaths(t *testing.T) {
	r := startRendezvous(t, t.TempDir(), "127.0.0.1:0")
	s := startHidden(t, t.TempDir(), r, newNAT(t, rvAddr(r)).addr())
	cfg, dir := join(t, s, "hotel", control.KindHub)
	h := startHub(t, HubOptions{Config: cfg, StateDir: dir, Transport: "ws"})
	eventually(t, "fetch over websocket paths through the rendezvous", 10*time.Second, wantPage(h))
	if up := pathsUp(h); !up["ws:a"] || !up["ws:b"] {
		t.Fatalf("paths: %v", up)
	}
	if body, err := fetch(h, "http://93.184.216.34/?bytes=500000"); err != nil || len(body) != 500_000 {
		t.Fatalf("500 kB over websocket: %d bytes, %v", len(body), err)
	}
}

func TestRelayThroughRendezvous(t *testing.T) {
	r := startRendezvous(t, t.TempDir(), "127.0.0.1:0")
	s := startHidden(t, t.TempDir(), r, netip.AddrPort{})
	rcfg, _ := join(t, s, "phone", control.KindRelay)
	rel, err := StartRelay(RelayOptions{Config: rcfg, Listen: "127.0.0.1:0", NoAdvertise: true, Admin: "-", Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rel.Close)
	cfg, dir := join(t, s, "studio", control.KindHub)
	h := startHub(t, HubOptions{Config: cfg, StateDir: dir, Links: func() []Link { return nil }})
	if err := h.AddRelay("127.0.0.1:" + strconv.Itoa(rel.Port())); err != nil {
		t.Fatal(err)
	}
	eventually(t, "fetch through relay and rendezvous", 10*time.Second, wantPage(h))
}

func TestAggregatorReregistersAfterRendezvousRestart(t *testing.T) {
	rdir := t.TempDir()
	r := startRendezvous(t, rdir, "127.0.0.1:0")
	listen := "127.0.0.1:" + strconv.Itoa(r.Port())
	s := startHidden(t, t.TempDir(), r, netip.AddrPort{})
	cfg, dir := join(t, s, "studio", control.KindHub)
	h := startHub(t, HubOptions{Config: cfg, StateDir: dir})
	eventually(t, "fetch", 10*time.Second, wantPage(h))

	r.Close()
	eventually(t, "aggregator notices", 5*time.Second, func() error {
		if v := s.Status().Via; v.State == "registered" {
			return fmt.Errorf("still %s", v.State)
		}
		return nil
	})
	r2 := startRendezvous(t, rdir, listen)
	if r2.Code() != r.Code() {
		t.Fatal("the rendezvous's pairing code changed across a restart")
	}
	// Nothing is done on the aggregator or the hub: the aggregator registers
	// again by itself, and the hub's paths find their way back.
	eventually(t, "registered again", 10*time.Second, registered(s))
	eventually(t, "fetch after the rendezvous came back", 10*time.Second, wantPage(h))
}

func TestViaRejectsPublicAndTLS(t *testing.T) {
	r := startRendezvous(t, t.TempDir(), "127.0.0.1:0")
	for _, o := range []ServerOptions{{Public: "203.0.113.5"}, {TLS: ":0"}} {
		o.StateDir, o.Listen, o.Via, o.Userspace, o.Admin, o.Log = t.TempDir(), "127.0.0.1:0", r.Code().String(), true, "-", quiet
		if s, err := StartServer(o); err == nil {
			s.Close()
			t.Fatalf("started with %+v", o)
		}
	}
}

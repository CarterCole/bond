package bond

import (
	"encoding/binary"
	"io"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/conn"

	"github.com/CarterCole/bond/core/path"
	"github.com/CarterCole/bond/core/sched"
	"github.com/CarterCole/bond/core/sim"
	"github.com/CarterCole/bond/core/wire"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

var testKey = [16]byte{1, 2, 3, 4, 5, 6, 7, 8}

const testSession = 0x00c0ffee

type pair struct {
	t        *testing.T
	hub, srv *Mux
	hs, ss   *Session
	paths    map[uint8]*sim.Path
	tap      atomic.Pointer[func(b []byte)] // sees every hub→aggregator datagram
}

func newPair(t *testing.T) *pair {
	p := &pair{t: t, paths: map[uint8]*sim.Path{}}
	p.hub = New(Options{Role: Hub, Gen: 100, Log: quiet})
	p.srv = New(Options{Role: Server, Gen: 200, Log: quiet})
	p.hs = p.hub.AddSession(testSession, testKey, "hub")
	p.ss = p.srv.AddSession(testSession, testKey, "hub")
	t.Cleanup(func() {
		p.hub.Close()
		p.srv.Close()
		for _, sp := range p.paths {
			sp.Close()
		}
	})
	return p
}

func (p *pair) srvRecv(b *path.Buf, from path.Remote) {
	if f := p.tap.Load(); f != nil {
		(*f)(b.Bytes())
	}
	p.srv.Receive(b, from)
}

func (p *pair) addPath(id uint8, policy sched.Policy, up, down sim.Link) *sim.Path {
	addr := netip.AddrPortFrom(netip.AddrFrom4([4]byte{10, 0, 0, id + 1}), 4000)
	// Read p.hub at delivery time so a test can swap in a restarted hub.
	sp := sim.NewPath(string(rune('A'+id)), up, down,
		func(b *path.Buf, from path.Remote) { p.hub.Receive(b, from) }, p.srvRecv, addr)
	p.paths[id] = sp
	p.hs.AddPath(id, sp.Name, "sim", policy, sp.HubRemote())
	return sp
}

func upCount(s *Session) int {
	n := 0
	for _, ps := range s.Status().Paths {
		if ps.Up {
			n++
		}
	}
	return n
}

func (p *pair) waitUp(n int) {
	p.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if upCount(p.hs) == n && upCount(p.ss) == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	p.t.Fatalf("want %d paths up on both ends; hub has %d, aggregator %d", n, upCount(p.hs), upCount(p.ss))
}

// sink drains what a Mux hands to WireGuard and records the test sequence
// number at the front of each datagram.
type sink struct {
	mu       sync.Mutex
	seqs     []uint32
	at       []time.Time
	disorder int
	max      uint32
	any      bool
}

func collect(t *testing.T, m *Mux) *sink {
	t.Helper()
	fns, _, err := m.Bind().Open(0)
	if err != nil {
		t.Fatal(err)
	}
	s := &sink{}
	go func() {
		bufs := [][]byte{make([]byte, 2048)}
		sizes := make([]int, 1)
		eps := make([]conn.Endpoint, 1)
		for {
			n, err := fns[0](bufs, sizes, eps)
			if err != nil {
				return
			}
			now := time.Now()
			s.mu.Lock()
			for i := 0; i < n; i++ {
				seq := binary.BigEndian.Uint32(bufs[i][:4])
				if s.any && seq < s.max {
					s.disorder++
				}
				if !s.any || seq > s.max {
					s.max, s.any = seq, true
				}
				s.seqs = append(s.seqs, seq)
				s.at = append(s.at, now)
			}
			s.mu.Unlock()
		}
	}()
	t.Cleanup(func() { m.Bind().Close() })
	return s
}

func (s *sink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seqs)
}

// countRange is how many distinct datagrams with lo <= seq < hi arrived.
func (s *sink) countRange(lo, hi uint32) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[uint32]bool{}
	for _, q := range s.seqs {
		if q >= lo && q < hi {
			seen[q] = true
		}
	}
	return len(seen)
}

// pump sends n datagrams of size bytes at the given bit rate.
func pump(s *Session, class wire.Class, first uint32, n int, size int, bps float64) {
	payload := make([]byte, size)
	gap := time.Duration(float64(size*8) / bps * float64(time.Second))
	start := time.Now()
	for i := 0; i < n; i++ {
		binary.BigEndian.PutUint32(payload, first+uint32(i))
		s.Send(payload, class)
		if d := time.Until(start.Add(time.Duration(i+1) * gap)); d > 0 {
			time.Sleep(d)
		}
	}
}

func TestBringUpNeedsRoundTrip(t *testing.T) {
	p := newPair(t)
	p.addPath(0, sched.PolicyPrimary, sim.Link{Delay: 5 * time.Millisecond}, sim.Link{Delay: 5 * time.Millisecond})
	p.waitUp(1)
	st := p.ss.Status().Paths[0]
	if st.Name != "A" || st.Kind != "sim" || st.Policy != "primary" {
		t.Fatalf("aggregator did not learn path metadata: %+v", st)
	}
	time.Sleep(600 * time.Millisecond)
	if rtt := p.hs.Status().Paths[0].RTTMs; rtt < 8 || rtt > 30 {
		t.Fatalf("hub measured rtt %.1fms on a 10ms path", rtt)
	}
}

func TestOrderedDeliveryAcrossUnequalPaths(t *testing.T) {
	p := newPair(t)
	// A low-latency path too small for the stream and a roomy one 35ms
	// further away: traffic has to be split, so it arrives interleaved.
	near := sim.Link{Delay: 5 * time.Millisecond, RateBps: 3e6}
	far := sim.Link{Delay: 40 * time.Millisecond, RateBps: 20e6}
	p.addPath(0, sched.PolicyBonded, near, near)
	p.addPath(1, sched.PolicyBonded, far, far)
	p.waitUp(2)
	out := collect(t, p.srv)

	const size, bps = 1000, 6e6
	n := int(bps / (size * 8) * 5)
	warm := n / 5 * 2
	pump(p.hs, wire.ClassBulk, 0, n, size, bps)
	time.Sleep(400 * time.Millisecond)

	a, b := p.paths[0].UpBytes(), p.paths[1].UpBytes()
	if a < 500_000 || b < 500_000 {
		t.Fatalf("stream was not split: paths carried %d / %d bytes", a, b)
	}
	got := out.countRange(uint32(warm), uint32(n))
	// Count order violations after warm-up only.
	out.mu.Lock()
	dis, hi := 0, uint32(0)
	for _, q := range out.seqs {
		if q < uint32(warm) {
			continue
		}
		if q < hi {
			dis++
		} else {
			hi = q
		}
	}
	out.mu.Unlock()
	st := p.ss.Status()
	t.Logf("paths carried %d / %d bytes; after warm-up %d of %d delivered, %d out of order; hold %.1fms, skipped %d",
		a, b, got, n-warm, dis, st.HoldMs, st.Skipped)
	if float64(got) < 0.97*float64(n-warm) {
		t.Fatalf("delivered %d of %d", got, n-warm)
	}
	if dis > (n-warm)/50 {
		t.Fatalf("%d of %d datagrams delivered out of order", dis, n-warm)
	}
}

func TestAggregatesCapacity(t *testing.T) {
	p := newPair(t)
	// Three unequal paths, none of them at the estimator's starting guess.
	links := []sim.Link{
		{Delay: 10 * time.Millisecond, RateBps: 5e6},
		{Delay: 25 * time.Millisecond, RateBps: 1.2e6},
		{Delay: 40 * time.Millisecond, RateBps: 3.3e6},
	}
	for i, l := range links {
		p.addPath(uint8(i), sched.PolicyBonded, l, l)
	}
	p.waitUp(3)
	out := collect(t, p.srv)

	// Offer 8.5 Mbit/s for 6s: 1.7x the best path, 90% of all three together.
	const size, bps = 1000, 8.5e6
	n := int(bps / (size * 8) * 6)
	warm := n / 6 * 2 // the estimator gets 2s to find the capacities
	pump(p.hs, wire.ClassBulk, 0, n, size, bps)
	time.Sleep(400 * time.Millisecond)

	got := out.countRange(uint32(warm), uint32(n))
	frac := float64(got) / float64(n-warm)
	st := p.hs.Status()
	t.Logf("delivered %.1f%% after warm-up; capacity estimates %.2f / %.2f / %.2f Mbit/s (true 5 / 1.2 / 3.3); carried %d / %d / %d bytes",
		frac*100, st.Paths[0].CapacityBps/1e6, st.Paths[1].CapacityBps/1e6, st.Paths[2].CapacityBps/1e6,
		p.paths[0].UpBytes(), p.paths[1].UpBytes(), p.paths[2].UpBytes())
	// The best single path could deliver at most 5/8.5 = 59%.
	if frac < 0.90 {
		t.Fatalf("delivered %.1f%% of an 8.5 Mbit/s stream over 5+1.2+3.3 Mbit/s paths", frac*100)
	}
}

func TestFailoverWhenPathDies(t *testing.T) {
	p := newPair(t)
	l := sim.Link{Delay: 10 * time.Millisecond}
	p.addPath(0, sched.PolicyBonded, l, l)
	p.addPath(1, sched.PolicyBonded, sim.Link{Delay: 15 * time.Millisecond}, sim.Link{Delay: 15 * time.Millisecond})
	p.waitUp(2)
	out := collect(t, p.srv)

	const size, bps = 1000, 2e6
	perSec := int(bps / (size * 8))
	done := make(chan struct{})
	go func() { pump(p.hs, wire.ClassBulk, 0, 3*perSec, size, bps); close(done) }()
	time.Sleep(time.Second)
	p.paths[0].SetDown(true)
	<-done
	time.Sleep(300 * time.Millisecond)

	// Everything sent from one second after the cut must arrive.
	lo, hi := uint32(2*perSec), uint32(3*perSec)
	if got := out.countRange(lo, hi); got != perSec {
		t.Fatalf("after failover delivered %d of %d", got, perSec)
	}
	lost := 3*perSec - out.countRange(0, hi)
	t.Logf("lost %d datagrams (%.0fms of traffic) across the failure", lost, float64(lost)/float64(perSec)*1000)
	if lost > perSec*8/10 {
		t.Fatalf("lost %d datagrams; failure detection is too slow", lost)
	}
	for _, ps := range p.hs.Status().Paths {
		if ps.ID == 0 && ps.Up {
			t.Fatal("hub still thinks the dead path is up")
		}
	}

	// And it comes back by itself.
	p.paths[0].SetDown(false)
	p.waitUp(2)
}

func TestRealtimeSurvivesLossyPaths(t *testing.T) {
	p := newPair(t)
	l := sim.Link{Delay: 10 * time.Millisecond, Loss: 0.25}
	p.addPath(0, sched.PolicyBonded, l, l)
	p.addPath(1, sched.PolicyBonded, l, l)
	p.waitUp(2)
	out := collect(t, p.srv)

	const n = 2000
	pump(p.hs, wire.ClassRealtime, 0, n, 600, 2e6)
	time.Sleep(200 * time.Millisecond)
	rt := float64(out.countRange(0, n)) / n

	pump(p.hs, wire.ClassBulk, 10_000, n, 600, 2e6)
	time.Sleep(200 * time.Millisecond)
	bulk := float64(out.countRange(10_000, 10_000+n)) / n

	t.Logf("25%% loss on both paths: realtime %.1f%% delivered, bulk %.1f%%", rt*100, bulk*100)
	// Two independent copies: expect about 1 - 0.25² = 94%.
	if rt < 0.90 {
		t.Fatalf("realtime delivered only %.1f%%", rt*100)
	}
	if bulk > rt-0.08 {
		t.Fatalf("redundancy made no difference: realtime %.1f%% vs bulk %.1f%%", rt*100, bulk*100)
	}
}

func TestBackupPathStaysIdleUntilNeeded(t *testing.T) {
	p := newPair(t)
	l := sim.Link{Delay: 10 * time.Millisecond}
	p.addPath(0, sched.PolicyPrimary, l, l)
	p.addPath(1, sched.PolicyBackup, l, l)
	p.waitUp(2)
	out := collect(t, p.srv)

	pump(p.hs, wire.ClassBulk, 0, 500, 1000, 4e6)
	time.Sleep(100 * time.Millisecond)
	if b := p.paths[1].UpBytes(); b > 20_000 {
		t.Fatalf("backup path carried %d bytes while the primary was healthy", b)
	}
	if got := out.countRange(0, 500); got != 500 {
		t.Fatalf("delivered %d of 500 on the primary", got)
	}

	p.paths[0].SetDown(true)
	time.Sleep(1200 * time.Millisecond) // idle path: detection rides on probes
	pump(p.hs, wire.ClassBulk, 1000, 500, 1000, 4e6)
	time.Sleep(100 * time.Millisecond)
	if got := out.countRange(1000, 1500); got != 500 {
		t.Fatalf("delivered %d of 500 over the backup", got)
	}
}

func TestPeerRestartResyncs(t *testing.T) {
	p := newPair(t)
	l := sim.Link{Delay: 5 * time.Millisecond}
	p.addPath(0, sched.PolicyBonded, l, l)
	p.waitUp(1)
	out := collect(t, p.srv)
	pump(p.hs, wire.ClassBulk, 0, 2000, 500, 8e6)
	time.Sleep(100 * time.Millisecond)
	if got := out.countRange(0, 2000); got != 2000 {
		t.Fatalf("before restart: %d of 2000", got)
	}

	// The hub process dies and comes back: sequence numbers start over.
	p.hub.Close()
	p.hub = New(Options{Role: Hub, Gen: 101, Log: quiet})
	p.hs = p.hub.AddSession(testSession, testKey, "hub")
	p.hs.AddPath(0, "A", "sim", sched.PolicyBonded, p.paths[0].HubRemote())
	p.waitUp(1)
	pump(p.hs, wire.ClassBulk, 5000, 200, 500, 2e6)
	time.Sleep(100 * time.Millisecond)
	if got := out.countRange(5000, 5200); got != 200 {
		t.Fatalf("after hub restart: %d of 200", got)
	}
}

func TestRejectsForgedAndReflected(t *testing.T) {
	p := newPair(t)
	l := sim.Link{}
	p.addPath(0, sched.PolicyBonded, l, l)
	p.waitUp(1)
	out := collect(t, p.srv)

	var captured [][]byte
	var mu sync.Mutex
	tap := func(b []byte) {
		mu.Lock()
		captured = append(captured, append([]byte(nil), b...))
		mu.Unlock()
	}
	p.tap.Store(&tap)
	pump(p.hs, wire.ClassBulk, 0, 20, 200, 1e6)
	time.Sleep(50 * time.Millisecond)
	p.tap.Store(nil)
	if out.count() != 20 {
		t.Fatalf("baseline: %d of 20", out.count())
	}
	mu.Lock()
	defer mu.Unlock()

	inject := func(m *Mux, raw []byte) {
		b := path.Get()
		b.N = copy(b.B[:], raw)
		m.Receive(b, path.Remote{S: evil{}, Addr: netip.MustParseAddrPort("203.0.113.66:6666")})
	}
	var data []byte
	for _, c := range captured {
		if h, _ := wire.Parse(c); h.Type == wire.TypeData {
			data = c
		}
	}

	before := p.srv.RxUnknown.Load()
	// Flipped payload bit: tag fails.
	bad := append([]byte(nil), data...)
	bad[len(bad)-1] ^= 1
	inject(p.srv, bad)
	// A hub→aggregator datagram bounced back at the hub: wrong direction.
	hubBefore := p.hub.RxUnknown.Load()
	inject(p.hub, data)
	// Unknown session.
	other := append([]byte(nil), data...)
	other[4] ^= 0xff
	inject(p.srv, other)
	if p.srv.RxUnknown.Load()-before != 2 || p.hub.RxUnknown.Load()-hubBefore != 1 {
		t.Fatalf("forgeries not all rejected: srv +%d hub +%d", p.srv.RxUnknown.Load()-before, p.hub.RxUnknown.Load()-hubBefore)
	}

	// Replaying genuine old traffic from another address must neither move
	// the path, nor deliver anything twice, nor revive a dead path.
	remote := p.ss.Status().Paths[0].Remote
	for _, c := range captured {
		inject(p.srv, c)
	}
	if got := p.ss.Status().Paths[0].Remote; got != remote {
		t.Fatalf("replay moved the path from %s to %s", remote, got)
	}
	if out.count() != 20 {
		t.Fatalf("replay delivered duplicates: %d", out.count())
	}
	p.paths[0].SetDown(true)
	deadline := time.Now().Add(4 * time.Second)
	for upCount(p.ss) != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	for range 5 {
		for _, c := range captured {
			inject(p.srv, c)
		}
		time.Sleep(30 * time.Millisecond)
	}
	if upCount(p.ss) != 0 {
		t.Fatal("replayed traffic revived a dead path")
	}
}

type evil struct{}

func (evil) SendTo([]byte, netip.AddrPort) error { return nil }

func TestBindClassifiesWireGuardMessages(t *testing.T) {
	p := newPair(t)
	l := sim.Link{}
	p.addPath(0, sched.PolicyBonded, l, l)
	p.addPath(1, sched.PolicyBonded, l, l)
	p.waitUp(2)

	var mu sync.Mutex
	seen := map[int][]wire.Header{} // by payload length
	tap := func(b []byte) {
		if h, _ := wire.Parse(b); h.Type == wire.TypeData {
			mu.Lock()
			seen[len(b)-wire.HeaderLen] = append(seen[len(b)-wire.HeaderLen], h)
			mu.Unlock()
		}
	}
	p.tap.Store(&tap)

	bind := p.hub.Bind()
	ep, err := bind.ParseEndpoint(EndpointString(testSession))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bind.ParseEndpoint("192.0.2.1:51820"); err == nil {
		t.Fatal("accepted an IP endpoint")
	}
	handshake := make([]byte, 148)
	handshake[0] = wgInitiation
	keepalive := make([]byte, wgKeepalive)
	keepalive[0] = wgTransport
	video := make([]byte, WGLen(1200, 1280))
	video[0] = wgTransport
	web := make([]byte, WGLen(900, 1280))
	web[0] = wgTransport

	p.hs.Classes.Push(len(video), wire.ClassRealtime)
	p.hs.Classes.Push(len(web), wire.ClassBulk)
	if err := bind.Send([][]byte{handshake, keepalive, video, web}, ep); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	check := func(name string, n int, class wire.Class, copies int) {
		t.Helper()
		hs := seen[n]
		if len(hs) != copies {
			t.Fatalf("%s: %d copies on the wire, want %d", name, len(hs), copies)
		}
		for _, h := range hs {
			if h.Class != class {
				t.Fatalf("%s: class %v, want %v", name, h.Class, class)
			}
		}
	}
	check("handshake", len(handshake), wire.ClassControl, 2)
	check("keepalive", len(keepalive), wire.ClassControl, 2)
	check("video", len(video), wire.ClassRealtime, 2)
	check("web", len(web), wire.ClassBulk, 1)
}

func TestClassQueue(t *testing.T) {
	var q ClassQueue
	if q.Pop(100) != wire.ClassBulk {
		t.Fatal("empty queue should give the default")
	}
	q.Push(100, wire.ClassRealtime)
	q.Push(200, wire.ClassBulk)
	q.Push(300, wire.ClassRealtime)
	// WireGuard dropped the 100: the 200 still lines up, and the stale hint goes with it.
	if q.Pop(200) != wire.ClassBulk || q.Pop(300) != wire.ClassRealtime {
		t.Fatal("did not resynchronise after a dropped datagram")
	}
	if q.Pop(100) != wire.ClassBulk {
		t.Fatal("stale hint survived")
	}
	q.SetDefault(wire.ClassRealtime)
	if q.Pop(999) != wire.ClassRealtime {
		t.Fatal("default class not applied")
	}
	for i := 0; i < 3*classRing; i++ { // overflow keeps the newest
		q.Push(i%1000+40, wire.ClassBulk)
	}
	q.Push(7, wire.ClassRealtime)
	q.SetDefault(wire.ClassBulk)
	for i := 0; i < classRing; i++ {
		if q.Pop(7) == wire.ClassRealtime {
			return
		}
		q.Pop((3*classRing-classRing+i)%1000 + 40)
	}
	t.Fatal("newest hint lost on overflow")
}

func TestWGLen(t *testing.T) {
	for _, c := range []struct{ n, mtu, want int }{
		{0, 1280, 32}, {1, 1280, 48}, {16, 1280, 48}, {17, 1280, 64},
		{1279, 1280, 32 + 1280}, {1280, 1280, 32 + 1280}, {1275, 1280, 32 + 1280}, {100, 0, 32 + 112},
	} {
		if got := WGLen(c.n, c.mtu); got != c.want {
			t.Errorf("WGLen(%d, %d) = %d, want %d", c.n, c.mtu, got, c.want)
		}
	}
}

// A path must be usable after one round trip, not after the next idle probe:
// that is the difference between a tethered phone helping within a blink and
// a quarter of a second of nothing.
func TestBringUpTakesOneRoundTrip(t *testing.T) {
	for _, d := range []time.Duration{0, 2 * time.Millisecond, 20 * time.Millisecond} {
		p := newPair(t)
		l := sim.Link{Delay: d}
		start := time.Now()
		p.addPath(0, sched.PolicyBonded, l, l)
		var hubAt, srvAt time.Duration
		for time.Since(start) < 2*time.Second && (hubAt == 0 || srvAt == 0) {
			if hubAt == 0 && upCount(p.hs) == 1 {
				hubAt = time.Since(start)
			}
			if srvAt == 0 && upCount(p.ss) == 1 {
				srvAt = time.Since(start)
			}
			time.Sleep(200 * time.Microsecond)
		}
		t.Logf("one-way delay %v: hub up after %v, aggregator after %v", d, hubAt.Round(100*time.Microsecond), srvAt.Round(100*time.Microsecond))
		// Hub: one round trip. Aggregator: half a trip more. Allow generous scheduling slack.
		limit := 4*d + 60*time.Millisecond
		if hubAt == 0 || srvAt == 0 || hubAt > limit || srvAt > limit {
			t.Fatalf("one-way delay %v: bring-up took hub %v / aggregator %v (limit %v)", d, hubAt, srvAt, limit)
		}
	}
}

// WireGuard's first handshake may be handed to the bond before any uplink
// is open. It must leave on the first path that appears.
func TestHandshakeBeforeFirstPathIsNotLost(t *testing.T) {
	p := newPair(t)
	out := collect(t, p.srv)
	hs := make([]byte, 148)
	hs[0] = wgInitiation
	binary.BigEndian.PutUint32(hs[4:], 0) // collect() reads a sequence number from the front
	p.hs.Send(hs, wire.ClassControl)
	if d := p.hs.Status().TxDropped; d != 0 {
		t.Fatalf("handshake counted as dropped (%d)", d)
	}
	time.Sleep(50 * time.Millisecond)
	l := sim.Link{Delay: time.Millisecond}
	p.addPath(0, sched.PolicyBonded, l, l)
	deadline := time.Now().Add(time.Second)
	for out.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if out.count() != 1 {
		t.Fatalf("aggregator received %d handshakes, want 1", out.count())
	}
}

// An uplink that goes away and comes back — a tether replugged, a phone
// changing WiFi — gets a new socket and fresh sequence numbers under the same
// path id. The aggregator must take it for the new path it is, at once, and
// not for old traffic arriving late (which it would ignore until the old
// entry expired, most of a minute later).
func TestRecreatedPathWithSameIDComesUpAtOnce(t *testing.T) {
	p := newPair(t)
	l := sim.Link{Delay: 2 * time.Millisecond}
	p.addPath(0, sched.PolicyBonded, l, l)
	p.waitUp(1)
	out := collect(t, p.srv)
	pump(p.hs, wire.ClassBulk, 0, 300, 500, 4e6) // run the old path's sequence numbers up
	time.Sleep(50 * time.Millisecond)
	if got := out.countRange(0, 300); got != 300 {
		t.Fatalf("before: %d of 300", got)
	}
	oldRemote := p.ss.Status().Paths[0].Remote

	// The uplink is replaced: a new simulated socket, same path id.
	p.paths[0].SetDown(true)
	fresh := sim.NewPath("A2", l, l, func(b *path.Buf, from path.Remote) { p.hub.Receive(b, from) }, p.srvRecv,
		netip.MustParseAddrPort("10.0.0.99:4999"))
	t.Cleanup(fresh.Close)
	start := time.Now()
	p.hs.AddPath(0, "A", "sim", sched.PolicyBonded, fresh.HubRemote())

	deadline := time.Now().Add(2 * time.Second)
	for (upCount(p.hs) != 1 || upCount(p.ss) != 1 || p.ss.Status().Paths[0].Remote == oldRemote) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	took := time.Since(start)
	st := p.ss.Status().Paths[0]
	if !st.Up || st.Remote == oldRemote {
		t.Fatalf("aggregator did not adopt the re-created path: up=%v remote=%s (was %s)", st.Up, st.Remote, oldRemote)
	}
	if took > 300*time.Millisecond {
		t.Fatalf("re-created path took %v to come up", took)
	}
	t.Logf("re-created path up on both ends after %v", took.Round(time.Millisecond))

	pump(p.hs, wire.ClassBulk, 1000, 200, 500, 4e6)
	time.Sleep(50 * time.Millisecond)
	if got := out.countRange(1000, 1200); got != 200 {
		t.Fatalf("after: %d of 200 delivered over the re-created path", got)
	}
}

// Package sim provides impaired in-process links so the bond can be tested
// against slow, lossy, laggy and dying paths without a network.
package sim

import (
	"math/rand"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/CarterCole/bond/core/path"
)

// Link describes one direction of a path.
type Link struct {
	Delay   time.Duration
	Jitter  time.Duration // uniform, added to Delay; order within the link is preserved
	Loss    float64       // 0..1, independent per datagram
	RateBps float64       // bits/s; 0 = unlimited
	Queue   time.Duration // bottleneck buffer depth; default 100ms
}

type item struct {
	b  *path.Buf
	at time.Time
}

// pipe is one direction: a rate-limited tail-drop queue followed by a delay line.
type pipe struct {
	mu      sync.Mutex
	cfg     Link
	down    bool
	rng     *rand.Rand
	depart  time.Time // when the bottleneck is next free
	arrival time.Time // last scheduled arrival, to keep order under jitter
	ch      chan item
	deliver func(b *path.Buf)

	Sent, Dropped atomic.Uint64
	Bytes         atomic.Uint64
}

func newPipe(cfg Link, seed int64, deliver func(b *path.Buf), stop <-chan struct{}) *pipe {
	p := &pipe{cfg: cfg, rng: rand.New(rand.NewSource(seed)), ch: make(chan item, 1<<15), deliver: deliver}
	go func() {
		for {
			select {
			case <-stop:
				return
			case it := <-p.ch:
				if d := time.Until(it.at); d > 0 {
					time.Sleep(d)
				}
				p.deliver(it.b)
			}
		}
	}()
	return p
}

func (p *pipe) send(data []byte) {
	now := time.Now()
	p.mu.Lock()
	cfg := p.cfg
	if p.down || (cfg.Loss > 0 && p.rng.Float64() < cfg.Loss) {
		p.mu.Unlock()
		p.Dropped.Add(1)
		return
	}
	depart := now
	if cfg.RateBps > 0 {
		q := cfg.Queue
		if q == 0 {
			q = 100 * time.Millisecond
		}
		if p.depart.After(now) {
			depart = p.depart
		}
		if depart.Sub(now) > q {
			p.mu.Unlock()
			p.Dropped.Add(1)
			return
		}
		depart = depart.Add(time.Duration(float64(len(data)*8) / cfg.RateBps * float64(time.Second)))
		p.depart = depart
	}
	at := depart.Add(cfg.Delay)
	if cfg.Jitter > 0 {
		at = at.Add(time.Duration(p.rng.Int63n(int64(cfg.Jitter))))
	}
	if at.Before(p.arrival) {
		at = p.arrival
	}
	p.arrival = at
	p.mu.Unlock()

	b := path.Get()
	b.N = copy(b.B[:], data)
	select {
	case p.ch <- item{b, at}:
		p.Sent.Add(1)
		p.Bytes.Add(uint64(len(data)))
	default:
		path.Put(b)
		p.Dropped.Add(1)
	}
}

// Path is a simulated two-way path between a hub and an aggregator.
type Path struct {
	Name string
	up   *pipe // hub → aggregator
	dn   *pipe // aggregator → hub
	hub  hubEnd
	srv  srvEnd
	stop chan struct{}
}

type hubEnd struct{ p *Path }
type srvEnd struct{ p *Path }

func (e *hubEnd) SendTo(b []byte, _ netip.AddrPort) error { e.p.up.send(b); return nil }
func (e *srvEnd) SendTo(b []byte, _ netip.AddrPort) error { e.p.dn.send(b); return nil }
func (e *hubEnd) String() string                          { return "sim:" + e.p.Name }
func (e *srvEnd) String() string                          { return "sim:" + e.p.Name }

var seed atomic.Int64

// NewPath wires a simulated path between two handlers. addr is the source
// address the aggregator sees the hub's datagrams arrive from.
func NewPath(name string, up, down Link, hubRecv, srvRecv path.Handler, addr netip.AddrPort) *Path {
	p := &Path{Name: name, stop: make(chan struct{})}
	p.hub.p, p.srv.p = p, p
	p.up = newPipe(up, seed.Add(1), func(b *path.Buf) { srvRecv(b, path.Remote{S: &p.srv, Addr: addr}) }, p.stop)
	p.dn = newPipe(down, seed.Add(1), func(b *path.Buf) { hubRecv(b, path.Remote{S: &p.hub}) }, p.stop)
	return p
}

// HubRemote is what the hub sends to in order to use this path.
func (p *Path) HubRemote() path.Remote { return path.Remote{S: &p.hub} }

// SetDown cuts (or restores) the path in both directions.
func (p *Path) SetDown(down bool) {
	for _, q := range []*pipe{p.up, p.dn} {
		q.mu.Lock()
		q.down = down
		q.mu.Unlock()
	}
}

// Set changes the impairment of both directions.
func (p *Path) Set(up, down Link) {
	p.up.mu.Lock()
	p.up.cfg = up
	p.up.mu.Unlock()
	p.dn.mu.Lock()
	p.dn.cfg = down
	p.dn.mu.Unlock()
}

// UpBytes is how many bytes the hub→aggregator direction has carried.
func (p *Path) UpBytes() uint64 { return p.up.Bytes.Load() }

// DownBytes is how many bytes the aggregator→hub direction has carried.
func (p *Path) DownBytes() uint64 { return p.dn.Bytes.Load() }

func (p *Path) Close() { close(p.stop) }

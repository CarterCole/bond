// Package relay is "share my connection": a device lends its uplink to a hub.
//
// A hub on the same LAN (shared WiFi, hotspot, USB) sends bond datagrams to
// the relay, which forwards them to the aggregator over its own uplink — a
// phone's cellular connection, typically — and passes the replies back. The
// datagrams are already WireGuard-encrypted end to end between hub and
// aggregator, so the relay can read none of it, and it will only ever
// forward to the aggregator it was told about, so it is of no use to anyone
// as an open proxy.
package relay

import (
	"errors"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/CarterCole/bond/core/path"
	"github.com/CarterCole/bond/core/wire"
)

// DefaultPort is where relays listen for hubs.
const DefaultPort = 7778

type Options struct {
	// Listen is the UDP address hubs send to. Default ":7778".
	Listen string
	// Upstream is the aggregator's address.
	Upstream netip.AddrPort
	// Bind says which uplink carries the forwarded traffic: an interface
	// name on a desktop, a logical network ("cellular") on a phone.
	Bind path.BindOpts
	// LAN is handed to the platform socket hook for the hub-facing socket.
	// A phone that is also running the VPN must keep that socket out of it.
	LAN path.BindOpts
	// Idle is how long a hub may be silent before its mapping is dropped.
	Idle time.Duration
	// MaxClients bounds the number of hubs served at once.
	MaxClients int
	Log        *slog.Logger
}

// Stats is a snapshot of a relay's activity.
type Stats struct {
	Clients   int    `json:"clients"`
	UpBytes   uint64 `json:"up_bytes"`   // hub → aggregator
	DownBytes uint64 `json:"down_bytes"` // aggregator → hub
	UpPkts    uint64 `json:"up_packets"`
	DownPkts  uint64 `json:"down_packets"`
	Refused   uint64 `json:"refused"` // datagrams that were not a hub's to forward
	Port      int    `json:"port"`
	Upstream  string `json:"upstream"`
}

type Relay struct {
	o        Options
	lan      *path.UDP
	upstream atomic.Pointer[netip.AddrPort]

	mu      sync.Mutex
	clients map[netip.AddrPort]*client
	closed  bool
	done    chan struct{}

	upBytes, downBytes, upPkts, downPkts, refused atomic.Uint64
	wg                                            sync.WaitGroup
}

// client is one hub socket's mapping to its own upstream socket, exactly
// like a NAT binding: the aggregator tells hubs' paths apart by source port.
type client struct {
	addr netip.AddrPort
	up   *path.UDP
	last atomic.Int64
}

func Start(o Options) (*Relay, error) {
	if o.Listen == "" {
		o.Listen = ":7778"
	}
	if o.Idle == 0 {
		o.Idle = 60 * time.Second
	}
	if o.MaxClients == 0 {
		o.MaxClients = 64
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if !o.Upstream.IsValid() {
		return nil, errors.New("relay: no aggregator address")
	}
	// The LAN-facing socket is not pinned to an uplink: hubs reach it over
	// whatever local network they share with us.
	lan, err := path.ListenUDP("udp", o.Listen, o.LAN)
	if err != nil {
		return nil, err
	}
	r := &Relay{o: o, lan: lan, clients: make(map[netip.AddrPort]*client), done: make(chan struct{})}
	r.upstream.Store(&o.Upstream)
	r.wg.Add(2)
	go r.serveLAN()
	go r.reap()
	return r, nil
}

// Port is the UDP port hubs should send to.
func (r *Relay) Port() int { return int(r.lan.LocalAddr().Port()) }

// Rebind drops every hub's mapping, so that each is rebuilt on a fresh
// uplink socket the next time that hub sends. Call it when the uplink
// network has been replaced: the old sockets are attached to a network that
// no longer exists. Hubs see their path roam and carry on.
func (r *Relay) Rebind() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for a, c := range r.clients {
		c.up.Close()
		delete(r.clients, a)
	}
}

// SetUpstream changes the aggregator address (after a DNS change, say).
func (r *Relay) SetUpstream(a netip.AddrPort) { r.upstream.Store(&a) }

func (r *Relay) Stats() Stats {
	r.mu.Lock()
	n := len(r.clients)
	r.mu.Unlock()
	return Stats{
		Clients: n, UpBytes: r.upBytes.Load(), DownBytes: r.downBytes.Load(),
		UpPkts: r.upPkts.Load(), DownPkts: r.downPkts.Load(), Refused: r.refused.Load(),
		Port: r.Port(), Upstream: r.upstream.Load().String(),
	}
}

func (r *Relay) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	close(r.done)
	for _, c := range r.clients {
		c.up.Close()
	}
	r.mu.Unlock()
	r.lan.Close()
	r.wg.Wait()
}

// local reports whether a is the kind of address a hub next to us would
// have: private, link-local, carrier-grade NAT (some hotspots) or loopback.
// Nothing arriving from the public Internet gets forwarded.
func local(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLoopback() || cgnat.Contains(a)
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// hubDatagram reports whether b looks like a bond datagram travelling from
// a hub to an aggregator.
func hubDatagram(b []byte) bool {
	h, err := wire.Parse(b)
	return err == nil && h.Flags&wire.FlagFromServer == 0
}

func (r *Relay) serveLAN() {
	defer r.wg.Done()
	r.lan.Serve(func(b *path.Buf, src path.Remote) {
		defer path.Put(b)
		from := src.Addr
		if !local(from.Addr()) || !hubDatagram(b.Bytes()) {
			r.refused.Add(1)
			return
		}
		c := r.clientFor(from)
		if c == nil {
			r.refused.Add(1)
			return
		}
		c.last.Store(time.Now().UnixNano())
		if err := c.up.SendTo(b.Bytes(), *r.upstream.Load()); err == nil {
			r.upPkts.Add(1)
			r.upBytes.Add(uint64(b.N))
		}
	})
}

func (r *Relay) clientFor(from netip.AddrPort) *client {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.clients[from]; c != nil {
		return c
	}
	if r.closed || len(r.clients) >= r.o.MaxClients {
		return nil
	}
	network := "udp4"
	if r.upstream.Load().Addr().Is6() {
		network = "udp6"
	}
	up, err := path.ListenUDP(network, ":0", r.o.Bind)
	if err != nil {
		r.o.Log.Warn("relay: cannot open uplink socket", "err", err)
		return nil
	}
	c := &client{addr: from, up: up}
	r.clients[from] = c
	r.o.Log.Info("relay: serving hub", "hub", from.String())
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		up.Serve(func(b *path.Buf, src path.Remote) {
			defer path.Put(b)
			// Only the aggregator may talk back through this mapping.
			if src.Addr != *r.upstream.Load() {
				r.refused.Add(1)
				return
			}
			if err := r.lan.SendTo(b.Bytes(), c.addr); err == nil {
				r.downPkts.Add(1)
				r.downBytes.Add(uint64(b.N))
			}
		})
	}()
	return c
}

func (r *Relay) reap() {
	defer r.wg.Done()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-t.C:
		}
		cutoff := time.Now().Add(-r.o.Idle).UnixNano()
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return
		}
		for a, c := range r.clients {
			if c.last.Load() < cutoff {
				c.up.Close()
				delete(r.clients, a)
				r.o.Log.Info("relay: hub went quiet", "hub", a.String())
			}
		}
		r.mu.Unlock()
	}
}

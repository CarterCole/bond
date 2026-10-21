// Package tunnel runs WireGuard over a bond: it owns the wireguard-go device,
// the TUN (kernel or userspace) it reads packets from, and the glue that
// tells the bond which class each packet belongs to.
package tunnel

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"sync"

	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"

	"github.com/CarterCole/bond/core/bond"
	"github.com/CarterCole/bond/core/classify"
)

// DefaultMTU fits a bonded WireGuard datagram inside 1500 bytes on any
// IPv4 or IPv6 path with room for PPPoE, CLAT and similar path overheads:
// 1280 + 32 (WireGuard) + 28 (bond) + 48 (IPv6/UDP) = 1388.
const DefaultMTU = 1280

// Peer is the other end of a WireGuard association and the bond session
// that carries it.
type Peer struct {
	PublicKey  [32]byte
	Session    uint32
	AllowedIPs []netip.Prefix
	// Keepalive, in seconds, makes WireGuard handshake straight away and
	// keep the association alive without traffic. A hub sets it so that it
	// is connected (and reachable from the mesh) before anything is sent.
	Keepalive int
}

type Tunnel struct {
	dev *device.Device
	tun *classTun
	mux *bond.Mux
}

// Options configure New.
type Options struct {
	PrivateKey [32]byte
	Classifier *classify.Classifier // nil = everything is bulk unless a session default says otherwise
	Log        *slog.Logger
}

// New starts WireGuard on t, sending and receiving through mux.
// It takes ownership of t.
func New(t tun.Device, mux *bond.Mux, o Options) (*Tunnel, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	mtu, err := t.MTU()
	if err != nil {
		mtu = DefaultMTU
	}
	ct := &classTun{Device: t, mtu: mtu, mux: mux, cls: o.Classifier}
	logger := &device.Logger{
		Verbosef: func(format string, args ...any) { o.Log.Debug("wireguard: " + fmt.Sprintf(format, args...)) },
		Errorf:   func(format string, args ...any) { o.Log.Warn("wireguard: " + fmt.Sprintf(format, args...)) },
	}
	dev := device.NewDevice(ct, mux.Bind(), logger)
	if err := dev.IpcSet("private_key=" + hex.EncodeToString(o.PrivateKey[:]) + "\n"); err != nil {
		dev.Close()
		return nil, fmt.Errorf("wireguard: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("wireguard: %w", err)
	}
	return &Tunnel{dev: dev, tun: ct, mux: mux}, nil
}

// SetPeers adds or updates peers; with replace it also removes all others.
func (t *Tunnel) SetPeers(replace bool, peers []Peer) error {
	var b strings.Builder
	if replace {
		b.WriteString("replace_peers=true\n")
	}
	for _, p := range peers {
		fmt.Fprintf(&b, "public_key=%s\n", hex.EncodeToString(p.PublicKey[:]))
		fmt.Fprintf(&b, "endpoint=%s\n", bond.EndpointString(p.Session))
		b.WriteString("replace_allowed_ips=true\n")
		for _, ip := range p.AllowedIPs {
			fmt.Fprintf(&b, "allowed_ip=%s\n", ip)
		}
		if p.Keepalive > 0 {
			fmt.Fprintf(&b, "persistent_keepalive_interval=%d\n", p.Keepalive)
		}
	}
	if err := t.dev.IpcSet(b.String()); err != nil {
		return fmt.Errorf("wireguard: %w", err)
	}
	t.tun.setRoutes(replace, peers)
	return nil
}

func (t *Tunnel) RemovePeer(pub [32]byte, session uint32) error {
	t.tun.dropRoutes(session)
	return t.dev.IpcSet(fmt.Sprintf("public_key=%s\nremove=true\n", hex.EncodeToString(pub[:])))
}

// Rehandshake throws away the session keys held for a peer and starts a new
// handshake at once. Use it when the peer is known to have restarted and so
// to have forgotten the old keys.
func (t *Tunnel) Rehandshake(pub [32]byte) {
	if p := t.dev.LookupPeer(device.NoisePublicKey(pub)); p != nil {
		p.ExpireCurrentKeypairs()
		p.SendHandshakeInitiation(false)
	}
}

// Status returns WireGuard's own view (handshake times, byte counters) in
// its key=value UAPI format.
func (t *Tunnel) Status() (string, error) { return t.dev.IpcGet() }

func (t *Tunnel) Close() { t.dev.Close() }

// Wait is closed when the device shuts down.
func (t *Tunnel) Wait() <-chan struct{} { return t.dev.Wait() }

// classTun sits between WireGuard and the TUN. For every packet WireGuard is
// about to encrypt it works out the scheduling class and queues that for the
// bond session the packet will leave on.
type classTun struct {
	tun.Device
	mtu int
	mux *bond.Mux
	cls *classify.Classifier

	mu     sync.RWMutex
	hosts  map[netip.Addr]uint32 // /32 and /128 routes: the common case, one lookup
	routes []route               // everything else, longest prefix first
	single uint32                // set when one session takes all traffic (a hub)
	one    bool
}

type route struct {
	prefix  netip.Prefix
	session uint32
}

func (c *classTun) setRoutes(replace bool, peers []Peer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if replace || c.hosts == nil {
		c.hosts = make(map[netip.Addr]uint32)
		c.routes = nil
	}
	for _, p := range peers {
		c.dropLocked(p.Session)
		for _, ip := range p.AllowedIPs {
			if ip.IsSingleIP() {
				c.hosts[ip.Addr()] = p.Session
				continue
			}
			c.routes = append(c.routes, route{ip.Masked(), p.Session})
		}
	}
	// Longest prefix first so a linear scan is a longest-prefix match.
	for i := 1; i < len(c.routes); i++ {
		for j := i; j > 0 && c.routes[j].prefix.Bits() > c.routes[j-1].prefix.Bits(); j-- {
			c.routes[j], c.routes[j-1] = c.routes[j-1], c.routes[j]
		}
	}
	c.one = len(c.hosts) == 0 && len(c.routes) > 0
	for _, r := range c.routes {
		if r.session != c.routes[0].session {
			c.one = false
		}
	}
	if c.one {
		c.single = c.routes[0].session
	}
}

func (c *classTun) dropRoutes(session uint32) {
	c.mu.Lock()
	c.dropLocked(session)
	c.mu.Unlock()
}

func (c *classTun) dropLocked(session uint32) {
	for a, s := range c.hosts {
		if s == session {
			delete(c.hosts, a)
		}
	}
	kept := c.routes[:0]
	for _, r := range c.routes {
		if r.session != session {
			kept = append(kept, r)
		}
	}
	c.routes = kept
}

// sessionFor mirrors WireGuard's own allowed-IPs lookup: which peer, and so
// which bond session, will this packet be encrypted for?
func (c *classTun) sessionFor(pkt []byte) (uint32, bool) {
	var dst netip.Addr
	switch {
	case len(pkt) >= 20 && pkt[0]>>4 == 4:
		dst = netip.AddrFrom4([4]byte(pkt[16:20]))
	case len(pkt) >= 40 && pkt[0]>>4 == 6:
		dst = netip.AddrFrom16([16]byte(pkt[24:40]))
	default:
		return 0, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if s, ok := c.hosts[dst]; ok {
		return s, true
	}
	for _, r := range c.routes {
		if r.prefix.Contains(dst) {
			return r.session, true
		}
	}
	return 0, false
}

func (c *classTun) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	n, err := c.Device.Read(bufs, sizes, offset)
	if c.cls == nil {
		return n, err
	}
	for i := 0; i < n; i++ {
		pkt := bufs[i][offset : offset+sizes[i]]
		id, ok := c.sessionFor(pkt)
		if !ok {
			continue // WireGuard will drop it: no hint, or the queue would slip
		}
		if s := c.mux.Session(id); s != nil {
			s.Classes.Push(bond.WGLen(len(pkt), c.mtu), c.cls.Outbound(pkt))
		}
	}
	return n, err
}

func (c *classTun) Write(bufs [][]byte, offset int) (int, error) {
	if c.cls != nil && c.cls.Learn {
		for _, b := range bufs {
			c.cls.Inbound(b[offset:])
		}
	}
	return c.Device.Write(bufs, offset)
}

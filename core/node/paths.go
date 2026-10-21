package node

import (
	"context"
	"net/netip"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/CarterCole/bond/core/discovery"
	"github.com/CarterCole/bond/core/path"
	"github.com/CarterCole/bond/core/sched"
	"github.com/CarterCole/bond/core/tunnel"
)

// Link is an uplink the hub may open a path over.
type Link struct {
	Name    string // what the user sees and sets policy by: "en0", "wifi", "cellular"
	Iface   string // OS interface to pin the socket to; "" leaves that to the platform hook
	Network string // logical network name handed to the platform socket hook
	Policy  string // policy to use until the user chooses one
	// Gen changes when the network behind the name has been replaced (a
	// phone roaming to another access point gets a new network object under
	// the same "wifi"). A socket stays attached to the network it was
	// opened on, so the path is reopened.
	Gen int
}

// desktopLinks enumerates real interfaces. The one the OS was already using
// to reach the Internet is the primary pipe; anything else that turns up — a
// tethered phone, a second NIC — is bonded in alongside it.
func desktopLinks(only []string, primary string) func() []Link {
	return func() []Link {
		ifs, err := path.Uplinks()
		if err != nil {
			return nil
		}
		var out []Link
		for _, ifi := range ifs {
			if len(only) > 0 && !slices.Contains(only, ifi.Name) {
				continue
			}
			l := Link{Name: ifi.Name, Iface: ifi.Name, Network: ifi.Name, Policy: "bonded"}
			if ifi.Name == primary {
				l.Policy = "primary"
			}
			out = append(out, l)
		}
		return out
	}
}

const (
	syncEvery     = 2 * time.Second
	resolveEvery  = 60 * time.Second
	retryResolve  = 15 * time.Second // after a failed lookup, when an older answer is in hand
	relayForget   = 3 * time.Minute  // a discovered relay not seen this long is dropped
	wsLinger      = 15 * time.Second // UDP must be back this long before its fallback closes
	browseRestart = 30 * time.Second
)

// uplink is one open path and the socket behind it.
type uplink struct {
	name, kind string
	id         uint8
	udp        *path.UDP
	ws         *path.WSClient
	link       Link
	relay      netip.AddrPort
	created    time.Time
	downSince  time.Time // zero while up
	upSince    time.Time // zero while down
}

func (u *uplink) close() {
	if u.udp != nil {
		u.udp.Close()
	}
	if u.ws != nil {
		u.ws.Close()
	}
}

type relayCand struct {
	name   string
	addr   netip.AddrPort
	seen   time.Time
	static bool
}

// pathManager keeps the hub's set of paths matching the world: interfaces
// that come and go, relays that appear on the LAN, and WebSocket fallbacks
// for uplinks where UDP does not get through.
type pathManager struct {
	h     *Hub
	links func() []Link
	wake  chan struct{}

	mu       sync.Mutex
	ups      map[string]*uplink
	relays   map[string]*relayCand
	srv4     netip.AddrPort
	srv6     netip.AddrPort
	resolved time.Time
	closed   bool
}

func newPathManager(h *Hub, links func() []Link) *pathManager {
	m := &pathManager{h: h, links: links, wake: make(chan struct{}, 1), ups: map[string]*uplink{}, relays: map[string]*relayCand{}}
	// Start from where the aggregator was last found; a fresh lookup follows.
	m.srv4, m.srv6 = h.cfg.resolved()
	return m
}

func (m *pathManager) kick() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *pathManager) run(ctx context.Context) {
	if !m.h.o.NoDiscovery {
		go m.browse(ctx)
	}
	t := time.NewTicker(syncEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.wake:
		}
		m.sync(ctx)
	}
}

// browse looks for relays of our mesh on the LAN. The search is restarted
// periodically so that relays which are still there are seen again.
func (m *pathManager) browse(ctx context.Context) {
	for ctx.Err() == nil {
		bctx, cancel := context.WithTimeout(ctx, browseRestart)
		err := discovery.Browse(bctx, m.h.cfg.Mesh.ID, func(f discovery.Found) {
			m.mu.Lock()
			c := m.relays[f.Name]
			fresh := c == nil
			if fresh {
				c = &relayCand{name: f.Name}
				m.relays[f.Name] = c
			}
			c.addr, c.seen = f.Addrs[0], time.Now()
			m.mu.Unlock()
			if fresh {
				m.h.log.Info("found a relay on the LAN", "name", f.Name, "addr", f.Addrs[0].String())
				m.kick()
			}
		})
		cancel()
		if err != nil && ctx.Err() == nil {
			m.h.log.Debug("relay discovery unavailable", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(browseRestart):
			}
		}
	}
}

type wanted struct {
	kind  string
	link  Link
	relay netip.AddrPort
}

func (m *pathManager) sync(ctx context.Context) {
	h := m.h
	mesh := h.cfg.Mesh
	now := time.Now()

	// Where is the aggregator? Names can move (a new Elastic IP, say).
	m.mu.Lock()
	stale := now.Sub(m.resolved) > resolveEvery || (!m.srv4.IsValid() && !m.srv6.IsValid())
	m.mu.Unlock()
	if stale {
		v4, v6, err := resolveUDP(ctx, mesh.UDP[0])
		if err != nil {
			m.mu.Lock()
			if m.srv4.IsValid() || m.srv6.IsValid() {
				// Carry on with the address we know, and do not hold up
				// path management retrying a lookup that takes seconds to
				// fail: look again in a while.
				m.resolved = now.Add(retryResolve - resolveEvery)
			}
			m.mu.Unlock()
		}
		if err == nil {
			if h.cfg.setResolved(v4, v6) {
				h.cfg.Save()
			}
			m.mu.Lock()
			moved := (m.srv4.IsValid() || m.srv6.IsValid()) && (v4 != m.srv4 || v6 != m.srv6)
			m.srv4, m.srv6, m.resolved = v4, v6, now
			if moved {
				h.log.Info("aggregator address changed", "addr", mesh.UDP[0])
				for name, u := range m.ups {
					if u.kind == "direct" {
						m.dropLocked(name)
					}
				}
			}
			m.mu.Unlock()
		} else {
			h.log.Debug("cannot resolve the aggregator yet", "addr", mesh.UDP[0], "err", err)
		}
	}

	up := map[string]bool{}
	for _, p := range h.sess.Status().Paths {
		up[p.Name] = p.Up
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || ctx.Err() != nil {
		return
	}
	for name, u := range m.ups {
		if up[name] {
			if u.upSince.IsZero() {
				u.upSince = now
			}
			u.downSince = time.Time{}
		} else {
			if u.downSince.IsZero() {
				u.downSince = now
			}
			u.upSince = time.Time{}
		}
	}

	want := map[string]wanted{}
	haveWS := len(mesh.WS) > 0
	for _, l := range m.links() {
		if h.o.Transport != "ws" {
			want[l.Name] = wanted{kind: "direct", link: l}
		}
		if !haveWS {
			continue
		}
		wsName := "ws:" + l.Name
		switch h.o.Transport {
		case "ws":
			want[wsName] = wanted{kind: "ws", link: l}
		case "auto":
			// Fall back where UDP has been dead for a while; stay until it
			// has clearly come back, so a flapping network does not thrash.
			d := m.ups[l.Name]
			_, open := m.ups[wsName]
			if h.cfg.policyFor(l.Name, l.Policy) == sched.PolicyOff {
				continue // switched off by the user: silent on purpose, not blocked
			}
			udpDead := d != nil && !d.downSince.IsZero() && now.Sub(d.downSince) >= h.o.wsAfter
			udpBack := d != nil && !d.upSince.IsZero() && now.Sub(d.upSince) >= wsLinger
			if udpDead || (open && !udpBack) {
				want[wsName] = wanted{kind: "ws", link: l}
			}
		}
	}
	if h.o.Transport != "ws" {
		h.cfg.mu.Lock()
		static := append([]string(nil), h.cfg.Relays...)
		h.cfg.mu.Unlock()
		for _, s := range static {
			if ap, err := netip.ParseAddrPort(s); err == nil {
				want["relay:"+s] = wanted{kind: "relay", relay: ap}
			}
		}
		for name, c := range m.relays {
			pname := "relay:" + name
			if u := m.ups[pname]; now.Sub(c.seen) > relayForget && (u == nil || !up[pname]) {
				delete(m.relays, name)
				continue
			}
			want[pname] = wanted{kind: "relay", relay: c.addr}
		}
	}

	for name, u := range m.ups {
		w, ok := want[name]
		if !ok || (u.kind == "relay" && w.relay != u.relay) || (u.kind != "relay" && w.link.Gen != u.link.Gen) {
			m.dropLocked(name)
		}
	}
	names := make([]string, 0, len(want))
	for name := range want {
		names = append(names, name)
	}
	sort.Strings(names) // stable path ids from run to run
	for _, name := range names {
		if m.ups[name] == nil {
			m.openLocked(name, want[name], now)
		}
	}
}

func (m *pathManager) dropLocked(name string) {
	u := m.ups[name]
	if u == nil {
		return
	}
	delete(m.ups, name)
	m.h.sess.RemovePath(u.id)
	u.close()
}

func (m *pathManager) freeID() (uint8, bool) {
	for id := 0; id < 256; id++ {
		if !m.h.sess.HasPath(uint8(id)) {
			return uint8(id), true
		}
	}
	return 0, false
}

func (m *pathManager) openLocked(name string, w wanted, now time.Time) {
	h := m.h
	id, ok := m.freeID()
	if !ok {
		return
	}
	u := &uplink{name: name, kind: w.kind, id: id, link: w.link, relay: w.relay, created: now, downSince: now}
	bind := path.BindOpts{Interface: w.link.Iface, Network: w.link.Network}
	var remote path.Remote
	policy := sched.PolicyBonded

	switch w.kind {
	case "direct", "relay":
		network, srv := "udp4", m.srv4
		if !srv.IsValid() {
			network, srv = "udp6", m.srv6
		}
		if w.kind == "relay" {
			// The hop to a relay is a LAN hop: let the OS pick the interface.
			bind, srv = path.BindOpts{}, w.relay
			network = "udp4"
			if srv.Addr().Is6() {
				network = "udp6"
			}
			policy = h.cfg.policyFor(name, "bonded")
		} else {
			policy = h.cfg.policyFor(name, w.link.Policy)
		}
		if !srv.IsValid() {
			return // aggregator not resolved yet; try again next round
		}
		sock, err := path.ListenUDP(network, ":0", bind)
		if err != nil {
			h.log.Debug("cannot open a socket on uplink", "path", name, "err", err)
			return
		}
		if w.link.Iface != "" {
			tunnel.LoosenReversePath(w.link.Iface)
		}
		u.udp = sock
		go sock.Serve(h.mux.Receive)
		remote = path.Remote{S: sock, Addr: srv}
	case "ws":
		u.ws = path.DialWS(h.cfg.Mesh.WS[0], bind, h.mux.Receive)
		remote = path.Remote{S: u.ws}
		// Unless told otherwise the fallback stands in for its uplink.
		policy = h.cfg.policyFor(name, h.cfg.policyFor(w.link.Name, w.link.Policy).String())
	}
	m.ups[name] = u
	h.sess.AddPath(id, name, w.kind, policy, remote)
}

func (m *pathManager) setPolicy(name string, p sched.Policy) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.ups[name]
	if u == nil {
		return false
	}
	m.h.sess.SetPolicy(u.id, p)
	return true
}

func (m *pathManager) relayStatus() []RelayStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []RelayStatus{}
	for _, c := range m.relays {
		out = append(out, RelayStatus{Name: c.name, Addr: c.addr.String(), Source: "discovered"})
	}
	m.h.cfg.mu.Lock()
	for _, s := range m.h.cfg.Relays {
		out = append(out, RelayStatus{Name: s, Addr: s, Source: "configured"})
	}
	m.h.cfg.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (m *pathManager) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for name := range m.ups {
		m.dropLocked(name)
	}
}

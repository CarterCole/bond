package node

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"golang.zx2c4.com/wireguard/tun"

	"github.com/CarterCole/bond/core/bond"
	"github.com/CarterCole/bond/core/classify"
	"github.com/CarterCole/bond/core/control"
	"github.com/CarterCole/bond/core/gateway"
	"github.com/CarterCole/bond/core/sched"
	"github.com/CarterCole/bond/core/tunnel"
)

// HubOptions configure a hub: the end that owns uplinks and bonds them.
type HubOptions struct {
	// Config is the joined device's identity (LoadOrJoin, ParseConfig).
	Config *Config
	// StateDir, when set, is where the process generation is kept.
	StateDir string

	// Userspace runs without a TUN device or root: the bond is reachable
	// through a SOCKS5 proxy instead of capturing the machine's traffic.
	Userspace bool
	// Socks is the proxy's listen address in userspace mode.
	// Default "127.0.0.1:1080"; "-" disables it.
	Socks string
	// Split leaves the default route alone: only traffic for the mesh's own
	// addresses goes through the tunnel.
	Split bool
	// Gateway shares the bond with the LAN: this machine becomes a router.
	Gateway bool
	// AllowIPv6 leaves the machine's IPv6 connectivity alone. By default a
	// hub that captures all traffic turns IPv6 to the Internet off while it
	// runs, because the tunnel carries only IPv4 and IPv6 would bypass it.
	AllowIPv6 bool

	// Transport is "auto" (UDP, with WebSocket as a fallback on uplinks
	// where UDP is blocked), "udp" or "ws".
	Transport string
	// Interfaces restricts direct paths to these interface names.
	Interfaces []string
	// NoDiscovery turns off looking for relays on the LAN.
	NoDiscovery bool

	// TUN is a ready-made tunnel device, on platforms where the OS creates
	// it (Android VpnService, iOS packet tunnel). StartHub owns it from the
	// call on and closes it if starting fails.
	TUN tun.Device
	// Links replaces interface enumeration, on platforms where the app has
	// to tell us what networks exist.
	Links func() []Link

	// Admin is the local address of the status API and dashboard.
	// Default "127.0.0.1:7780"; "-" disables it.
	Admin string
	Log   *slog.Logger

	wsAfter time.Duration // tests: how long UDP must be down before the fallback opens
}

// Hub is a running hub.
type Hub struct {
	o       HubOptions
	log     *slog.Logger
	cfg     *Config
	mux     *bond.Mux
	sess    *bond.Session
	tun     *tunnel.Tunnel
	stack   *tunnel.UserStack
	cls     *classify.Classifier
	paths   *pathManager
	socks   net.Listener
	admin   *http.Server
	undoGW  gateway.Undo
	mode    string
	tunName string
	started time.Time
	stop    context.CancelFunc
	closing sync.Once
	tunp    atomic.Pointer[tunnel.Tunnel] // h.tun, for callbacks that can fire before StartHub returns
	nudging atomic.Bool
}

// nudge gets WireGuard connected without waiting out its retry timer.
//
// WireGuard retries a handshake only every five seconds. If its first
// attempt left before any path worked (a WebSocket still connecting, a relay
// not yet found), or every path was down for a while, then the moment a
// path comes up is the moment to try again.
//
// It must not trample a handshake that is merely in flight: WireGuard
// timestamps handshakes coarsely and the aggregator would refuse a second
// one sent within the same instant. So wait a moment, and act only if there
// is still no session.
func (h *Hub) nudge() {
	if !h.nudging.CompareAndSwap(false, true) {
		return
	}
	time.AfterFunc(300*time.Millisecond, func() {
		defer time.AfterFunc(2*time.Second, func() { h.nudging.Store(false) })
		t := h.tunp.Load()
		if t == nil {
			return
		}
		wg, err := t.Status()
		if err != nil {
			return
		}
		key := h.cfg.Mesh.ServerWG
		if age, ok := wgHandshakes(wg)[hex.EncodeToString(key[:])]; !ok || age < 0 || age > 150 {
			h.log.Debug("no WireGuard session with a path up: handshaking now")
			t.Rehandshake(key)
		}
	})
}

func StartHub(o HubOptions) (_ *Hub, err error) {
	defer func() {
		if err != nil && o.TUN != nil {
			o.TUN.Close()
		}
	}()
	if o.Config == nil || o.Config.Mesh == nil {
		return nil, errors.New("hub: no mesh config; join first")
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Admin == "" {
		o.Admin = "127.0.0.1:7780"
	}
	if o.Socks == "" {
		o.Socks = "127.0.0.1:1080"
	}
	switch o.Transport {
	case "":
		o.Transport = "auto"
	case "auto", "udp", "ws":
	default:
		return nil, fmt.Errorf("unknown transport %q (want auto, udp or ws)", o.Transport)
	}
	if o.wsAfter == 0 {
		o.wsAfter = 8 * time.Second
	}
	cfg, mesh := o.Config, o.Config.Mesh
	key, err := mesh.Key()
	if err != nil {
		return nil, err
	}
	if len(mesh.UDP) == 0 {
		return nil, errors.New("hub: mesh config names no aggregator address")
	}
	if mesh.MTU == 0 {
		mesh.MTU = tunnel.DefaultMTU
	}

	ctx, cancel := context.WithCancel(context.Background())
	h := &Hub{o: o, log: o.Log, cfg: cfg, started: time.Now(), stop: cancel}
	defer func() {
		if err != nil {
			h.Close()
		}
	}()

	// Look the aggregator up, and find the primary pipe, while routing and
	// DNS are still the user's own: once the tunnel owns them, neither answer
	// can be had until the tunnel is up — which needs both.
	v4, v6, rerr := resolveUDP(ctx, mesh.UDP[0])
	if rerr == nil {
		if cfg.setResolved(v4, v6) {
			cfg.Save()
		}
	} else if v4, v6 = cfg.resolved(); !v4.IsValid() && !v6.IsValid() {
		o.Log.Warn("cannot resolve the aggregator yet; will keep trying", "addr", mesh.UDP[0], "err", rerr)
	}
	primary := ""
	if o.Links == nil {
		dst := v4
		if !dst.IsValid() {
			dst = v6
		}
		primary = outboundInterface(dst)
	}

	h.mux = bond.New(bond.Options{Role: bond.Hub, Gen: control.NextGen(o.StateDir), Log: o.Log,
		OnPeerRestart: func(uint32) {
			// The aggregator came back as a new process: reconnect now
			// rather than when WireGuard's own timers get round to it.
			if t := h.tunp.Load(); t != nil {
				t.Rehandshake(mesh.ServerWG)
			}
		},
		OnPath: func(_ uint32, _ bond.PathStatus, event string) {
			if event == "up" {
				h.nudge()
			}
		}})
	h.sess = h.mux.AddSession(mesh.Session, key, cfg.Name)

	var dev tun.Device
	switch {
	case o.TUN != nil:
		dev, h.mode = o.TUN, "vpn"
	case o.Userspace:
		h.mode = "userspace"
		h.stack, err = tunnel.NewUserStack(tunnel.UserStackOptions{Addrs: []netip.Addr{mesh.Address.Addr()}, MTU: mesh.MTU, Log: o.Log})
		if err != nil {
			return nil, err
		}
		dev = h.stack
	default:
		h.mode = "kernel"
		ko := tunnel.KernelOptions{MTU: mesh.MTU, Address: mesh.Address}
		if !o.Split {
			ko.Routes = tunnel.FullTunnel
			// The tunnel is IPv4 inside. Left alone, IPv6 would carry much
			// of the machine's traffic straight past the bond. (Not when the
			// aggregator itself is only reachable over IPv6.)
			ko.BlockIPv6 = !o.AllowIPv6 && v4.IsValid()
		}
		if dev, h.tunName, err = tunnel.OpenKernelTUN(ko); err != nil {
			return nil, err
		}
	}

	rules := cfg.priority()
	if h.cls, err = classify.New(h.classifierRules(rules)); err != nil {
		dev.Close()
		return nil, fmt.Errorf("priority rules: %w", err)
	}
	h.cls.Stamp = true
	if h.tun, err = tunnel.New(dev, h.mux, tunnel.Options{PrivateKey: cfg.WGPrivate, Classifier: h.cls, Log: o.Log}); err != nil {
		return nil, err
	}
	h.tunp.Store(h.tun)
	allowed := []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}
	if err = h.tun.SetPeers(true, []tunnel.Peer{{PublicKey: mesh.ServerWG, Session: mesh.Session, AllowedIPs: allowed, Keepalive: 25}}); err != nil {
		return nil, err
	}

	if o.Gateway {
		if h.mode != "kernel" {
			return nil, errors.New("gateway mode needs a kernel tunnel (not --userspace)")
		}
		if h.undoGW, err = gateway.Hub(h.tunName, lanDevices(rules)); err != nil {
			return nil, fmt.Errorf("gateway mode: %w", err)
		}
		o.Log.Info("sharing the bond with the LAN: point devices' default gateway at this machine")
	}

	links := o.Links
	if links == nil {
		links = desktopLinks(o.Interfaces, primary)
	}
	h.paths = newPathManager(h, links)
	h.paths.sync(ctx) // open what is there now, before anything asks for the tunnel
	go h.paths.run(ctx)

	if h.stack != nil && o.Socks != "-" {
		if h.socks, err = net.Listen("tcp", o.Socks); err != nil {
			return nil, fmt.Errorf("socks proxy: %w", err)
		}
		go serveSocks(h.socks, h.stack, mesh.DNS, o.Log)
		o.Log.Info("no-root mode: use the bond through the SOCKS5 proxy", "socks5", h.socks.Addr().String())
	}
	if o.Admin != "-" {
		if h.admin, err = serveAdmin(o.Admin, h.adminRoutes(), o.Log); err != nil {
			return nil, err
		}
	}
	o.Log.Info("hub running", "name", cfg.Name, "mode", h.mode, "address", mesh.Address.String(), "aggregator", mesh.UDP[0])
	return h, nil
}

// classifierRules is what the in-tunnel classifier matches. In gateway mode
// LAN devices are hidden behind NAT by the time packets reach the tunnel, so
// those are matched in the firewall instead (see gateway.Hub) and reach the
// classifier already marked.
func (h *Hub) classifierRules(r classify.Rules) classify.Rules {
	if !h.o.Gateway {
		// MAC addresses only mean something to a router.
		r.Devices = ipDevices(r.Devices)
		return r
	}
	r.Devices = nil
	return r
}

func ipDevices(devs []string) []string {
	var out []string
	for _, d := range devs {
		if !gateway.IsMAC(d) {
			out = append(out, d)
		}
	}
	return out
}

func lanDevices(r classify.Rules) []string { return r.Devices }

// SetPolicy changes how a path may be used and remembers the choice.
func (h *Hub) SetPolicy(name, policy string) error {
	p, err := sched.ParsePolicy(policy)
	if err != nil {
		return err
	}
	if !h.paths.setPolicy(name, p) {
		return fmt.Errorf("no path called %q", name)
	}
	h.cfg.setPolicy(name, p)
	return h.cfg.Save()
}

// SetPriority replaces the priority rules.
func (h *Hub) SetPriority(r classify.Rules) error {
	if h.o.Gateway {
		for _, d := range r.Devices {
			if _, err := netip.ParseAddr(d); err != nil {
				if _, err := netip.ParsePrefix(d); err != nil && !gateway.IsMAC(d) {
					return fmt.Errorf("priority device %q is not an IP address, prefix or MAC address", d)
				}
			}
		}
	} else {
		for _, d := range r.Devices {
			if gateway.IsMAC(d) {
				return fmt.Errorf("priority by MAC address (%s) needs gateway mode; use the device's IP address", d)
			}
		}
	}
	if err := h.cls.SetRules(h.classifierRules(r)); err != nil {
		return err
	}
	if h.o.Gateway {
		// Firewall marks are rebuilt from scratch: remove the old, add the new.
		undo, err := gateway.Hub(h.tunName, lanDevices(r))
		if err != nil {
			return err
		}
		if h.undoGW != nil {
			h.undoGW()
		}
		h.undoGW = undo
	}
	h.cfg.mu.Lock()
	h.cfg.Priority = r
	h.cfg.mu.Unlock()
	return h.cfg.Save()
}

// AddRelay adds a relay by address ("192.168.1.23:7778"), for networks
// where discovery does not work.
func (h *Hub) AddRelay(addr string) error {
	if _, err := netip.ParseAddrPort(addr); err != nil {
		return fmt.Errorf("relay address %q must be ip:port", addr)
	}
	h.cfg.mu.Lock()
	for _, r := range h.cfg.Relays {
		if r == addr {
			h.cfg.mu.Unlock()
			return nil
		}
	}
	h.cfg.Relays = append(h.cfg.Relays, addr)
	h.cfg.mu.Unlock()
	h.paths.kick()
	return h.cfg.Save()
}

// LinksChanged tells the hub to look at its links again now (a phone app
// calls it when the OS reports a network change).
func (h *Hub) LinksChanged() { h.paths.kick() }

// Dial opens a connection through the bond in userspace mode.
func (h *Hub) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	if h.stack == nil {
		return nil, errors.New("Dial is only available in userspace mode")
	}
	return h.stack.DialContext(ctx, network, address)
}

// SocksAddr is the SOCKS5 proxy's address in userspace mode, or "".
func (h *Hub) SocksAddr() string {
	if h.socks == nil {
		return ""
	}
	return h.socks.Addr().String()
}

// Config returns the hub's config (for a phone app to persist).
func (h *Hub) Config() *Config { return h.cfg }

func (h *Hub) Close() {
	h.closing.Do(func() {
		h.tunp.Store(nil) // callbacks still in flight find nothing to act on
		h.stop()
		if h.admin != nil {
			h.admin.Close()
		}
		if h.socks != nil {
			h.socks.Close()
		}
		if h.undoGW != nil {
			h.undoGW()
		}
		if h.paths != nil {
			h.paths.close()
		}
		if h.tun != nil {
			h.tun.Close()
		}
		if h.mux != nil {
			h.mux.Close()
		}
	})
}

// HubStatus is the hub's status document.
type HubStatus struct {
	Role       string             `json:"role"`
	Version    string             `json:"version"`
	Name       string             `json:"name"`
	Mode       string             `json:"mode"`
	MeshID     string             `json:"mesh_id"`
	Address    string             `json:"address"`
	Aggregator string             `json:"aggregator"`
	Interface  string             `json:"interface,omitempty"`
	Socks      string             `json:"socks5,omitempty"`
	Gateway    bool               `json:"gateway"`
	Connected  bool               `json:"connected"`
	Handshake  int64              `json:"handshake_age_sec"` // -1 = never
	Uptime     int64              `json:"uptime_sec"`
	Session    bond.SessionStatus `json:"session"`
	Priority   classify.Rules     `json:"priority"`
	Realtime   uint64             `json:"realtime_packets"`
	Relays     []RelayStatus      `json:"relays"`
}

// RelayStatus is a relay the hub knows about.
type RelayStatus struct {
	Name   string `json:"name"`
	Addr   string `json:"addr"`
	Source string `json:"source"` // discovered | configured
}

func (h *Hub) Status() HubStatus {
	mesh := h.cfg.Mesh
	st := HubStatus{
		Role: "hub", Version: Version, Name: h.cfg.Name, Mode: h.mode, MeshID: mesh.ID,
		Address: mesh.Address.Addr().String(), Aggregator: mesh.UDP[0], Interface: h.tunName,
		Gateway: h.o.Gateway, Handshake: -1, Uptime: int64(time.Since(h.started).Seconds()),
		Session: h.sess.Status(), Priority: h.cfg.priority(), Realtime: h.cls.Realtime.Load(),
		Relays: h.paths.relayStatus(),
	}
	if h.socks != nil {
		st.Socks = h.socks.Addr().String()
	}
	if wg, err := h.tun.Status(); err == nil {
		if age, ok := wgHandshakes(wg)[hex.EncodeToString(mesh.ServerWG[:])]; ok {
			st.Handshake = age
		}
	}
	// WireGuard re-handshakes every two minutes while traffic flows.
	st.Connected = st.Handshake >= 0 && st.Handshake < 180
	return st
}

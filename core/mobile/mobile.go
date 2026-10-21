// Package mobile is the bond core as the Android and iOS apps see it.
//
// It is built with gomobile into bond.aar (Kotlin calls it as the Java
// package `mobile`) and Bond.xcframework (Swift calls it as `Mobile…`).
// Everything here sticks to the types gomobile can carry across: strings,
// numbers, booleans, byte slices, errors, and interfaces made of those.
// Anything structured travels as JSON.
//
// The app owns what only the operating system can do: asking the user for
// VPN permission, creating the TUN device, knowing which networks exist,
// attaching a socket to one of them, keeping itself alive in the
// background and announcing a relay on the LAN. The core does the rest.
package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/CarterCole/bond/core/control"
	"github.com/CarterCole/bond/core/discovery"
	"github.com/CarterCole/bond/core/node"
	"github.com/CarterCole/bond/core/path"
	"github.com/CarterCole/bond/core/tunnel"
)

// Platform is implemented by the app.
type Platform interface {
	// BindSocket is called for every socket the core opens, before it is
	// used. network names what the socket is for:
	//
	//	"wifi", "cellular", …  a name the app gave in a networks list: attach
	//	                       the socket to that network
	//	"lan"                  a relay's hub-facing socket: keep it on the
	//	                       local network
	//	""                     no preference
	//
	// On Android the app must also exempt the socket from the VPN
	// (VpnService.protect) in every case. Return true if the socket was
	// attached to the named network; return false to let the core pin it by
	// interface name instead (iOS, where that is all it takes).
	BindSocket(fd int32, network string) bool
	// Log receives the core's log lines.
	Log(line string)
}

// Version is the core's version string.
func Version() string { return node.Version }

// RelayServiceType is the DNS-SD service type a relay must advertise so that
// hubs find it ("_bond-relay._udp"). The app advertises it with the
// platform's own API (NsdManager, NWListener) on the port from Relay.Port.
func RelayServiceType() string { return discovery.Service }

// Join registers this device with the aggregator named in the join code and
// returns the device's config as JSON. The config holds private keys: store
// it in app-private storage and pass it back to the other calls.
// kind is "phone" (bonds its own networks) or "relay" (only lends its uplink).
func Join(code, deviceName, kind string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cfg, err := node.Join(ctx, "", code, deviceName, kind)
	if err != nil {
		return "", err
	}
	return string(cfg.JSON()), nil
}

// Refresh looks the aggregator's name up and returns the config with the
// answer recorded in it. Call it just BEFORE asking the OS to establish the
// tunnel (VpnService.Builder.establish, setTunnelNetworkSettings), pass the
// result to StartClient or StartRelay, and store it.
//
// Once the tunnel is up, name lookups go through it — so the aggregator
// cannot be looked up in order to bring the tunnel up. The core connects on
// the address remembered in the config; this is what keeps that address
// current if the aggregator has moved since the phone was last connected.
// It blocks for up to a few seconds and never fails: if the lookup does not
// work, the config comes back unchanged.
func Refresh(configJSON string) string {
	cfg, err := node.ParseConfig([]byte(configJSON))
	if err != nil {
		return configJSON
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if !cfg.Refresh(ctx) {
		return configJSON
	}
	return string(cfg.JSON())
}

// description is what the OS VPN API needs to know to set up the tunnel.
type description struct {
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	MeshID     string   `json:"mesh_id"`
	Address    string   `json:"address"`    // this device's tunnel address
	Prefix     int      `json:"prefix"`     // mesh prefix length
	Gateway    string   `json:"gateway"`    // aggregator's tunnel address
	Aggregator string   `json:"aggregator"` // host:port outside the tunnel
	MTU        int      `json:"mtu"`
	DNS        []string `json:"dns"`
	// RelayTXT are the TXT entries ("key=value") a relay must advertise.
	RelayTXT []string `json:"relay_txt"`
}

// Describe returns, as JSON, the tunnel parameters in a config: address,
// prefix, gateway, mtu, dns — what VpnService.Builder or
// NEPacketTunnelNetworkSettings must be given — plus the mesh id and the
// TXT records for advertising a relay.
func Describe(configJSON string) (string, error) {
	cfg, err := node.ParseConfig([]byte(configJSON))
	if err != nil {
		return "", err
	}
	m := cfg.Mesh
	d := description{
		Name: cfg.Name, Kind: cfg.Kind, MeshID: m.ID,
		Address: m.Address.Addr().String(), Prefix: m.Address.Bits(), Gateway: m.Gateway.String(),
		MTU: m.MTU, RelayTXT: discovery.TXT(m.ID, cfg.Name),
	}
	if len(m.UDP) > 0 {
		d.Aggregator = m.UDP[0]
	}
	if d.MTU == 0 {
		d.MTU = tunnel.DefaultMTU
	}
	for _, a := range m.DNS {
		d.DNS = append(d.DNS, a.String())
	}
	b, _ := json.Marshal(d)
	return string(b), nil
}

// network is one entry of a networks list.
type network struct {
	Name   string `json:"name"`   // "wifi", "cellular", …: shown to the user, passed to BindSocket
	Iface  string `json:"iface"`  // OS interface name; leave empty when BindSocket does the attaching
	Policy string `json:"policy"` // default policy: primary | bonded | backup | off
	// Gen: change it when the OS has replaced the network behind this name
	// (a new android.net.Network for "wifi", say). The path is then reopened
	// on a fresh socket, which BindSocket attaches to the new network.
	Gen int `json:"gen"`
}

func parseNetworks(js string) ([]node.Link, error) {
	var nets []network
	if strings.TrimSpace(js) != "" {
		if err := json.Unmarshal([]byte(js), &nets); err != nil {
			return nil, errors.New("networks must be a JSON list of {name, iface, policy}")
		}
	}
	links := make([]node.Link, 0, len(nets))
	for _, n := range nets {
		if n.Name == "" {
			return nil, errors.New("every network needs a name")
		}
		links = append(links, node.Link{Name: n.Name, Iface: n.Iface, Network: n.Name, Policy: n.Policy, Gen: n.Gen})
	}
	return links, nil
}

// One process, one platform: the hook is global, and so are sockets.
var (
	hookMu    sync.Mutex
	hookUsers int
)

func install(p Platform) {
	hookMu.Lock()
	defer hookMu.Unlock()
	hookUsers++
	path.SetSocketHook(func(fd int, network string) bool { return p.BindSocket(int32(fd), network) })
}

func uninstall() {
	hookMu.Lock()
	defer hookMu.Unlock()
	if hookUsers--; hookUsers <= 0 {
		hookUsers = 0
		path.SetSocketHook(nil)
	}
}

// logger sends the core's log through the app.
func logger(p Platform) *slog.Logger {
	return slog.New(slog.NewTextHandler(lineWriter{p}, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

type lineWriter struct{ p Platform }

func (w lineWriter) Write(b []byte) (int, error) {
	w.p.Log(strings.TrimRight(string(b), "\n"))
	return len(b), nil
}

// Client bonds this phone's own networks: the VPN side of the app.
type Client struct {
	hub *node.Hub

	mu    sync.Mutex
	links []node.Link
	once  sync.Once
}

// StartClient starts bonding. tunFD is the TUN file descriptor the OS
// created (VpnService.Builder.establish().detachFd() on Android, a dup of
// the packet tunnel's utun socket on iOS). The core takes ownership of it
// the moment this is called and closes it on Stop — or before returning, if
// starting fails. Never close it yourself.
// networksJSON lists the networks to bond, e.g.
//
//	[{"name":"wifi","policy":"primary"},{"name":"cellular","policy":"bonded"}]
//
// with "iface" set on iOS ("en0", "pdp_ip0") and left out on Android.
func StartClient(configJSON string, tunFD int32, networksJSON string, p Platform) (*Client, error) {
	// The descriptor is ours from here on, whatever happens: wrap it first so
	// that every way out of this function closes it exactly once.
	dev, err := tunnel.FromFD(int(tunFD))
	if err != nil {
		return nil, err
	}
	cfg, err := node.ParseConfig([]byte(configJSON))
	if err != nil {
		dev.Close()
		return nil, err
	}
	links, err := parseNetworks(networksJSON)
	if err != nil {
		dev.Close()
		return nil, err
	}
	c := &Client{links: links}
	install(p)
	c.hub, err = node.StartHub(node.HubOptions{
		Config: cfg,
		TUN:    dev,
		Links: func() []node.Link {
			c.mu.Lock()
			defer c.mu.Unlock()
			return append([]node.Link(nil), c.links...)
		},
		// Phones find relays through the OS, not by raw multicast.
		NoDiscovery: true,
		Admin:       "-",
		Log:         logger(p),
	})
	if err != nil {
		uninstall()
		return nil, err
	}
	return c, nil
}

// SetNetworks replaces the list of networks, when the OS reports that one
// appeared or went away. Paths are opened and closed to match.
func (c *Client) SetNetworks(networksJSON string) error {
	links, err := parseNetworks(networksJSON)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.links = links
	c.mu.Unlock()
	c.hub.LinksChanged()
	return nil
}

// SetPolicy sets how a path may be used: primary, bonded, backup or off.
func (c *Client) SetPolicy(pathName, policy string) error { return c.hub.SetPolicy(pathName, policy) }

// SetPriority makes everything this phone sends priority (realtime)
// traffic, or stops doing so.
func (c *Client) SetPriority(all bool) error {
	r := c.hub.Config().Priority
	r.All = all
	return c.hub.SetPriority(r)
}

// AddRelay adds a relay by "ip:port".
func (c *Client) AddRelay(addr string) error { return c.hub.AddRelay(addr) }

// Status returns the hub status document as JSON (the same one the desktop
// dashboard reads).
func (c *Client) Status() string {
	b, _ := json.Marshal(c.hub.Status())
	return string(b)
}

// ConfigJSON returns the current config, including settings changed since
// start, for the app to store.
func (c *Client) ConfigJSON() string { return string(c.hub.Config().JSON()) }

// Stop shuts the tunnel down and closes the TUN descriptor.
func (c *Client) Stop() {
	c.once.Do(func() {
		c.hub.Close()
		uninstall()
	})
}

// Relay lends this phone's connection to hubs on the same local network:
// the "share my cellular" side of the app.
type Relay struct {
	n    *node.RelayNode
	once sync.Once
}

// StartRelay starts forwarding. network names the uplink to share — it is
// passed to Platform.BindSocket for every upstream socket — and iface is
// its OS interface name where pinning by name is enough (iOS: "pdp_ip0";
// leave empty on Android). port is the UDP port to listen on, 0 for the
// default (7778).
func StartRelay(configJSON, network, iface string, port int32, p Platform) (*Relay, error) {
	cfg, err := node.ParseConfig([]byte(configJSON))
	if err != nil {
		return nil, err
	}
	if cfg.Kind == "" {
		cfg.Kind = control.KindRelay
	}
	if port != 0 {
		cfg.Share.Port = int(port)
	}
	install(p)
	n, err := node.StartRelay(node.RelayOptions{
		Config:      cfg,
		Bind:        path.BindOpts{Interface: iface, Network: network},
		LAN:         path.BindOpts{Network: "lan"},
		NoAdvertise: true, // the app advertises with the platform's API
		Admin:       "-",
		Log:         logger(p),
	})
	if err != nil {
		uninstall()
		return nil, err
	}
	return &Relay{n: n}, nil
}

// NetworksChanged tells the relay that the uplink network it shares has
// been replaced (or has come back). Its uplink sockets are reopened — and
// offered to BindSocket again — the next time a hub sends.
func (r *Relay) NetworksChanged() { r.n.Rebind() }

// Port is the UDP port the relay is listening on: advertise this.
func (r *Relay) Port() int32 { return int32(r.n.Port()) }

// Status returns the relay status document as JSON.
func (r *Relay) Status() string {
	b, _ := json.Marshal(r.n.Status())
	return string(b)
}

func (r *Relay) Stop() {
	r.once.Do(func() {
		r.n.Close()
		uninstall()
	})
}

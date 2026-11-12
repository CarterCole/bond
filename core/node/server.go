package node

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.zx2c4.com/wireguard/tun"

	"github.com/CarterCole/bond/core/bond"
	"github.com/CarterCole/bond/core/classify"
	"github.com/CarterCole/bond/core/control"
	"github.com/CarterCole/bond/core/gateway"
	"github.com/CarterCole/bond/core/path"
	"github.com/CarterCole/bond/core/portmap"
	"github.com/CarterCole/bond/core/rendezvous"
	"github.com/CarterCole/bond/core/tunnel"
)

// ServerOptions configure an aggregator.
type ServerOptions struct {
	StateDir string
	// Listen is the address for bonded traffic (UDP) and for joins and the
	// WebSocket fallback (TCP). Default ":7777".
	Listen string
	// Public is how devices reach Listen from outside: "host" or
	// "host:port". Detected when empty and no state exists yet.
	Public string
	// WS overrides the WebSocket fallback URL devices are told to use, for
	// when the aggregator's TCP port is published under another name — behind
	// a Cloudflare Tunnel, say: "wss://bond.example.com/v1/ws". Devices learn
	// it when they join.
	WS string
	// TLS is a second TCP address, ":443" normally, that serves joins and the
	// WebSocket fallback over TLS. Devices are then told to use
	// wss://<public host>/v1/ws, which gets out of networks that let only
	// HTTPS through. Empty disables it.
	TLS string
	// TLSCert and TLSKey are PEM files for TLS, renewed by something else.
	// Without them the certificate comes from Let's Encrypt when Public is a
	// DNS name and TLS is reachable from the Internet on port 443, and is
	// self-signed otherwise.
	TLSCert, TLSKey string
	// Userspace routes to the Internet from inside this process instead of
	// through a kernel TUN device and NAT rules. No root needed. It is the
	// only mode off Linux.
	Userspace bool
	// Admin is the local address of the status API and dashboard.
	// Default "127.0.0.1:7780"; "-" disables it.
	Admin string
	// NoUPnP stops the aggregator asking the router in front of it to
	// forward its port. It only asks when this machine has a private address.
	NoUPnP bool
	// Via is a rendezvous's pairing code. The aggregator then needs no
	// inbound port: it dials out to the rendezvous and registers there, and
	// devices are told to reach it through the rendezvous. It replaces
	// Public and TLS.
	Via string
	Log *slog.Logger

	// Dial overrides how userspace mode reaches the Internet (tests).
	Dial    tunnel.Dialer
	acme    *acme.Client   // tests: an ACME server instead of Let's Encrypt
	viaLink netip.AddrPort // tests: send the rendezvous link through here (a simulated NAT)
}

// Server is a running aggregator.
type Server struct {
	o       ServerOptions
	log     *slog.Logger
	reg     *control.Registry
	mux     *bond.Mux
	tun     *tunnel.Tunnel
	udp     *path.UDP
	web     *http.Server
	admin   *http.Server
	portmap *portmap.Mapper
	tlsMap  *portmap.Mapper // the TLS port's forward, when there is one
	via     *rendezvous.Client
	undoNAT gateway.Undo
	mode    string
	tunName string
	started time.Time
	port    int

	mu       sync.Mutex
	sessions map[uint32]bool
}

func StartServer(o ServerOptions) (_ *Server, err error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.StateDir == "" {
		o.StateDir = DefaultStateDir()
	}
	if o.Listen == "" {
		o.Listen = ":" + strconv.Itoa(control.DefaultPort)
	}
	if o.Admin == "" {
		o.Admin = "127.0.0.1:7780"
	}
	if runtime.GOOS != "linux" && !o.Userspace {
		o.Log.Info("kernel routing is Linux-only; running the aggregator in userspace mode")
		o.Userspace = true
	}
	if o.WS != "" && !strings.HasPrefix(o.WS, "ws://") && !strings.HasPrefix(o.WS, "wss://") {
		return nil, fmt.Errorf("--ws-url %q must start with ws:// or wss://", o.WS)
	}
	if (o.TLSCert == "") != (o.TLSKey == "") {
		return nil, errors.New("--tls-cert and --tls-key go together")
	}
	if o.TLSCert != "" && o.TLS == "" {
		return nil, errors.New("--tls-cert needs --tls, the address to serve TLS on")
	}
	var via rendezvous.Code
	if o.Via != "" {
		var err error
		if via, err = rendezvous.ParseCode(o.Via); err != nil {
			return nil, err
		}
		if o.Public != "" || o.TLS != "" {
			return nil, errors.New("--via replaces --public and --tls: devices reach this aggregator through the rendezvous")
		}
		// Nothing would use a forward: devices are told the rendezvous's address.
		o.NoUPnP = true
	}
	s := &Server{o: o, log: o.Log, started: time.Now(), sessions: map[uint32]bool{}}
	// Listeners are the web server's to close once it serves them.
	var lns []net.Listener
	defer func() {
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			s.Close()
		}
	}()

	// Bind first: the port we actually got is part of the public address.
	if s.udp, err = path.ListenUDP("udp", o.Listen, path.BindOpts{}); err != nil {
		return nil, fmt.Errorf("listening on udp %s: %w", o.Listen, err)
	}
	s.port = int(s.udp.LocalAddr().Port())
	tcpAddr := o.Listen
	if host, _, err := net.SplitHostPort(o.Listen); err == nil {
		tcpAddr = net.JoinHostPort(host, strconv.Itoa(s.port))
	}
	ln, err := net.Listen("tcp", tcpAddr)
	if err != nil {
		return nil, fmt.Errorf("listening on tcp %s: %w", tcpAddr, err)
	}
	lns = append(lns, ln)
	var tlsLn net.Listener
	if o.TLS != "" {
		addr := tlsListenAddr(o.TLS)
		if tlsLn, err = net.Listen("tcp", addr); err != nil {
			return nil, fmt.Errorf("listening on tcp %s for TLS: %w", addr, err)
		}
		lns = append(lns, tlsLn)
	}

	ro := control.RegistryOptions{}
	if o.Via != "" {
		ro.Public = []string{via.HostPort()}
	} else if o.Public != "" {
		pub := o.Public
		if _, _, err := net.SplitHostPort(pub); err != nil {
			pub = net.JoinHostPort(strings.Trim(pub, "[]"), strconv.Itoa(s.port))
		}
		ro.Public = []string{pub}
	}
	if s.reg, err = control.OpenRegistry(o.StateDir, ro); err != nil {
		return nil, err
	}
	if len(s.reg.Public()) == 0 {
		host := detectPublicIP(o.Log)
		pub := net.JoinHostPort(host, strconv.Itoa(s.port))
		o.Log.Warn("no --public address given; guessed one. If devices cannot join, restart with --public <address they can reach>", "public", pub)
		if s.reg, err = control.OpenRegistry(o.StateDir, control.RegistryOptions{Public: []string{pub}}); err != nil {
			return nil, err
		}
	}
	// What devices are told to use where UDP does not get through.
	pubHost, pubPort, err := net.SplitHostPort(s.reg.Public()[0])
	if err != nil {
		return nil, fmt.Errorf("bad public address: %w", err)
	}
	port, _ := strconv.Atoi(pubPort)
	ws := wsURL("ws", pubHost, port)
	var tlsCfg *tls.Config
	if tlsLn != nil {
		var certs string
		if tlsCfg, certs, err = serverTLS(o, pubHost, o.Log); err != nil {
			return nil, err
		}
		tlsPort := tlsLn.Addr().(*net.TCPAddr).Port
		ws = wsURL("wss", pubHost, tlsPort)
		tlsLn = tls.NewListener(tlsLn, tlsCfg)
		o.Log.Info("serving the WebSocket fallback over TLS", "listen", tlsLn.Addr().String(), "certificate", certs)
	}
	if o.WS != "" {
		ws = o.WS
	}
	if err = s.reg.SetWS([]string{ws}); err != nil {
		return nil, err
	}

	s.mux = bond.New(bond.Options{Role: bond.Server, Gen: control.NextGen(o.StateDir), Log: o.Log})

	var dev tun.Device
	gw := s.reg.Gateway()
	if o.Userspace {
		s.mode = "userspace"
		dial := o.Dial
		if dial == nil {
			dial = tunnel.InternetDialer()
		}
		if dev, err = tunnel.NewUserStack(tunnel.UserStackOptions{Addrs: []netip.Addr{gw}, MTU: s.reg.MTU(), Forward: dial, Log: o.Log}); err != nil {
			return nil, err
		}
	} else {
		s.mode = "kernel"
		dev, s.tunName, err = tunnel.OpenKernelTUN(tunnel.KernelOptions{MTU: s.reg.MTU(), Address: netip.PrefixFrom(gw, s.reg.Network().Bits())})
		if err != nil {
			return nil, err
		}
		if s.undoNAT, err = gateway.Aggregator(s.tunName, s.reg.Network()); err != nil {
			dev.Close()
			return nil, fmt.Errorf("setting up NAT: %w", err)
		}
	}

	// The aggregator needs no priority rules of its own: it learns which
	// flows are realtime from the marks hubs put on them.
	cls, _ := classify.New(classify.Rules{})
	cls.Learn = true
	if s.tun, err = tunnel.New(dev, s.mux, tunnel.Options{PrivateKey: s.reg.PrivateKey(), Classifier: cls, Log: o.Log}); err != nil {
		return nil, err
	}
	for _, n := range s.reg.Nodes() {
		if err := s.addNode(n); err != nil {
			return nil, err
		}
	}
	s.reg.OnJoin = func(n control.Node) {
		if err := s.addNode(n); err != nil {
			s.log.Error("could not activate joined node", "node", n.Name, "err", err)
			return
		}
		s.log.Info("node joined", "name", n.Name, "kind", n.Kind, "ip", n.IP.String())
	}

	go s.udp.Serve(s.mux.Receive)

	if o.Via != "" {
		// Mint the standing code first, so that the first registration
		// already routes joins with it.
		if _, err = s.reg.StandingCode(); err != nil {
			return nil, err
		}
		s.via = rendezvous.Dial(rendezvous.ClientOptions{
			Code: via, Mesh: s.reg.MeshID(), Handler: s.mux.Receive, Join: s.reg.Join,
			Routes: s.routes, LinkVia: o.viaLink, Log: o.Log,
		})
		s.reg.OnChange = s.via.Changed
	}

	hm := http.NewServeMux()
	hm.HandleFunc("/v1/join", s.reg.ServeJoin)
	hm.Handle("/v1/ws", path.WSHandler(s.mux.Receive))
	hm.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "bond aggregator\n")
	})
	s.web = &http.Server{Handler: hm, ReadHeaderTimeout: 10 * time.Second}
	go s.web.Serve(ln)
	if tlsLn != nil {
		go s.web.Serve(tlsLn)
		warmCert(tlsCfg, pubHost)
	}

	// At home or in an office the aggregator sits behind a router: ask it,
	// with UPnP, to forward the port devices are told to use. A cloud VM's
	// private address is the provider's NAT, which nobody can ask.
	if !o.NoUPnP && !loopbackListen(o.Listen) {
		if cloud := portmap.CloudVM(); cloud != "" {
			o.Log.Debug("not asking for a port forward: this is a VM in " + cloud + ", whose firewall opens the port")
		} else if _, private := portmap.LocalAddr(); private {
			s.portmap = portmap.Start(portmap.Options{
				Port: s.port, External: publicPort(s.reg.Public(), s.port), Description: "bond aggregator", Log: o.Log,
			})
			if tlsLn != nil {
				p := tlsLn.Addr().(*net.TCPAddr).Port
				s.tlsMap = portmap.Start(portmap.Options{Port: p, External: p, Description: "bond aggregator (TLS)", Log: o.Log})
			}
		}
	}

	if o.Admin != "-" {
		if s.admin, err = serveAdmin(o.Admin, s.adminRoutes(), o.Log); err != nil {
			return nil, err
		}
	}
	o.Log.Info("aggregator running", "mode", s.mode, "listen", o.Listen, "mesh", s.reg.Network().String(), "nodes", len(s.reg.Nodes()))
	return s, nil
}

func (s *Server) addNode(n control.Node) error {
	s.mu.Lock()
	known := s.sessions[n.Session]
	s.sessions[n.Session] = true
	s.mu.Unlock()
	if !known {
		s.mux.AddSession(n.Session, n.Key(), n.Name)
	}
	allowed := append([]netip.Prefix{netip.PrefixFrom(n.IP, n.IP.BitLen())}, n.Routes...)
	return s.tun.SetPeers(false, []tunnel.Peer{{PublicKey: n.WGPublic, Session: n.Session, AllowedIPs: allowed}})
}

// routes is what a rendezvous should send here: every node's session and
// every join token.
func (s *Server) routes() (sessions []uint32, tokens []string) {
	for _, n := range s.reg.Nodes() {
		sessions = append(sessions, n.Session)
	}
	return sessions, s.reg.TokenIDs()
}

// RemoveNode expels a node from the mesh, effective immediately.
func (s *Server) RemoveNode(id int) error {
	n, err := s.reg.RemoveNode(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.sessions, n.Session)
	s.mu.Unlock()
	s.mux.RemoveSession(n.Session)
	return s.tun.RemovePeer(n.WGPublic, n.Session)
}

// JoinCode is the mesh's standing join code.
func (s *Server) JoinCode() (control.JoinCode, error) { return s.reg.StandingCode() }

// Registry exposes the node registry (for minting one-off codes).
func (s *Server) Registry() *control.Registry { return s.reg }

// Port is the UDP/TCP port the aggregator is listening on.
func (s *Server) Port() int { return s.port }

func (s *Server) Close() {
	if s.via != nil {
		s.via.Close()
	}
	if s.portmap != nil {
		s.portmap.Close()
	}
	if s.tlsMap != nil {
		s.tlsMap.Close()
	}
	if s.admin != nil {
		s.admin.Close()
	}
	if s.web != nil {
		s.web.Close()
	}
	if s.udp != nil {
		s.udp.Close()
	}
	if s.tun != nil {
		s.tun.Close()
	}
	if s.mux != nil {
		s.mux.Close()
	}
	if s.undoNAT != nil {
		s.undoNAT()
	}
}

// NodeStatus is one mesh member as the aggregator sees it.
type NodeStatus struct {
	ID        int                 `json:"id"`
	Name      string              `json:"name"`
	Kind      string              `json:"kind"`
	Platform  string              `json:"platform,omitempty"`
	IP        string              `json:"ip"`
	Online    bool                `json:"online"`
	Handshake int64               `json:"handshake_age_sec"` // -1 = never
	Session   *bond.SessionStatus `json:"session,omitempty"`
}

// ServerStatus is the aggregator's status document.
type ServerStatus struct {
	Role     string       `json:"role"`
	Version  string       `json:"version"`
	Mode     string       `json:"mode"`
	MeshID   string       `json:"mesh_id"`
	Network  string       `json:"network"`
	Public   []string     `json:"public"`
	JoinCode string       `json:"join_code"`
	Uptime   int64        `json:"uptime_sec"`
	Nodes    []NodeStatus `json:"nodes"`
	// PortMap is the router's port forward, when the aggregator is behind
	// one and asked for it.
	PortMap *portmap.Status `json:"port_map,omitempty"`
	// Via is the rendezvous devices reach the aggregator through, when there is one.
	Via *rendezvous.ClientStatus `json:"via,omitempty"`
}

func (s *Server) Status() ServerStatus {
	st := ServerStatus{
		Role: "aggregator", Version: Version, Mode: s.mode, MeshID: s.reg.MeshID(),
		Network: s.reg.Network().String(), Public: s.reg.Public(), Uptime: int64(time.Since(s.started).Seconds()),
	}
	if code, err := s.reg.StandingCode(); err == nil {
		st.JoinCode = code.String()
	}
	if s.portmap != nil {
		pm := s.portmap.Status()
		st.PortMap = &pm
	}
	if s.via != nil {
		v := s.via.Status()
		st.Via = &v
	}
	wg, _ := s.tun.Status()
	hs := wgHandshakes(wg)
	for _, n := range s.reg.Nodes() {
		ns := NodeStatus{ID: n.ID, Name: n.Name, Kind: n.Kind, Platform: n.Platform, IP: n.IP.String(), Handshake: -1}
		if age, ok := hs[hex.EncodeToString(n.WGPublic[:])]; ok {
			ns.Handshake = age
		}
		if sess := s.mux.Session(n.Session); sess != nil {
			ss := sess.Status()
			ns.Session = &ss
			for _, p := range ss.Paths {
				ns.Online = ns.Online || p.Up
			}
		}
		st.Nodes = append(st.Nodes, ns)
	}
	// A relay has no tunnel of its own to judge by. It is in use — which is
	// what "online" can mean for it — when some hub has a live path through it.
	relaying := map[string]bool{}
	for _, n := range st.Nodes {
		if n.Session == nil {
			continue
		}
		for _, p := range n.Session.Paths {
			if name, ok := strings.CutPrefix(p.Name, "relay:"); ok && p.Up {
				relaying[name] = true
			}
		}
	}
	for i := range st.Nodes {
		if st.Nodes[i].Kind == control.KindRelay {
			st.Nodes[i].Online = relaying[st.Nodes[i].Name]
		}
	}
	return st
}

// loopbackListen reports whether the aggregator only listens on loopback,
// where no router forward could reach it.
func loopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	return host == "localhost" || (err == nil && ip.IsLoopback())
}

// publicPort is the port devices are told to reach the aggregator on: the
// one a router has to forward.
func publicPort(public []string, port int) int {
	if len(public) > 0 {
		if _, p, err := net.SplitHostPort(public[0]); err == nil {
			if n, err := strconv.Atoi(p); err == nil {
				return n
			}
		}
	}
	return port
}

// detectPublicIP asks the outside world what address this machine appears
// as. Cloud VMs sit behind 1:1 NAT and cannot tell from their interfaces.
// Falls back to the address of the default route's interface.
func detectPublicIP(log *slog.Logger) string {
	hc := &http.Client{Timeout: 3 * time.Second}
	for _, u := range []string{"https://checkip.amazonaws.com", "https://api.ipify.org"} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		resp, err := hc.Do(req)
		if err == nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
			resp.Body.Close()
			cancel()
			if ip, err := netip.ParseAddr(strings.TrimSpace(string(b))); err == nil {
				return ip.String()
			}
			continue
		}
		cancel()
	}
	if c, err := net.Dial("udp", "192.0.2.1:9"); err == nil {
		defer c.Close()
		return c.LocalAddr().(*net.UDPAddr).IP.String()
	}
	log.Warn("could not work out any address for this machine")
	return "127.0.0.1"
}

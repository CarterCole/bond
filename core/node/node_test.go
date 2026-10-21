package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/proxy"

	"github.com/CarterCole/bond/core/classify"
	"github.com/CarterCole/bond/core/control"
)

// These tests run real nodes against each other over loopback sockets: an
// aggregator in userspace mode whose "Internet" is a local HTTP server, hubs
// reaching it through their SOCKS proxy, and relays in between.

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

const page = "hello from the internet\n"

// internet is the web server the aggregator's NAT is pointed at, whatever
// address the hub asked for.
func internet(t *testing.T) (dial func(ctx context.Context, network string, dst netip.AddrPort) (net.Conn, error)) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n, _ := strconv.Atoi(r.URL.Query().Get("bytes")); n > 0 {
			w.Write(bytes.Repeat([]byte{'x'}, n))
			return
		}
		io.WriteString(w, page)
	}))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	return func(ctx context.Context, network string, _ netip.AddrPort) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
}

func startServer(t *testing.T, dir, listen string) *Server {
	t.Helper()
	s, err := StartServer(ServerOptions{StateDir: dir, Listen: listen, Public: "127.0.0.1", Userspace: true, Admin: "127.0.0.1:0", Log: quiet, Dial: internet(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func join(t *testing.T, s *Server, name, kind string) (*Config, string) {
	t.Helper()
	code, err := s.JoinCode()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg, err := Join(context.Background(), dir, code.String(), name, kind)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, dir
}

func twoLinks() []Link { return []Link{{Name: "a", Policy: "primary"}, {Name: "b"}} }

func startHub(t *testing.T, o HubOptions) *Hub {
	t.Helper()
	o.Userspace, o.NoDiscovery, o.Log = true, true, quiet
	o.Socks, o.Admin = "127.0.0.1:0", "127.0.0.1:0"
	if o.Links == nil {
		o.Links = twoLinks
	}
	h, err := StartHub(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

// fetch GETs a URL on "the Internet" through the hub's SOCKS proxy.
func fetch(h *Hub, url string) (string, error) {
	d, err := proxy.SOCKS5("tcp", h.SocksAddr(), nil, proxy.Direct)
	if err != nil {
		return "", err
	}
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Dial: d.Dial, DisableKeepAlives: true}}
	resp, err := hc.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

// eventually retries f until it succeeds or the deadline passes.
func eventually(t *testing.T, what string, within time.Duration, f func() error) {
	t.Helper()
	var err error
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if err = f(); err == nil {
			return
		}
	}
	t.Fatalf("%s: %v", what, err)
}

func pathsUp(h *Hub) map[string]bool {
	out := map[string]bool{}
	for _, p := range h.Status().Session.Paths {
		out[p.Name] = p.Up
	}
	return out
}

func wantPage(h *Hub) func() error {
	return func() error {
		body, err := fetch(h, "http://93.184.216.34/")
		if err != nil {
			return err
		}
		if body != page {
			return fmt.Errorf("unexpected body %q", body)
		}
		return nil
	}
}

func TestHubReachesInternetThroughAggregator(t *testing.T) {
	s := startServer(t, t.TempDir(), "127.0.0.1:0")
	cfg, dir := join(t, s, "studio", control.KindHub)
	h := startHub(t, HubOptions{Config: cfg, StateDir: dir})

	eventually(t, "fetch through the bond", 10*time.Second, wantPage(h))
	if up := pathsUp(h); !up["a"] || !up["b"] {
		t.Fatalf("paths: %v", up)
	}
	hs := h.Status()
	if !hs.Connected || hs.Address != "10.207.0.2" || hs.Mode != "userspace" {
		t.Fatalf("hub status: %+v", hs)
	}
	// A larger transfer exercises both paths and the reorder buffer for real.
	body, err := fetch(h, "http://93.184.216.34/?bytes=2000000")
	if err != nil || len(body) != 2_000_000 {
		t.Fatalf("2 MB download: %d bytes, %v", len(body), err)
	}

	ss := s.Status()
	if len(ss.Nodes) != 1 || !ss.Nodes[0].Online || ss.Nodes[0].Name != "studio" || ss.Nodes[0].Handshake < 0 {
		t.Fatalf("aggregator status: %+v", ss.Nodes)
	}
	// The aggregator learned what the hub calls its paths, and their policies.
	got := map[string]string{}
	for _, p := range ss.Nodes[0].Session.Paths {
		got[p.Name] = p.Policy
	}
	if got["a"] != "primary" || got["b"] != "bonded" {
		t.Fatalf("aggregator's view of the paths: %v", got)
	}
}

func TestAdminAPI(t *testing.T) {
	s := startServer(t, t.TempDir(), "127.0.0.1:0")
	cfg, dir := join(t, s, "studio", control.KindHub)
	h := startHub(t, HubOptions{Config: cfg, StateDir: dir})
	eventually(t, "paths up", 5*time.Second, func() error {
		if up := pathsUp(h); !up["a"] || !up["b"] {
			return fmt.Errorf("%v", up)
		}
		return nil
	})
	base := "http://" + h.AdminAddr()

	do := func(method, url, ctype, body string, host string) (int, string) {
		req, _ := http.NewRequest(method, url, strings.NewReader(body))
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		if host != "" {
			req.Host = host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	code, body := do("GET", base+"/api/status", "", "", "")
	var st HubStatus
	if code != 200 || json.Unmarshal([]byte(body), &st) != nil || st.Role != "hub" || len(st.Session.Paths) != 2 {
		t.Fatalf("status: %d %s", code, body)
	}
	if code, body := do("GET", base+"/", "", "", ""); code != 200 || !strings.Contains(body, "<title>Bond</title>") {
		t.Fatalf("dashboard: %d", code)
	}

	// Change a path's policy: applied, persisted, and told to the aggregator.
	if code, body := do("POST", base+"/api/path", "application/json", `{"name":"b","policy":"backup"}`, ""); code != 200 {
		t.Fatalf("set policy: %d %s", code, body)
	}
	saved, err := LoadConfig(dir)
	if err != nil || saved.Paths["b"] != "backup" {
		t.Fatalf("policy not persisted: %v %v", saved, err)
	}
	eventually(t, "aggregator learns the new policy", 5*time.Second, func() error {
		for _, p := range s.Status().Nodes[0].Session.Paths {
			if p.Name == "b" && p.Policy == "backup" {
				return nil
			}
		}
		return fmt.Errorf("still not backup")
	})
	if code, _ := do("POST", base+"/api/path", "application/json", `{"name":"nope","policy":"off"}`, ""); code != 400 {
		t.Fatalf("unknown path accepted: %d", code)
	}
	if code, _ := do("POST", base+"/api/path", "application/json", `{"name":"a","policy":"turbo"}`, ""); code != 400 {
		t.Fatalf("unknown policy accepted: %d", code)
	}

	// Priority rules.
	if code, body := do("POST", base+"/api/priority", "application/json", `{"ports":["tcp/1935"],"devices":["192.168.8.50"]}`, ""); code != 200 {
		t.Fatalf("set priority: %d %s", code, body)
	}
	if got := h.Status().Priority; len(got.Ports) != 1 || len(got.Devices) != 1 {
		t.Fatalf("priority not applied: %+v", got)
	}
	if code, _ := do("POST", base+"/api/priority", "application/json", `{"devices":["aa:bb:cc:dd:ee:ff"]}`, ""); code != 400 {
		t.Fatalf("MAC priority accepted outside gateway mode: %d", code)
	}

	// The guards: a web page must not be able to drive this API.
	if code, _ := do("GET", base+"/api/status", "", "", "evil.example.com"); code != 403 {
		t.Fatalf("DNS-rebinding style Host accepted: %d", code)
	}
	if code, _ := do("POST", base+"/api/path", "text/plain", `{"name":"a","policy":"off"}`, ""); code != 415 {
		t.Fatalf("form-style POST accepted: %d", code)
	}
	if up := h.Status().Session.Paths; up[0].Policy == "off" {
		t.Fatal("rejected request still took effect")
	}

	// Aggregator side: mint a single-use code, see the QR, remove a node.
	sbase := "http://" + s.AdminAddr()
	code, body = do("POST", sbase+"/api/code", "application/json", `{"max_uses":1,"ttl_seconds":600}`, "")
	var minted struct{ Code string }
	if code != 200 || json.Unmarshal([]byte(body), &minted) != nil || !strings.HasPrefix(minted.Code, "bond://127.0.0.1:") {
		t.Fatalf("mint code: %d %s", code, body)
	}
	if code, body := do("GET", sbase+"/api/qr.svg", "", "", ""); code != 200 || !strings.HasPrefix(body, "<svg") {
		t.Fatalf("qr: %d", code)
	}
}

func TestKickedNodeLosesAccess(t *testing.T) {
	s := startServer(t, t.TempDir(), "127.0.0.1:0")
	cfg, dir := join(t, s, "studio", control.KindHub)
	h := startHub(t, HubOptions{Config: cfg, StateDir: dir})
	eventually(t, "fetch", 10*time.Second, wantPage(h))
	if err := s.RemoveNode(s.Status().Nodes[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fetch(h, "http://93.184.216.34/"); err == nil {
		t.Fatal("a removed node could still reach the Internet through the aggregator")
	}
}

func TestRelayLendsItsUplink(t *testing.T) {
	s := startServer(t, t.TempDir(), "127.0.0.1:0")
	rcfg, _ := join(t, s, "pixel", control.KindRelay)
	r, err := StartRelay(RelayOptions{Config: rcfg, Listen: "127.0.0.1:0", NoAdvertise: true, Admin: "-", Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// A hub with no uplink of its own, only the relay.
	cfg, dir := join(t, s, "studio", control.KindHub)
	h := startHub(t, HubOptions{Config: cfg, StateDir: dir, Links: func() []Link { return nil }})
	relayAddr := "127.0.0.1:" + strconv.Itoa(r.Port())
	if err := h.AddRelay(relayAddr); err != nil {
		t.Fatal(err)
	}
	eventually(t, "fetch through the relay", 10*time.Second, wantPage(h))
	if up := pathsUp(h); len(up) != 1 || !up["relay:"+relayAddr] {
		t.Fatalf("paths: %v", up)
	}
	st := r.Status()
	if st.Relay.Clients != 1 || st.Relay.UpPkts == 0 || st.Relay.DownPkts == 0 {
		t.Fatalf("relay stats: %+v", st.Relay)
	}
	if saved, _ := LoadConfig(dir); len(saved.Relays) != 1 {
		t.Fatal("relay not remembered")
	}

	// The relay is no use to anything but a bond hub.
	c, _ := net.Dial("udp", relayAddr)
	defer c.Close()
	c.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
	eventually(t, "junk refused", 2*time.Second, func() error {
		if r.Status().Relay.Refused == 0 {
			return fmt.Errorf("not counted yet")
		}
		return nil
	})
}

func TestWebSocketOnlyTransport(t *testing.T) {
	s := startServer(t, t.TempDir(), "127.0.0.1:0")
	cfg, dir := join(t, s, "hotel", control.KindHub)
	h := startHub(t, HubOptions{Config: cfg, StateDir: dir, Transport: "ws"})
	eventually(t, "fetch over websocket paths", 10*time.Second, wantPage(h))
	up := pathsUp(h)
	if len(up) != 2 || !up["ws:a"] || !up["ws:b"] {
		t.Fatalf("paths: %v", up)
	}
	if body, err := fetch(h, "http://93.184.216.34/?bytes=500000"); err != nil || len(body) != 500_000 {
		t.Fatalf("500 kB over websocket: %d bytes, %v", len(body), err)
	}
}

// UDP to the aggregator is silently dropped (a hotel firewall): the hub must
// notice and bring up the WebSocket fallback by itself.
func TestFallsBackToWebSocketWhenUDPIsBlocked(t *testing.T) {
	s := startServer(t, t.TempDir(), "127.0.0.1:0")
	cfg, dir := join(t, s, "hotel", control.KindHub)
	hole, err := net.ListenPacket("udp", "127.0.0.1:0") // swallows everything
	if err != nil {
		t.Fatal(err)
	}
	defer hole.Close()
	cfg.Mesh.UDP = []string{hole.LocalAddr().String()}

	h := startHub(t, HubOptions{Config: cfg, StateDir: dir, wsAfter: 500 * time.Millisecond, Links: func() []Link { return []Link{{Name: "a"}} }})
	eventually(t, "fetch via the fallback", 15*time.Second, wantPage(h))
	up := pathsUp(h)
	if up["a"] || !up["ws:a"] {
		t.Fatalf("paths: %v", up)
	}
}

func TestSurvivesAggregatorRestart(t *testing.T) {
	sdir := t.TempDir()
	s := startServer(t, sdir, "127.0.0.1:0")
	listen := "127.0.0.1:" + strconv.Itoa(s.Port())
	cfg, dir := join(t, s, "studio", control.KindHub)
	h := startHub(t, HubOptions{Config: cfg, StateDir: dir})
	eventually(t, "fetch", 10*time.Second, wantPage(h))

	s.Close()
	if _, err := fetch(h, "http://93.184.216.34/"); err == nil {
		t.Fatal("fetch worked with the aggregator stopped")
	}
	s2 := startServer(t, sdir, listen)
	if n := s2.Status().Nodes; len(n) != 1 || n[0].Name != "studio" {
		t.Fatalf("aggregator forgot its nodes: %+v", n)
	}
	// Nothing is done on the hub: it has to find its own way back, and
	// promptly — not after WireGuard's fifteen-second dead-peer timer.
	back := time.Now()
	eventually(t, "fetch after the aggregator came back", 4*time.Second, wantPage(h))
	took := time.Since(back)
	t.Logf("traffic flowing again %.1fs after the aggregator restarted", took.Seconds())
	if took > 3*time.Second {
		t.Fatalf("hub took %v to recover from an aggregator restart", took)
	}
}

func TestHubRestartKeepsIdentity(t *testing.T) {
	s := startServer(t, t.TempDir(), "127.0.0.1:0")
	cfg, dir := join(t, s, "studio", control.KindHub)
	h := startHub(t, HubOptions{Config: cfg, StateDir: dir})
	eventually(t, "fetch", 10*time.Second, wantPage(h))
	h.Close()

	// A new process: config from disk, no join code.
	cfg2, err := LoadOrJoin(context.Background(), dir, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Mesh.Address != cfg.Mesh.Address || cfg2.WGPrivate != cfg.WGPrivate {
		t.Fatal("identity changed across restart")
	}
	// WireGuard stamps handshakes with ~16ms granularity and refuses one
	// that is not newer than the last: no real process restarts faster
	// than that, but this test would.
	time.Sleep(50 * time.Millisecond)
	back := time.Now()
	h2 := startHub(t, HubOptions{Config: cfg2, StateDir: dir})
	// The aggregator still has paths to the old process's sockets; it must
	// not answer the new one's handshake down those.
	eventually(t, "fetch after hub restart", 3*time.Second, wantPage(h2))
	took := time.Since(back)
	t.Logf("traffic flowing %.2fs after the hub restarted", took.Seconds())
	if took > 2*time.Second {
		t.Fatalf("hub took %v to get traffic flowing after a restart", took)
	}
	if len(s.Status().Nodes) != 1 {
		t.Fatal("restart registered a second node")
	}
}

func TestNotJoined(t *testing.T) {
	_, err := LoadOrJoin(context.Background(), t.TempDir(), "", "", "")
	if err == nil || !strings.Contains(err.Error(), "join code") {
		t.Fatalf("unhelpful error for an unjoined device: %v", err)
	}
	if _, err := os.Stat(DefaultStateDir()); err != nil && !os.IsNotExist(err) {
		t.Fatalf("default state dir: %v", err)
	}
}

func TestConfigRoundTripsThroughJSON(t *testing.T) {
	s := startServer(t, t.TempDir(), "127.0.0.1:0")
	cfg, _ := join(t, s, "phone", control.KindPhone)
	cfg.Priority = classify.Rules{All: true}
	back, err := ParseConfig(cfg.JSON())
	if err != nil || back.Mesh.Session != cfg.Mesh.Session || back.WGPrivate != cfg.WGPrivate || !back.Priority.All || back.Kind != control.KindPhone {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}

// A device whose DNS goes through the tunnel cannot look the aggregator up
// while the tunnel is down. It has to get there on the address it remembers.
func TestConnectsWhenAggregatorNameDoesNotResolve(t *testing.T) {
	s := startServer(t, t.TempDir(), "127.0.0.1:0")
	cfg, dir := join(t, s, "studio", control.KindHub)
	real := cfg.Mesh.UDP[0]
	if len(cfg.Resolved) != 1 || cfg.Resolved[0] != real {
		t.Fatalf("join did not record where the aggregator is: %v", cfg.Resolved)
	}
	// From now on the name is unresolvable, as it would be with DNS behind a tunnel that is down.
	cfg.Mesh.UDP = []string{"aggregator.invalid:" + strconv.Itoa(s.Port())}
	cfg.Mesh.WS = nil
	h := startHub(t, HubOptions{Config: cfg, StateDir: dir})
	eventually(t, "fetch using the remembered address", 10*time.Second, wantPage(h))
	if up := pathsUp(h); !up["a"] || !up["b"] {
		t.Fatalf("paths: %v", up)
	}
}

// The OS replaced the network behind a link (same name, new network): the
// hub must reopen the path on a fresh socket and be back at once.
func TestReplacedNetworkReopensThePath(t *testing.T) {
	s := startServer(t, t.TempDir(), "127.0.0.1:0")
	cfg, dir := join(t, s, "phone", control.KindPhone)
	var gen atomic.Int64
	h := startHub(t, HubOptions{Config: cfg, StateDir: dir, Links: func() []Link {
		return []Link{{Name: "wifi", Policy: "primary", Gen: int(gen.Load())}}
	}})
	eventually(t, "fetch", 10*time.Second, wantPage(h))
	remote := func() string {
		for _, p := range s.Status().Nodes[0].Session.Paths {
			if p.Name == "wifi" {
				return p.Remote
			}
		}
		return ""
	}
	before := remote()

	gen.Add(1)
	h.LinksChanged()
	start := time.Now()
	eventually(t, "aggregator sees the path from a new socket", 3*time.Second, func() error {
		if r := remote(); r == before || r == "" {
			return fmt.Errorf("still %s", r)
		}
		if !pathsUp(h)["wifi"] {
			return fmt.Errorf("not up")
		}
		return nil
	})
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Fatalf("reopened path took %v to come up", took)
	}
	if err := wantPage(h)(); err != nil {
		t.Fatalf("fetch after the network was replaced: %v", err)
	}
}

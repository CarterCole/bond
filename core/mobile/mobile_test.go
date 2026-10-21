package mobile

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CarterCole/bond/core/node"
)

// fakePlatform records what the core asks of the app.
type fakePlatform struct {
	mu    sync.Mutex
	binds map[string]int
	logs  []string
	gone  string // a network BindSocket cannot attach to
}

func (p *fakePlatform) BindSocket(fd int32, network string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.binds == nil {
		p.binds = map[string]int{}
	}
	p.binds[network]++
	// "attached" — on loopback there is nothing to attach to — unless the
	// test says this network is gone.
	return network != p.gone
}

func (p *fakePlatform) Log(line string) {
	p.mu.Lock()
	p.logs = append(p.logs, line)
	p.mu.Unlock()
}

func (p *fakePlatform) bound(network string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.binds[network]
}

func aggregator(t *testing.T) (*node.Server, string) {
	t.Helper()
	s, err := node.StartServer(node.ServerOptions{
		StateDir: t.TempDir(), Listen: "127.0.0.1:0", Public: "127.0.0.1", Userspace: true, Admin: "-",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	code, err := s.JoinCode()
	if err != nil {
		t.Fatal(err)
	}
	return s, code.String()
}

func TestJoinAndDescribe(t *testing.T) {
	_, code := aggregator(t)
	cfg, err := Join(code, "Pixel 9", "phone")
	if err != nil {
		t.Fatal(err)
	}
	js, err := Describe(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var d description
	if err := json.Unmarshal([]byte(js), &d); err != nil {
		t.Fatal(err)
	}
	if d.Name != "Pixel 9" || d.Kind != "phone" || d.Address != "10.207.0.2" || d.Prefix != 16 || d.Gateway != "10.207.0.1" || d.MTU != 1280 || len(d.DNS) == 0 {
		t.Fatalf("description: %+v", d)
	}
	if !strings.HasPrefix(d.Aggregator, "127.0.0.1:") || len(d.RelayTXT) != 3 || d.RelayTXT[1] != "mesh="+d.MeshID {
		t.Fatalf("description: %+v", d)
	}
	if _, err := Join("bond://127.0.0.1:1/AAAA", "x", "phone"); err == nil {
		t.Fatal("damaged code accepted")
	}
	if _, err := Describe("{}"); err == nil {
		t.Fatal("empty config accepted")
	}
	if RelayServiceType() != "_bond-relay._udp" {
		t.Fatal(RelayServiceType())
	}
}

func TestRefreshRecordsWhereTheAggregatorIs(t *testing.T) {
	s, code := aggregator(t)
	cfg, err := Join(code, "Pixel 9", "phone")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := node.ParseConfig([]byte(cfg))
	want := "127.0.0.1:" + strconv.Itoa(s.Port())
	if len(parsed.Resolved) != 1 || parsed.Resolved[0] != want {
		t.Fatalf("join recorded %v, want [%s]", parsed.Resolved, want)
	}
	// Nothing moved: the config comes back exactly as it went in.
	if Refresh(cfg) != cfg {
		t.Fatal("Refresh rewrote a config that was already current")
	}
	// The aggregator "moved": the name now points somewhere else.
	stale := strings.Replace(cfg, `"`+want+`"`, `"192.0.2.9:7777"`, 2)
	stale = strings.Replace(stale, `"192.0.2.9:7777"`, `"localhost:`+strconv.Itoa(s.Port())+`"`, 1) // udp: a name
	moved, _ := node.ParseConfig([]byte(stale))
	if moved.Mesh.UDP[0] != "localhost:"+strconv.Itoa(s.Port()) || moved.Resolved[0] != "192.0.2.9:7777" {
		t.Fatalf("test setup: udp %v resolved %v", moved.Mesh.UDP, moved.Resolved)
	}
	fresh, _ := node.ParseConfig([]byte(Refresh(stale)))
	if len(fresh.Resolved) == 0 || fresh.Resolved[0] != want {
		t.Fatalf("Refresh left %v, want %s first", fresh.Resolved, want)
	}
	// Garbage in, the same garbage out: it must never fail the caller.
	if Refresh("not json") != "not json" {
		t.Fatal("Refresh changed an unparseable config")
	}
}

func TestNetworksJSON(t *testing.T) {
	links, err := parseNetworks(`[{"name":"wifi","iface":"en0","policy":"primary"},{"name":"cellular"}]`)
	if err != nil || len(links) != 2 {
		t.Fatal(err)
	}
	if links[0].Name != "wifi" || links[0].Iface != "en0" || links[0].Network != "wifi" || links[0].Policy != "primary" || links[1].Network != "cellular" {
		t.Fatalf("%+v", links)
	}
	if l, err := parseNetworks(""); err != nil || len(l) != 0 {
		t.Fatal("empty list should be fine")
	}
	for _, bad := range []string{`{"name":"wifi"}`, `[{"iface":"en0"}]`, `nonsense`} {
		if _, err := parseNetworks(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

// The relay as a phone runs it: every socket goes through the platform, the
// hub-facing one tagged "lan" and the upstream ones tagged with the network
// being shared.
func TestRelaySocketsGoThroughThePlatform(t *testing.T) {
	s, code := aggregator(t)
	cfg, err := Join(code, "Pixel 9", "relay")
	if err != nil {
		t.Fatal(err)
	}
	p := &fakePlatform{}
	r, err := StartRelay(cfg, "cellular", "", 0, p)
	if err != nil {
		// Port 7778 may be taken on the machine running the tests.
		if r, err = StartRelay(cfg, "cellular", "", 47778, p); err != nil {
			t.Fatal(err)
		}
	}
	defer r.Stop()
	if p.bound("lan") != 1 {
		t.Fatalf("hub-facing socket not offered to the platform as \"lan\": %v", p.binds)
	}

	// A desktop hub uses the phone as its only uplink.
	hubCfg, err := node.Join(t.Context(), t.TempDir(), code, "studio", "hub")
	if err != nil {
		t.Fatal(err)
	}
	h, err := node.StartHub(node.HubOptions{
		Config: hubCfg, Userspace: true, NoDiscovery: true, Socks: "-", Admin: "-",
		Links: func() []node.Link { return nil }, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err := h.AddRelay(net.JoinHostPort("127.0.0.1", strconv.Itoa(int(r.Port())))); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !h.Status().Connected && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !h.Status().Connected {
		t.Fatal("hub did not connect through the phone's relay")
	}
	if p.bound("cellular") == 0 {
		t.Fatalf("upstream socket not offered to the platform as \"cellular\": %v", p.binds)
	}
	var st struct {
		Role  string
		Relay struct{ Clients, UpPkts int }
	}
	if err := json.Unmarshal([]byte(r.Status()), &st); err != nil || st.Role != "relay" || st.Relay.Clients != 1 {
		t.Fatalf("relay status: %s", r.Status())
	}
	if len(s.Status().Nodes) != 2 {
		t.Fatalf("aggregator sees %d nodes", len(s.Status().Nodes))
	}
	p.mu.Lock()
	logged := len(p.logs)
	p.mu.Unlock()
	if logged == 0 {
		t.Fatal("core log did not reach the platform")
	}
}

// The phone has no cellular right now. A relay must not quietly forward the
// hub's traffic over whatever network is left — that is the WiFi it came in
// on — and call it sharing.
func TestRelayRefusesWhenItsUplinkIsUnavailable(t *testing.T) {
	_, code := aggregator(t)
	cfg, err := Join(code, "Pixel 9", "relay")
	if err != nil {
		t.Fatal(err)
	}
	p := &fakePlatform{gone: "cellular"}
	r, err := StartRelay(cfg, "cellular", "", 47779, p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()

	hubCfg, _ := node.Join(t.Context(), t.TempDir(), code, "studio", "hub")
	h, err := node.StartHub(node.HubOptions{
		Config: hubCfg, Userspace: true, NoDiscovery: true, Socks: "-", Admin: "-",
		Links: func() []node.Link { return nil }, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.AddRelay("127.0.0.1:47779")
	time.Sleep(700 * time.Millisecond)
	var st struct {
		Relay struct{ Clients, Refused, UpPkts int }
	}
	json.Unmarshal([]byte(r.Status()), &st)
	if st.Relay.UpPkts != 0 || st.Relay.Clients != 0 || st.Relay.Refused == 0 {
		t.Fatalf("relay forwarded without its uplink: %+v", st.Relay)
	}
	if h.Status().Connected {
		t.Fatal("hub connected through a relay that had no uplink")
	}

	// Cellular comes back: the app says so, and the next datagram goes through.
	p.mu.Lock()
	p.gone = ""
	p.mu.Unlock()
	r.NetworksChanged()
	deadline := time.Now().Add(5 * time.Second)
	for !h.Status().Connected && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !h.Status().Connected {
		t.Fatal("hub did not connect after the relay's uplink came back")
	}
}

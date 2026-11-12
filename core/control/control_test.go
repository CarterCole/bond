package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/CarterCole/bond/core/tunnel"
)

func newKeyPair(t *testing.T) (priv, pub tunnel.Key) {
	t.Helper()
	priv, err := tunnel.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, err = priv.Public()
	if err != nil {
		t.Fatal(err)
	}
	return priv, pub
}

// serve starts a registry behind a real HTTP server and returns it with a
// join code that points at that server.
func serve(t *testing.T) (*Registry, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	reg, err := OpenRegistry(t.TempDir(), RegistryOptions{Public: []string{strings.TrimPrefix(srv.URL, "http://")}})
	if err != nil {
		t.Fatal(err)
	}
	mux.HandleFunc("/v1/join", reg.ServeJoin)
	return reg, srv
}

func TestJoinCodeRoundTrip(t *testing.T) {
	for _, host := range []string{"203.0.113.5", "bond.example.com", "2001:db8::7"} {
		c, err := NewJoinCode(host, 7777)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseJoinCode("  " + c.String() + "\n")
		if err != nil || got != c {
			t.Fatalf("%s: %v (%s)", host, err, c)
		}
		// Typed by hand in lower case.
		if got, err := ParseJoinCode(strings.ToLower(c.String())); err != nil || got.Token != c.Token {
			t.Fatalf("lower case: %v", err)
		}
	}
	for _, bad := range []string{"", "https://x/y", "bond://host/short", "bond:///AAAA", "bond://host:99999/" + strings.Repeat("A", 32)} {
		if _, err := ParseJoinCode(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestJoinAllocatesAndIsIdempotent(t *testing.T) {
	reg, _ := serve(t)
	var joined []Node
	reg.OnJoin = func(n Node) { joined = append(joined, n) }
	code, err := reg.StandingCode()
	if err != nil {
		t.Fatal(err)
	}
	again, _ := reg.StandingCode()
	if again != code {
		t.Fatal("standing code changed between calls")
	}

	_, pubA := newKeyPair(t)
	_, pubB := newKeyPair(t)
	ctx := context.Background()
	a, err := Join(ctx, code, JoinRequest{Name: "studio-mac", Kind: KindHub, WGPublic: pubA}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Join(ctx, code, JoinRequest{Name: "pixel", Kind: KindRelay, WGPublic: pubB}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Address.String() != "10.207.0.2/16" || b.Address.String() != "10.207.0.3/16" || a.Gateway.String() != "10.207.0.1" {
		t.Fatalf("addresses: %v %v gateway %v", a.Address, b.Address, a.Gateway)
	}
	if a.Session == b.Session || a.Session == 0 || len(a.SessionKey) != 16 || string(a.SessionKey) == string(b.SessionKey) {
		t.Fatal("sessions are not distinct")
	}
	srvPub, _ := reg.PrivateKey().Public()
	if a.ServerWG != srvPub || a.ID != reg.MeshID() || a.MTU != tunnel.DefaultMTU || len(a.UDP) != 1 {
		t.Fatalf("mesh parameters wrong: %+v", a)
	}

	// The same device joining again keeps its identity.
	a2, err := Join(ctx, code, JoinRequest{Name: "studio-mac-renamed", Kind: KindHub, WGPublic: pubA}, nil)
	if err != nil || a2.Address != a.Address || a2.Session != a.Session {
		t.Fatalf("rejoin changed identity: %v %+v", err, a2)
	}
	nodes := reg.Nodes()
	if len(nodes) != 2 || nodes[0].Name != "studio-mac-renamed" || len(joined) != 3 {
		t.Fatalf("registry: %d nodes, %d join callbacks", len(nodes), len(joined))
	}
}

func TestStatePersists(t *testing.T) {
	dir := t.TempDir()
	reg, err := OpenRegistry(dir, RegistryOptions{Public: []string{"203.0.113.5:7777"}})
	if err != nil {
		t.Fatal(err)
	}
	code, _ := reg.StandingCode()
	_, pub := newKeyPair(t)
	reg.mu.Lock()
	_, _, err = reg.admit(reg.st.Tokens[0], JoinRequest{Name: "n", Kind: KindHub, WGPublic: pub})
	reg.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	reg2, err := OpenRegistry(dir, RegistryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	code2, _ := reg2.StandingCode()
	if code2 != code || reg2.MeshID() != reg.MeshID() || reg2.PrivateKey() != reg.PrivateKey() || len(reg2.Nodes()) != 1 {
		t.Fatal("state did not survive a restart")
	}
	// A new public address is picked up and reflected in the code.
	reg3, _ := OpenRegistry(dir, RegistryOptions{Public: []string{"bond.example.com:9000"}})
	code3, _ := reg3.StandingCode()
	if code3.Host != "bond.example.com" || code3.Port != 9000 || code3.Token != code.Token {
		t.Fatalf("public address not updated: %s", code3)
	}
}

func TestJoinRejections(t *testing.T) {
	reg, srv := serve(t)
	ctx := context.Background()
	_, pub := newKeyPair(t)
	req := JoinRequest{Name: "x", Kind: KindHub, WGPublic: pub}
	host := strings.TrimPrefix(srv.URL, "http://")
	h, p, _ := splitHostPort(host)

	// A code the aggregator never issued.
	forged, _ := NewJoinCode(h, p)
	if _, err := Join(ctx, forged, req, nil); err == nil {
		t.Fatal("forged code accepted")
	}

	// Single-use code: second device is refused, first can still rejoin.
	once, _ := reg.NewToken("one", 0, 1)
	if _, err := Join(ctx, once, req, nil); err != nil {
		t.Fatal(err)
	}
	_, pub2 := newKeyPair(t)
	if _, err := Join(ctx, once, JoinRequest{Name: "y", Kind: KindHub, WGPublic: pub2}, nil); err == nil || !strings.Contains(err.Error(), "used up") {
		t.Fatalf("single-use code admitted a second device: %v", err)
	}
	if _, err := Join(ctx, once, req, nil); err != nil {
		t.Fatalf("first device could not rejoin: %v", err)
	}

	// Expired code.
	exp, _ := reg.NewToken("short", time.Nanosecond, 0)
	time.Sleep(2 * time.Millisecond)
	if _, err := Join(ctx, exp, req, nil); err == nil {
		t.Fatal("expired code accepted")
	}

	// Revoked code.
	rev, _ := reg.NewToken("rev", 0, 0)
	if err := reg.RevokeToken(rev.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := Join(ctx, rev, JoinRequest{Name: "z", Kind: KindHub, WGPublic: pub2}, nil); err == nil {
		t.Fatal("revoked code accepted")
	}

	// Bad device kind.
	std, _ := reg.StandingCode()
	if _, err := Join(ctx, std, JoinRequest{Name: "z", Kind: "toaster", WGPublic: pub2}, nil); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

// Someone who can see and alter the exchange, but does not hold the code,
// learns nothing and cannot impersonate the aggregator.
func TestJoinExchangeIsSealed(t *testing.T) {
	reg, _ := serve(t)
	code, _ := reg.StandingCode()
	_, pub := newKeyPair(t)

	env, err := seal(code.Token[:], dirRequest, []byte(`{"name":"n","kind":"hub"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env.Box), "hub") {
		t.Fatal("request is not encrypted")
	}
	if _, err := open(code.Token[:], dirReply, env); err == nil {
		t.Fatal("a request could be replayed as a reply")
	}
	other, _ := NewJoinCode("h", 1)
	if _, err := open(other.Token[:], dirRequest, env); err == nil {
		t.Fatal("opened with the wrong token")
	}

	// An impostor aggregator that does not know the token cannot satisfy the client.
	impostor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fake, _ := seal(other.Token[:], dirReply, []byte(`{"mesh":{"id":"evil"}}`))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"t":"` + fake.Token + `","n":"AAAA","c":"AAAA"}`))
	}))
	defer impostor.Close()
	h, p, _ := splitHostPort(strings.TrimPrefix(impostor.URL, "http://"))
	evil := code
	evil.Host, evil.Port = h, p
	if _, err := Join(context.Background(), evil, JoinRequest{Name: "n", Kind: KindHub, WGPublic: pub}, nil); err == nil {
		t.Fatal("client accepted a reply from an impostor")
	}
}

func TestAddressAllocationSkipsEdges(t *testing.T) {
	reg, err := OpenRegistry(t.TempDir(), RegistryOptions{Network: "10.9.0.0/23", Public: []string{"h:1"}})
	if err != nil {
		t.Fatal(err)
	}
	reg.StandingCode()
	seen := map[netip.Addr]bool{}
	for i := 0; i < 400; i++ {
		_, pub := newKeyPair(t)
		reg.mu.Lock()
		m, _, err := reg.admit(reg.st.Tokens[0], JoinRequest{Name: strconv.Itoa(i), Kind: KindPhone, WGPublic: pub})
		reg.mu.Unlock()
		if err != nil {
			t.Fatalf("node %d: %v", i, err)
		}
		ip := m.Address.Addr()
		if b := ip.As4(); seen[ip] || b[3] == 0 || b[3] == 255 || ip == reg.Gateway() || !reg.Network().Contains(ip) {
			t.Fatalf("bad allocation %v", ip)
		}
		seen[ip] = true
	}
	// A /23 has 510 usable-looking addresses minus the gateway; it must run out cleanly.
	var last error
	for i := 0; i < 200 && last == nil; i++ {
		_, pub := newKeyPair(t)
		reg.mu.Lock()
		_, _, last = reg.admit(reg.st.Tokens[0], JoinRequest{Name: "x", Kind: KindPhone, WGPublic: pub})
		reg.mu.Unlock()
	}
	if last == nil || !strings.Contains(last.Error(), "run out") {
		t.Fatalf("exhaustion not reported: %v", last)
	}
}

func TestNextGenNeverGoesBackwards(t *testing.T) {
	dir := t.TempDir()
	a := NextGen(dir)
	b := NextGen(dir)
	c := NextGen(dir)
	if !(a < b && b < c) {
		t.Fatalf("generations not increasing: %d %d %d", a, b, c)
	}
}

// recordingTransport keeps the last response body it carried.
type recordingTransport struct{ last []byte }

func (r *recordingTransport) RoundTrip(q *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(q)
	if err == nil {
		r.last, _ = io.ReadAll(resp.Body)
		resp.Body = io.NopCloser(bytes.NewReader(r.last))
	}
	return resp, err
}

// replying serves body to every request: something on the path answering
// in place of the aggregator.
func replying(t *testing.T, code JoinCode, body []byte) JoinCode {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	h, p, _ := splitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	code.Host, code.Port = h, p
	return code
}

// A reply recorded on its way to one device must not be accepted by
// another device joining with the same code: it would get the first one's
// address and session key.
func TestJoinReplyIsBoundToItsRequest(t *testing.T) {
	reg, _ := serve(t)
	code, _ := reg.StandingCode()
	_, pubA := newKeyPair(t)
	_, pubB := newKeyPair(t)
	rec := &recordingTransport{}
	if _, err := Join(context.Background(), code, JoinRequest{Name: "A", Kind: KindHub, WGPublic: pubA}, &http.Client{Transport: rec}); err != nil {
		t.Fatal(err)
	}
	_, err := Join(context.Background(), replying(t, code, rec.last), JoinRequest{Name: "B", Kind: KindHub, WGPublic: pubB}, nil)
	if err == nil || !strings.Contains(err.Error(), "not an answer to this request") {
		t.Fatalf("a replayed reply was accepted, or refused for the wrong reason: %v", err)
	}
}

// Devices and aggregators on different protocol versions say so, in a
// reply the older side can read.
func TestJoinAcrossProtocolVersions(t *testing.T) {
	reg, srv := serve(t)
	code, _ := reg.StandingCode()
	_, pub := newKeyPair(t)

	// A version 1 device: no proto in the request, a version 1 reply.
	body, _ := json.Marshal(JoinRequest{Name: "old", Kind: KindHub, WGPublic: pub})
	env, _ := seal(code.Token[:], dirRequest, body)
	raw, _ := json.Marshal(env)
	resp, err := http.Post(srv.URL+"/v1/join", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var renv envelope
	json.NewDecoder(resp.Body).Decode(&renv)
	pt, err := open(code.Token[:], dirReplyV1, renv)
	if err != nil || !strings.Contains(string(pt), "update this device") {
		t.Fatalf("a version 1 device could not read why it was refused: %v %s", err, pt)
	}
	if len(reg.Nodes()) != 0 {
		t.Fatal("a version 1 device was admitted")
	}

	// A version 1 aggregator: this device is told to update it.
	old, _ := seal(code.Token[:], dirReplyV1, []byte(`{"mesh":{"id":"m"}}`))
	oldRaw, _ := json.Marshal(old)
	_, err = Join(context.Background(), replying(t, code, oldRaw), JoinRequest{Name: "n", Kind: KindHub, WGPublic: pub}, nil)
	if err == nil || !strings.Contains(err.Error(), "older version") {
		t.Fatalf("a version 1 aggregator's reply: %v", err)
	}
}

// Rotating the standing code is what stops a removed device from joining
// again with the code it had.
func TestRotateStandingCode(t *testing.T) {
	reg, _ := serve(t)
	old, _ := reg.StandingCode()
	_, pub := newKeyPair(t)
	if _, err := Join(context.Background(), old, JoinRequest{Name: "a", Kind: KindHub, WGPublic: pub}, nil); err != nil {
		t.Fatal(err)
	}
	changed := 0
	reg.OnChange = func() { changed++ }
	fresh, err := reg.RotateStanding()
	if err != nil || fresh.Token == old.Token || changed == 0 {
		t.Fatalf("rotate: %v, new token %v, told the rendezvous %d times", err, fresh.Token != old.Token, changed)
	}
	if again, _ := reg.StandingCode(); again.Token != fresh.Token {
		t.Fatal("standing code is not the rotated one")
	}
	_, pub2 := newKeyPair(t)
	if _, err := Join(context.Background(), old, JoinRequest{Name: "b", Kind: KindHub, WGPublic: pub2}, nil); err == nil {
		t.Fatal("the replaced standing code still admits devices")
	}
	if _, err := Join(context.Background(), fresh, JoinRequest{Name: "b", Kind: KindHub, WGPublic: pub2}, nil); err != nil {
		t.Fatal(err)
	}
	var standing int
	for _, ti := range reg.Tokens() {
		if ti.Standing {
			standing++
		}
	}
	if standing != 1 || len(reg.Nodes()) != 2 {
		t.Fatalf("%d standing codes, %d nodes", standing, len(reg.Nodes()))
	}
}

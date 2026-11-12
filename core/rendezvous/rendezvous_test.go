package rendezvous

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/CarterCole/bond/core/path"
	"github.com/CarterCole/bond/core/wire"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatal(what)
}

func TestCodeRoundTrip(t *testing.T) {
	c, err := NewCode("rv.example.com", 7777)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseCode(c.String())
	if err != nil || got != c {
		t.Fatalf("round trip: %v %v", got, err)
	}
	if !strings.HasPrefix(c.String(), "bond-rv://rv.example.com:7777/") || len(c.ID()) != 12 {
		t.Fatalf("code %s id %s", c, c.ID())
	}
	for _, bad := range []string{"bond://rv.example.com:7777/" + b32.EncodeToString(c.Secret[:]), "bond-rv://rv.example.com/SHORT", "bond-rv:///x", "bond-rv://h:0/" + b32.EncodeToString(c.Secret[:])} {
		if _, err := ParseCode(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestFrameTag(t *testing.T) {
	k := wire.NewKey([16]byte{1, 2, 3})
	b := make([]byte, FrameLen+5)
	copy(b[FrameLen:], "hello")
	frame{typ: frameData, link: 7, handle: 9, seq: 11}.put(b, &k)
	if !isFrame(b) || !verifyFrame(b, &k) {
		t.Fatal("a frame does not verify")
	}
	if f := parseFrame(b); f != (frame{typ: frameData, link: 7, handle: 9, seq: 11}) {
		t.Fatalf("parsed %+v", f)
	}
	for _, i := range []int{0, 4, 8, 12, FrameLen + 2} {
		c := bytes.Clone(b)
		c[i] ^= 1
		if verifyFrame(c, &k) {
			t.Errorf("flipping byte %d went unnoticed", i)
		}
	}
	other := wire.NewKey([16]byte{9})
	if verifyFrame(b, &other) {
		t.Error("verified under the wrong key")
	}
	// A bond datagram is never mistaken for a frame.
	d := make([]byte, 64)
	(&wire.Header{Type: wire.TypeData}).Put(d)
	if isFrame(d) {
		t.Error("bond datagram looks like a frame")
	}
}

// datagram builds a bond datagram for session; the rendezvous does not check tags.
func datagram(session uint32, fromServer bool, payload string) []byte {
	h := wire.Header{Type: wire.TypeData, Session: session}
	if fromServer {
		h.Flags = wire.FlagFromServer
	}
	b := make([]byte, wire.HeaderLen+len(payload))
	h.Put(b)
	copy(b[wire.HeaderLen:], payload)
	return b
}

type rig struct {
	srv  *Server
	cli  *Client
	got  chan string
	hub  *net.UDPConn
	addr netip.AddrPort // the rendezvous's UDP address
}

// start runs a rendezvous and an aggregator end registered with it whose
// handler answers every datagram with "re:" + its payload, and confirms the
// source as the bond does a datagram that verifies.
func start(t *testing.T, sessions []uint32, tokens []string, join func([]byte) (int, []byte)) *rig {
	return startWith(t, Options{}, sessions, tokens, join, func(string) bool { return true })
}

// startWith is start with options for the rendezvous and a say in which
// payloads count as genuine.
func startWith(t *testing.T, o Options, sessions []uint32, tokens []string, join func([]byte) (int, []byte), genuine func(payload string) bool) *rig {
	t.Helper()
	o.Listen, o.Public, o.StateDir, o.Log = "127.0.0.1:0", "127.0.0.1", t.TempDir(), quiet
	srv, err := Start(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	r := &rig{srv: srv, got: make(chan string, 16), addr: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(srv.Port()))}
	r.cli = Dial(ClientOptions{
		Code: srv.Code(), Mesh: "m1", Join: join, Log: quiet,
		Routes: func() ([]uint32, []string) { return sessions, tokens },
		Handler: func(b *path.Buf, from path.Remote) {
			defer path.Put(b)
			h, _ := wire.Parse(b.Bytes())
			payload := string(b.Bytes()[wire.HeaderLen:])
			if c, ok := from.S.(interface{ Confirm() }); ok && genuine(payload) {
				c.Confirm()
			}
			r.got <- payload
			from.Send(datagram(h.Session, true, "re:"+payload))
		},
	})
	t.Cleanup(r.cli.Close)
	eventually(t, "registered with a working link", func() bool {
		st := r.cli.Status()
		return st.State == "registered" && st.LinkUp
	})
	if r.hub, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.hub.Close() })
	return r
}

func (r *rig) send(b []byte) { r.hub.WriteToUDPAddrPort(b, r.addr) }

func (r *rig) reply(t *testing.T) string {
	t.Helper()
	r.hub.SetReadDeadline(time.Now().Add(2 * time.Second))
	b := make([]byte, 2048)
	n, err := r.hub.Read(b)
	if err != nil {
		return ""
	}
	return string(b[wire.HeaderLen:n])
}

func TestForwardsBothWays(t *testing.T) {
	r := start(t, []uint32{42}, nil, nil)
	r.send(datagram(42, false, "ping"))
	select {
	case p := <-r.got:
		if p != "ping" {
			t.Fatalf("aggregator got %q", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("datagram never reached the aggregator")
	}
	if got := r.reply(t); got != "re:ping" {
		t.Fatalf("hub got %q", got)
	}
	st := r.srv.Status()
	if len(st.Aggregators) != 1 || st.Aggregators[0].Sources != 1 || st.Aggregators[0].Link == "" || st.UpBytes == 0 || st.DownBytes == 0 {
		t.Fatalf("rendezvous status: %+v", st)
	}
	// The aggregator learns where the hub socket was seen from, once it has
	// confirmed the socket's traffic.
	r.cli.mu.Lock()
	h := r.cli.cur.handle(1)
	r.cli.mu.Unlock()
	eventually(t, "source notice", func() bool { return r.cli.Status().Sources == 1 && h.addr.Load() != nil })
	if h.String() != r.hub.LocalAddr().String() || h.Via() != r.srv.Code().HostPort() {
		t.Fatalf("sender %q via %q", h.String(), h.Via())
	}
}

func TestRefusesWhatIsNotRegistered(t *testing.T) {
	r := start(t, []uint32{42}, nil, nil)
	before := r.srv.Status().Refused
	r.send(datagram(99, false, "unregistered session"))
	r.send(datagram(42, true, "wrong direction"))
	r.send([]byte("junk"))
	frameish := make([]byte, FrameLen+4)
	frame{typ: frameData, link: 1, handle: 1}.put(frameish, &wire.Key{})
	r.send(frameish) // a frame on a link nobody holds, under no real key
	eventually(t, "refusals counted", func() bool { return r.srv.Status().Refused >= before+4 })
	select {
	case p := <-r.got:
		t.Fatalf("aggregator got %q", p)
	default:
	}

	// The aggregator cannot send to a handle no hub socket holds.
	r.cli.mu.Lock()
	ghost := r.cli.cur.handle(77)
	r.cli.mu.Unlock()
	before = r.srv.Status().Refused
	ghost.SendTo(datagram(42, true, "to nobody"), netip.AddrPort{})
	eventually(t, "send to an unknown handle refused", func() bool { return r.srv.Status().Refused > before })
}

func TestJoinIsForwarded(t *testing.T) {
	r := start(t, nil, []string{"tok1"}, func(body []byte) (int, []byte) {
		if !bytes.Contains(body, []byte(`"t":"tok1"`)) {
			return http.StatusBadRequest, []byte("wrong body\n")
		}
		return http.StatusOK, []byte(`{"sealed":"reply"}`)
	})
	post := func(token string) (int, string) {
		resp, err := http.Post("http://"+r.srv.Code().HostPort()+"/v1/join", "application/json", strings.NewReader(`{"t":"`+token+`","n":"","c":""}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := post("tok1"); code != http.StatusOK || body != `{"sealed":"reply"}` {
		t.Fatalf("join: %d %q", code, body)
	}
	if code, _ := post("nope"); code != http.StatusForbidden {
		t.Fatalf("unknown token: %d", code)
	}
}

func TestWrongPairingCodeIsRefused(t *testing.T) {
	srv, err := Start(Options{Listen: "127.0.0.1:0", Public: "127.0.0.1", StateDir: t.TempDir(), Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	wrong := srv.Code()
	wrong.Secret[0] ^= 1
	c := Dial(ClientOptions{Code: wrong, Mesh: "m1", Log: quiet, Routes: func() ([]uint32, []string) { return nil, nil }})
	defer c.Close()
	eventually(t, "refusal reported", func() bool { return strings.Contains(c.Status().Error, "unknown pairing code") })
	if len(srv.Status().Aggregators) != 0 {
		t.Fatal("registered with the wrong code")
	}
}

func TestPairingSecretPersists(t *testing.T) {
	dir := t.TempDir()
	a, err := Start(Options{Listen: "127.0.0.1:0", Public: "rv.example.com:443", StateDir: dir, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	code := a.Code()
	a.Close()
	b, err := Start(Options{Listen: "127.0.0.1:0", Public: "rv.example.com:443", StateDir: dir, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.Code() != code || code.HostPort() != "rv.example.com:443" {
		t.Fatalf("codes %s and %s", code, b.Code())
	}
}

// Anyone who knows a session id can make sources, from as many addresses as
// they have. They must not be able to crowd out hub sockets the aggregator
// has confirmed.
func TestUnconfirmedSourcesMakeWay(t *testing.T) {
	r := startWith(t, Options{MaxSources: 2}, []uint32{42}, nil, nil, func(p string) bool { return strings.HasPrefix(p, "good") })
	hub := func() *net.UDPConn {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	reaches := func(c *net.UDPConn, payload string) bool {
		t.Helper()
		c.WriteToUDPAddrPort(datagram(42, false, payload), r.addr)
		select {
		case p := <-r.got:
			if p != payload {
				t.Fatalf("aggregator got %q, want %q", p, payload)
			}
			return true
		case <-time.After(500 * time.Millisecond):
			return false
		}
	}
	a, b, c, d := hub(), hub(), hub(), hub()
	if !reaches(a, "good a") {
		t.Fatal("a genuine hub socket was refused")
	}
	eventually(t, "a confirmed", func() bool { return r.cli.Status().Sources == 1 })
	if !reaches(b, "junk b") || !reaches(c, "junk c") {
		t.Fatal("an unconfirmed newcomer was refused while an unconfirmed source could make way")
	}
	if !reaches(a, "good a again") {
		t.Fatal("the confirmed source was pushed out")
	}
	if !reaches(c, "good c") {
		t.Fatal("c lost its place")
	}
	eventually(t, "c confirmed", func() bool { return r.cli.Status().Sources == 2 })
	if reaches(d, "junk d") {
		t.Fatal("a newcomer pushed out a confirmed source")
	}
	if r.srv.Status().Aggregators[0].Sources != 2 {
		t.Fatalf("sources: %+v", r.srv.Status().Aggregators[0])
	}
}

package tunnel

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/CarterCole/bond/core/bond"
	"github.com/CarterCole/bond/core/classify"
	"github.com/CarterCole/bond/core/path"
	"github.com/CarterCole/bond/core/sched"
	"github.com/CarterCole/bond/core/sim"
)

// These tests run the whole stack in one process: an application socket on a
// hub, through a userspace TCP/IP stack, WireGuard, the bond, simulated
// impaired paths, the aggregator's bond and WireGuard, its userspace NAT and
// finally a real TCP/UDP server on loopback standing in for "the Internet".

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

const (
	session = 0xb0ad0001
	hubIP   = "10.207.0.2"
	srvIP   = "10.207.0.1"
)

type world struct {
	t        *testing.T
	hubMux   *bond.Mux
	srvMux   *bond.Mux
	hs, ss   *bond.Session
	hub      *UserStack
	paths    []*sim.Path
	internet netip.AddrPort // any address a hub app might dial; the test NAT maps it to loopback
}

// internetServer speaks a tiny protocol:
//
//	'U'            client uploads until it half-closes; server replies with the byte count
//	'D' + uint64   server sends that many bytes
func internetServer(t *testing.T) (tcpAddr, udpAddr string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var mode [1]byte
				if _, err := io.ReadFull(c, mode[:]); err != nil {
					return
				}
				switch mode[0] {
				case 'U':
					n, _ := io.Copy(io.Discard, c)
					var out [8]byte
					binary.BigEndian.PutUint64(out[:], uint64(n))
					c.Write(out[:])
				case 'D':
					var nb [8]byte
					if _, err := io.ReadFull(c, nb[:]); err != nil {
						return
					}
					io.CopyN(c, zeros{}, int64(binary.BigEndian.Uint64(nb[:])))
				}
			}()
		}
	}()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(buf[:n], from)
		}
	}()
	return ln.Addr().String(), pc.LocalAddr().String()
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func newWorld(t *testing.T, links []sim.Link, policies []sched.Policy, rules *classify.Rules) *world {
	t.Helper()
	w := &world{t: t, internet: netip.MustParseAddrPort("93.184.216.34:443")}
	tcpAddr, udpAddr := internetServer(t)

	hubPriv, _ := NewPrivateKey()
	srvPriv, _ := NewPrivateKey()
	hubPub, _ := hubPriv.Public()
	srvPub, _ := srvPriv.Public()
	key := [16]byte{9, 8, 7}

	onPath := func(side string) func(uint32, bond.PathStatus, string) {
		return func(_ uint32, p bond.PathStatus, ev string) {
			if ev == "up" || ev == "down" {
				t.Logf("[%s] path %s %s (rtt %.0fms loss %.0f%%)", side, p.Name, ev, p.RTTMs, p.Loss*100)
			}
		}
	}
	w.hubMux = bond.New(bond.Options{Role: bond.Hub, Log: quiet, OnPath: onPath("hub")})
	w.srvMux = bond.New(bond.Options{Role: bond.Server, Log: quiet, OnPath: onPath("agg")})
	w.hs = w.hubMux.AddSession(session, key, "hub")
	w.ss = w.srvMux.AddSession(session, key, "hub")

	// Aggregator: userspace NAT whose "Internet" is the loopback server.
	nat := func(ctx context.Context, network string, dst netip.AddrPort) (net.Conn, error) {
		var d net.Dialer
		if network == "udp" {
			return d.DialContext(ctx, "udp", udpAddr)
		}
		return d.DialContext(ctx, "tcp", tcpAddr)
	}
	srvStack, err := NewUserStack(UserStackOptions{Addrs: []netip.Addr{netip.MustParseAddr(srvIP)}, Forward: nat, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	srvCls, _ := classify.New(classify.Rules{})
	srvCls.Learn = true
	srvTun, err := New(srvStack, w.srvMux, Options{PrivateKey: srvPriv, Classifier: srvCls, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	if err := srvTun.SetPeers(true, []Peer{{PublicKey: hubPub, Session: session, AllowedIPs: []netip.Prefix{netip.MustParsePrefix(hubIP + "/32")}}}); err != nil {
		t.Fatal(err)
	}

	// Hub: userspace stack the test dials from.
	w.hub, err = NewUserStack(UserStackOptions{Addrs: []netip.Addr{netip.MustParseAddr(hubIP)}, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	var hubCls *classify.Classifier
	if rules != nil {
		hubCls, err = classify.New(*rules)
		if err != nil {
			t.Fatal(err)
		}
		hubCls.Stamp = true
	}
	hubTun, err := New(w.hub, w.hubMux, Options{PrivateKey: hubPriv, Classifier: hubCls, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	if err := hubTun.SetPeers(true, []Peer{{PublicKey: srvPub, Session: session, AllowedIPs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}}); err != nil {
		t.Fatal(err)
	}

	for i, l := range links {
		pol := sched.PolicyBonded
		if policies != nil {
			pol = policies[i]
		}
		addr := netip.AddrPortFrom(netip.AddrFrom4([4]byte{198, 51, 100, byte(i + 1)}), 4000)
		sp := sim.NewPath(string(rune('A'+i)), l, l, w.hubMux.Receive, w.srvMux.Receive, addr)
		w.paths = append(w.paths, sp)
		w.hs.AddPath(uint8(i), sp.Name, "sim", pol, sp.HubRemote())
	}
	t.Cleanup(func() {
		hubTun.Close()
		srvTun.Close()
		w.hubMux.Close()
		w.srvMux.Close()
		for _, sp := range w.paths {
			sp.Close()
		}
	})
	return w
}

func (w *world) dial(network string) net.Conn {
	w.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := w.hub.DialContext(ctx, network, w.internet.String())
	if err != nil {
		w.t.Fatalf("dial through the bond: %v", err)
	}
	return c
}

// upload pushes n bytes hub → Internet and returns the goodput in Mbit/s.
func (w *world) upload(n int) float64 {
	w.t.Helper()
	c := w.dial("tcp")
	defer c.Close()
	c.SetDeadline(time.Now().Add(90 * time.Second))
	start := time.Now()
	if _, err := c.Write([]byte{'U'}); err != nil {
		w.t.Fatal(err)
	}
	if _, err := io.CopyN(c, zeros{}, int64(n)); err != nil {
		w.t.Fatalf("upload: %v", err)
	}
	c.(interface{ CloseWrite() error }).CloseWrite()
	var got [8]byte
	if _, err := io.ReadFull(c, got[:]); err != nil {
		w.t.Fatalf("upload: reading the server's count: %v", err)
	}
	if binary.BigEndian.Uint64(got[:]) != uint64(n) {
		w.t.Fatalf("server received %d of %d bytes", binary.BigEndian.Uint64(got[:]), n)
	}
	return float64(n) * 8 / time.Since(start).Seconds() / 1e6
}

// download pulls n bytes Internet → hub and returns the goodput in Mbit/s.
func (w *world) download(n int) float64 {
	w.t.Helper()
	c := w.dial("tcp")
	defer c.Close()
	c.SetDeadline(time.Now().Add(90 * time.Second))
	req := []byte{'D', 0, 0, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint64(req[1:], uint64(n))
	start := time.Now()
	if _, err := c.Write(req); err != nil {
		w.t.Fatal(err)
	}
	got, err := io.Copy(io.Discard, c)
	if err != nil || got != int64(n) {
		w.t.Fatalf("download: got %d of %d bytes: %v", got, n, err)
	}
	return float64(n) * 8 / time.Since(start).Seconds() / 1e6
}

func TestTCPAndUDPThroughTheBond(t *testing.T) {
	l := sim.Link{Delay: 5 * time.Millisecond}
	w := newWorld(t, []sim.Link{l, l}, nil, nil)
	w.upload(200_000)
	w.download(200_000)

	u := w.dial("udp")
	defer u.Close()
	msg := []byte("bonded datagram")
	u.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := u.Write(msg); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := u.Read(buf)
	if err != nil || !bytes.Equal(buf[:n], msg) {
		t.Fatalf("udp echo through the bond: %q %v", buf[:n], err)
	}
}

func TestTCPAggregatesPaths(t *testing.T) {
	if testing.Short() {
		t.Skip("throughput test")
	}
	links := []sim.Link{
		{Delay: 10 * time.Millisecond, RateBps: 5e6},
		{Delay: 25 * time.Millisecond, RateBps: 3e6},
		{Delay: 40 * time.Millisecond, RateBps: 4e6},
	}
	single := newWorld(t, links[:1], nil, nil)
	one := single.upload(3_000_000)

	w := newWorld(t, links, nil, nil)
	w.upload(500_000) // let the estimator and TCP warm up
	up := w.upload(6_000_000)
	a := w.ss.Status()
	t.Logf("after upload: aggregator reorder: hold %.0fms, late %d, skipped %d, of %d datagrams", a.HoldMs, a.Reordered, a.Skipped, a.RxPackets)
	down := w.download(6_000_000)
	h := w.hs.Status()
	t.Logf("after download: hub reorder: hold %.0fms, late %d, skipped %d, of %d datagrams", h.HoldMs, h.Reordered, h.Skipped, h.RxPackets)
	st := w.hs.Status()
	t.Logf("best single path (5 Mbit/s): %.2f Mbit/s", one)
	t.Logf("bonded 5+3+4 Mbit/s: upload %.2f Mbit/s, download %.2f Mbit/s", up, down)
	for _, p := range st.Paths {
		t.Logf("  path %s: rtt %.0fms loss %.1f%% capacity %.2f Mbit/s", p.Name, p.RTTMs, p.Loss*100, p.CapacityBps/1e6)
	}
	if up < 1.6*one || down < 1.6*one {
		t.Fatalf("bonding three paths gave upload %.2f / download %.2f Mbit/s against %.2f on the best one alone", up, down, one)
	}
}

func TestTransferSurvivesPathFailure(t *testing.T) {
	links := []sim.Link{
		{Delay: 10 * time.Millisecond, RateBps: 6e6},
		{Delay: 30 * time.Millisecond, RateBps: 4e6},
	}
	w := newWorld(t, links, nil, nil)
	w.upload(200_000)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		w.paths[0].SetDown(true) // the better path dies mid-transfer
	}()
	start := time.Now()
	rate := w.upload(4_000_000) // fails the test if any byte goes missing
	t.Logf("4 MB upload with the 6 Mbit/s path cut after 1.5s: %.2f Mbit/s overall, %.1fs", rate, time.Since(start).Seconds())
}

func TestPrimaryWithBackup(t *testing.T) {
	links := []sim.Link{
		{Delay: 10 * time.Millisecond, RateBps: 8e6},
		{Delay: 30 * time.Millisecond, RateBps: 8e6},
	}
	w := newWorld(t, links, []sched.Policy{sched.PolicyPrimary, sched.PolicyBackup}, nil)
	w.upload(1_500_000)
	w.download(1_500_000)
	if b := w.paths[1].UpBytes() + w.paths[1].DownBytes(); b > 100_000 {
		t.Fatalf("backup path carried %d bytes while the primary was healthy", b)
	}
	w.paths[0].SetDown(true)
	w.upload(1_000_000)
	if b := w.paths[1].UpBytes(); b < 1_000_000 {
		t.Fatalf("backup path carried only %d bytes after the primary died", b)
	}
}

// A hub rule marks one port as priority. Its packets must be duplicated on
// the way up, and — without the aggregator knowing the rule — on the way
// back down too.
func TestPriorityFlowIsRedundantBothWays(t *testing.T) {
	l := sim.Link{Delay: 10 * time.Millisecond}
	w := newWorld(t, []sim.Link{l, l}, nil, &classify.Rules{Ports: []string{"tcp/443"}})

	w.upload(300_000)
	upDups := w.ss.Status().Dups
	if upDups < 100 {
		t.Fatalf("aggregator dropped only %d duplicates: uplink priority traffic was not sent redundantly", upDups)
	}
	w.download(300_000)
	downDups := w.hs.Status().Dups
	if downDups < 100 {
		t.Fatalf("hub dropped only %d duplicates: the aggregator did not mirror priority onto the replies", downDups)
	}
	t.Logf("duplicates absorbed: %d uplink, %d downlink", upDups, downDups)

	// A different port is ordinary traffic.
	time.Sleep(300 * time.Millisecond) // let the priority connections finish closing
	w.internet = netip.MustParseAddrPort("93.184.216.34:8080")
	b0, h0 := w.ss.Status(), w.hs.Status()
	w.upload(300_000)
	b1, h1 := w.ss.Status(), w.hs.Status()
	t.Logf("non-priority upload: hub sent %d bulk, %d realtime, %d control datagrams; aggregator absorbed %d duplicates",
		h1.TxBulk-h0.TxBulk, h1.TxRealtime-h0.TxRealtime, h1.TxControl-h0.TxControl, b1.Dups-b0.Dups)
	if rt := h1.TxRealtime - h0.TxRealtime; rt > 0 {
		t.Fatalf("%d datagrams of a non-priority flow were sent as realtime", rt)
	}
}

var _ = path.MaxPacket

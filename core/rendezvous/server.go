package rendezvous

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/CarterCole/bond/core/path"
	"github.com/CarterCole/bond/core/wire"
)

// Options configure a rendezvous.
type Options struct {
	// Listen is the address for UDP (hubs' datagrams, aggregators' links)
	// and TCP (joins, WebSocket paths, aggregators' control). Default ":7777".
	Listen string
	// Public is how hubs and aggregators reach Listen: "host" or
	// "host:port". It goes into the pairing code.
	Public string
	// StateDir keeps the pairing secret.
	StateDir string
	// MaxSources bounds how many hub sockets one aggregator is reached
	// from at once. Default 1024.
	MaxSources int
	// Idle is how long a hub socket may be silent before its handle is
	// forgotten. Default 60s.
	Idle time.Duration
	Log  *slog.Logger
}

const (
	joinTimeout = 15 * time.Second
	maxPending  = 64 // joins in flight per aggregator
)

// Server is a running rendezvous.
type Server struct {
	o    Options
	code Code
	udp  *path.UDP
	web  *http.Server
	port int

	mu        sync.Mutex
	regs      map[string]*reg // by mesh id
	byLink    map[uint32]*reg
	bySession map[uint32]*reg
	byToken   map[string]*reg
	closed    bool
	done      chan struct{}
	wg        sync.WaitGroup

	refused, upBytes, downBytes atomic.Uint64
}

// reg is one aggregator's registration. It lives as long as the control
// connection that made it.
type reg struct {
	mesh     string
	link     uint32
	keys     keys
	ctl      *ctlConn
	since    time.Time
	linkAddr netip.AddrPort // where its frames come from; unset until the first
	rxSeq    uint32
	rxAny    bool
	txSeq    uint32
	sessions []uint32
	tokens   []string
	sources  map[path.Remote]*source
	byHandle map[uint32]*source
	next     uint32
	pending  map[uint64]chan ctlMsg
	nextJoin uint64

	upBytes, downBytes uint64
}

// source is one hub socket sending to an aggregator: a UDP address and
// port, or a WebSocket connection.
type source struct {
	handle uint32
	remote path.Remote
	last   int64 // unix nanos
}

type state struct {
	Secret  []byte    `json:"secret"`
	Created time.Time `json:"created"`
}

func Start(o Options) (_ *Server, err error) {
	if o.Listen == "" {
		o.Listen = ":" + strconv.Itoa(DefaultPort)
	}
	if o.MaxSources == 0 {
		o.MaxSources = 1024
	}
	if o.Idle == 0 {
		o.Idle = 60 * time.Second
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Public == "" {
		return nil, errors.New("rendezvous: no public address")
	}
	s := &Server{
		o: o, regs: map[string]*reg{}, byLink: map[uint32]*reg{}, bySession: map[uint32]*reg{},
		byToken: map[string]*reg{}, done: make(chan struct{}),
	}
	defer func() {
		if err != nil {
			s.Close()
		}
	}()
	secret, err := loadSecret(o.StateDir)
	if err != nil {
		return nil, err
	}
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
	host, port := strings.Trim(o.Public, "[]"), s.port
	if h, p, err := net.SplitHostPort(o.Public); err == nil {
		host = h
		if port, err = strconv.Atoi(p); err != nil || port <= 0 || port > 65535 {
			ln.Close()
			return nil, fmt.Errorf("bad public address %q", o.Public)
		}
	}
	s.code = Code{Host: host, Port: port}
	copy(s.code.Secret[:], secret)

	hm := http.NewServeMux()
	hm.HandleFunc("/v1/join", s.serveJoin)
	hm.Handle("/v1/ws", path.WSHandler(s.fromHub))
	hm.HandleFunc("/v1/rendezvous", s.serveControl)
	hm.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "bond rendezvous\n")
	})
	s.web = &http.Server{Handler: hm, ReadHeaderTimeout: 10 * time.Second}
	go s.web.Serve(ln)
	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		s.udp.Serve(s.onUDP)
	}()
	go s.reap()
	o.Log.Info("rendezvous running", "listen", o.Listen, "public", s.code.HostPort())
	return s, nil
}

// loadSecret reads the pairing secret, making one the first time.
func loadSecret(dir string) ([]byte, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	file := filepath.Join(dir, "rendezvous.json")
	var st state
	data, err := os.ReadFile(file)
	switch {
	case err == nil:
		if json.Unmarshal(data, &st) != nil || len(st.Secret) != secretLen {
			return nil, fmt.Errorf("%s is damaged", file)
		}
		return st.Secret, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	st = state{Secret: make([]byte, secretLen), Created: time.Now().UTC()}
	rand.Read(st.Secret)
	data, _ = json.MarshalIndent(st, "", "  ")
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return nil, err
	}
	return st.Secret, os.Rename(tmp, file)
}

// Code is the pairing code aggregators register with.
func (s *Server) Code() Code { return s.code }

// Port is the UDP and TCP port the rendezvous listens on.
func (s *Server) Port() int { return s.port }

func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.done)
	for _, r := range s.regs {
		r.ctl.close()
	}
	s.mu.Unlock()
	if s.web != nil {
		s.web.Close()
	}
	if s.udp != nil {
		s.udp.Close()
	}
	s.wg.Wait()
}

func (s *Server) onUDP(b *path.Buf, from path.Remote) {
	if isFrame(b.Bytes()) {
		s.onFrame(b, from.Addr)
		return
	}
	s.fromHub(b, from)
}

// fromHub passes a hub's datagram, from a UDP socket or a WebSocket, along
// the link of the aggregator that registered its session.
func (s *Server) fromHub(b *path.Buf, from path.Remote) {
	defer path.Put(b)
	h, err := wire.Parse(b.Bytes())
	if err != nil || h.Flags&wire.FlagFromServer != 0 || b.N > path.MaxPacket-FrameLen {
		s.refused.Add(1)
		return
	}
	s.mu.Lock()
	r := s.bySession[h.Session]
	if s.closed || r == nil || !r.linkAddr.IsValid() {
		s.mu.Unlock()
		s.refused.Add(1)
		return
	}
	src := r.sources[from]
	if src == nil {
		if len(r.sources) >= s.o.MaxSources {
			s.mu.Unlock()
			s.refused.Add(1)
			return
		}
		r.next++
		src = &source{handle: r.next, remote: from}
		r.sources[from], r.byHandle[src.handle] = src, src
		r.ctl.send(ctlMsg{Type: "source", Handle: src.handle, Addr: from.String()})
	}
	src.last = time.Now().UnixNano()
	n := b.N
	r.txSeq++
	f := frame{typ: frameData, link: r.link, handle: src.handle, seq: r.txSeq}
	dst, key := r.linkAddr, r.keys.link
	r.upBytes += uint64(n)
	s.mu.Unlock()

	// Make room for the frame header in place.
	copy(b.B[FrameLen:], b.B[:n])
	b.N = n + FrameLen
	f.put(b.Bytes(), &key)
	if s.udp.SendTo(b.Bytes(), dst) == nil {
		s.upBytes.Add(uint64(n))
	}
}

// onFrame handles a frame on an aggregator's link.
func (s *Server) onFrame(b *path.Buf, from netip.AddrPort) {
	defer path.Put(b)
	pkt := b.Bytes()
	f := parseFrame(pkt)
	var notice *ctlMsg
	s.mu.Lock()
	r := s.byLink[f.link]
	if r == nil || !verifyFrame(pkt, &r.keys.link) {
		s.mu.Unlock()
		s.refused.Add(1)
		return
	}
	// Only a frame newer than any seen moves the link: a replayed one, from
	// wherever, cannot pull it away from the aggregator.
	if !r.rxAny || wire.SeqLess(r.rxSeq, f.seq) {
		r.rxAny, r.rxSeq = true, f.seq
		if r.linkAddr != from {
			if r.linkAddr.IsValid() {
				s.o.Log.Info("rendezvous: aggregator's link moved", "mesh", r.mesh, "to", from.String())
			}
			r.linkAddr = from
			notice = &ctlMsg{Type: "link", Addr: from.String()}
		}
	}
	switch f.typ {
	case framePing:
		r.txSeq++
		pong := frame{typ: framePong, link: r.link, seq: r.txSeq}
		key := r.keys.link
		ctl := r.ctl
		s.mu.Unlock()
		if notice != nil {
			ctl.send(*notice)
		}
		var out [FrameLen]byte
		pong.put(out[:], &key)
		s.udp.SendTo(out[:], from)
	case frameData:
		src := r.byHandle[f.handle]
		payload := pkt[FrameLen:]
		h, err := wire.Parse(payload)
		// A reply goes only to a socket that has sent something lately, as
		// through a NAT, and only if it is one: nobody can use the
		// rendezvous to send whatever they like wherever they like.
		ok := src != nil && time.Now().UnixNano()-src.last < int64(s.o.Idle) && err == nil && h.Flags&wire.FlagFromServer != 0
		var to path.Remote
		if ok {
			to = src.remote
			r.downBytes += uint64(len(payload))
		}
		ctl := r.ctl
		s.mu.Unlock()
		if notice != nil {
			ctl.send(*notice)
		}
		if !ok {
			s.refused.Add(1)
			return
		}
		if to.Send(payload) == nil {
			s.downBytes.Add(uint64(len(payload)))
		}
	default:
		ctl := r.ctl
		s.mu.Unlock()
		if notice != nil {
			ctl.send(*notice)
		}
	}
}

// reap forgets hub sockets that have gone quiet.
func (s *Server) reap() {
	defer s.wg.Done()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		cutoff := time.Now().Add(-s.o.Idle).UnixNano()
		s.mu.Lock()
		for _, r := range s.regs {
			for h, src := range r.byHandle {
				if src.last < cutoff {
					delete(r.byHandle, h)
					delete(r.sources, src.remote)
					r.ctl.send(ctlMsg{Type: "gone", Handle: h})
				}
			}
		}
		s.mu.Unlock()
	}
}

// serveJoin passes a device's join request to the aggregator that
// registered its token and hands back the answer. Both are sealed under the
// join code; this end sees only which token the request names.
func (s *Server) serveJoin(w http.ResponseWriter, hr *http.Request) {
	if hr.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	data, err := io.ReadAll(io.LimitReader(hr.Body, 16<<10))
	var env struct {
		Token string `json:"t"`
	}
	if err != nil || json.Unmarshal(data, &env) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	r := s.byToken[env.Token]
	if r == nil {
		s.mu.Unlock()
		// What an aggregator would say about a code it does not know.
		http.Error(w, "unknown or expired join code", http.StatusForbidden)
		return
	}
	if len(r.pending) >= maxPending {
		s.mu.Unlock()
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	r.nextJoin++
	id, ch := r.nextJoin, make(chan ctlMsg, 1)
	r.pending[id] = ch
	ctl := r.ctl
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(r.pending, id)
		s.mu.Unlock()
	}()

	if !ctl.send(ctlMsg{Type: "join", ID: id, Body: data}) {
		http.Error(w, "the aggregator is not connected", http.StatusBadGateway)
		return
	}
	select {
	case m := <-ch:
		if m.Status == http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
		} else {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		}
		w.WriteHeader(m.Status)
		w.Write(m.Body)
	case <-time.After(joinTimeout):
		http.Error(w, "the aggregator did not answer", http.StatusGatewayTimeout)
	case <-hr.Context().Done():
	}
}

// serveControl is an aggregator's control connection: pairing, then
// registration and everything after it, until the connection ends.
func (s *Server) serveControl(w http.ResponseWriter, hr *http.Request) {
	c, err := websocket.Accept(w, hr, &websocket.AcceptOptions{
		// Not a browser API: everything after the upgrade is sealed.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	c.SetReadLimit(ctlReadLimit)
	r, err := s.pair(c)
	if err != nil {
		s.o.Log.Debug("rendezvous: pairing failed", "from", hr.RemoteAddr, "err", err)
		c.Close(websocket.StatusPolicyViolation, err.Error())
		return
	}
	defer r.ctl.close()
	// The connection only becomes the mesh's registration once it has sent
	// a sealed message, which proves it is fresh and not a replayed hello.
	m, err := r.ctl.read()
	if err != nil || m.Type != "register" || m.Mesh != r.mesh {
		return
	}
	if !s.register(r, m) {
		return
	}
	s.o.Log.Info("rendezvous: aggregator registered", "mesh", r.mesh, "from", hr.RemoteAddr, "sessions", len(m.Sessions))
	defer s.unregister(r)
	for {
		m, err := r.ctl.read()
		if err != nil {
			s.o.Log.Info("rendezvous: aggregator disconnected", "mesh", r.mesh, "err", err)
			return
		}
		switch m.Type {
		case "register":
			s.register(r, m)
		case "joined":
			s.mu.Lock()
			ch := r.pending[m.ID]
			s.mu.Unlock()
			if ch != nil {
				select {
				case ch <- m:
				default:
				}
			}
		}
	}
}

// pair runs the pairing exchange on a new control connection.
func (s *Server) pair(c *websocket.Conn) (*reg, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ctlTimeout)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		return nil, err
	}
	var m sealedMsg
	if json.Unmarshal(data, &m) != nil {
		return nil, errors.New("malformed hello")
	}
	if m.ID != s.code.ID() {
		return nil, errors.New("unknown pairing code")
	}
	var hello helloBody
	if err := openPairing(s.code.Secret[:], aadHello, m, &hello); err != nil {
		return nil, errors.New("unknown pairing code")
	}
	if len(hello.R) != randLen || hello.Mesh == "" {
		return nil, errors.New("malformed hello")
	}
	rr := make([]byte, randLen)
	rand.Read(rr)
	k, err := deriveKeys(s.code.Secret[:], hello.R, rr)
	if err != nil {
		return nil, err
	}
	r := &reg{
		mesh: hello.Mesh, keys: k, since: time.Now(), sources: map[path.Remote]*source{},
		byHandle: map[uint32]*source{}, pending: map[uint64]chan ctlMsg{},
	}
	s.mu.Lock()
	for r.link == 0 || s.byLink[r.link] != nil {
		var b [4]byte
		rand.Read(b[:])
		r.link = binary.BigEndian.Uint32(b[:])
	}
	s.mu.Unlock()
	out, err := sealPairing(s.code.Secret[:], "", aadWelcome+string(hello.R), welcomeBody{R: rr, Link: r.link})
	if err != nil {
		return nil, err
	}
	if err := c.Write(ctx, websocket.MessageBinary, out); err != nil {
		return nil, err
	}
	r.ctl = newCtl(c, k.down, k.up)
	return r, nil
}

// register makes r the mesh's registration, or updates it, and tells the
// aggregator which of its sessions another aggregator already holds.
func (s *Server) register(r *reg, m ctlMsg) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}
	if old := s.regs[r.mesh]; old != nil && old != r {
		// The same aggregator, reconnected before its old connection was
		// noticed to be gone.
		s.dropLocked(old)
		old.ctl.close()
	}
	s.regs[r.mesh], s.byLink[r.link] = r, r
	for _, id := range r.sessions {
		if s.bySession[id] == r {
			delete(s.bySession, id)
		}
	}
	for _, t := range r.tokens {
		if s.byToken[t] == r {
			delete(s.byToken, t)
		}
	}
	var refused []uint32
	r.sessions, r.tokens = r.sessions[:0], r.tokens[:0]
	for _, id := range m.Sessions {
		if o := s.bySession[id]; o != nil && o != r {
			refused = append(refused, id)
			continue
		}
		s.bySession[id] = r
		r.sessions = append(r.sessions, id)
	}
	for _, t := range m.Tokens {
		if o := s.byToken[t]; o != nil && o != r {
			continue
		}
		s.byToken[t] = r
		r.tokens = append(r.tokens, t)
	}
	s.mu.Unlock()
	if len(refused) > 0 {
		s.o.Log.Warn("rendezvous: sessions already held by another aggregator", "mesh", r.mesh, "sessions", len(refused))
	}
	r.ctl.send(ctlMsg{Type: "registered", Refused: refused})
	return true
}

func (s *Server) unregister(r *reg) {
	s.mu.Lock()
	if s.regs[r.mesh] == r {
		s.dropLocked(r)
	}
	s.mu.Unlock()
}

func (s *Server) dropLocked(r *reg) {
	delete(s.regs, r.mesh)
	delete(s.byLink, r.link)
	for _, id := range r.sessions {
		if s.bySession[id] == r {
			delete(s.bySession, id)
		}
	}
	for _, t := range r.tokens {
		if s.byToken[t] == r {
			delete(s.byToken, t)
		}
	}
	for _, ch := range r.pending {
		select {
		case ch <- ctlMsg{Status: http.StatusBadGateway, Body: []byte("the aggregator went away\n")}:
		default:
		}
	}
}

// ServerStatus is a rendezvous's status.
type ServerStatus struct {
	Public      string             `json:"public"`
	Aggregators []AggregatorStatus `json:"aggregators"`
	UpBytes     uint64             `json:"up_bytes"`   // hubs → aggregators
	DownBytes   uint64             `json:"down_bytes"` // aggregators → hubs
	Refused     uint64             `json:"refused"`    // datagrams and frames nobody registered or that failed checks
}

// AggregatorStatus is one registered aggregator.
type AggregatorStatus struct {
	Mesh      string `json:"mesh"`
	Link      string `json:"link"` // where its link is seen from; empty until its first frame
	Connected int64  `json:"connected_sec"`
	Sessions  int    `json:"sessions"`
	Tokens    int    `json:"tokens"`
	Sources   int    `json:"sources"` // hub sockets reaching it now
	UpBytes   uint64 `json:"up_bytes"`
	DownBytes uint64 `json:"down_bytes"`
}

func (s *Server) Status() ServerStatus {
	st := ServerStatus{Public: s.code.HostPort(), UpBytes: s.upBytes.Load(), DownBytes: s.downBytes.Load(), Refused: s.refused.Load()}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.regs {
		a := AggregatorStatus{
			Mesh: r.mesh, Connected: int64(time.Since(r.since).Seconds()), Sessions: len(r.sessions),
			Tokens: len(r.tokens), Sources: len(r.sources), UpBytes: r.upBytes, DownBytes: r.downBytes,
		}
		if r.linkAddr.IsValid() {
			a.Link = r.linkAddr.String()
		}
		st.Aggregators = append(st.Aggregators, a)
	}
	slices.SortFunc(st.Aggregators, func(a, b AggregatorStatus) int { return strings.Compare(a.Mesh, b.Mesh) })
	return st
}

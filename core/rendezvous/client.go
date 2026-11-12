package rendezvous

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/CarterCole/bond/core/path"
)

// ClientOptions configure an aggregator's end of a rendezvous.
type ClientOptions struct {
	Code Code
	Mesh string
	// Handler gets each datagram the rendezvous passes along, with a remote
	// that sends back the same way. It takes ownership of the buffer.
	Handler path.Handler
	// Join answers a join request the rendezvous forwarded: the request's
	// body in, an HTTP status and the reply's body out.
	Join func(body []byte) (int, []byte)
	// Routes lists what the rendezvous should send here: the mesh's session
	// ids and the ids of its live join tokens.
	Routes func() (sessions []uint32, tokens []string)
	// LinkVia sends the link somewhere other than the rendezvous's own
	// address: tests put a simulated NAT there.
	LinkVia netip.AddrPort
	Log     *slog.Logger
}

const (
	pingIdle   = 25 * time.Second // a link with nothing else crossing it pings this often
	linkDead   = 60 * time.Second // and is reported down when nothing has come back for this long
	maxBackoff = 30 * time.Second
)

// Client keeps an aggregator registered with a rendezvous, reconnecting
// whenever the connection is lost.
type Client struct {
	o       ClientOptions
	changed chan struct{}
	stop    chan struct{}
	once    sync.Once
	wg      sync.WaitGroup

	mu      sync.Mutex
	cur     *session
	lastErr string
}

// session is one control connection and the link that goes with it. Handles
// belong to it: a new connection is a new registration, whose handles mean
// other sockets, so senders from an old one go nowhere.
type session struct {
	c      *Client
	keys   keys
	link   uint32
	ctl    *ctlConn
	udp    *path.UDP
	rv     netip.AddrPort
	since  time.Time
	txSeq  atomic.Uint32
	lastTx atomic.Int64
	lastRx atomic.Int64
	done   chan struct{}
	once   sync.Once

	mu         sync.Mutex
	handles    map[uint32]*Sender
	registered bool
	refused    []uint32
	seen       string
}

// Sender sends to one hub socket through the rendezvous. It is the remote of
// every path that arrives that way.
type Sender struct {
	s    *session
	id   uint32
	addr atomic.Pointer[string]
}

func (h *Sender) SendTo(b []byte, _ netip.AddrPort) error { return h.s.send(frameData, h.id, b) }

// String is the hub socket's address as the rendezvous sees it.
func (h *Sender) String() string {
	if a := h.addr.Load(); a != nil {
		return *a
	}
	return fmt.Sprintf("rendezvous handle %d", h.id)
}

// Via names the rendezvous the path goes through.
func (h *Sender) Via() string { return h.s.c.o.Code.HostPort() }

// Dial starts keeping the aggregator registered.
func Dial(o ClientOptions) *Client {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	c := &Client{o: o, changed: make(chan struct{}, 1), stop: make(chan struct{})}
	c.wg.Add(1)
	go c.run()
	return c
}

// Changed says the routes have changed and should be registered again.
func (c *Client) Changed() {
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

func (c *Client) Close() {
	c.once.Do(func() { close(c.stop) })
	c.mu.Lock()
	if c.cur != nil {
		c.cur.close()
	}
	c.mu.Unlock()
	c.wg.Wait()
}

func (c *Client) stopped() bool {
	select {
	case <-c.stop:
		return true
	default:
		return false
	}
}

func (c *Client) run() {
	defer c.wg.Done()
	backoff := time.Second
	for {
		start := time.Now()
		err := c.connect()
		if c.stopped() {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		c.mu.Lock()
		c.lastErr = err.Error()
		c.mu.Unlock()
		c.o.Log.Warn("rendezvous: connection lost; retrying", "rendezvous", c.o.Code.HostPort(), "err", err, "in", backoff)
		select {
		case <-c.stop:
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (c *Client) register() ctlMsg {
	m := ctlMsg{Type: "register", Mesh: c.o.Mesh}
	m.Sessions, m.Tokens = c.o.Routes()
	return m
}

// connect pairs, registers and serves one control connection until it ends.
func (c *Client) connect() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rv := c.o.LinkVia
	if !rv.IsValid() {
		var err error
		if rv, err = resolve(ctx, c.o.Code.Host, c.o.Code.Port); err != nil {
			return err
		}
	}
	hc := &http.Client{Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext}}
	defer hc.CloseIdleConnections()
	ws, _, err := websocket.Dial(ctx, "ws://"+c.o.Code.HostPort()+"/v1/rendezvous", &websocket.DialOptions{
		HTTPClient: hc, CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return fmt.Errorf("cannot reach the rendezvous at %s: %w", c.o.Code.HostPort(), err)
	}
	ws.SetReadLimit(ctlReadLimit)
	k, link, err := c.pair(ctx, ws)
	if err != nil {
		ws.CloseNow()
		return err
	}
	network := "udp4"
	if rv.Addr().Is6() {
		network = "udp6"
	}
	udp, err := path.ListenUDP(network, ":0", path.BindOpts{})
	if err != nil {
		ws.CloseNow()
		return err
	}
	s := &session{
		c: c, keys: k, link: link, udp: udp, rv: rv, since: time.Now(), done: make(chan struct{}),
		ctl: newCtl(ws, k.up, k.down), handles: map[uint32]*Sender{},
	}
	c.mu.Lock()
	if c.stopped() {
		c.mu.Unlock()
		s.close()
		return errors.New("closed")
	}
	c.cur = s
	c.mu.Unlock()
	defer func() {
		s.close()
		c.mu.Lock()
		c.cur = nil
		c.mu.Unlock()
	}()

	s.ctl.send(c.register())
	go udp.Serve(s.onFrame)
	go s.keepalive()
	for {
		m, err := s.ctl.read()
		if err != nil {
			return err
		}
		s.onCtl(m)
	}
}

// pair runs the pairing exchange; see Server.pair.
func (c *Client) pair(ctx context.Context, ws *websocket.Conn) (keys, uint32, error) {
	secret := c.o.Code.Secret[:]
	ra := make([]byte, randLen)
	rand.Read(ra)
	hello, err := sealPairing(secret, c.o.Code.ID(), aadHello, helloBody{Mesh: c.o.Mesh, R: ra})
	if err != nil {
		return keys{}, 0, err
	}
	if err := ws.Write(ctx, websocket.MessageBinary, hello); err != nil {
		return keys{}, 0, err
	}
	_, data, err := ws.Read(ctx)
	if err != nil {
		var ce websocket.CloseError
		if errors.As(err, &ce) {
			return keys{}, 0, fmt.Errorf("the rendezvous refused: %s", ce.Reason)
		}
		return keys{}, 0, err
	}
	var m sealedMsg
	var w welcomeBody
	if json.Unmarshal(data, &m) != nil || openPairing(secret, aadWelcome+string(ra), m, &w) != nil || len(w.R) != randLen {
		return keys{}, 0, errors.New("the rendezvous's answer was not sealed with this pairing code")
	}
	k, err := deriveKeys(secret, ra, w.R)
	return k, w.Link, err
}

func (s *session) close() {
	s.once.Do(func() {
		close(s.done)
		s.ctl.close()
		s.udp.Close()
	})
}

func (s *session) send(typ byte, handle uint32, payload []byte) error {
	select {
	case <-s.done:
		return net.ErrClosed
	default:
	}
	if len(payload) > path.MaxPacket-FrameLen {
		return errors.New("datagram too large for the rendezvous link")
	}
	b := path.Get()
	defer path.Put(b)
	b.N = FrameLen + copy(b.B[FrameLen:], payload)
	frame{typ: typ, link: s.link, handle: handle, seq: s.txSeq.Add(1)}.put(b.Bytes(), &s.keys.link)
	s.lastTx.Store(time.Now().UnixNano())
	return s.udp.SendTo(b.Bytes(), s.rv)
}

// keepalive pings until the link has proved itself, then whenever it has
// been idle long enough for a NAT to forget it, and re-registers when the
// routes change.
func (s *session) keepalive() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-s.c.changed:
			s.ctl.send(s.c.register())
		case <-t.C:
			if s.lastRx.Load() == 0 || time.Since(time.Unix(0, s.lastTx.Load())) >= pingIdle {
				s.send(framePing, 0, nil)
			}
		}
	}
}

func (s *session) handle(id uint32) *Sender {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.handles[id]
	if h == nil {
		h = &Sender{s: s, id: id}
		s.handles[id] = h
	}
	return h
}

func (s *session) onFrame(b *path.Buf, from path.Remote) {
	pkt := b.Bytes()
	if from.Addr != s.rv || !isFrame(pkt) {
		path.Put(b)
		return
	}
	f := parseFrame(pkt)
	if f.link != s.link || !verifyFrame(pkt, &s.keys.link) {
		path.Put(b)
		return
	}
	s.lastRx.Store(time.Now().UnixNano())
	if f.typ != frameData {
		path.Put(b)
		return
	}
	h := s.handle(f.handle)
	b.N = copy(b.B[:], pkt[FrameLen:])
	s.c.o.Handler(b, path.Remote{S: h})
}

func (s *session) onCtl(m ctlMsg) {
	switch m.Type {
	case "registered":
		s.mu.Lock()
		first := !s.registered
		s.registered, s.refused = true, m.Refused
		s.mu.Unlock()
		if first {
			s.c.o.Log.Info("registered with the rendezvous", "rendezvous", s.c.o.Code.HostPort())
		}
		if len(m.Refused) > 0 {
			s.c.o.Log.Warn("the rendezvous will not route some sessions: another aggregator registered them first; those devices must join again",
				"sessions", len(m.Refused))
		}
	case "join":
		go func() {
			status, body := s.c.o.Join(m.Body)
			// The joined device will send at once: register its session
			// before answering, on the same ordered channel.
			s.ctl.send(s.c.register())
			s.ctl.send(ctlMsg{Type: "joined", ID: m.ID, Status: status, Body: body})
		}()
	case "source":
		a := m.Addr
		s.handle(m.Handle).addr.Store(&a)
	case "gone":
		s.mu.Lock()
		delete(s.handles, m.Handle)
		s.mu.Unlock()
	case "link":
		s.mu.Lock()
		s.seen = m.Addr
		s.mu.Unlock()
	}
}

// ClientStatus is an aggregator's view of its rendezvous.
type ClientStatus struct {
	Rendezvous string   `json:"rendezvous"`
	State      string   `json:"state"` // connecting | pairing | registered
	LinkUp     bool     `json:"link_up"`
	Seen       string   `json:"seen_as,omitempty"` // this end of the link, as the rendezvous sees it
	Sources    int      `json:"sources"`           // hub sockets reaching this aggregator through it
	Refused    []uint32 `json:"refused_sessions,omitempty"`
	Connected  int64    `json:"connected_sec,omitempty"`
	Error      string   `json:"error,omitempty"` // why the last connection ended
}

func (c *Client) Status() ClientStatus {
	c.mu.Lock()
	st := ClientStatus{Rendezvous: c.o.Code.HostPort(), State: "connecting", Error: c.lastErr}
	s := c.cur
	c.mu.Unlock()
	if s == nil {
		return st
	}
	st.State, st.Connected = "pairing", int64(time.Since(s.since).Seconds())
	if rx := s.lastRx.Load(); rx != 0 {
		st.LinkUp = time.Since(time.Unix(0, rx)) < linkDead
	}
	s.mu.Lock()
	if s.registered {
		st.State, st.Error = "registered", ""
	}
	st.Seen, st.Sources, st.Refused = s.seen, len(s.handles), s.refused
	s.mu.Unlock()
	return st
}

// resolve finds the rendezvous's UDP address, preferring IPv4.
func resolve(ctx context.Context, host string, port int) (netip.AddrPort, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		return netip.AddrPortFrom(a.Unmap(), uint16(port)), nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addrs) == 0 {
		return netip.AddrPort{}, fmt.Errorf("cannot look up the rendezvous %s: %w", host, err)
	}
	best := addrs[0]
	for _, a := range addrs {
		if a.Unmap().Is4() {
			best = a
			break
		}
	}
	return netip.AddrPortFrom(best.Unmap(), uint16(port)), nil
}

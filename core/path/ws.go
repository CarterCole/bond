package path

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// The WebSocket transport is the fallback for networks that block or mangle
// UDP (hotel and corporate WiFi, some captive portals). Each bond datagram
// is one binary message. Being TCP underneath it suffers head-of-line
// blocking, so the scheduler will only lean on it as hard as it deserves —
// but a slow path beats no path. Because it is plain HTTP(S) it can also sit
// behind a CDN or tunnel such as Cloudflare.

const (
	wsQueue        = 256
	wsWriteTimeout = 5 * time.Second
	wsPingEvery    = 20 * time.Second // well inside proxy idle timeouts
)

// wsConn adapts one WebSocket to Sender. Sends never block the caller: a
// backed-up connection drops datagrams, as a congested UDP path would.
type wsConn struct {
	c    *websocket.Conn
	out  chan *Buf
	done chan struct{}
	once sync.Once
	name string
}

func newWSConn(c *websocket.Conn, name string) *wsConn {
	c.SetReadLimit(MaxPacket)
	w := &wsConn{c: c, out: make(chan *Buf, wsQueue), done: make(chan struct{}), name: name}
	go w.writer()
	return w
}

func (w *wsConn) String() string { return w.name }

func (w *wsConn) SendTo(b []byte, _ netip.AddrPort) error {
	buf := Get()
	buf.N = copy(buf.B[:], b)
	select {
	case w.out <- buf:
		return nil
	case <-w.done:
		Put(buf)
		return net.ErrClosed
	default:
		Put(buf)
		return errors.New("websocket path backed up")
	}
}

func (w *wsConn) writer() {
	ping := time.NewTicker(wsPingEvery)
	defer ping.Stop()
	for {
		select {
		case <-w.done:
			return
		case b := <-w.out:
			ctx, cancel := context.WithTimeout(context.Background(), wsWriteTimeout)
			err := w.c.Write(ctx, websocket.MessageBinary, b.Bytes())
			cancel()
			Put(b)
			if err != nil {
				w.close()
				return
			}
		case <-ping.C:
			ctx, cancel := context.WithTimeout(context.Background(), wsWriteTimeout)
			err := w.c.Ping(ctx)
			cancel()
			if err != nil {
				w.close()
				return
			}
		}
	}
}

func (w *wsConn) close() {
	w.once.Do(func() {
		close(w.done)
		w.c.CloseNow()
	})
}

// read delivers messages to h until the connection ends.
func (w *wsConn) read(h Handler, from func() Remote) {
	defer w.close()
	for {
		typ, data, err := w.c.Read(context.Background())
		if err != nil {
			return
		}
		if typ != websocket.MessageBinary || len(data) > MaxPacket {
			continue
		}
		b := Get()
		b.N = copy(b.B[:], data)
		h(b, from())
	}
}

// WSHandler serves the aggregator end of the WebSocket transport.
func WSHandler(h Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(rw, r, &websocket.AcceptOptions{
			// Not a browser API: there are no cookies or ambient credentials
			// for a cross-origin page to abuse, and every datagram is
			// authenticated by the bond itself.
			InsecureSkipVerify: true,
			CompressionMode:    websocket.CompressionDisabled,
		})
		if err != nil {
			return
		}
		w := newWSConn(c, "ws:"+r.RemoteAddr)
		w.read(h, func() Remote { return Remote{S: w} })
	})
}

// WSClient is the hub end: a WebSocket to the aggregator that redials
// itself, pinned to one uplink. It is a Sender whose identity survives
// reconnects.
type WSClient struct {
	url  string
	opts BindOpts
	h    Handler
	cur  atomic.Pointer[wsConn]
	stop chan struct{}
	once sync.Once
	// Connected reports whether a connection is currently established.
	Connected atomic.Bool
}

// DialWS starts a self-healing WebSocket path to url ("ws://…" or "wss://…").
func DialWS(url string, o BindOpts, h Handler) *WSClient {
	w := &WSClient{url: url, opts: o, h: h, stop: make(chan struct{})}
	go w.run()
	return w
}

func (w *WSClient) String() string { return w.url }

func (w *WSClient) run() {
	d := &net.Dialer{Timeout: 10 * time.Second, Control: control(w.opts)}
	hc := &http.Client{Transport: &http.Transport{DialContext: d.DialContext, ForceAttemptHTTP2: false}}
	backoff := time.Second
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		c, _, err := websocket.Dial(ctx, w.url, &websocket.DialOptions{HTTPClient: hc, CompressionMode: websocket.CompressionDisabled})
		cancel()
		if err == nil {
			backoff = time.Second
			conn := newWSConn(c, w.url)
			w.cur.Store(conn)
			w.Connected.Store(true)
			go func() {
				select {
				case <-w.stop:
					conn.close()
				case <-conn.done:
				}
			}()
			conn.read(w.h, func() Remote { return Remote{S: w} })
			w.Connected.Store(false)
			w.cur.Store(nil)
		}
		select {
		case <-w.stop:
			hc.CloseIdleConnections()
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 15*time.Second)
	}
}

func (w *WSClient) SendTo(b []byte, to netip.AddrPort) error {
	c := w.cur.Load()
	if c == nil {
		return errors.New("websocket path not connected")
	}
	return c.SendTo(b, to)
}

func (w *WSClient) Close() error {
	w.once.Do(func() { close(w.stop) })
	return nil
}

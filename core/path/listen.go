package path

import (
	"net"
	"net/netip"
	"sync"
)

// Limits on the TCP connections an aggregator or rendezvous holds open for
// joins, WebSocket paths and control. Anyone on the Internet can open them,
// and in userspace mode the same file descriptors carry the mesh's traffic
// out, so they must not be able to take them all.
const (
	MaxConns      = 2048 // in all
	MaxConnsPerIP = 64   // from one address; a mesh's devices need a handful
)

// LimitListener accepts at most total connections at once, and at most
// perIP from any one address. Loopback is exempt from the per-address
// limit: a tunnel such as Cloudflare's delivers everything from there.
// Connections over a limit are closed as soon as they are accepted, so that
// they do not wait in the kernel's queue in front of the ones that are not.
func LimitListener(ln net.Listener, total, perIP int) net.Listener {
	return &limitListener{Listener: ln, total: total, perIP: perIP, byIP: map[netip.Addr]int{}}
}

type limitListener struct {
	net.Listener
	total, perIP int

	mu   sync.Mutex
	n    int
	byIP map[netip.Addr]int
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		var ip netip.Addr
		if a, ok := c.RemoteAddr().(*net.TCPAddr); ok {
			ip = a.AddrPort().Addr().Unmap()
		}
		l.mu.Lock()
		ok := l.n < l.total && (ip.IsLoopback() || l.byIP[ip] < l.perIP)
		if ok {
			l.n++
			l.byIP[ip]++
		}
		l.mu.Unlock()
		if !ok {
			c.Close()
			continue
		}
		return &limitConn{Conn: c, release: func() { l.release(ip) }}, nil
	}
}

func (l *limitListener) release(ip netip.Addr) {
	l.mu.Lock()
	l.n--
	if l.byIP[ip]--; l.byIP[ip] <= 0 {
		delete(l.byIP, ip)
	}
	l.mu.Unlock()
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

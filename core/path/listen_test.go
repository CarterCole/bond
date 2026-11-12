package path

import (
	"net"
	"net/netip"
	"testing"
	"time"
)

// fakeListener hands out pipe ends that claim to come from a given address.
type fakeListener struct{ conns chan net.Conn }

func (f *fakeListener) Accept() (net.Conn, error) {
	c, ok := <-f.conns
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}
func (f *fakeListener) Close() error   { return nil }
func (f *fakeListener) Addr() net.Addr { return &net.TCPAddr{} }

type fromConn struct {
	net.Conn
	from *net.TCPAddr
	shut chan struct{}
}

func (c *fromConn) RemoteAddr() net.Addr { return c.from }
func (c *fromConn) Close() error {
	select {
	case <-c.shut:
	default:
		close(c.shut)
	}
	return c.Conn.Close()
}

func TestLimitListener(t *testing.T) {
	f := &fakeListener{conns: make(chan net.Conn, 16)}
	ln := LimitListener(f, 3, 2)
	dial := func(addr string) *fromConn {
		a, _ := net.Pipe()
		c := &fromConn{Conn: a, from: net.TCPAddrFromAddrPort(netip.MustParseAddrPort(addr)), shut: make(chan struct{})}
		f.conns <- c
		return c
	}
	// One acceptor, as a server has; accept takes what it let through, or
	// nil if nothing arrives (everything queued so far was refused).
	accepted := make(chan net.Conn, 16)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	t.Cleanup(func() { close(f.conns) })
	accept := func() net.Conn {
		select {
		case c := <-accepted:
			return c
		case <-time.After(100 * time.Millisecond):
			return nil
		}
	}
	shut := func(c *fromConn) bool {
		select {
		case <-c.shut:
			return true
		case <-time.After(100 * time.Millisecond):
			return false
		}
	}

	a1 := dial("198.51.100.1:1000")
	first := accept()
	dial("198.51.100.1:1001")
	if accept() == nil {
		t.Fatal("second connection from one address refused")
	}
	over := dial("198.51.100.1:1002")
	if !shut(over) {
		t.Fatal("third connection from one address not closed")
	}
	dial("127.0.0.1:1000") // loopback: no per-address limit, but it counts
	if accept() == nil {
		t.Fatal("loopback refused")
	}
	full := dial("198.51.100.2:1000")
	if !shut(full) {
		t.Fatal("connection over the total not closed")
	}
	first.Close()
	first.Close() // closing twice gives back one place, not two
	dial("198.51.100.3:1000")
	if accept() == nil {
		t.Fatal("place not given back on close")
	}
	if shut(a1) != true {
		t.Fatal("close did not reach the connection")
	}
	if late := dial("198.51.100.4:1000"); !shut(late) {
		t.Fatal("a double close gave back two places")
	}
}

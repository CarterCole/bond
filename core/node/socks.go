package node

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/CarterCole/bond/core/tunnel"
)

// serveSocks is a minimal SOCKS5 proxy (CONNECT, no authentication) whose
// outgoing connections go through the bond. It is how a hub without root
// gets traffic into the tunnel: point a browser, OBS or curl at it.
// Names are resolved through the tunnel too, so nothing leaks around it.
func serveSocks(ln net.Listener, st *tunnel.UserStack, dns []netip.Addr, log *slog.Logger) {
	res := st.Resolver(dns)
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			if err := socksConn(c, st, res); err != nil {
				log.Debug("socks", "err", err)
			}
		}()
	}
}

const (
	socksOK          = 0
	socksFailure     = 1
	socksUnreachable = 4
	socksBadCommand  = 7
	socksBadAddress  = 8
)

func socksReply(c net.Conn, code byte) {
	c.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
}

func socksConn(c net.Conn, st *tunnel.UserStack, res *net.Resolver) error {
	c.SetDeadline(time.Now().Add(30 * time.Second))
	var b [262]byte
	if _, err := io.ReadFull(c, b[:2]); err != nil {
		return err
	}
	if b[0] != 5 {
		return errors.New("not SOCKS5")
	}
	if _, err := io.ReadFull(c, b[:b[1]]); err != nil {
		return err
	}
	if _, err := c.Write([]byte{5, 0}); err != nil { // no authentication
		return err
	}
	if _, err := io.ReadFull(c, b[:4]); err != nil {
		return err
	}
	if b[1] != 1 { // CONNECT only
		socksReply(c, socksBadCommand)
		return errors.New("unsupported SOCKS command")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var addr netip.Addr
	switch b[3] {
	case 1:
		if _, err := io.ReadFull(c, b[:4]); err != nil {
			return err
		}
		addr = netip.AddrFrom4([4]byte(b[:4]))
	case 4:
		if _, err := io.ReadFull(c, b[:16]); err != nil {
			return err
		}
		addr = netip.AddrFrom16([16]byte(b[:16]))
	case 3:
		if _, err := io.ReadFull(c, b[:1]); err != nil {
			return err
		}
		n := int(b[0])
		if _, err := io.ReadFull(c, b[:n]); err != nil {
			return err
		}
		host := string(b[:n])
		// The tunnel is IPv4 inside.
		ips, err := res.LookupNetIP(ctx, "ip4", host)
		if err != nil || len(ips) == 0 {
			socksReply(c, socksUnreachable)
			return errors.New("cannot resolve " + host)
		}
		addr = ips[0]
	default:
		socksReply(c, socksBadAddress)
		return errors.New("unsupported SOCKS address type")
	}
	if _, err := io.ReadFull(c, b[:2]); err != nil {
		return err
	}
	dst := netip.AddrPortFrom(addr.Unmap(), binary.BigEndian.Uint16(b[:2]))

	out, err := st.DialContext(ctx, "tcp", dst.String())
	if err != nil {
		socksReply(c, socksFailure)
		return err
	}
	defer out.Close()
	socksReply(c, socksOK)
	c.SetDeadline(time.Time{})

	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
	}
	go cp(out, c)
	go cp(c, out)
	wg.Wait()
	return nil
}

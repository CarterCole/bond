package path

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// UDP is a UDP socket, optionally pinned to one interface.
type UDP struct {
	c *net.UDPConn
}

// control returns a net.ListenConfig/net.Dialer Control function that hands
// the socket to the platform hook or pins it to o.Interface.
func control(o BindOpts) func(network, address string, c syscall.RawConn) error {
	return func(network, _ string, c syscall.RawConn) error {
		var berr error
		err := c.Control(func(fd uintptr) {
			if h := socketHook.Load(); h != nil {
				if (*h)(int(fd), o.Network) {
					return
				}
				// The platform was asked for a particular uplink and could
				// not provide it, and there is no interface name to pin to
				// instead. An unpinned socket would leave by whatever the
				// default network is — for a relay, straight back out the
				// way the traffic came in. Better no socket than that one.
				if o.Interface == "" && o.Network != "" && o.Network != "lan" {
					berr = fmt.Errorf("network %q is not available", o.Network)
					return
				}
			}
			if o.Interface != "" {
				berr = bindToInterface(int(fd), network, o.Interface)
			}
		})
		if err != nil {
			return err
		}
		return berr
	}
}

// ListenUDP opens a UDP socket. network is "udp", "udp4" or "udp6".
func ListenUDP(network, laddr string, o BindOpts) (*UDP, error) {
	lc := net.ListenConfig{Control: control(o)}
	pc, err := lc.ListenPacket(context.Background(), network, laddr)
	if err != nil {
		return nil, err
	}
	c := pc.(*net.UDPConn)
	// Best effort: the defaults are small enough to drop bursts at a few hundred Mbit/s.
	c.SetReadBuffer(4 << 20)
	c.SetWriteBuffer(4 << 20)
	return &UDP{c: c}, nil
}

func (u *UDP) SendTo(b []byte, to netip.AddrPort) error {
	_, err := u.c.WriteToUDPAddrPort(b, to)
	return err
}

func (u *UDP) LocalAddr() netip.AddrPort {
	return u.c.LocalAddr().(*net.UDPAddr).AddrPort()
}

func (u *UDP) Close() error { return u.c.Close() }

// Serve reads datagrams until the socket is closed.
func (u *UDP) Serve(h Handler) error {
	for {
		b := Get()
		n, from, err := u.c.ReadFromUDPAddrPort(b.B[:])
		if err != nil {
			Put(b)
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			// ICMP errors and interfaces going away surface here; they are
			// the path-health logic's business, not a reason to stop reading.
			time.Sleep(5 * time.Millisecond)
			continue
		}
		b.N = n
		h(b, Remote{S: u, Addr: netip.AddrPortFrom(from.Addr().Unmap(), from.Port())})
	}
}

package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync"
	"syscall"
	"time"

	"golang.zx2c4.com/wireguard/tun"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const nicID = 1

// Dialer opens a real connection for a flow leaving the mesh.
// network is "tcp" or "udp".
type Dialer func(ctx context.Context, network string, dst netip.AddrPort) (net.Conn, error)

// UserStackOptions configure NewUserStack.
type UserStackOptions struct {
	Addrs []netip.Addr // the stack's own addresses inside the tunnel
	MTU   int
	// Forward, when set, makes the stack terminate connections to ANY
	// address and re-originate them with this dialer. That is what turns an
	// aggregator into a NAT gateway without root, a TUN device or iptables.
	Forward Dialer
	Log     *slog.Logger
}

// UserStack is a TCP/IP stack in this process (gVisor netstack) that stands
// in for a kernel TUN device. A hub uses it to offer a proxy without
// administrator rights; an aggregator uses it to route to the Internet from
// an unprivileged container.
type UserStack struct {
	ep     *channel.Endpoint
	stack  *stack.Stack
	notify *channel.NotificationHandle
	events chan tun.Event
	in     chan *buffer.View
	done   chan struct{}
	once   sync.Once
	mtu    int
	fwd    Dialer
	log    *slog.Logger
}

func NewUserStack(o UserStackOptions) (*UserStack, error) {
	if o.MTU == 0 {
		o.MTU = DefaultMTU
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	u := &UserStack{
		ep: channel.New(1024, uint32(o.MTU), ""),
		stack: stack.New(stack.Options{
			NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
			TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4, icmp.NewProtocol6},
			HandleLocal:        o.Forward == nil,
		}),
		events: make(chan tun.Event, 4),
		in:     make(chan *buffer.View, 256),
		done:   make(chan struct{}),
		mtu:    o.MTU,
		fwd:    o.Forward,
		log:    o.Log,
	}
	s := u.stack
	sack := tcpip.TCPSACKEnabled(true)
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &sack)
	// Bonded paths are long and fat compared with gVisor's defaults.
	rcv := tcpip.TCPReceiveBufferSizeRangeOption{Min: 4 << 10, Default: 1 << 20, Max: 8 << 20}
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &rcv)
	snd := tcpip.TCPSendBufferSizeRangeOption{Min: 4 << 10, Default: 1 << 20, Max: 8 << 20}
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &snd)
	cc := tcpip.CongestionControlOption("cubic")
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &cc)

	u.notify = u.ep.AddNotify(u)
	if err := s.CreateNIC(nicID, u.ep); err != nil {
		return nil, fmt.Errorf("userstack: create nic: %v", err)
	}
	for _, a := range o.Addrs {
		proto := ipv4.ProtocolNumber
		if a.Is6() {
			proto = ipv6.ProtocolNumber
		}
		pa := tcpip.ProtocolAddress{Protocol: proto, AddressWithPrefix: tcpip.AddrFromSlice(a.AsSlice()).WithPrefix()}
		if err := s.AddProtocolAddress(nicID, pa, stack.AddressProperties{}); err != nil {
			return nil, fmt.Errorf("userstack: add %v: %v", a, err)
		}
	}
	s.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: nicID},
		{Destination: header.IPv6EmptySubnet, NIC: nicID},
	})

	if o.Forward != nil {
		// Accept packets for every destination and answer as that destination.
		if err := s.SetPromiscuousMode(nicID, true); err != nil {
			return nil, fmt.Errorf("userstack: promiscuous: %v", err)
		}
		if err := s.SetSpoofing(nicID, true); err != nil {
			return nil, fmt.Errorf("userstack: spoofing: %v", err)
		}
		tf := tcp.NewForwarder(s, 0, 2048, u.forwardTCP)
		s.SetTransportProtocolHandler(tcp.ProtocolNumber, tf.HandlePacket)
		uf := udp.NewForwarder(s, u.forwardUDP)
		s.SetTransportProtocolHandler(udp.ProtocolNumber, uf.HandlePacket)
	}
	u.events <- tun.EventUp
	return u, nil
}

// tun.Device

func (u *UserStack) File() *os.File           { return nil }
func (u *UserStack) Name() (string, error)    { return "userstack", nil }
func (u *UserStack) MTU() (int, error)        { return u.mtu, nil }
func (u *UserStack) Events() <-chan tun.Event { return u.events }
func (u *UserStack) BatchSize() int           { return 1 }

func (u *UserStack) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	select {
	case v := <-u.in:
		n, err := v.Read(bufs[0][offset:])
		if err != nil {
			return 0, err
		}
		sizes[0] = n
		return 1, nil
	case <-u.done:
		return 0, os.ErrClosed
	}
}

func (u *UserStack) Write(bufs [][]byte, offset int) (int, error) {
	for _, b := range bufs {
		pkt := b[offset:]
		if len(pkt) == 0 {
			continue
		}
		pb := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(pkt)})
		switch pkt[0] >> 4 {
		case 4:
			u.ep.InjectInbound(header.IPv4ProtocolNumber, pb)
		case 6:
			u.ep.InjectInbound(header.IPv6ProtocolNumber, pb)
		default:
			pb.DecRef()
			return 0, syscall.EAFNOSUPPORT
		}
		pb.DecRef()
	}
	return len(bufs), nil
}

// WriteNotify is called by the channel endpoint when the stack has a packet to send.
func (u *UserStack) WriteNotify() {
	pkt := u.ep.Read()
	if pkt == nil {
		return
	}
	v := pkt.ToView()
	pkt.DecRef()
	select {
	case u.in <- v:
	case <-u.done:
	}
}

func (u *UserStack) Close() error {
	u.once.Do(func() {
		close(u.done)
		u.ep.RemoveNotify(u.notify)
		u.stack.RemoveNIC(nicID)
		u.stack.Close()
		u.ep.Close()
		close(u.events)
	})
	return nil
}

// Sockets inside the tunnel.

func fullAddr(ap netip.AddrPort) (tcpip.FullAddress, tcpip.NetworkProtocolNumber) {
	proto := ipv4.ProtocolNumber
	if ap.Addr().Is6() {
		proto = ipv6.ProtocolNumber
	}
	return tcpip.FullAddress{NIC: nicID, Addr: tcpip.AddrFromSlice(ap.Addr().AsSlice()), Port: ap.Port()}, proto
}

// DialContext connects to an ip:port through the tunnel. network is "tcp" or "udp".
func (u *UserStack) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return nil, fmt.Errorf("userstack: dial %s: need ip:port: %w", address, err)
	}
	fa, proto := fullAddr(netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()))
	switch network {
	case "tcp", "tcp4", "tcp6":
		return gonet.DialContextTCP(ctx, u.stack, fa, proto)
	case "udp", "udp4", "udp6":
		return gonet.DialUDP(u.stack, nil, &fa, proto)
	}
	return nil, fmt.Errorf("userstack: unsupported network %q", network)
}

// ListenTCP accepts connections arriving through the tunnel.
func (u *UserStack) ListenTCP(ap netip.AddrPort) (net.Listener, error) {
	fa, proto := fullAddr(ap)
	return gonet.ListenTCP(u.stack, fa, proto)
}

// ListenUDP receives datagrams arriving through the tunnel.
func (u *UserStack) ListenUDP(ap netip.AddrPort) (net.PacketConn, error) {
	fa, proto := fullAddr(ap)
	return gonet.DialUDP(u.stack, &fa, nil, proto)
}

// Resolver looks names up through the tunnel using the given DNS servers.
func (u *UserStack) Resolver(servers []netip.Addr) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var last error
			for _, s := range servers {
				c, err := u.DialContext(ctx, network, netip.AddrPortFrom(s, 53).String())
				if err == nil {
					return c, nil
				}
				last = err
			}
			if last == nil {
				last = errors.New("userstack: no DNS servers configured")
			}
			return nil, last
		},
	}
}

// Forwarding (aggregator in userspace mode).

func addrPort(a tcpip.Address, port uint16) netip.AddrPort {
	ip, _ := netip.AddrFromSlice(a.AsSlice())
	return netip.AddrPortFrom(ip.Unmap(), port)
}

func (u *UserStack) forwardTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	dst := addrPort(id.LocalAddress, id.LocalPort)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		out, err := u.fwd(ctx, "tcp", dst)
		cancel()
		if err != nil {
			u.log.Debug("forward tcp refused", "dst", dst, "err", err)
			r.Complete(true)
			return
		}
		var wq waiter.Queue
		ep, terr := r.CreateEndpoint(&wq)
		if terr != nil {
			r.Complete(true)
			out.Close()
			return
		}
		r.Complete(false)
		splice(gonet.NewTCPConn(&wq, ep), out)
	}()
}

type closeWriter interface{ CloseWrite() error }

// splice copies both ways until both directions are finished.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		if cw, ok := dst.(closeWriter); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
	a.Close()
	b.Close()
}

const udpIdle = 60 * time.Second

func (u *UserStack) forwardUDP(r *udp.ForwarderRequest) {
	id := r.ID()
	dst := addrPort(id.LocalAddress, id.LocalPort)
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	if terr != nil {
		return
	}
	in := gonet.NewUDPConn(&wq, ep)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err := u.fwd(ctx, "udp", dst)
		cancel()
		if err != nil {
			u.log.Debug("forward udp refused", "dst", dst, "err", err)
			in.Close()
			return
		}
		var wg sync.WaitGroup
		wg.Add(2)
		pump := func(dst, src net.Conn) {
			defer wg.Done()
			buf := make([]byte, 65535)
			for {
				src.SetReadDeadline(time.Now().Add(udpIdle))
				n, err := src.Read(buf)
				if err != nil {
					// Unblock the other direction too.
					dst.SetReadDeadline(time.Now())
					return
				}
				if _, err := dst.Write(buf[:n]); err != nil {
					return
				}
			}
		}
		go pump(out, in)
		go pump(in, out)
		wg.Wait()
		in.Close()
		out.Close()
	}()
}

// InternetDialer is the Forward dialer an aggregator uses: plain sockets on
// the host, refusing destinations a mesh member has no business reaching
// through it — the host's own loopback and link-local space, which on cloud
// machines includes the instance metadata service.
func InternetDialer() Dialer {
	d := &net.Dialer{Timeout: 10 * time.Second}
	return func(ctx context.Context, network string, dst netip.AddrPort) (net.Conn, error) {
		a := dst.Addr()
		if a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
			return nil, fmt.Errorf("destination %s not allowed", a)
		}
		return d.DialContext(ctx, network, dst.String())
	}
}

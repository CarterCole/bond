// Package path provides the transports a bond can send datagrams over and
// the plumbing they share: pooled buffers, remote addressing and pinning a
// socket to one physical interface.
package path

import (
	"net/netip"
	"sync"
	"sync/atomic"
)

// MaxPacket is the largest datagram a path carries: bond header + WireGuard
// overhead + the largest tunnel MTU we allow (1500), with room to spare.
const MaxPacket = 1700

// Buf is a pooled datagram buffer.
type Buf struct {
	B [MaxPacket]byte
	N int
}

func (b *Buf) Bytes() []byte { return b.B[:b.N] }

var pool = sync.Pool{New: func() any { return new(Buf) }}

func Get() *Buf  { return pool.Get().(*Buf) }
func Put(b *Buf) { pool.Put(b) }

// Sender is one end of a transport: a UDP socket, a WebSocket, a simulated link.
// Implementations must be comparable (pointers) and safe for concurrent use.
type Sender interface {
	SendTo(b []byte, to netip.AddrPort) error
}

// Remote is where to send to reach the peer over one path.
type Remote struct {
	S    Sender
	Addr netip.AddrPort
}

func (r Remote) Send(b []byte) error { return r.S.SendTo(b, r.Addr) }

func (r Remote) String() string {
	if s, ok := r.S.(interface{ String() string }); ok && !r.Addr.IsValid() {
		return s.String()
	}
	return r.Addr.String()
}

// Handler receives one datagram and takes ownership of b.
type Handler func(b *Buf, from Remote)

// SocketHook lets the host platform claim a freshly created socket before it
// is used. Android needs this to exempt the socket from the VPN and attach it
// to a specific Network (wifi, cellular); it returns true when it has done
// so. network is the logical name the path was created with.
type SocketHook func(fd int, network string) bool

var socketHook atomic.Pointer[SocketHook]

// SetSocketHook installs (or, with nil, removes) the platform hook. Install
// it before any path is opened.
func SetSocketHook(h SocketHook) {
	if h == nil {
		socketHook.Store(nil)
		return
	}
	socketHook.Store(&h)
}

// BindOpts says which uplink a socket must use.
type BindOpts struct {
	Interface string // OS interface name to pin to; "" lets the OS route
	Network   string // logical network name handed to the SocketHook
}

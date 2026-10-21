package bond

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"

	"golang.zx2c4.com/wireguard/conn"

	"github.com/CarterCole/bond/core/path"
	"github.com/CarterCole/bond/core/wire"
)

// Endpoint is the address WireGuard knows a peer by: not an IP and port, but
// a whole bonded session. It stays the same however the paths underneath
// come, go or roam.
type Endpoint struct {
	m  *Mux
	id uint32
}

func (e *Endpoint) ClearSrc()           {}
func (e *Endpoint) SrcToString() string { return "" }
func (e *Endpoint) DstToString() string { return EndpointString(e.id) }
func (e *Endpoint) SrcIP() netip.Addr   { return netip.Addr{} }

// DstToBytes feeds WireGuard's cookie MAC; it only has to be stable per peer.
func (e *Endpoint) DstToBytes() []byte {
	a := e.DstIP().As16()
	return a[:]
}

// DstIP keys WireGuard's handshake rate limiter. Each session gets its own
// synthetic address so one noisy session cannot throttle the others.
func (e *Endpoint) DstIP() netip.Addr {
	a := [16]byte{0: 0xfd, 1: 0xb0, 2: 0x0d}
	binary.BigEndian.PutUint32(a[12:], e.id)
	return netip.AddrFrom16(a)
}

// EndpointString is the WireGuard endpoint string for a session, as accepted
// by the Bind's ParseEndpoint.
func EndpointString(session uint32) string { return fmt.Sprintf("bond:%08x", session) }

var errNoSession = errors.New("bond: no such session")

// WireGuard message types (first byte of every datagram WireGuard emits).
const (
	wgInitiation = 1
	wgResponse   = 2
	wgCookie     = 3
	wgTransport  = 4
	wgKeepalive  = 32 // a transport message with no payload is exactly this long
)

type muxBind struct{ m *Mux }

// Bind returns the conn.Bind to hand to wireguard-go's device.NewDevice.
func (m *Mux) Bind() conn.Bind { return muxBind{m} }

func (b muxBind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) {
	m := b.m
	m.bindMu.Lock()
	defer m.bindMu.Unlock()
	if m.bindOpen {
		return nil, 0, conn.ErrBindAlreadyOpen
	}
	m.bindOpen = true
	m.bindClosed = make(chan struct{})
	closed := m.bindClosed
	// One receive function: the reorder buffers have already merged every
	// path into a single ordered stream, and splitting it again would undo that.
	recv := func(packets [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
		var it rxItem
		select {
		case it = <-m.rxq:
		case <-closed:
			return 0, net.ErrClosed
		}
		n := 0
		for {
			sizes[n] = copy(packets[n], it.b.B[wire.HeaderLen:it.b.N])
			eps[n] = it.ep
			path.Put(it.b)
			n++
			if n == len(packets) {
				return n, nil
			}
			select {
			case it = <-m.rxq:
			default:
				return n, nil
			}
		}
	}
	return []conn.ReceiveFunc{recv}, port, nil
}

func (b muxBind) Close() error {
	m := b.m
	m.bindMu.Lock()
	defer m.bindMu.Unlock()
	if m.bindOpen {
		m.bindOpen = false
		close(m.bindClosed)
	}
	return nil
}

func (b muxBind) SetMark(uint32) error { return nil }

func (b muxBind) BatchSize() int { return batchSize }

func (b muxBind) ParseEndpoint(s string) (conn.Endpoint, error) {
	var id uint32
	if _, err := fmt.Sscanf(s, "bond:%08x", &id); err != nil {
		return nil, fmt.Errorf("bond: endpoint %q is not of the form bond:<session>", s)
	}
	return &Endpoint{m: b.m, id: id}, nil
}

func (b muxBind) Send(bufs [][]byte, ep conn.Endpoint) error {
	e, ok := ep.(*Endpoint)
	if !ok {
		return conn.ErrWrongEndpointType
	}
	s := b.m.Session(e.id)
	if s == nil {
		return errNoSession
	}
	for _, buf := range bufs {
		if len(buf) == 0 {
			continue
		}
		class := wire.ClassControl
		if buf[0] == wgTransport && len(buf) != wgKeepalive {
			class = s.Classes.Pop(len(buf))
		}
		s.Send(buf, class)
	}
	return nil
}

// ClassQueue carries each datagram's scheduling class from where it is known
// (the plaintext packet read off the TUN) to where it is needed (the
// encrypted datagram WireGuard hands the Bind). WireGuard keeps a peer's
// packets in order and its padding is deterministic, so the two ends of the
// queue are matched up by position and encrypted length. A mismatch — say
// WireGuard dropped a packet while handshaking — costs a datagram its
// class, never its delivery.
type ClassQueue struct {
	mu   sync.Mutex
	def  wire.Class
	ring [classRing]classHint
	head uint32
	n    uint32
}

type classHint struct {
	wgLen uint16
	class wire.Class
}

const (
	classRing = 1024
	classScan = 64
)

// SetDefault sets the class for datagrams the queue knows nothing about.
func (q *ClassQueue) SetDefault(c wire.Class) {
	q.mu.Lock()
	q.def = c
	q.mu.Unlock()
}

// Push records that a plaintext packet of this class is about to be
// encrypted into a WireGuard datagram wgLen bytes long.
func (q *ClassQueue) Push(wgLen int, c wire.Class) {
	q.mu.Lock()
	if q.n == classRing {
		q.head++
		q.n--
	}
	q.ring[(q.head+q.n)%classRing] = classHint{uint16(wgLen), c}
	q.n++
	q.mu.Unlock()
}

// Pop returns the class recorded for the next datagram of this length.
func (q *ClassQueue) Pop(wgLen int) wire.Class {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := uint32(0); i < min(q.n, classScan); i++ {
		if h := q.ring[(q.head+i)%classRing]; int(h.wgLen) == wgLen {
			q.head += i + 1
			q.n -= i + 1
			return h.class
		}
	}
	return q.def
}

// WGLen is the size of the WireGuard datagram that carries an n-byte packet
// on a tunnel with the given MTU (16-byte header, payload padded to a
// multiple of 16 but never beyond the MTU, 16-byte tag).
func WGLen(n, mtu int) int {
	last := n
	if mtu > 0 && last > mtu {
		last %= mtu
	}
	padded := (last + 15) &^ 15
	if mtu > 0 && padded > mtu {
		padded = mtu
	}
	return 32 + n + (padded - last)
}

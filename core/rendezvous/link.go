// Package rendezvous lets an aggregator that nothing can reach — behind a
// router that will not forward a port, behind carrier NAT — serve hubs
// anyway. The aggregator dials out to a rendezvous on a public host and keeps
// two connections open to it: a WebSocket for control (registration, joins,
// notices) and a UDP link for bonded traffic. Hubs send their datagrams to
// the rendezvous as if it were the aggregator, and it passes them along the
// link.
//
// The rendezvous can read none of it. Datagrams are sealed by WireGuard and
// tagged with session keys it never sees, and the join exchange is sealed
// under the join code. What it can do is route: by the session id in each
// datagram's header and the token id in each join request, both of which the
// aggregator registers with it.
//
// docs/TOPOLOGY.md has the design.
package rendezvous

import (
	"context"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/chacha20poly1305"

	"github.com/CarterCole/bond/core/wire"
)

// DefaultPort is the rendezvous's port: UDP for hubs' datagrams and
// aggregators' links, TCP for joins, WebSocket paths and control.
const DefaultPort = 7777

const secretLen = 20 // as a join code's token: 160 bits

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// Code is a pairing code: where the rendezvous is, and a secret that an
// aggregator must hold to register with it.
//
//	bond-rv://rv.example.com:7777/MFRGGZDFMZTWQ2LKNNWG23TPOBYXE43U
//
// Like a join code, the secret never crosses the network: it keys the
// encryption of the pairing exchange, from which each connection's keys are
// derived.
type Code struct {
	Host   string
	Port   int
	Secret [secretLen]byte
}

func NewCode(host string, port int) (Code, error) {
	c := Code{Host: host, Port: port}
	_, err := rand.Read(c.Secret[:])
	return c, err
}

func (c Code) String() string {
	return "bond-rv://" + c.HostPort() + "/" + b32.EncodeToString(c.Secret[:])
}

// HostPort is the rendezvous's address, for UDP and TCP alike.
func (c Code) HostPort() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

// ID names the secret on the wire without revealing it.
func (c Code) ID() string {
	h := sha256.Sum256(append([]byte("bond rendezvous id v1\x00"), c.Secret[:]...))
	return hex.EncodeToString(h[:6])
}

func ParseCode(s string) (Code, error) {
	var c Code
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Scheme != "bond-rv" || u.Host == "" {
		return c, errors.New("not a pairing code: expected bond-rv://host:port/SECRET")
	}
	c.Host, c.Port = u.Hostname(), DefaultPort
	if p := u.Port(); p != "" {
		if c.Port, err = strconv.Atoi(p); err != nil || c.Port <= 0 || c.Port > 65535 {
			return c, errors.New("pairing code has a bad port")
		}
	}
	raw, err := b32.DecodeString(strings.ToUpper(strings.Trim(u.Path, "/")))
	if err != nil || len(raw) != secretLen {
		return c, errors.New("pairing code has a damaged secret")
	}
	copy(c.Secret[:], raw)
	return c, nil
}

// A link frame carries one datagram between rendezvous and aggregator:
//
//	 0       1        5          9          13         21
//	+-------+--------+----------+----------+----------+-------------------
//	| type  |  link  |  handle  | link seq |   tag    | bond datagram ...
//	+-------+--------+----------+----------+----------+-------------------
//
// link says which aggregator's link this is, handle which hub socket the
// datagram came from or is going to, and the tag is SipHash-2-4 over the
// whole frame keyed by the link key. A bond datagram has its version (1) in
// the top four bits of its first byte; a frame has 0xB there, so the two
// share the rendezvous's UDP port.
const (
	frameData byte = 0xB1
	framePing byte = 0xB2 // aggregator → rendezvous: keeps the NAT mapping, proves the link
	framePong byte = 0xB3

	FrameLen = 21
	frameTag = 13
)

type frame struct {
	typ    byte
	link   uint32
	handle uint32
	seq    uint32
}

func isFrame(b []byte) bool { return len(b) >= FrameLen && b[0]>>4 == 0xB }

func parseFrame(b []byte) frame {
	return frame{typ: b[0], link: binary.BigEndian.Uint32(b[1:]), handle: binary.BigEndian.Uint32(b[5:]), seq: binary.BigEndian.Uint32(b[9:])}
}

// put writes f into the first FrameLen bytes of b and tags all of b, so the
// payload must already be in place.
func (f frame) put(b []byte, k *wire.Key) {
	b[0] = f.typ
	binary.BigEndian.PutUint32(b[1:], f.link)
	binary.BigEndian.PutUint32(b[5:], f.handle)
	binary.BigEndian.PutUint32(b[9:], f.seq)
	clear(b[frameTag:FrameLen])
	binary.LittleEndian.PutUint64(b[frameTag:], wire.Sum(k, b))
}

// verifyFrame checks b's tag, leaving b unchanged.
func verifyFrame(b []byte, k *wire.Key) bool {
	got := binary.LittleEndian.Uint64(b[frameTag:])
	clear(b[frameTag:FrameLen])
	want := wire.Sum(k, b)
	binary.LittleEndian.PutUint64(b[frameTag:], got)
	return got == want
}

// Pairing. The aggregator opens the control WebSocket and sends a hello
// sealed under the pairing secret, carrying fresh randomness; the rendezvous
// answers with a welcome sealed the same way, bound to the hello, carrying
// its own. Both derive the connection's keys from the secret and both
// randomnesses, so a recorded hello replayed later leads nowhere: whoever
// replays it cannot derive the keys the next message must be sealed with.
const (
	aadHello   = "bond rendezvous hello v1"
	aadWelcome = "bond rendezvous welcome v1"
	randLen    = 32
)

type sealedMsg struct {
	ID    string `json:"id,omitempty"` // the pairing code's id (hello only)
	Nonce []byte `json:"n"`
	Box   []byte `json:"c"`
}

type helloBody struct {
	Mesh string `json:"mesh"`
	R    []byte `json:"r"`
}

type welcomeBody struct {
	R    []byte `json:"r"`
	Link uint32 `json:"link"`
}

func pairingAEAD(secret []byte) (cipher.AEAD, error) {
	k, err := hkdf.Key(sha256.New, secret, nil, "bond rendezvous pairing v1", chacha20poly1305.KeySize)
	if err != nil {
		return nil, err
	}
	return chacha20poly1305.NewX(k)
}

func sealPairing(secret []byte, id, aad string, v any) ([]byte, error) {
	a, err := pairingAEAD(secret)
	if err != nil {
		return nil, err
	}
	pt, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	m := sealedMsg{ID: id, Nonce: make([]byte, a.NonceSize())}
	rand.Read(m.Nonce)
	m.Box = a.Seal(nil, m.Nonce, pt, []byte(aad))
	return json.Marshal(m)
}

func openPairing(secret []byte, aad string, m sealedMsg, v any) error {
	a, err := pairingAEAD(secret)
	if err != nil {
		return err
	}
	if len(m.Nonce) != a.NonceSize() {
		return errors.New("bad nonce")
	}
	pt, err := a.Open(nil, m.Nonce, m.Box, []byte(aad))
	if err != nil {
		return errors.New("not sealed with this pairing code")
	}
	return json.Unmarshal(pt, v)
}

// keys are one control connection's: the link key that tags frames both
// ways, and a key for each direction of control messages.
type keys struct {
	link     wire.Key
	up, down cipher.AEAD // aggregator → rendezvous, rendezvous → aggregator
}

func deriveKeys(secret, ra, rr []byte) (keys, error) {
	salt := append(append([]byte(nil), ra...), rr...)
	m, err := hkdf.Key(sha256.New, secret, salt, "bond rendezvous session v1", 16+2*chacha20poly1305.KeySize)
	if err != nil {
		return keys{}, err
	}
	var lk [16]byte
	copy(lk[:], m)
	up, err := chacha20poly1305.New(m[16:48])
	if err != nil {
		return keys{}, err
	}
	down, err := chacha20poly1305.New(m[48:80])
	if err != nil {
		return keys{}, err
	}
	return keys{link: wire.NewKey(lk), up: up, down: down}, nil
}

// ctlMsg is a control message. Which fields mean anything depends on Type:
//
//	register    aggregator → rendezvous: what to route to it (a full snapshot)
//	registered  rendezvous → aggregator: sessions held by someone else
//	join        rendezvous → aggregator: a device's sealed join request
//	joined      aggregator → rendezvous: the answer, as HTTP status and body
//	source      rendezvous → aggregator: a new hub socket, its handle and address
//	gone        rendezvous → aggregator: a handle forgotten after going quiet
//	link        rendezvous → aggregator: where the link is seen from
type ctlMsg struct {
	Type     string   `json:"type"`
	Mesh     string   `json:"mesh,omitempty"`
	Sessions []uint32 `json:"sessions,omitempty"`
	Tokens   []string `json:"tokens,omitempty"`
	Refused  []uint32 `json:"refused,omitempty"`
	ID       uint64   `json:"id,omitempty"`
	Status   int      `json:"status,omitempty"`
	Body     []byte   `json:"body,omitempty"`
	Handle   uint32   `json:"handle,omitempty"`
	Addr     string   `json:"addr,omitempty"`
}

const (
	ctlQueue     = 256
	ctlTimeout   = 10 * time.Second
	ctlPingEvery = 20 * time.Second
	ctlReadLimit = 1 << 20 // a registration lists every session in a mesh
)

// ctlConn is the sealed control channel over one WebSocket. Every message is
// sealed under its direction's key with a counter for nonce, and the reader
// insists on the next counter, so nothing can be replayed, reordered or
// dropped unnoticed. Sends never block: the queue is drained by one writer.
type ctlConn struct {
	c          *websocket.Conn
	seal, open cipher.AEAD
	out        chan ctlMsg
	done       chan struct{}
	once       sync.Once
	got        uint64 // reader's counter
}

func newCtl(c *websocket.Conn, seal, open cipher.AEAD) *ctlConn {
	k := &ctlConn{c: c, seal: seal, open: open, out: make(chan ctlMsg, ctlQueue), done: make(chan struct{})}
	go k.writer()
	return k
}

func nonce(n uint64) []byte {
	b := make([]byte, chacha20poly1305.NonceSize)
	binary.BigEndian.PutUint64(b[4:], n)
	return b
}

// send queues m, reporting false when the connection is closed or backed up.
func (k *ctlConn) send(m ctlMsg) bool {
	select {
	case <-k.done:
		return false
	default:
	}
	select {
	case k.out <- m:
		return true
	default:
		return false
	}
}

func (k *ctlConn) writer() {
	ping := time.NewTicker(ctlPingEvery)
	defer ping.Stop()
	var sent uint64
	for {
		select {
		case <-k.done:
			return
		case m := <-k.out:
			pt, _ := json.Marshal(m)
			box := k.seal.Seal(nil, nonce(sent), pt, nil)
			sent++
			ctx, cancel := context.WithTimeout(context.Background(), ctlTimeout)
			err := k.c.Write(ctx, websocket.MessageBinary, box)
			cancel()
			if err != nil {
				k.close()
				return
			}
		case <-ping.C:
			// A peer that vanished without closing (a home line dropping)
			// is noticed here rather than whenever TCP gives up.
			ctx, cancel := context.WithTimeout(context.Background(), ctlTimeout)
			err := k.c.Ping(ctx)
			cancel()
			if err != nil {
				k.close()
				return
			}
		}
	}
}

// read returns the next message. Only one goroutine may read.
func (k *ctlConn) read() (ctlMsg, error) {
	var m ctlMsg
	typ, data, err := k.c.Read(context.Background())
	if err != nil {
		return m, err
	}
	if typ != websocket.MessageBinary {
		return m, errors.New("control: unexpected text message")
	}
	pt, err := k.open.Open(nil, nonce(k.got), data, nil)
	if err != nil {
		return m, errors.New("control: message failed authentication")
	}
	k.got++
	if err := json.Unmarshal(pt, &m); err != nil {
		return m, errors.New("control: malformed message")
	}
	return m, nil
}

func (k *ctlConn) close() {
	k.once.Do(func() {
		close(k.done)
		k.c.CloseNow()
	})
}

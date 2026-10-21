package control

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/CarterCole/bond/core/tunnel"
)

// DefaultNetwork is the address range of a mesh. It sits in 10/8 well away
// from the ranges home routers, phone hotspots and Tailscale (100.64/10) use.
const DefaultNetwork = "10.207.0.0/16"

// Token is one join code's secret as the aggregator remembers it.
type Token struct {
	ID      string    `json:"id"`
	Secret  []byte    `json:"secret"`
	Note    string    `json:"note,omitempty"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires,omitzero"`
	MaxUses int       `json:"max_uses,omitempty"` // 0 = unlimited
	Uses    int       `json:"uses"`
}

// Node is one registered member of the mesh.
type Node struct {
	ID         int            `json:"id"`
	Name       string         `json:"name"`
	Kind       string         `json:"kind"`
	Platform   string         `json:"platform,omitempty"`
	WGPublic   tunnel.Key     `json:"wg_public"`
	IP         netip.Addr     `json:"ip"`
	Session    uint32         `json:"session"`
	SessionKey []byte         `json:"session_key"`
	Routes     []netip.Prefix `json:"routes,omitempty"` // LAN prefixes reachable through this node
	Joined     time.Time      `json:"joined"`
}

func (n *Node) Key() (k [16]byte) {
	copy(k[:], n.SessionKey)
	return k
}

// state is the aggregator's persistent memory: one JSON file.
type state struct {
	MeshID    string       `json:"mesh_id"`
	WGPrivate tunnel.Key   `json:"wg_private"`
	Network   netip.Prefix `json:"network"`
	Public    []string     `json:"public"` // host:port devices should send bonded traffic to
	WS        []string     `json:"ws,omitempty"`
	MTU       int          `json:"mtu"`
	DNS       []netip.Addr `json:"dns"`
	Tokens    []*Token     `json:"tokens"`
	Nodes     []*Node      `json:"nodes"`
}

// Registry is the aggregator's view of who is in the mesh.
type Registry struct {
	mu   sync.Mutex
	file string
	st   state

	// OnJoin is called, outside the lock, whenever a node is added or its
	// details change, so the data plane can be told.
	OnJoin func(n Node)
}

// RegistryOptions seed a new mesh. They are ignored when state already exists,
// except Public and WS, which update it when given.
type RegistryOptions struct {
	Network string
	Public  []string
	WS      []string
	MTU     int
	DNS     []netip.Addr
}

// OpenRegistry loads the mesh state in dir, creating a new mesh if there is none.
func OpenRegistry(dir string, o RegistryOptions) (*Registry, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	r := &Registry{file: filepath.Join(dir, "server.json")}
	err := LoadJSON(r.file, &r.st)
	switch {
	case err == nil:
		changed := false
		if len(o.Public) > 0 && strings.Join(o.Public, ",") != strings.Join(r.st.Public, ",") {
			r.st.Public, changed = o.Public, true
		}
		if o.WS != nil && strings.Join(o.WS, ",") != strings.Join(r.st.WS, ",") {
			r.st.WS, changed = o.WS, true
		}
		if changed {
			return r, r.save()
		}
		return r, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("reading %s: %w", r.file, err)
	}

	if o.Network == "" {
		o.Network = DefaultNetwork
	}
	network, err := netip.ParsePrefix(o.Network)
	if err != nil || !network.Addr().Is4() || network.Bits() > 24 {
		return nil, fmt.Errorf("mesh network %q must be an IPv4 prefix of /24 or larger", o.Network)
	}
	if o.MTU == 0 {
		o.MTU = tunnel.DefaultMTU
	}
	if len(o.DNS) == 0 {
		o.DNS = []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("1.0.0.1")}
	}
	priv, err := tunnel.NewPrivateKey()
	if err != nil {
		return nil, err
	}
	var id [6]byte
	rand.Read(id[:])
	r.st = state{
		MeshID:    hex.EncodeToString(id[:]),
		WGPrivate: priv,
		Network:   network.Masked(),
		Public:    o.Public,
		WS:        o.WS,
		MTU:       o.MTU,
		DNS:       o.DNS,
	}
	return r, r.save()
}

func (r *Registry) save() error { return SaveJSON(r.file, &r.st) }

func (r *Registry) MeshID() string         { return r.st.MeshID }
func (r *Registry) PrivateKey() tunnel.Key { return r.st.WGPrivate }
func (r *Registry) Network() netip.Prefix  { return r.st.Network }
func (r *Registry) MTU() int               { return r.st.MTU }

// Gateway is the aggregator's own address inside the mesh: the first host.
func (r *Registry) Gateway() netip.Addr { return r.st.Network.Addr().Next() }

func (r *Registry) Public() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.st.Public...)
}

// Nodes returns a copy of the registered nodes.
func (r *Registry) Nodes() []Node {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Node, len(r.st.Nodes))
	for i, n := range r.st.Nodes {
		out[i] = *n
	}
	return out
}

// NewToken mints a join code. ttl 0 never expires; maxUses 0 is unlimited.
func (r *Registry) NewToken(note string, ttl time.Duration, maxUses int) (JoinCode, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.st.Public) == 0 {
		return JoinCode{}, errors.New("the aggregator's public address is not set")
	}
	hp, err := netip.ParseAddrPort(r.st.Public[0])
	host, port := "", DefaultPort
	if err == nil {
		host, port = hp.Addr().String(), int(hp.Port())
	} else if h, p, ok := splitHostPort(r.st.Public[0]); ok {
		host, port = h, p
	} else {
		return JoinCode{}, fmt.Errorf("bad public address %q", r.st.Public[0])
	}
	code, err := NewJoinCode(host, port)
	if err != nil {
		return JoinCode{}, err
	}
	t := &Token{ID: code.ID(), Secret: code.Token[:], Note: note, Created: time.Now().UTC(), MaxUses: maxUses}
	if ttl > 0 {
		t.Expires = t.Created.Add(ttl)
	}
	r.st.Tokens = append(r.st.Tokens, t)
	return code, r.save()
}

// StandingCode returns the mesh's reusable join code, minting it on first use.
// It is what `bondd server` prints every time it starts.
func (r *Registry) StandingCode() (JoinCode, error) {
	r.mu.Lock()
	for _, t := range r.st.Tokens {
		if t.Note == "standing" && len(r.st.Public) > 0 {
			var code JoinCode
			copy(code.Token[:], t.Secret)
			if h, p, ok := splitHostPort(r.st.Public[0]); ok {
				code.Host, code.Port = h, p
				r.mu.Unlock()
				return code, nil
			}
		}
	}
	r.mu.Unlock()
	return r.NewToken("standing", 0, 0)
}

// RevokeToken invalidates a join code by id. Devices already joined stay joined.
func (r *Registry) RevokeToken(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, t := range r.st.Tokens {
		if t.ID == id {
			r.st.Tokens = append(r.st.Tokens[:i], r.st.Tokens[i+1:]...)
			return r.save()
		}
	}
	return fmt.Errorf("no join code with id %s", id)
}

// RemoveNode expels a node. The caller must also drop it from the data plane.
func (r *Registry) RemoveNode(id int) (Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, n := range r.st.Nodes {
		if n.ID == id {
			r.st.Nodes = append(r.st.Nodes[:i], r.st.Nodes[i+1:]...)
			return *n, r.save()
		}
	}
	return Node{}, fmt.Errorf("no node with id %d", id)
}

func splitHostPort(s string) (string, int, bool) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return s, DefaultPort, s != ""
	}
	var port int
	if _, err := fmt.Sscanf(s[i+1:], "%d", &port); err != nil || port <= 0 || port > 65535 {
		return "", 0, false
	}
	return strings.Trim(s[:i], "[]"), port, true
}

// admit registers (or re-registers) a node. Joining again with the same
// WireGuard key returns the same identity, so a device can re-scan a code
// without ending up with two addresses.
func (r *Registry) admit(t *Token, req JoinRequest) (*Mesh, *Node, error) {
	if req.WGPublic == (tunnel.Key{}) {
		return nil, nil, errors.New("missing WireGuard key")
	}
	switch req.Kind {
	case KindHub, KindPhone, KindRelay:
	default:
		return nil, nil, fmt.Errorf("unknown device kind %q", req.Kind)
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		req.Name = req.Kind
	}
	if len(req.Name) > 64 {
		req.Name = req.Name[:64]
	}

	var node *Node
	for _, n := range r.st.Nodes {
		if n.WGPublic == req.WGPublic {
			node = n
		}
	}
	if node == nil {
		ip, err := r.freeIP()
		if err != nil {
			return nil, nil, err
		}
		node = &Node{ID: r.nextID(), WGPublic: req.WGPublic, IP: ip, Session: r.freeSession(), SessionKey: make([]byte, 16), Joined: time.Now().UTC()}
		rand.Read(node.SessionKey)
		r.st.Nodes = append(r.st.Nodes, node)
		t.Uses++
	}
	node.Name, node.Kind, node.Platform = req.Name, req.Kind, req.Platform
	if err := r.save(); err != nil {
		return nil, nil, err
	}
	pub, err := r.st.WGPrivate.Public()
	if err != nil {
		return nil, nil, err
	}
	return &Mesh{
		ID:         r.st.MeshID,
		ServerWG:   pub,
		Address:    netip.PrefixFrom(node.IP, r.st.Network.Bits()),
		Gateway:    r.st.Network.Addr().Next(),
		Session:    node.Session,
		SessionKey: node.SessionKey,
		UDP:        r.st.Public,
		WS:         r.st.WS,
		MTU:        r.st.MTU,
		DNS:        r.st.DNS,
	}, node, nil
}

func (r *Registry) nextID() int {
	id := 1
	for _, n := range r.st.Nodes {
		id = max(id, n.ID+1)
	}
	return id
}

func (r *Registry) freeIP() (netip.Addr, error) {
	used := map[netip.Addr]bool{}
	for _, n := range r.st.Nodes {
		used[n.IP] = true
	}
	// .0 is the network, .1 the aggregator.
	for ip := r.st.Network.Addr().Next().Next(); r.st.Network.Contains(ip); ip = ip.Next() {
		if b := ip.As4(); b[3] == 0 || b[3] == 255 {
			continue // keep clear of addresses that look like network/broadcast
		}
		if !used[ip] {
			return ip, nil
		}
	}
	return netip.Addr{}, errors.New("the mesh has run out of addresses")
}

func (r *Registry) freeSession() uint32 {
	for {
		var b [4]byte
		rand.Read(b[:])
		id := binary.BigEndian.Uint32(b[:])
		if id == 0 {
			continue
		}
		clash := false
		for _, n := range r.st.Nodes {
			clash = clash || n.Session == id
		}
		if !clash {
			return id
		}
	}
}

// ServeJoin handles POST /v1/join.
func (r *Registry) ServeJoin(w http.ResponseWriter, hr *http.Request) {
	if hr.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	data, err := io.ReadAll(io.LimitReader(hr.Body, 16<<10))
	var env envelope
	if err != nil || json.Unmarshal(data, &env) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	r.mu.Lock()
	var tok *Token
	for _, t := range r.st.Tokens {
		if t.ID == env.Token {
			tok = t
		}
	}
	// Unknown, expired and exhausted codes all look the same from outside.
	now := time.Now()
	if tok == nil || (!tok.Expires.IsZero() && now.After(tok.Expires)) {
		r.mu.Unlock()
		http.Error(w, "unknown or expired join code", http.StatusForbidden)
		return
	}
	pt, err := open(tok.Secret, dirRequest, env)
	if err != nil {
		r.mu.Unlock()
		http.Error(w, "unknown or expired join code", http.StatusForbidden)
		return
	}
	var reply joinReply
	var joined *Node
	var req JoinRequest
	if err := json.Unmarshal(pt, &req); err != nil {
		reply.Error = "malformed request"
	} else if known := r.known(req.WGPublic); !known && tok.MaxUses > 0 && tok.Uses >= tok.MaxUses {
		reply.Error = "this join code has been used up"
	} else if mesh, node, err := r.admit(tok, req); err != nil {
		reply.Error = err.Error()
	} else {
		reply.Mesh = mesh
		cp := *node
		joined = &cp
	}
	secret := tok.Secret
	r.mu.Unlock()

	// From here the reply is sealed: only the holder of the code can read
	// even the error.
	body, _ := json.Marshal(reply)
	out, err := seal(secret, dirReply, body)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
	if joined != nil && r.OnJoin != nil {
		r.OnJoin(*joined)
	}
}

func (r *Registry) known(pub tunnel.Key) bool {
	for _, n := range r.st.Nodes {
		if n.WGPublic == pub {
			return true
		}
	}
	return false
}

// Package control is how nodes get into a mesh and how the aggregator keeps
// track of them: join codes, the join exchange, and the persistent registry
// of nodes with their keys, addresses and sessions.
package control

import (
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

// DefaultPort is the aggregator's port: UDP for bonded traffic, TCP for the
// join exchange and the WebSocket fallback transport.
const DefaultPort = 7777

const tokenLen = 20 // 160 bits: far too many to guess, short enough for a QR code

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// JoinCode is everything a new device needs to find the aggregator and prove
// it was invited: where to connect, and a secret both sides know.
//
//	bond://203.0.113.5:7777/MFRGGZDFMZTWQ2LKNNWG23TPOBYXE43U
//
// The secret never crosses the network. It keys the encryption of the join
// exchange, so only someone holding the code can read the mesh's parameters
// or get a device registered, and a device knows it reached the real
// aggregator because nobody else could have produced the reply.
type JoinCode struct {
	Host  string
	Port  int
	Token [tokenLen]byte
}

func NewJoinCode(host string, port int) (JoinCode, error) {
	j := JoinCode{Host: host, Port: port}
	_, err := rand.Read(j.Token[:])
	return j, err
}

func (j JoinCode) String() string {
	return "bond://" + net.JoinHostPort(j.Host, strconv.Itoa(j.Port)) + "/" + b32.EncodeToString(j.Token[:])
}

// HostPort is the aggregator address, for both the join exchange (TCP) and
// bonded traffic (UDP).
func (j JoinCode) HostPort() string { return net.JoinHostPort(j.Host, strconv.Itoa(j.Port)) }

func ParseJoinCode(s string) (JoinCode, error) {
	var j JoinCode
	s = strings.TrimSpace(s)
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "bond" || u.Host == "" {
		return j, errors.New("not a join code: expected bond://host:port/TOKEN")
	}
	j.Host = u.Hostname()
	j.Port = DefaultPort
	if p := u.Port(); p != "" {
		if j.Port, err = strconv.Atoi(p); err != nil || j.Port <= 0 || j.Port > 65535 {
			return j, errors.New("join code has a bad port")
		}
	}
	raw, err := b32.DecodeString(strings.ToUpper(strings.Trim(u.Path, "/")))
	if err != nil || len(raw) != tokenLen {
		return j, errors.New("join code has a damaged token")
	}
	copy(j.Token[:], raw)
	return j, nil
}

// tokenID names a token on the wire without revealing it.
func tokenID(token []byte) string {
	h := sha256.Sum256(append([]byte("bond token id v1\x00"), token...))
	return hex.EncodeToString(h[:6])
}

func (j JoinCode) ID() string { return tokenID(j.Token[:]) }

// sealer encrypts the join exchange under a key derived from the token.
func sealer(token []byte) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, token, nil, "bond join v1", chacha20poly1305.KeySize)
	if err != nil {
		return nil, err
	}
	return chacha20poly1305.NewX(key)
}

// envelope is a sealed join request or reply as it appears on the wire.
type envelope struct {
	Token string `json:"t"` // token id
	Nonce []byte `json:"n"`
	Box   []byte `json:"c"`
}

// The direction is bound into the ciphertext so a request can never be
// replayed back as a reply.
const (
	dirRequest = "bond join request v1"
	dirReply   = "bond join reply v1"
)

func seal(token []byte, dir string, plaintext []byte) (envelope, error) {
	a, err := sealer(token)
	if err != nil {
		return envelope{}, err
	}
	e := envelope{Token: tokenID(token), Nonce: make([]byte, a.NonceSize())}
	if _, err := rand.Read(e.Nonce); err != nil {
		return envelope{}, err
	}
	e.Box = a.Seal(nil, e.Nonce, plaintext, []byte(dir))
	return e, nil
}

func open(token []byte, dir string, e envelope) ([]byte, error) {
	a, err := sealer(token)
	if err != nil {
		return nil, err
	}
	if len(e.Nonce) != a.NonceSize() {
		return nil, errors.New("bad nonce")
	}
	pt, err := a.Open(nil, e.Nonce, e.Box, []byte(dir))
	if err != nil {
		return nil, fmt.Errorf("could not decrypt: wrong or revoked join code")
	}
	return pt, nil
}

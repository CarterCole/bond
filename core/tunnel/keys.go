package tunnel

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// Key is a WireGuard (Curve25519) private or public key.
type Key [32]byte

// NewPrivateKey generates a WireGuard private key.
func NewPrivateKey() (Key, error) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return Key{}, err
	}
	return Key(k.Bytes()), nil
}

// Public derives the public key of a private key.
func (k Key) Public() (Key, error) {
	priv, err := ecdh.X25519().NewPrivateKey(k[:])
	if err != nil {
		return Key{}, err
	}
	return Key(priv.PublicKey().Bytes()), nil
}

// String is the standard base64 form used by WireGuard tools.
func (k Key) String() string { return base64.StdEncoding.EncodeToString(k[:]) }

func (k Key) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

func (k *Key) UnmarshalText(b []byte) error {
	raw, err := base64.StdEncoding.DecodeString(string(b))
	if err != nil || len(raw) != 32 {
		return fmt.Errorf("not a WireGuard key")
	}
	copy(k[:], raw)
	return nil
}

func ParseKey(s string) (Key, error) {
	var k Key
	return k, k.UnmarshalText([]byte(s))
}

package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/CarterCole/bond/core/tunnel"
)

// Node kinds.
const (
	KindHub   = "hub"   // owns uplinks and bonds them
	KindPhone = "phone" // a phone bonding its own wifi and cellular
	KindRelay = "relay" // only lends its uplink to hubs
)

// JoinRequest is what a new device tells the aggregator about itself.
type JoinRequest struct {
	Name     string     `json:"name"`
	Kind     string     `json:"kind"`
	Platform string     `json:"platform,omitempty"`
	WGPublic tunnel.Key `json:"wg_public"`
}

// Mesh is everything a node needs to take part. It is the reply to a join
// and is kept in the node's config from then on.
type Mesh struct {
	ID         string       `json:"id"`
	ServerWG   tunnel.Key   `json:"server_wg_public"`
	Address    netip.Prefix `json:"address"` // this node's tunnel address and the mesh's prefix length
	Gateway    netip.Addr   `json:"gateway"` // the aggregator's tunnel address
	Session    uint32       `json:"session"`
	SessionKey []byte       `json:"session_key"`
	UDP        []string     `json:"udp"`          // aggregator host:port for bonded traffic
	WS         []string     `json:"ws,omitempty"` // WebSocket fallback URLs
	MTU        int          `json:"mtu"`
	DNS        []netip.Addr `json:"dns,omitempty"`
}

func (m *Mesh) Key() (k [16]byte, err error) {
	if len(m.SessionKey) != 16 {
		return k, errors.New("mesh config has a bad session key")
	}
	copy(k[:], m.SessionKey)
	return k, nil
}

// joinReply carries either the mesh or a reason.
type joinReply struct {
	Mesh  *Mesh  `json:"mesh,omitempty"`
	Error string `json:"error,omitempty"`
}

// Join performs the join exchange with the aggregator named in code.
func Join(ctx context.Context, code JoinCode, req JoinRequest, hc *http.Client) (*Mesh, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	env, err := seal(code.Token[:], dirRequest, body)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(env)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+code.HostPort()+"/v1/join", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("could not reach the aggregator at %s: %w", code.HostPort(), err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		// Anything but a sealed reply means the aggregator did not recognise the code.
		return nil, fmt.Errorf("join refused: %s", strings.TrimSpace(string(data)))
	}
	var renv envelope
	if err := json.Unmarshal(data, &renv); err != nil {
		return nil, errors.New("join: unintelligible reply; is that address really an aggregator?")
	}
	pt, err := open(code.Token[:], dirReply, renv)
	if err != nil {
		return nil, fmt.Errorf("join: the reply was not from a holder of this join code: %w", err)
	}
	var reply joinReply
	if err := json.Unmarshal(pt, &reply); err != nil {
		return nil, err
	}
	if reply.Error != "" {
		return nil, errors.New("join refused: " + reply.Error)
	}
	if reply.Mesh == nil {
		return nil, errors.New("join: empty reply")
	}
	return reply.Mesh, nil
}

// NextGen returns a process generation number that is larger than any
// returned before for this state directory, even if the clock went
// backwards (think of a Raspberry Pi booting without a battery-backed
// clock). Peers use it to tell a restart from a replay.
func NextGen(dir string) uint64 {
	gen := uint64(time.Now().UnixMilli())
	if dir == "" {
		return gen
	}
	file := filepath.Join(dir, "generation")
	if b, err := os.ReadFile(file); err == nil {
		if last, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); err == nil && last >= gen {
			gen = last + 1
		}
	}
	os.WriteFile(file, []byte(strconv.FormatUint(gen, 10)+"\n"), 0o600)
	return gen
}

// writeFileAtomic replaces name with data without ever leaving a torn file.
func writeFileAtomic(name string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(name), filepath.Base(name)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), name)
}

// SaveJSON writes v to name atomically with owner-only permissions; the
// files it is used for all hold keys.
func SaveJSON(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(name, append(data, '\n'), 0o600)
}

func LoadJSON(name string, v any) error {
	data, err := os.ReadFile(name)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

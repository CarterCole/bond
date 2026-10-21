// Package node assembles the pieces into things you run: an aggregator, a
// hub and a relay. The daemons and the phone apps are thin shells around it.
package node

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"

	"github.com/CarterCole/bond/core/classify"
	"github.com/CarterCole/bond/core/control"
	"github.com/CarterCole/bond/core/sched"
	"github.com/CarterCole/bond/core/tunnel"
)

// Version is stamped by the build (-ldflags "-X .../node.Version=…").
var Version = "dev"

// DefaultStateDir is where keys and mesh state live: a system directory when
// running as root (as a service does), the user's config directory otherwise.
func DefaultStateDir() string {
	if os.Geteuid() == 0 {
		if runtime.GOOS == "darwin" {
			return "/usr/local/var/bond"
		}
		return "/var/lib/bond"
	}
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "bond")
	}
	return ".bond"
}

// Config is a joined device's identity and settings, kept in node.json.
type Config struct {
	Name      string            `json:"name"`
	Kind      string            `json:"kind"`
	WGPrivate tunnel.Key        `json:"wg_private"`
	Mesh      *control.Mesh     `json:"mesh"`
	Paths     map[string]string `json:"paths,omitempty"`   // path name → policy chosen by the user
	Priority  classify.Rules    `json:"priority,omitzero"` // what counts as realtime
	Relays    []string          `json:"relays,omitempty"`  // relays to use besides discovered ones
	// Resolved is where the aggregator's name last resolved to. A device
	// whose DNS goes through the tunnel cannot look the aggregator up while
	// the tunnel is down — which is exactly when it needs to.
	Resolved []string    `json:"resolved,omitempty"`
	Share    ShareConfig `json:"share,omitzero"` // relay role settings
	file     string
	mu       sync.Mutex
}

// ShareConfig are a relay's settings.
type ShareConfig struct {
	Port int `json:"port,omitempty"`
}

func configFile(dir string) string { return filepath.Join(dir, "node.json") }

// LoadConfig reads a joined device's config. The error wraps os.ErrNotExist
// if the device has not joined a mesh yet.
func LoadConfig(dir string) (*Config, error) {
	c := &Config{file: configFile(dir)}
	if err := control.LoadJSON(c.file, c); err != nil {
		return nil, err
	}
	if c.Mesh == nil {
		return nil, fmt.Errorf("%s has no mesh in it", c.file)
	}
	return c, nil
}

// ParseConfig reads a config from memory; Save is then a no-op unless the
// config is given a home with SetFile. Phones keep their config in app storage.
func ParseConfig(data []byte) (*Config, error) {
	c := &Config{}
	if err := jsonUnmarshal(data, c); err != nil {
		return nil, err
	}
	if c.Mesh == nil {
		return nil, errors.New("config has no mesh in it")
	}
	return c, nil
}

func (c *Config) Save() error {
	if c.file == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return control.SaveJSON(c.file, c)
}

// JSON serialises the config, keys included.
func (c *Config) JSON() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, _ := jsonMarshal(c)
	return b
}

// Join registers this device with the aggregator named by the join code and
// returns its new config. Nothing is written to disk unless dir is set.
func Join(ctx context.Context, dir, code, name, kind string) (*Config, error) {
	jc, err := control.ParseJoinCode(code)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name, _ = os.Hostname()
	}
	if kind == "" {
		kind = control.KindHub
	}
	c := &Config{Name: name, Kind: kind}
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		c.file = configFile(dir)
		// Re-joining keeps the device's key, and so its identity in the mesh.
		if old, err := LoadConfig(dir); err == nil {
			c.WGPrivate, c.Paths, c.Priority, c.Relays, c.Share = old.WGPrivate, old.Paths, old.Priority, old.Relays, old.Share
		}
	}
	if c.WGPrivate == (tunnel.Key{}) {
		if c.WGPrivate, err = tunnel.NewPrivateKey(); err != nil {
			return nil, err
		}
	}
	pub, err := c.WGPrivate.Public()
	if err != nil {
		return nil, err
	}
	c.Mesh, err = control.Join(ctx, jc, control.JoinRequest{Name: name, Kind: kind, Platform: runtime.GOOS, WGPublic: pub}, nil)
	if err != nil {
		return nil, err
	}
	// We just reached the aggregator, so its name resolves right now.
	if len(c.Mesh.UDP) > 0 {
		if v4, v6, err := resolveUDP(ctx, c.Mesh.UDP[0]); err == nil {
			c.setResolved(v4, v6)
		}
	}
	return c, c.Save()
}

// Refresh looks the aggregator's name up now and remembers the answer. It
// reports whether the remembered addresses changed. Call it while DNS is
// still the system's own — before a tunnel that will take DNS over comes up.
func (c *Config) Refresh(ctx context.Context) bool {
	if c.Mesh == nil || len(c.Mesh.UDP) == 0 {
		return false
	}
	v4, v6, err := resolveUDP(ctx, c.Mesh.UDP[0])
	return err == nil && c.setResolved(v4, v6)
}

// setResolved records the aggregator's addresses and reports whether they changed.
func (c *Config) setResolved(v4, v6 netip.AddrPort) bool {
	var list []string
	for _, a := range []netip.AddrPort{v4, v6} {
		if a.IsValid() {
			list = append(list, a.String())
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if slices.Equal(list, c.Resolved) {
		return false
	}
	c.Resolved = list
	return true
}

// resolved returns the remembered aggregator addresses.
func (c *Config) resolved() (v4, v6 netip.AddrPort) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.Resolved {
		if a, err := netip.ParseAddrPort(s); err == nil {
			if a.Addr().Is4() && !v4.IsValid() {
				v4 = a
			} else if a.Addr().Is6() && !v6.IsValid() {
				v6 = a
			}
		}
	}
	return v4, v6
}

// LoadOrJoin returns the device's config, joining first if it has none and a
// code was supplied.
func LoadOrJoin(ctx context.Context, dir, code, name, kind string) (*Config, error) {
	c, err := LoadConfig(dir)
	if err == nil {
		if code == "" {
			return c, nil
		}
		// A code was given explicitly: honour it (it may be for a new mesh).
		return Join(ctx, dir, code, firstNonEmpty(name, c.Name), firstNonEmpty(kind, c.Kind))
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if code == "" {
		return nil, fmt.Errorf("this device has not joined a mesh yet: pass the join code the aggregator printed (--join bond://…)")
	}
	return Join(ctx, dir, code, name, kind)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// policyFor resolves the policy of a path: the user's choice if there is
// one, otherwise the default that came with the link.
func (c *Config) policyFor(name, def string) sched.Policy {
	c.mu.Lock()
	chosen := c.Paths[name]
	c.mu.Unlock()
	for _, s := range []string{chosen, def} {
		if p, err := sched.ParsePolicy(s); err == nil {
			return p
		}
	}
	return sched.PolicyBonded
}

func (c *Config) setPolicy(name string, p sched.Policy) {
	c.mu.Lock()
	if c.Paths == nil {
		c.Paths = map[string]string{}
	}
	c.Paths[name] = p.String()
	c.mu.Unlock()
}

func (c *Config) priority() classify.Rules {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Priority
}

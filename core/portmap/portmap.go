// Package portmap asks the router in front of this machine to forward a port
// to it, with UPnP, so that an aggregator at home or in an office is
// reachable from the Internet without anyone logging in to the router.
//
// It forwards the same port for UDP and TCP, renews the forward while it
// runs, and removes it on Close. A router with UPnP switched off, or no
// router at all (a cloud VM), never answers; the mapper keeps asking now and
// then, so switching UPnP on later is enough.
package portmap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Options configure a Mapper.
type Options struct {
	// Port is forwarded to on this machine, for UDP and TCP.
	Port int
	// External is the port on the router's Internet side. Default Port.
	External int
	// Description labels the forward in the router's list.
	Description string
	Log         *slog.Logger

	lease    time.Duration // asked for each forward; renewed at half
	retry    time.Duration // first wait after a failure; it doubles, to 5×
	discover func(context.Context) ([]string, error)
}

// Status is what the mapper has achieved so far.
type Status struct {
	// State is "searching" (no router has answered), "forwarded", or
	// "failed" (a router answered and refused).
	State string `json:"state"`
	// Router is the router's model and address.
	Router string `json:"router,omitempty"`
	// Internet is the router's own Internet address, as it reports it.
	Internet string `json:"internet,omitempty"`
	// Forward is this machine's address and port the router forwards to.
	Forward string `json:"forward,omitempty"`
	// Note says all of it in a line, for people.
	Note string `json:"note"`
}

// Mapper keeps a port forwarded for as long as it runs.
type Mapper struct {
	o      Options
	log    *slog.Logger
	hc     *http.Client
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once

	mu    sync.Mutex
	st    Status
	gw    *gateway        // the router holding our forwards
	added map[string]bool // protocols this mapper forwarded there, to undo on Close
}

// Start begins forwarding in the background.
func Start(o Options) *Mapper {
	if o.External == 0 {
		o.External = o.Port
	}
	if o.Description == "" {
		o.Description = "bond"
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.lease == 0 {
		o.lease = time.Hour
	}
	if o.retry == 0 {
		o.retry = time.Minute
	}
	if o.discover == nil {
		o.discover = func(ctx context.Context) ([]string, error) { return discover(ctx, 3*time.Second) }
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Mapper{
		o: o, log: o.Log, cancel: cancel, done: make(chan struct{}),
		hc:    &http.Client{Timeout: 5 * time.Second},
		added: map[string]bool{},
	}
	m.st = Status{State: "searching", Note: "looking for a UPnP router"}
	go m.run(ctx)
	return m
}

// Status reports the forward.
func (m *Mapper) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st
}

// Close stops renewing and removes the forwards this mapper made.
func (m *Mapper) Close() {
	m.once.Do(func() {
		m.cancel()
		<-m.done
		m.mu.Lock()
		gw, added := m.gw, m.added
		m.mu.Unlock()
		if gw == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		for proto := range added {
			if err := gw.deleteMapping(ctx, proto, m.o.External); err != nil {
				m.log.Debug("removing the port forward", "proto", proto, "err", err)
			}
		}
	})
}

func (m *Mapper) run(ctx context.Context) {
	defer close(m.done)
	fails := 0
	for {
		wait := m.o.lease / 2
		if err := m.refresh(ctx); err != nil {
			wait = min(m.o.retry<<min(fails, 3), 5*m.o.retry)
			fails++
		} else {
			fails = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

var errNoRouter = errors.New("no UPnP router answered")

// refresh finds the router and makes the forwards again: since last time it
// may have restarted, been replaced, or had UPnP switched on.
func (m *Mapper) refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	locs, err := m.o.discover(ctx)
	if err == nil && len(locs) == 0 {
		err = errNoRouter
	}
	if err != nil {
		m.set(slog.LevelInfo, Status{State: "searching", Note: m.noRouterNote()})
		return err
	}
	var gw *gateway
	for _, loc := range locs {
		if gw, err = openGateway(ctx, m.hc, loc); err == nil {
			break
		}
	}
	if gw == nil {
		m.set(slog.LevelWarn, Status{State: "failed", Note: "a UPnP router answered but could not be used: " + err.Error()})
		return err
	}
	router := gw.name + " at " + gw.host
	local, err := localAddrFor(gw.host)
	if err != nil {
		m.set(slog.LevelWarn, Status{State: "failed", Router: router, Note: "cannot reach the router at " + gw.host + ": " + err.Error()})
		return err
	}
	forward := netip.AddrPortFrom(local, uint16(m.o.Port))

	m.mu.Lock()
	if m.gw == nil || m.gw.host != gw.host {
		m.added = map[string]bool{}
	}
	m.gw = gw
	m.mu.Unlock()

	for _, proto := range []string{"UDP", "TCP"} {
		ours, err := m.forward(ctx, gw, proto, forward)
		if err != nil {
			m.set(slog.LevelWarn, Status{State: "failed", Router: router, Forward: forward.String(),
				Note: fmt.Sprintf("%s refused to forward port %d (%s) to this machine: %v", router, m.o.External, proto, err)})
			return err
		}
		if ours {
			m.mu.Lock()
			m.added[proto] = true
			m.mu.Unlock()
		}
	}

	st := Status{State: "forwarded", Router: router, Forward: forward.String(),
		Note: fmt.Sprintf("%s forwards port %d (UDP and TCP) to %s by UPnP", router, m.o.External, forward)}
	// What the router calls its Internet address is worth knowing, and a
	// private one means another router in front of it, which also needs the
	// port forwarded. Not every router answers; that is no reason to fail.
	if ext, err := gw.externalIP(ctx); err == nil && ext.IsValid() {
		st.Internet = ext.String()
		if behindNAT(ext) {
			st.Note += fmt.Sprintf(", but the router's own Internet address %s is private: the router in front of it must forward port %d too", ext, m.o.External)
		}
	}
	m.set(slog.LevelInfo, st)
	return nil
}

// forward makes one forward. ours is false when the router already had it,
// pointing here, and refused to take it again: someone set it up by hand, so
// it is not this mapper's to remove.
func (m *Mapper) forward(ctx context.Context, gw *gateway, proto string, to netip.AddrPort) (ours bool, err error) {
	_, err = gw.addMapping(ctx, proto, m.o.External, to.Addr(), int(to.Port()), m.o.Description, m.o.lease)
	if !isUPnPError(err, errConflict) {
		return err == nil, err
	}
	owner, oerr := gw.mappingOwner(ctx, proto, m.o.External)
	if oerr == nil && owner == to {
		return false, nil
	}
	if oerr == nil {
		return false, fmt.Errorf("the router already forwards it to %s", owner)
	}
	return false, err
}

// set records a new status, and says so in the log when it is news.
func (m *Mapper) set(level slog.Level, st Status) {
	m.mu.Lock()
	news := st.Note != m.st.Note
	m.st = st
	m.mu.Unlock()
	if news {
		m.log.Log(context.Background(), level, st.Note)
	}
}

func (m *Mapper) noRouterNote() string {
	where := "this machine"
	if a, _ := LocalAddr(); a.IsValid() {
		where = a.String()
	}
	return fmt.Sprintf("no UPnP router has answered: switch UPnP on in the router, or forward port %d (UDP and TCP) to %s there yourself", m.o.External, where)
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// behindNAT reports whether an address is one only reachable through a NAT:
// private, or carrier-grade NAT space.
func behindNAT(a netip.Addr) bool {
	return a.IsPrivate() || cgnat.Contains(a)
}

// CloudVM names the cloud this machine is a VM in, or is "". AWS, Google
// Cloud, Azure and the like give VMs private addresses behind a NAT of their
// own that has no router to ask: the public address and its firewall are set
// in the provider's console. Read from what the hypervisor reports (Linux).
func CloudVM() string { return cloudVM("/sys/class/dmi/id") }

func cloudVM(dmi string) string {
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(dmi, name))
		return strings.TrimSpace(string(b))
	}
	vendor, product, tag := read("sys_vendor"), read("product_name"), read("chassis_asset_tag")
	switch {
	case vendor == "Amazon EC2" || strings.HasSuffix(read("bios_version"), ".amazon"): // Nitro, older Xen
		return "AWS"
	case vendor == "Google" || product == "Google Compute Engine":
		return "Google Cloud"
	case tag == "7783-7084-3265-9085-8269-3286-77": // Azure's; Hyper-V at home has none
		return "Azure"
	case tag == "OracleCloud.com":
		return "Oracle Cloud"
	case vendor == "Alibaba Cloud":
		return "Alibaba Cloud"
	}
	return ""
}

// LocalAddr is the address this machine's default route leaves from, and
// whether it is a private one: a sign of a NAT router worth asking.
func LocalAddr() (addr netip.Addr, private bool) {
	c, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return netip.Addr{}, false
	}
	defer c.Close()
	a := c.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap()
	return a, behindNAT(a)
}

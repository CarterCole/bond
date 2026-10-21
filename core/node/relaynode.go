package node

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/CarterCole/bond/core/discovery"
	"github.com/CarterCole/bond/core/path"
	"github.com/CarterCole/bond/core/relay"
)

// RelayOptions configure a relay: a device that lends its uplink to hubs.
type RelayOptions struct {
	Config *Config
	// Listen is the UDP address hubs on the LAN send to. Default ":7778".
	Listen string
	// Bind selects the uplink that carries the forwarded traffic.
	Bind path.BindOpts
	// LAN is handed to the platform socket hook for the hub-facing socket.
	LAN path.BindOpts
	// NoAdvertise skips the mDNS announcement, for platforms that announce
	// with their own API (phones).
	NoAdvertise bool
	// Admin is the local address of the status API. Default "127.0.0.1:7780"; "-" disables.
	Admin string
	Log   *slog.Logger
}

// RelayNode is a running relay.
type RelayNode struct {
	o       RelayOptions
	cfg     *Config
	r       *relay.Relay
	stopAdv func()
	admin   *http.Server
	stop    context.CancelFunc
	started time.Time
}

func StartRelay(o RelayOptions) (_ *RelayNode, err error) {
	if o.Config == nil || o.Config.Mesh == nil {
		return nil, errors.New("relay: no mesh config; join first")
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Admin == "" {
		o.Admin = "127.0.0.1:7780"
	}
	if o.Listen == "" {
		port := o.Config.Share.Port
		if port == 0 {
			port = relay.DefaultPort
		}
		o.Listen = ":" + strconv.Itoa(port)
	}
	mesh := o.Config.Mesh
	if len(mesh.UDP) == 0 {
		return nil, errors.New("relay: mesh config names no aggregator address")
	}
	ctx, cancel := context.WithCancel(context.Background())
	n := &RelayNode{o: o, cfg: o.Config, stop: cancel, started: time.Now()}
	defer func() {
		if err != nil {
			n.Close()
		}
	}()

	v4, v6, err := resolveUDP(ctx, mesh.UDP[0])
	if err != nil {
		return nil, err
	}
	up := v4
	if !up.IsValid() {
		up = v6
	}
	if n.r, err = relay.Start(relay.Options{Listen: o.Listen, Upstream: up, Bind: o.Bind, LAN: o.LAN, Log: o.Log}); err != nil {
		return nil, err
	}
	if !o.NoAdvertise {
		if n.stopAdv, err = discovery.Advertise(o.Config.Name, mesh.ID, n.r.Port()); err != nil {
			// Hubs can still be pointed at us by address.
			o.Log.Warn("could not announce on the LAN; add this relay to hubs by address", "err", err, "port", n.r.Port())
			err = nil
		}
	}
	// Follow the aggregator if its address changes.
	go func() {
		t := time.NewTicker(resolveEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if v4, v6, err := resolveUDP(ctx, mesh.UDP[0]); err == nil {
					if v4.IsValid() {
						n.r.SetUpstream(v4)
					} else {
						n.r.SetUpstream(v6)
					}
				}
			}
		}
	}()
	if o.Admin != "-" {
		if n.admin, err = serveAdmin(o.Admin, n.adminRoutes(), o.Log); err != nil {
			return nil, err
		}
	}
	o.Log.Info("relay running: sharing this connection with hubs on the LAN", "name", o.Config.Name, "port", n.r.Port(), "uplink", o.Bind.Interface, "aggregator", up.String())
	return n, nil
}

// Rebind reopens the relay's uplink sockets; see relay.Rebind.
func (n *RelayNode) Rebind() { n.r.Rebind() }

// Port is the UDP port hubs send to.
func (n *RelayNode) Port() int { return n.r.Port() }

func (n *RelayNode) Close() {
	n.stop()
	if n.admin != nil {
		n.admin.Close()
	}
	if n.stopAdv != nil {
		n.stopAdv()
	}
	if n.r != nil {
		n.r.Close()
	}
}

// RelayDoc is a relay's status document.
type RelayDoc struct {
	Role    string      `json:"role"`
	Version string      `json:"version"`
	Name    string      `json:"name"`
	MeshID  string      `json:"mesh_id"`
	Uplink  string      `json:"uplink,omitempty"`
	Uptime  int64       `json:"uptime_sec"`
	Relay   relay.Stats `json:"relay"`
}

func (n *RelayNode) Status() RelayDoc {
	return RelayDoc{
		Role: "relay", Version: Version, Name: n.cfg.Name, MeshID: n.cfg.Mesh.ID,
		Uplink: firstNonEmpty(n.o.Bind.Interface, n.o.Bind.Network), Uptime: int64(time.Since(n.started).Seconds()),
		Relay: n.r.Stats(),
	}
}

package node

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/CarterCole/bond/core/rendezvous"
)

// RendezvousOptions configure a rendezvous: a public host that aggregators
// with no inbound port register with, and that hubs reach them through.
type RendezvousOptions struct {
	StateDir string
	// Listen is the address for UDP and TCP. Default ":7777".
	Listen string
	// Public is how hubs and aggregators reach Listen: "host" or
	// "host:port". Detected when empty.
	Public string
	// Admin is the local address of the status API and dashboard.
	// Default "127.0.0.1:7780"; "-" disables it.
	Admin string
	Log   *slog.Logger
}

// RendezvousNode is a running rendezvous.
type RendezvousNode struct {
	r       *rendezvous.Server
	admin   *http.Server
	started time.Time
}

func StartRendezvous(o RendezvousOptions) (_ *RendezvousNode, err error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.StateDir == "" {
		o.StateDir = DefaultStateDir()
	}
	if o.Admin == "" {
		o.Admin = "127.0.0.1:7780"
	}
	if o.Public == "" {
		o.Public = detectPublicIP(o.Log)
		o.Log.Warn("no --public address given; guessed one. If aggregators cannot register, restart with --public <address they can reach>", "public", o.Public)
	}
	n := &RendezvousNode{started: time.Now()}
	defer func() {
		if err != nil {
			n.Close()
		}
	}()
	if n.r, err = rendezvous.Start(rendezvous.Options{Listen: o.Listen, Public: o.Public, StateDir: o.StateDir, Log: o.Log}); err != nil {
		return nil, err
	}
	if o.Admin != "-" {
		if n.admin, err = serveAdmin(o.Admin, base(func() any { return n.Status() }), o.Log); err != nil {
			return nil, err
		}
	}
	return n, nil
}

// Code is the pairing code aggregators register with.
func (n *RendezvousNode) Code() rendezvous.Code { return n.r.Code() }

// Port is the UDP and TCP port the rendezvous listens on.
func (n *RendezvousNode) Port() int { return n.r.Port() }

// AdminAddr is where the rendezvous's status API and dashboard are listening.
func (n *RendezvousNode) AdminAddr() string { return adminAddr(n.admin) }

func (n *RendezvousNode) Close() {
	if n.admin != nil {
		n.admin.Close()
	}
	if n.r != nil {
		n.r.Close()
	}
}

// RendezvousDoc is a rendezvous's status document.
type RendezvousDoc struct {
	Role        string                  `json:"role"`
	Version     string                  `json:"version"`
	PairingCode string                  `json:"pairing_code"`
	Uptime      int64                   `json:"uptime_sec"`
	Rendezvous  rendezvous.ServerStatus `json:"rendezvous"`
}

func (n *RendezvousNode) Status() RendezvousDoc {
	return RendezvousDoc{
		Role: "rendezvous", Version: Version, PairingCode: n.r.Code().String(),
		Uptime: int64(time.Since(n.started).Seconds()), Rendezvous: n.r.Status(),
	}
}

// bondd is the bond daemon. One binary, three roles:
//
//	bondd server   the aggregator: runs on a machine with a public address
//	bondd hub      bonds this machine's uplinks (and relays it finds) to the aggregator
//	bondd relay    lends this machine's uplink to hubs on the same LAN
//
// and `bondd service` to keep any of them running in the background.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/mdp/qrterminal/v3"

	"github.com/CarterCole/bond/core/control"
	"github.com/CarterCole/bond/core/node"
	"github.com/CarterCole/bond/core/path"
)

const usage = `bondd — bonded internet daemon

Usage:
  bondd server  [flags]     run the aggregator (the cloud end)
  bondd hub     [flags]     bond this machine's uplinks to an aggregator
  bondd relay   [flags]     share this machine's uplink with hubs on the LAN
  bondd service install|uninstall|restart|status <server|hub|relay> [role flags]
  bondd version

Run "bondd <role> -h" for that role's flags.

Quick start:
  on a server with a public address:   bondd server --public <address>
  on the machine to speed up:          sudo bondd hub --join <code the server printed>
  on anything with a spare connection: bondd relay --join <code>
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "server":
		err = runServer(args)
	case "hub":
		err = runHub(args)
	case "relay":
		err = runRelay(args)
	case "service":
		err = runService(args)
	case "version", "-v", "--version":
		fmt.Println("bondd", node.Version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "bondd: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bondd:", err)
		os.Exit(1)
	}
}

// common are the flags every role takes.
type common struct {
	state   string
	admin   string
	verbose bool
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.state, "state", "", "directory for keys and state (default "+node.DefaultStateDir()+")")
	fs.StringVar(&c.admin, "admin", "", `address of the local status API and dashboard (default 127.0.0.1:7780, "-" to disable)`)
	fs.BoolVar(&c.verbose, "v", false, "verbose logging")
}

func (c *common) logger() *slog.Logger {
	level := slog.LevelInfo
	if c.verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

func (c *common) stateDir() string {
	if c.state != "" {
		return c.state
	}
	return node.DefaultStateDir()
}

func newFlags(name, summary string) *flag.FlagSet {
	fs := flag.NewFlagSet("bondd "+name, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "bondd %s — %s\n\nFlags:\n", name, summary)
		fs.PrintDefaults()
	}
	return fs
}

// wait blocks until the process is asked to stop.
func wait() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
	fmt.Fprintln(os.Stderr)
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func runServer(args []string) error {
	var c common
	var o node.ServerOptions
	fs := newFlags("server", "run the aggregator")
	c.register(fs)
	fs.StringVar(&o.Listen, "listen", "", "address to listen on, UDP and TCP (default :7777)")
	fs.StringVar(&o.Public, "public", "", "address devices reach this server at: host or host:port (detected if omitted)")
	fs.BoolVar(&o.Userspace, "userspace", false, "route to the Internet from inside this process: no root, TUN or iptables needed")
	fs.StringVar(&o.WS, "ws-url", "", "WebSocket fallback URL to hand to devices, e.g. wss://bond.example.com/v1/ws behind a Cloudflare Tunnel")
	fs.BoolVar(&o.NoUPnP, "no-upnp", false, "do not ask the router in front of this machine to forward the port (UPnP)")
	fs.Parse(args)

	o.StateDir, o.Admin, o.Log = c.stateDir(), c.admin, c.logger()
	s, err := node.StartServer(o)
	if err != nil {
		return err
	}
	defer s.Close()
	code, err := s.JoinCode()
	if err != nil {
		return err
	}
	printJoin(code)
	wait()
	return nil
}

func printJoin(code control.JoinCode) {
	fmt.Println()
	fmt.Println("  Join code (anyone holding it can join this mesh — treat it like a password):")
	fmt.Println()
	fmt.Println("    " + code.String())
	fmt.Println()
	if isTerminal(os.Stdout) {
		fmt.Println("  Scan with the Bond app:")
		fmt.Println()
		qrterminal.GenerateWithConfig(code.String(), qrterminal.Config{
			Level: qrterminal.M, Writer: os.Stdout, HalfBlocks: true, QuietZone: 2,
			BlackChar: qrterminal.BLACK_BLACK, WhiteBlackChar: qrterminal.WHITE_BLACK,
			WhiteChar: qrterminal.WHITE_WHITE, BlackWhiteChar: qrterminal.BLACK_WHITE,
		})
	}
	fmt.Println("  On a computer:  sudo bondd hub --join '" + code.String() + "'")
	fmt.Println()
}

func runHub(args []string) error {
	var c common
	var o node.HubOptions
	var join, name, ifaces, relays string
	var priority bool
	fs := newFlags("hub", "bond this machine's uplinks to an aggregator")
	c.register(fs)
	fs.StringVar(&join, "join", "", "join code from the aggregator (only needed the first time)")
	fs.StringVar(&name, "name", "", "this device's name in the mesh (default: hostname)")
	fs.BoolVar(&o.Userspace, "userspace", false, "no root: offer a SOCKS5 proxy instead of capturing this machine's traffic")
	fs.StringVar(&o.Socks, "socks", "", `SOCKS5 proxy address in userspace mode (default 127.0.0.1:1080, "-" to disable)`)
	fs.BoolVar(&o.Split, "split", false, "leave the default route alone: only mesh addresses use the tunnel")
	fs.BoolVar(&o.Gateway, "gateway", false, "share the bond with the LAN: act as a router for other devices")
	fs.BoolVar(&o.AllowIPv6, "allow-ipv6", false, "leave IPv6 alone (by default it is turned off while bonded: the tunnel is IPv4 and IPv6 would bypass it)")
	fs.StringVar(&o.Transport, "transport", "auto", "auto (UDP, WebSocket where UDP is blocked), udp or ws")
	fs.StringVar(&ifaces, "iface", "", "only use these interfaces as uplinks (comma separated; default: all)")
	fs.StringVar(&relays, "relay", "", "relays to use besides discovered ones (ip:port, comma separated)")
	fs.BoolVar(&o.NoDiscovery, "no-discovery", false, "do not look for relays on the LAN")
	fs.BoolVar(&priority, "priority", false, "treat everything this device sends as priority (realtime) traffic")
	fs.Parse(args)

	dir := c.stateDir()
	cfg, err := node.LoadOrJoin(context.Background(), dir, join, name, control.KindHub)
	if err != nil {
		return err
	}
	o.Config, o.StateDir, o.Admin, o.Log = cfg, dir, c.admin, c.logger()
	o.Interfaces = splitList(ifaces)
	h, err := node.StartHub(o)
	if err != nil {
		return err
	}
	defer h.Close()
	for _, r := range splitList(relays) {
		if err := h.AddRelay(r); err != nil {
			return err
		}
	}
	if priority {
		rules := cfg.Priority
		rules.All = true
		if err := h.SetPriority(rules); err != nil {
			return err
		}
	}
	wait()
	return nil
}

func runRelay(args []string) error {
	var c common
	var o node.RelayOptions
	var join, name, iface string
	fs := newFlags("relay", "share this machine's uplink with hubs on the LAN")
	c.register(fs)
	fs.StringVar(&join, "join", "", "join code from the aggregator (only needed the first time)")
	fs.StringVar(&name, "name", "", "this device's name in the mesh (default: hostname)")
	fs.StringVar(&iface, "iface", "", "the uplink to share, e.g. a cellular modem (default: whatever the OS routes by)")
	fs.StringVar(&o.Listen, "listen", "", "UDP address hubs send to (default :7778)")
	fs.Parse(args)

	dir := c.stateDir()
	cfg, err := node.LoadOrJoin(context.Background(), dir, join, name, control.KindRelay)
	if err != nil {
		return err
	}
	o.Config, o.Admin, o.Log = cfg, c.admin, c.logger()
	o.Bind = path.BindOpts{Interface: iface, Network: iface}
	r, err := node.StartRelay(o)
	if err != nil {
		return err
	}
	defer r.Close()
	wait()
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

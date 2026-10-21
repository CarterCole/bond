package tunnel

import (
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
	"sync"

	"golang.zx2c4.com/wireguard/tun"
)

// KernelOptions describe a real TUN interface.
type KernelOptions struct {
	Name    string // preferred interface name; the OS may choose another
	MTU     int
	Address netip.Prefix   // this node's address and the mesh prefix length
	Routes  []netip.Prefix // extra destinations to send into the tunnel
	// BlockIPv6 makes the Internet unreachable over IPv6 for as long as the
	// device exists. The tunnel carries IPv4 only; on a dual-stack network
	// most traffic would otherwise prefer IPv6 and quietly go around the
	// bond. With IPv6 refused outright, applications fall back to IPv4 at
	// once. Addresses on the local network keep working.
	BlockIPv6 bool
}

// ipv6Internet is all of IPv6 as two halves, for the same reason as
// FullTunnel: more specific than the default route, without replacing it.
var ipv6Internet = []string{"::/1", "8000::/1"}

// undoDevice runs clean-up that the OS will not do by itself when the TUN
// interface goes away.
type undoDevice struct {
	tun.Device
	undo func()
	once sync.Once
}

func (u *undoDevice) Close() error {
	u.once.Do(u.undo)
	return u.Device.Close()
}

// FullTunnel is the pair of routes that captures all IPv4 traffic without
// replacing the default route. Being more specific than 0.0.0.0/0 they win
// the lookup, while every uplink keeps its own default route — which is what
// the bond's interface-pinned sockets use to get out.
var FullTunnel = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/1"),
	netip.MustParsePrefix("128.0.0.0/1"),
}

// OpenKernelTUN creates and configures a TUN interface. It needs root (or
// CAP_NET_ADMIN). The interface and its routes disappear when the returned
// device is closed.
func OpenKernelTUN(o KernelOptions) (tun.Device, string, error) {
	if o.MTU == 0 {
		o.MTU = DefaultMTU
	}
	dev, err := tun.CreateTUN(tunName(o.Name), o.MTU)
	if err != nil {
		return nil, "", fmt.Errorf("creating a TUN interface (this needs root; --userspace runs without): %w", err)
	}
	name, err := dev.Name()
	if err != nil {
		dev.Close()
		return nil, "", err
	}
	if err := configure(name, o); err != nil {
		dev.Close()
		return nil, "", err
	}
	if o.BlockIPv6 {
		if undo := blockIPv6(); undo != nil {
			return &undoDevice{Device: dev, undo: undo}, name, nil
		}
	}
	return dev, name, nil
}

// run executes a system command, folding its output into the error.
func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

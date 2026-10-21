//go:build linux && !android

package tunnel

import "os"

func tunName(want string) string {
	if want == "" {
		return "bonded0"
	}
	return want
}

func configure(name string, o KernelOptions) error {
	// The kernel adds the mesh prefix route itself from the address.
	if err := run("ip", "addr", "add", o.Address.String(), "dev", name); err != nil {
		return err
	}
	if err := run("ip", "link", "set", "dev", name, "up"); err != nil {
		return err
	}
	for _, r := range prefixStrings(o.Routes) {
		if err := run("ip", "route", "add", r, "dev", name); err != nil {
			return err
		}
	}
	return nil
}

// LoosenReversePath lets replies come back in on an uplink that is not the
// one the main routing table would use to reach the sender. With several
// uplinks and the tunnel holding the best route to everything, strict
// reverse-path filtering would silently drop the bond's own return traffic.
func LoosenReversePath(iface string) {
	os.WriteFile("/proc/sys/net/ipv4/conf/"+iface+"/rp_filter", []byte("2\n"), 0o644)
}

// blockIPv6 installs "unreachable" routes: a connection attempt fails
// immediately with "network is unreachable" instead of timing out. Best
// effort — a machine without IPv6 has nothing to block.
func blockIPv6() (undo func()) {
	var added []string
	for _, half := range ipv6Internet {
		if run("ip", "-6", "route", "add", "unreachable", half) == nil {
			added = append(added, half)
		}
	}
	if len(added) == 0 {
		return nil
	}
	return func() {
		for _, half := range added {
			run("ip", "-6", "route", "del", "unreachable", half)
		}
	}
}

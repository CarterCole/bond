package path

import (
	"net"
	"net/netip"
	"sort"
	"strings"
)

// Iface is a network interface that could be an uplink.
type Iface struct {
	Name  string
	Index int
	V4    []netip.Addr // global or private unicast
	V6    []netip.Addr // global unicast only
}

// Interfaces that are never uplinks: tunnels (ours included), bridges,
// container plumbing and Apple's peer-to-peer radios.
var skipPrefixes = []string{
	"lo", "utun", "tun", "tap", "wg", "tailscale", "bonded", "zt", "ipsec", "gif", "stf",
	"docker", "br-", "veth", "virbr", "vmnet", "vmenet", "bridge", "vnic",
	"awdl", "llw", "anpi", "ap", "pktap", "XHC",
}

func skipName(name string) bool {
	for _, p := range skipPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Uplinks lists the interfaces worth trying as paths: up, not a tunnel, and
// holding a routable address. Whether one actually reaches the aggregator is
// for the probes to find out.
func Uplinks() ([]Iface, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []Iface
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 || skipName(ifi.Name) {
			continue
		}
		// Point-to-point links are tunnels, except cellular modems.
		if ifi.Flags&net.FlagPointToPoint != 0 && !strings.HasPrefix(ifi.Name, "ppp") && !strings.HasPrefix(ifi.Name, "wwan") {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		it := Iface{Name: ifi.Name, Index: ifi.Index}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipn.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			switch {
			case ip.IsLinkLocalUnicast(), ip.IsLoopback(), ip.IsMulticast(), ip.IsUnspecified():
			case ip.Is4():
				it.V4 = append(it.V4, ip)
			case ip.IsGlobalUnicast() && !ip.IsPrivate():
				it.V6 = append(it.V6, ip)
			}
		}
		if len(it.V4)+len(it.V6) > 0 {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

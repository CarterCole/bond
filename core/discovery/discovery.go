// Package discovery lets hubs find relays on the local network with no
// configuration, using DNS-SD over mDNS (Bonjour).
//
// A relay advertises _bond-relay._udp with the id of the mesh it serves; a
// hub browses for that service and adds a path through every relay of its
// own mesh. Phones advertise with their platform's own API (NsdManager,
// NWListener) and are found by the same browse.
package discovery

import (
	"context"
	"net"
	"net/netip"
	"strings"

	"github.com/libp2p/zeroconf/v2"
)

const (
	Service = "_bond-relay._udp"
	domain  = "local."
)

// Found is a relay seen on the LAN.
type Found struct {
	Name  string // the relay's device name
	Mesh  string // mesh id it forwards for
	Addrs []netip.AddrPort
}

// TXT builds the TXT record a relay advertises.
func TXT(mesh, name string) []string {
	return []string{"v=1", "mesh=" + mesh, "name=" + name}
}

// Advertise announces a relay until stop is called.
func Advertise(name, mesh string, port int) (stop func(), err error) {
	s, err := zeroconf.Register(sanitize(name), Service, domain, port, TXT(mesh, name), nil)
	if err != nil {
		return nil, err
	}
	return s.Shutdown, nil
}

// sanitize makes a device name usable as a DNS-SD instance label.
func sanitize(s string) string {
	s = strings.TrimSpace(strings.NewReplacer(".", "-", "\\", "-").Replace(s))
	if len(s) > 60 {
		s = s[:60]
	}
	if s == "" {
		s = "bond-relay"
	}
	return s
}

// Browse reports relays of the given mesh until ctx ends. found may be
// called repeatedly for the same relay as it re-announces.
func Browse(ctx context.Context, mesh string, found func(Found)) error {
	entries := make(chan *zeroconf.ServiceEntry, 16)
	go func() {
		for e := range entries {
			f := Found{Name: e.Instance}
			for _, t := range e.Text {
				k, v, _ := strings.Cut(t, "=")
				switch k {
				case "mesh":
					f.Mesh = v
				case "name":
					if v != "" {
						f.Name = v
					}
				}
			}
			if f.Mesh != mesh || e.Port <= 0 || e.Port > 65535 {
				continue
			}
			// IPv4 only: hub↔relay hops are LAN hops, and link-local IPv6
			// needs a zone we do not get from the record.
			for _, ip := range e.AddrIPv4 {
				f.Addrs = append(f.Addrs, addrPort(ip, e.Port))
			}
			if len(f.Addrs) > 0 {
				found(f)
			}
		}
	}()
	return zeroconf.Browse(ctx, Service, domain, entries, zeroconf.SelectIPTraffic(zeroconf.IPv4))
}

func addrPort(ip net.IP, port int) netip.AddrPort {
	a, _ := netip.AddrFromSlice(ip)
	return netip.AddrPortFrom(a.Unmap(), uint16(port))
}

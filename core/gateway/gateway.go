// Package gateway turns a machine into a router for the bond: the
// aggregator NATing the mesh out to the Internet, or a hub sharing its
// bonded connection with the LAN so cameras, encoders and consoles need no
// software at all.
//
// It drives the operating system's own firewall (iptables on Linux, pf on
// macOS). Every rule it adds is remembered and removed again by the
// returned undo function.
package gateway

import (
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
)

// Rule is one firewall rule in a form that can be both added and removed.
type rule struct {
	table string
	chain string
	spec  []string
	head  bool // insert at the top rather than append
}

func (r rule) args(op string) []string {
	a := []string{"-w", "-t", r.table, op, r.chain}
	return append(a, r.spec...)
}

// Undo removes what a call added. It is safe to call more than once.
type Undo func()

func sh(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// AggregatorRules are the rules that let mesh members reach the Internet
// through this machine.
func aggregatorRules(tun string, mesh netip.Prefix) []rule {
	m := mesh.String()
	return []rule{
		// Mesh members must not reach this host's link-local neighbours:
		// on a cloud VM that is where the instance metadata service lives.
		{"filter", "FORWARD", []string{"-i", tun, "-d", "169.254.0.0/16", "-j", "DROP"}, true},
		{"filter", "FORWARD", []string{"-i", tun, "-j", "ACCEPT"}, false},
		{"filter", "FORWARD", []string{"-o", tun, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}, false},
		{"nat", "POSTROUTING", []string{"-s", m, "!", "-o", tun, "-j", "MASQUERADE"}, false},
		// The tunnel MTU is below 1500: tell TCP on the way in so nothing
		// depends on path-MTU discovery surviving the Internet.
		{"mangle", "FORWARD", []string{"-o", tun, "-p", "tcp", "--tcp-flags", "SYN,RST", "SYN", "-j", "TCPMSS", "--clamp-mss-to-pmtu"}, false},
	}
}

// hubRules are the rules that let LAN devices use a hub as their router.
// priority lists LAN devices (IPv4 addresses or MAC addresses) whose traffic
// is marked DSCP EF before NAT hides who sent it; the bond treats EF as
// realtime.
func hubRules(tun string, priority []string) ([]rule, error) {
	rs := []rule{
		{"filter", "FORWARD", []string{"-o", tun, "-j", "ACCEPT"}, false},
		{"filter", "FORWARD", []string{"-i", tun, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}, false},
		{"nat", "POSTROUTING", []string{"-o", tun, "-j", "MASQUERADE"}, false},
		{"mangle", "FORWARD", []string{"-o", tun, "-p", "tcp", "--tcp-flags", "SYN,RST", "SYN", "-j", "TCPMSS", "--clamp-mss-to-pmtu"}, false},
	}
	for _, dev := range priority {
		match, err := deviceMatch(dev)
		if err != nil {
			return nil, err
		}
		spec := append(match, "-j", "DSCP", "--set-dscp", "46")
		rs = append(rs, rule{"mangle", "PREROUTING", spec, false})
	}
	return rs, nil
}

// deviceMatch turns "192.168.1.50", "192.168.1.0/28" or "aa:bb:cc:dd:ee:ff"
// into iptables match arguments.
func deviceMatch(dev string) ([]string, error) {
	if _, err := netip.ParseAddr(dev); err == nil {
		return []string{"-s", dev}, nil
	}
	if _, err := netip.ParsePrefix(dev); err == nil {
		return []string{"-s", dev}, nil
	}
	if isMAC(dev) {
		return []string{"-m", "mac", "--mac-source", strings.ToLower(dev)}, nil
	}
	return nil, fmt.Errorf("priority device %q is not an IP address, prefix or MAC address", dev)
}

func isMAC(s string) bool {
	if len(s) != 17 {
		return false
	}
	for i, c := range s {
		if i%3 == 2 {
			if c != ':' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

// IsMAC reports whether s is a MAC address like aa:bb:cc:dd:ee:ff.
func IsMAC(s string) bool { return isMAC(s) }

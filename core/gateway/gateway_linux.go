//go:build linux && !android

package gateway

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync"
)

func apply(rules []rule) (Undo, error) {
	if _, err := exec.LookPath("iptables"); err != nil {
		return nil, errors.New("iptables not found: install it (the iptables-nft flavour is fine), or run the aggregator with --userspace")
	}
	var done []rule
	undo := func() {
		for i := len(done) - 1; i >= 0; i-- {
			sh("iptables", done[i].args("-D")...)
		}
		done = nil
	}
	for _, r := range rules {
		// -C tells us whether the rule is already there (a previous run
		// that was killed, say); if so leave it and do not claim it.
		if sh("iptables", r.args("-C")...) == nil {
			continue
		}
		op := "-A"
		if r.head {
			op = "-I"
		}
		if err := sh("iptables", r.args(op)...); err != nil {
			undo()
			return nil, err
		}
		done = append(done, r)
	}
	var once sync.Once
	return func() { once.Do(undo) }, nil
}

// enableForwarding turns on IPv4 routing. Where it is already on — set by the
// distribution, cloud-init or a container runtime, often with /proc/sys
// read-only — there is nothing to do and nothing to fail at.
func enableForwarding() error {
	const knob = "/proc/sys/net/ipv4/ip_forward"
	if b, err := os.ReadFile(knob); err == nil && strings.TrimSpace(string(b)) == "1" {
		return nil
	}
	if err := os.WriteFile(knob, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("cannot enable IP forwarding (set net.ipv4.ip_forward=1 yourself): %w", err)
	}
	return nil
}

// Aggregator makes this machine the mesh's way out to the Internet.
func Aggregator(tun string, mesh netip.Prefix) (Undo, error) {
	if err := enableForwarding(); err != nil {
		return nil, err
	}
	return apply(aggregatorRules(tun, mesh))
}

// Hub makes this machine a router that sends the LAN's traffic over the bond.
func Hub(tun string, priority []string) (Undo, error) {
	rs, err := hubRules(tun, priority)
	if err != nil {
		return nil, err
	}
	if err := enableForwarding(); err != nil {
		return nil, err
	}
	return apply(rs)
}

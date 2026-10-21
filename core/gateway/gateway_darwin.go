//go:build darwin && !ios

package gateway

import (
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
	"sync"
)

// macOS support is deliberately narrow: a hub can share the bond with its
// LAN through pf. The rules live in their own anchor under com.apple/*,
// which the stock /etc/pf.conf already evaluates, so nothing else in the
// system's ruleset is touched.
const anchor = "com.apple/bond"

// Aggregator needs Linux for kernel NAT; on macOS the aggregator runs in
// userspace mode instead.
func Aggregator(string, netip.Prefix) (Undo, error) {
	return nil, errors.New("kernel NAT for the aggregator is Linux-only: use --userspace on macOS")
}

// Hub makes this Mac a router that sends the LAN's traffic over the bond.
// Priority by LAN device is not available here (pf cannot mark DSCP before
// NAT); port and DSCP rules still apply.
func Hub(tun string, priority []string) (Undo, error) {
	if len(priority) > 0 {
		return nil, errors.New("prioritising individual LAN devices needs a Linux hub; on macOS use port rules or mark the device's own traffic")
	}
	rules := fmt.Sprintf("nat on %s inet from ! (%s) to any -> (%s)\n", tun, tun, tun)
	cmd := exec.Command("pfctl", "-a", anchor, "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("pfctl: %v: %s", err, strings.TrimSpace(string(out)))
	}
	prev, _ := exec.Command("sysctl", "-n", "net.inet.ip.forwarding").Output()
	if err := sh("sysctl", "-w", "net.inet.ip.forwarding=1"); err != nil {
		return nil, err
	}
	// -E takes a reference on pf being enabled and prints a token for releasing it.
	out, _ := exec.Command("pfctl", "-E").CombinedOutput()
	token := ""
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Token : "); ok {
			token = rest
		}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			sh("pfctl", "-a", anchor, "-F", "all")
			if token != "" {
				sh("pfctl", "-X", token)
			}
			if strings.TrimSpace(string(prev)) == "0" {
				sh("sysctl", "-w", "net.inet.ip.forwarding=0")
			}
		})
	}, nil
}

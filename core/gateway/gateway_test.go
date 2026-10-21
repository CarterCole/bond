package gateway

import (
	"net/netip"
	"strings"
	"testing"
)

func lines(rs []rule) []string {
	var out []string
	for _, r := range rs {
		op := "-A"
		if r.head {
			op = "-I"
		}
		out = append(out, strings.Join(r.args(op), " "))
	}
	return out
}

func has(t *testing.T, got []string, want string) {
	t.Helper()
	for _, g := range got {
		if g == want {
			return
		}
	}
	t.Errorf("missing rule:\n  %s\nhave:\n  %s", want, strings.Join(got, "\n  "))
}

func TestAggregatorRules(t *testing.T) {
	got := lines(aggregatorRules("bonded0", netip.MustParsePrefix("10.207.0.0/16")))
	has(t, got, "-w -t nat -A POSTROUTING -s 10.207.0.0/16 ! -o bonded0 -j MASQUERADE")
	// The metadata-service block must be inserted ahead of the accept.
	has(t, got, "-w -t filter -I FORWARD -i bonded0 -d 169.254.0.0/16 -j DROP")
	has(t, got, "-w -t filter -A FORWARD -i bonded0 -j ACCEPT")
	has(t, got, "-w -t mangle -A FORWARD -o bonded0 -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu")
}

func TestHubRulesMarkPriorityDevices(t *testing.T) {
	rs, err := hubRules("bonded0", []string{"192.168.8.50", "AA:BB:CC:00:11:22", "192.168.9.0/28"})
	if err != nil {
		t.Fatal(err)
	}
	got := lines(rs)
	has(t, got, "-w -t nat -A POSTROUTING -o bonded0 -j MASQUERADE")
	has(t, got, "-w -t mangle -A PREROUTING -s 192.168.8.50 -j DSCP --set-dscp 46")
	has(t, got, "-w -t mangle -A PREROUTING -m mac --mac-source aa:bb:cc:00:11:22 -j DSCP --set-dscp 46")
	has(t, got, "-w -t mangle -A PREROUTING -s 192.168.9.0/28 -j DSCP --set-dscp 46")

	// Removal is the same rule with -D.
	if d := strings.Join(rs[0].args("-D"), " "); d != "-w -t filter -D FORWARD -o bonded0 -j ACCEPT" {
		t.Fatalf("delete form: %s", d)
	}
	if _, err := hubRules("bonded0", []string{"the-encoder"}); err == nil {
		t.Fatal("accepted a hostname as a priority device")
	}
}

func TestIsMAC(t *testing.T) {
	for s, want := range map[string]bool{
		"aa:bb:cc:dd:ee:ff": true, "AA:BB:CC:00:11:22": true,
		"aa:bb:cc:dd:ee": false, "aa-bb-cc-dd-ee-ff": false, "gg:bb:cc:dd:ee:ff": false, "192.168.1.1": false,
	} {
		if IsMAC(s) != want {
			t.Errorf("IsMAC(%q) = %v", s, !want)
		}
	}
}

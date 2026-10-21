package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

func jsonMarshal(v any) ([]byte, error)   { return json.MarshalIndent(v, "", "  ") }
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// resolveUDP turns "host:port" into addresses, IPv4 first.
func resolveUDP(ctx context.Context, hostport string) (v4, v6 netip.AddrPort, err error) {
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		return v4, v6, fmt.Errorf("bad aggregator address %q: %w", hostport, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return v4, v6, fmt.Errorf("bad aggregator port in %q", hostport)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		ap := netip.AddrPortFrom(ip.Unmap(), uint16(port))
		if ap.Addr().Is4() {
			return ap, v6, nil
		}
		return v4, ap, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return v4, v6, err
	}
	for _, ip := range ips {
		ip = ip.Unmap()
		switch {
		case ip.Is4() && !v4.IsValid():
			v4 = netip.AddrPortFrom(ip, uint16(port))
		case ip.Is6() && !v6.IsValid():
			v6 = netip.AddrPortFrom(ip, uint16(port))
		}
	}
	if !v4.IsValid() && !v6.IsValid() {
		return v4, v6, fmt.Errorf("%s has no addresses", host)
	}
	return v4, v6, nil
}

// outboundInterface reports which interface the OS would currently use to
// reach dst: the "primary pipe". Call it before the tunnel takes the routes.
func outboundInterface(dst netip.AddrPort) string {
	if !dst.IsValid() {
		dst = netip.MustParseAddrPort("192.0.2.1:9") // TEST-NET: never sent, only routed
	}
	c, err := net.Dial("udp", dst.String())
	if err != nil {
		return ""
	}
	defer c.Close()
	local := c.LocalAddr().(*net.UDPAddr).IP
	ifs, _ := net.Interfaces()
	for _, ifi := range ifs {
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.Equal(local) {
				return ifi.Name
			}
		}
	}
	return ""
}

// wgField pulls one value out of WireGuard's key=value status text.
func wgField(status, key string) string {
	for _, line := range strings.Split(status, "\n") {
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return v
		}
	}
	return ""
}

// wgPeers splits WireGuard's status into per-peer handshake ages (seconds
// since the last handshake, -1 for never), keyed by hex public key.
func wgHandshakes(status string) map[string]int64 {
	out := map[string]int64{}
	now := time.Now().Unix()
	cur := ""
	for _, line := range strings.Split(status, "\n") {
		if v, ok := strings.CutPrefix(line, "public_key="); ok {
			cur = v
			out[cur] = -1
		} else if v, ok := strings.CutPrefix(line, "last_handshake_time_sec="); ok && cur != "" {
			if t, err := strconv.ParseInt(v, 10, 64); err == nil && t > 0 {
				out[cur] = now - t
			}
		}
	}
	return out
}

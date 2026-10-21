package path

import (
	"net"
	"strings"

	"golang.org/x/sys/unix"
)

// bindToInterface scopes the socket to one interface (macOS and iOS), so its
// traffic uses that interface's own default route whatever the main routing
// table says.
func bindToInterface(fd int, network, name string) error {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return err
	}
	if strings.HasSuffix(network, "6") {
		return unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF, ifi.Index)
	}
	return unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_BOUND_IF, ifi.Index)
}

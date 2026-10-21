//go:build darwin || linux

package tunnel

import (
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/tun"
)

// FromFD wraps a TUN file descriptor that the operating system created and
// configured: Android's VpnService.Builder.establish() or the utun behind an
// iOS NEPacketTunnelProvider.
//
// The descriptor belongs to the returned device from the moment this is
// called: it is closed with the device, and closed here if wrapping fails.
// The device is used as configured — in particular its MTU is read, never
// set, because an app is not allowed to set it.
func FromFD(fd int) (tun.Device, error) {
	// Non-blocking before anything else: it is what lets a read be
	// interrupted when the device is closed.
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return fromFD(fd)
}

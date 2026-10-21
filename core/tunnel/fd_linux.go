package tunnel

import "golang.zx2c4.com/wireguard/tun"

// On Linux and Android the descriptor is used as it comes: no netlink
// monitoring (apps are not allowed it), MTU as configured by the creator.
//
// If this fails the descriptor is already inside an os.File that wireguard-go
// has dropped; its finalizer closes it. Closing it here as well would close
// whatever unrelated descriptor had reused the number by then.
func fromFD(fd int) (tun.Device, error) {
	dev, _, err := tun.CreateUnmonitoredTUNFromFD(fd)
	return dev, err
}

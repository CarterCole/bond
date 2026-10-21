package tunnel

import (
	"os"

	"golang.zx2c4.com/wireguard/tun"
)

// MTU 0: leave the interface's MTU alone. Setting it needs root, which a
// network extension does not have; the OS sets it from the tunnel settings.
// CreateTUNFromFile closes the file itself if it fails.
func fromFD(fd int) (tun.Device, error) {
	return tun.CreateTUNFromFile(os.NewFile(uintptr(fd), "utun"), 0)
}

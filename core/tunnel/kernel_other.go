//go:build !(darwin && !ios) && !(linux && !android)

package tunnel

import (
	"errors"
	"runtime"
)

// On phones the operating system creates and configures the TUN (VpnService,
// NEPacketTunnelProvider) and hands us a file descriptor; see FromFD.
func tunName(want string) string { return want }

func configure(string, KernelOptions) error {
	return errors.New("configuring a TUN interface is not implemented on " + runtime.GOOS)
}

func LoosenReversePath(string) {}

func blockIPv6() func() { return nil }

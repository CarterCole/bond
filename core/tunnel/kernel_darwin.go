//go:build darwin && !ios

package tunnel

// macOS only has utun devices and numbers them itself.
func tunName(string) string { return "utun" }

func configure(name string, o KernelOptions) error {
	addr := o.Address.Addr().String()
	// A point-to-point interface whose peer is itself, as wg-quick does.
	if err := run("ifconfig", name, "inet", o.Address.String(), addr, "alias"); err != nil {
		return err
	}
	if err := run("ifconfig", name, "up"); err != nil {
		return err
	}
	routes := append([]string{o.Address.Masked().String()}, prefixStrings(o.Routes)...)
	for _, r := range routes {
		if err := run("route", "-q", "-n", "add", "-inet", r, "-interface", name); err != nil {
			return err
		}
	}
	return nil
}

// LoosenReversePath is a Linux concern.
func LoosenReversePath(string) {}

// blockIPv6 installs reject routes, so a connection attempt fails
// immediately instead of timing out. Best effort.
func blockIPv6() (undo func()) {
	var added []string
	for _, half := range ipv6Internet {
		if run("route", "-q", "-n", "add", "-inet6", half, "::1", "-reject") == nil {
			added = append(added, half)
		}
	}
	if len(added) == 0 {
		return nil
	}
	return func() {
		for _, half := range added {
			run("route", "-q", "-n", "delete", "-inet6", half, "::1")
		}
	}
}

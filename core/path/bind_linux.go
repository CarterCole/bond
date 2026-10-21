package path

import "golang.org/x/sys/unix"

// bindToInterface pins the socket to one device with SO_BINDTODEVICE, which
// restricts route lookup to routes through that device. It needs
// CAP_NET_RAW; Android apps don't have it and use the SocketHook instead.
func bindToInterface(fd int, _, name string) error {
	return unix.BindToDevice(fd, name)
}

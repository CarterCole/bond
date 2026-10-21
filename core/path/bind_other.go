//go:build !darwin && !linux

package path

import (
	"fmt"
	"runtime"
)

func bindToInterface(_ int, _, name string) error {
	return fmt.Errorf("pinning a socket to interface %s is not implemented on %s", name, runtime.GOOS)
}

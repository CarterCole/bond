//go:build !(darwin && !ios) && !(linux && !android)

package gateway

import (
	"errors"
	"net/netip"
	"runtime"
)

var errUnsupported = errors.New("router mode is not available on " + runtime.GOOS)

func Aggregator(string, netip.Prefix) (Undo, error) { return nil, errUnsupported }
func Hub(string, []string) (Undo, error)            { return nil, errUnsupported }

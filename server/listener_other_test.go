//go:build !linux

package server

import (
	"net"
	"testing"
)

// ponytail: SO_ACCEPTCONN lookup is Linux-only here, so release checks pass
// elsewhere; CI runs Linux.
func listenerFD(int) int { return -1 }

func shutdownListener(t *testing.T, _ *net.TCPAddr) {
	t.Skip("needs a Unix listening socket")
}

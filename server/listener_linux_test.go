//go:build linux

package server

import (
	"net"
	"syscall"
	"testing"
)

// shutdownListener makes the kernel fail accept on the socket listening at
// addr, as a listener that died would.
func shutdownListener(t *testing.T, addr *net.TCPAddr) {
	t.Helper()
	fd := listenerFD(addr.Port)
	if fd < 0 {
		t.Fatal("listener socket not found")
	}
	if err := syscall.Shutdown(fd, syscall.SHUT_RD); err != nil {
		t.Fatal(err)
	}
}

// listenerFD finds this process's IPv4 TCP listening socket on port, or -1.
// Tests check release this way rather than by dialling or rebinding, which
// another process may answer or race for once the port is free.
func listenerFD(port int) int {
	for fd := 3; fd < 4096; fd++ {
		sa, err := syscall.Getsockname(fd)
		in, ok := sa.(*syscall.SockaddrInet4)
		if err != nil || !ok || in.Port != port {
			continue
		}
		if listening, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_ACCEPTCONN); err == nil && listening == 1 {
			return fd
		}
	}
	return -1
}

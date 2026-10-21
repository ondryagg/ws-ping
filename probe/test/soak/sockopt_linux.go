//go:build soak && linux

package soak

import "syscall"

// rcvBufControl sets SO_RCVBUF before connect so the advertised TCP window is
// small from the first packet.
func rcvBufControl(n int) func(network, address string, c syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, n)
		}); err != nil {
			return err
		}
		return serr
	}
}

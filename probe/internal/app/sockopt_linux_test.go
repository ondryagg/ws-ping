package app_test

import "syscall"

type syscallConn = syscall.RawConn

func setRcvBuf(rc syscall.RawConn, n int) error {
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, n)
	}); err != nil {
		return err
	}
	return serr
}

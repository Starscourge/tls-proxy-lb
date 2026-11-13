package upstream

import (
	"net"
	"syscall"
)

// alive peeks at an idle connection's socket without blocking. A backend
// that closed the connection has left a FIN (read returns 0) or a reset
// (an error); one that sent bytes no request asked for has broken the
// framing. Only "nothing to read yet" means the connection is usable.
//
// It costs one recvfrom system call. The connection must have no read
// deadline in the past, or the runtime refuses the read.
func alive(c net.Conn) bool {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return true
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return false
	}
	var buf [1]byte
	usable := false
	err = rc.Read(func(fd uintptr) bool {
		for {
			n, _, err := syscall.Recvfrom(int(fd), buf[:], syscall.MSG_PEEK|syscall.MSG_DONTWAIT)
			if err == syscall.EINTR {
				continue
			}
			usable = n <= 0 && err == syscall.EAGAIN
			// Returning true always: false would park the goroutine until
			// the socket is readable, which for a healthy idle connection
			// is never.
			return true
		}
	})
	return err == nil && usable
}

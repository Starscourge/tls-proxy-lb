package upstream

import "syscall"

// alive peeks at an idle connection's socket without blocking. A backend
// that closed the connection has left a FIN (read returns 0) or a reset
// (an error); one that sent bytes no request asked for has broken the
// framing. Only "nothing to read yet" means the connection is usable.
//
// It costs one recvfrom system call. The connection must have no read
// deadline in the past, or the runtime refuses the read.
func (c *Conn) alive() bool {
	if c.peek == nil {
		sc, ok := c.Conn.(syscall.Conn)
		if !ok {
			return true
		}
		raw, err := sc.SyscallConn()
		if err != nil {
			return false
		}
		c.raw = raw
		var buf [1]byte
		c.peek = func(fd uintptr) bool {
			for {
				_, _, err := syscall.Recvfrom(int(fd), buf[:], syscall.MSG_PEEK|syscall.MSG_DONTWAIT)
				if err == syscall.EINTR {
					continue
				}
				c.peekLive = err == syscall.EAGAIN
				// Always true: false would park the goroutine until the
				// socket is readable, which for a healthy idle connection
				// is never.
				return true
			}
		}
	}
	c.peekLive = false
	return c.raw.Read(c.peek) == nil && c.peekLive
}

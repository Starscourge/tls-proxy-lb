//go:build !linux

package upstream

import "net"

// alive cannot peek without blocking on this platform; a stale connection
// is found when the request on it fails, and the request is retried.
func alive(net.Conn) bool { return true }

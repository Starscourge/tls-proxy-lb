//go:build !linux

package upstream

// alive cannot peek without blocking on this platform; a stale connection
// is found when the request on it fails, and the request is retried.
func (c *Conn) alive() bool { return true }

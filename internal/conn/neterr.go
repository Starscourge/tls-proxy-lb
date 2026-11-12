package conn

import (
	"errors"
	"net"
	"os"
)

// isTimeout reports whether err is a deadline or dial timeout.
func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

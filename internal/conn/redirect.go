package conn

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/config"
	"github.com/Starscourge/tls-proxy-lb/internal/httpx"
)

// Redirect answers every request on a plain HTTP connection with a 308
// Permanent Redirect to the same URL over HTTPS. It implements Handler.
//
// Requests are read with the same deadlines and header limit as the proxy
// uses. Only hosts the proxy would serve are redirected (a host with a
// route, or any host when there is a default pool); others get 404, so the
// listener cannot be used to bounce clients to an arbitrary site. As with
// every response the proxy generates itself, the connection closes after
// it.
type Redirect struct {
	proxy Proxy
	port  int
}

// NewRedirect returns a handler redirecting to HTTPS on port; 443 is left
// out of the URL. A nil logger discards.
func NewRedirect(store *config.Store, logger *slog.Logger, port int) *Redirect {
	return &Redirect{proxy: Proxy{Config: store, Logger: logger}, port: port}
}

// Validate checks the handler's settings.
func (r *Redirect) Validate() error {
	if r.port < 1 || r.port > 65535 {
		return errors.New("redirect: port must be from 1 to 65535")
	}
	return r.proxy.Validate()
}

// ServeConn reads requests from tc and answers each with a redirect.
func (r *Redirect) ServeConn(ctx context.Context, tc *TrackedConn) {
	cc := newClientConn(&r.proxy, tc, r.proxy.Config.Load())
	defer cc.release()
	cc.redirectPort = r.port
	for first := true; cc.serveRequest(ctx, first); first = false {
	}
	if cc.linger {
		cc.lingeringClose()
	}
}

// redirect answers req with a redirect to HTTPS and ends the connection.
func (cc *clientConn) redirect(snap *config.Snapshot, req *http.Request, start time.Time) bool {
	rec := cc.newRecord(start, req)
	if snap.Route(req.Host) == nil {
		return cc.reject(rec, req, http.StatusNotFound, "no route for host")
	}
	loc := httpsURL(config.NormaliseHost(req.Host), cc.redirectPort, req.URL.RequestURI())
	cc.linger = req.Body != http.NoBody
	cc.dw.Timeout = errorWriteTimeout
	if err := httpx.WriteRedirect(cc.bw, http.StatusPermanentRedirect, loc, req.Method == http.MethodHead); err != nil {
		rec.err = err
	}
	rec.status = http.StatusPermanentRedirect
	cc.logAccess(rec)
	return false
}

// httpsURL builds https://host[:port]uri. host is normalised (no port, no
// brackets); an IPv6 literal gets its brackets back.
func httpsURL(host string, port int, uri string) string {
	var b strings.Builder
	b.WriteString("https://")
	if strings.IndexByte(host, ':') >= 0 {
		b.WriteString("[" + host + "]")
	} else {
		b.WriteString(host)
	}
	if port != 443 {
		b.WriteString(":" + strconv.Itoa(port))
	}
	b.WriteString(uri)
	return b.String()
}

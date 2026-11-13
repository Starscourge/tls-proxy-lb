package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/textproto"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/Starscourge/tls-proxy-lb/internal/lb"
)

// The file format, as decoded. Every field is checked by validate.
type file struct {
	DefaultPool string              `toml:"default_pool"`
	Listeners   []fileListener      `toml:"listener"`
	Routes      []fileRoute         `toml:"route"`
	Pools       map[string]filePool `toml:"pool"`
	TLS         fileTLS             `toml:"tls"`
	Timeouts    fileTimeouts        `toml:"timeouts"`
	Limits      fileLimits          `toml:"limits"`
	Upstream    fileUpstream        `toml:"upstream"`
	Admin       fileAdmin           `toml:"admin"`
	Log         fileLog             `toml:"log"`
}

type fileLog struct {
	BufferBytes int64 `toml:"buffer_bytes"`
	FlushMS     int64 `toml:"flush_ms"`
}

type fileUpstream struct {
	MaxIdlePerBackend int64 `toml:"max_idle_per_backend"`
	IdleTimeoutMS     int64 `toml:"idle_timeout_ms"`
}

type fileAdmin struct {
	Address              string `toml:"address"`
	MutexProfileFraction int64  `toml:"mutex_profile_fraction"`
	BlockProfileRateNS   int64  `toml:"block_profile_rate_ns"`
}

type fileListener struct {
	Address      string `toml:"address"`
	Mode         string `toml:"mode"`
	RedirectPort *int64 `toml:"redirect_port"` // nil when absent
}

type fileTLS struct {
	DefaultCert string `toml:"default_cert"`
	DefaultKey  string `toml:"default_key"`
}

type fileRoute struct {
	Host string `toml:"host"`
	Pool string `toml:"pool"`
	Cert string `toml:"cert"`
	Key  string `toml:"key"`
}

type filePool struct {
	Algorithm string        `toml:"algorithm"`
	HashKey   string        `toml:"hash_key"`
	Backends  []fileBackend `toml:"backends"`
}

type fileBackend struct {
	Address string `toml:"address"`
}

type fileTimeouts struct {
	HandshakeMS        int64 `toml:"handshake_ms"`
	HeaderReadMS       int64 `toml:"header_read_ms"`
	IdleMS             int64 `toml:"idle_ms"`
	ClientIOMS         int64 `toml:"client_io_ms"`
	UpstreamConnectMS  int64 `toml:"upstream_connect_ms"`
	UpstreamResponseMS int64 `toml:"upstream_response_ms"`
}

type fileLimits struct {
	MaxConns       int64 `toml:"max_conns"`
	MaxHeaderBytes int64 `toml:"max_header_bytes"`
}

// Bounds on numeric settings, so a typo cannot ask for something absurd.
const (
	maxTimeoutMS      = 24 * 60 * 60 * 1000 // a day
	maxMaxConns       = 10_000_000
	maxMaxHeaderBytes = 16 << 20
	maxMaxIdle        = 100_000
	maxProfileRate    = 1_000_000_000
	maxLogBuffer      = 16 << 20
	maxLogFlushMS     = 60_000
)

// hashKeyClientIP and hashKeyHeaderPrefix are the forms of hash_key.
const (
	hashKeyClientIP     = "client_ip"
	hashKeyHeaderPrefix = "header:"
)

// FieldError is one problem with a config file, located by the field's path
// in the file, such as "pool.pool_a.backends[1].address".
type FieldError struct {
	File  string
	Field string
	Msg   string
}

func (e *FieldError) Error() string {
	if e.File == "" {
		return e.Field + ": " + e.Msg
	}
	return e.File + ": " + e.Field + ": " + e.Msg
}

// Load reads and parses the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(path, data)
}

// Parse decodes and validates a config file's contents. name prefixes every
// error message, and relative certificate paths are resolved against its
// directory. It returns every problem found, joined with errors.Join,
// each a *FieldError, or a single decoding error for a file that is not
// valid TOML or has a value of the wrong type.
func Parse(name string, data []byte) (*Config, error) {
	var f file
	md, err := toml.Decode(string(data), &f)
	if err != nil {
		// The decoder's messages name the line and the last key read.
		return nil, fmt.Errorf("%s: %s", name, strings.TrimPrefix(err.Error(), "toml: "))
	}
	v := validator{file: name, dir: filepath.Dir(name), md: md}
	for _, k := range md.Undecoded() {
		v.errorf(k.String(), "unknown field")
	}
	c := v.config(&f)
	if len(v.errs) > 0 {
		return nil, errors.Join(v.errs...)
	}
	return c, nil
}

// validator collects field errors while building a Config.
type validator struct {
	file string
	dir  string // where relative paths start
	md   toml.MetaData
	errs []error
}

func (v *validator) errorf(field, format string, args ...any) {
	v.errs = append(v.errs, &FieldError{File: v.file, Field: field, Msg: fmt.Sprintf(format, args...)})
}

func (v *validator) config(f *file) *Config {
	c := &Config{
		Source:   v.file,
		Timeouts: v.timeouts(&f.Timeouts),
		Limits:   v.limits(&f.Limits),
		Upstream: v.upstream(&f.Upstream),
		Admin:    v.admin(&f.Admin),
		Log:      v.log(&f.Log),
	}
	c.Listeners = v.listeners(f.Listeners)
	c.TLS = v.tls(&f.TLS)
	c.Pools = v.pools(f.Pools)
	known := make(map[string]bool, len(f.Pools))
	for name := range f.Pools {
		known[name] = true
	}
	c.Routes = v.routes(f.Routes, known)
	if f.DefaultPool != "" && !known[f.DefaultPool] {
		v.errorf("default_pool", "no pool named %q", f.DefaultPool)
	}
	c.DefaultPool = f.DefaultPool
	if len(f.Routes) == 0 && f.DefaultPool == "" {
		v.errorf("route", "at least one [[route]] or a default_pool is required")
	}
	v.certificatesCover(c, f.Routes)
	return c
}

// certificatesCover checks that HTTPS listeners can complete a handshake
// for every route: each route needs its own certificate unless there is a
// default one.
func (v *validator) certificatesCover(c *Config, routes []fileRoute) {
	first := slices.IndexFunc(c.Listeners, func(l Listener) bool { return l.Mode == ModeHTTPS })
	if first < 0 || c.TLS.DefaultCert != "" {
		return
	}
	if len(routes) == 0 {
		v.errorf(fmt.Sprintf("listener[%d].mode", first), "%q needs a certificate: set default_cert and default_key in [tls], or add routes with cert and key", ModeHTTPS)
		return
	}
	for i, r := range routes {
		if r.Cert == "" && r.Key == "" {
			v.errorf(fmt.Sprintf("route[%d].cert", i), "is required: an %s listener serves this host and [tls] has no default_cert", ModeHTTPS)
		}
	}
}

// tls validates the [tls] table.
func (v *validator) tls(ft *fileTLS) TLS {
	cert, key := v.certPair("tls.default_cert", "tls.default_key", ft.DefaultCert, ft.DefaultKey)
	return TLS{DefaultCert: cert, DefaultKey: key}
}

// certPair checks that a certificate and key are set together and resolves
// them against the config file's directory.
func (v *validator) certPair(certField, keyField, cert, key string) (string, string) {
	switch {
	case cert == "" && key == "":
		return "", ""
	case cert == "":
		v.errorf(certField, "is required when %s is set", keyField[strings.LastIndexByte(keyField, '.')+1:])
		return "", ""
	case key == "":
		v.errorf(keyField, "is required when %s is set", certField[strings.LastIndexByte(certField, '.')+1:])
		return "", ""
	}
	return v.path(cert), v.path(key)
}

func (v *validator) path(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(v.dir, p)
}

func (v *validator) listeners(ls []fileListener) []Listener {
	if len(ls) == 0 {
		v.errorf("listener", "at least one [[listener]] is required")
	}
	seen := map[string]int{}
	out := make([]Listener, 0, len(ls))
	for i, l := range ls {
		prefix := fmt.Sprintf("listener[%d]", i)
		ok := true
		mode := l.Mode
		if mode == "" {
			mode = ModeHTTPS
		}
		if !slices.Contains(Modes, mode) {
			v.errorf(prefix+".mode", "%q is not one of %s", l.Mode, strings.Join(Modes, ", "))
			ok = false
		}
		port := 0
		if l.RedirectPort != nil {
			switch rp := *l.RedirectPort; {
			case mode != ModeRedirect:
				v.errorf(prefix+".redirect_port", "is only valid with mode = %q", ModeRedirect)
				ok = false
			case rp < 1 || rp > 65535:
				v.errorf(prefix+".redirect_port", "must be from 1 to 65535, got %d", rp)
				ok = false
			default:
				port = int(rp)
			}
		} else if mode == ModeRedirect {
			port = 443
		}
		field := prefix + ".address"
		if l.Address == "" {
			v.errorf(field, "is required")
			continue
		}
		if err := checkHostPort(l.Address, true); err != nil {
			v.errorf(field, "%v", err)
			continue
		}
		// Port 0 asks for any free port, so two such listeners never clash.
		if _, p, _ := net.SplitHostPort(l.Address); p != "0" {
			if j, dup := seen[l.Address]; dup {
				v.errorf(field, "%q is also listener[%d]", l.Address, j)
				continue
			}
			seen[l.Address] = i
		}
		if ok {
			out = append(out, Listener{Address: l.Address, Mode: mode, RedirectPort: port})
		}
	}
	return out
}

func (v *validator) pools(ps map[string]filePool) []Pool {
	if len(ps) == 0 {
		v.errorf("pool", "at least one [pool.<name>] is required")
	}
	names := make([]string, 0, len(ps))
	for name := range ps {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]Pool, 0, len(ps))
	for _, name := range names {
		out = append(out, v.pool(name, ps[name]))
	}
	return out
}

func (v *validator) pool(name string, fp filePool) Pool {
	prefix := "pool." + name
	if !validPoolName(name) {
		v.errorf(prefix, "pool name %q may use only letters, digits, '_' and '-'", name)
	}
	p := Pool{Name: name, Algorithm: lb.Algorithm(fp.Algorithm)}
	if p.Algorithm == "" {
		p.Algorithm = lb.RoundRobin
	}
	if !slices.Contains(lb.Algorithms, p.Algorithm) {
		v.errorf(prefix+".algorithm", "%q is not one of %s", fp.Algorithm, algorithmList())
	}
	if fp.HashKey != "" {
		field := prefix + ".hash_key"
		switch {
		case p.Algorithm != lb.ConsistentHash:
			v.errorf(field, "is only valid with algorithm = %q", lb.ConsistentHash)
		case fp.HashKey == hashKeyClientIP:
		case strings.HasPrefix(fp.HashKey, hashKeyHeaderPrefix):
			h := strings.TrimPrefix(fp.HashKey, hashKeyHeaderPrefix)
			if !validToken(h) {
				v.errorf(field, "%q is not a valid header name", h)
			} else {
				p.HashHeader = textproto.CanonicalMIMEHeaderKey(h)
			}
		default:
			v.errorf(field, "%q must be %q or %q", fp.HashKey, hashKeyClientIP, hashKeyHeaderPrefix+"<Header-Name>")
		}
	}
	if len(fp.Backends) == 0 {
		v.errorf(prefix+".backends", "at least one backend is required")
	}
	seen := map[string]int{}
	for i, b := range fp.Backends {
		field := fmt.Sprintf("%s.backends[%d].address", prefix, i)
		switch err := checkHostPort(b.Address, false); {
		case b.Address == "":
			v.errorf(field, "is required")
		case err != nil:
			v.errorf(field, "%v", err)
		default:
			if j, dup := seen[b.Address]; dup {
				v.errorf(field, "%q is also backends[%d]", b.Address, j)
				continue
			}
			seen[b.Address] = i
			p.Backends = append(p.Backends, b.Address)
		}
	}
	return p
}

func (v *validator) routes(rs []fileRoute, pools map[string]bool) []Route {
	seen := map[string]int{}
	out := make([]Route, 0, len(rs))
	for i, r := range rs {
		prefix := fmt.Sprintf("route[%d]", i)
		ok := true
		host, err := normaliseRouteHost(r.Host)
		if err != nil {
			v.errorf(prefix+".host", "%v", err)
			ok = false
		} else if j, dup := seen[host]; dup {
			v.errorf(prefix+".host", "%q is also route[%d]", host, j)
			ok = false
		}
		switch {
		case r.Pool == "":
			v.errorf(prefix+".pool", "is required")
			ok = false
		case !pools[r.Pool]:
			v.errorf(prefix+".pool", "no pool named %q", r.Pool)
			ok = false
		}
		cert, key := v.certPair(prefix+".cert", prefix+".key", r.Cert, r.Key)
		if ok {
			seen[host] = i
			out = append(out, Route{Host: host, Pool: r.Pool, Cert: cert, Key: key})
		}
	}
	return out
}

func (v *validator) timeouts(ft *fileTimeouts) Timeouts {
	t := DefaultTimeouts()
	for _, f := range []struct {
		key string
		ms  int64
		dst *time.Duration
	}{
		{"handshake_ms", ft.HandshakeMS, &t.Handshake},
		{"header_read_ms", ft.HeaderReadMS, &t.HeaderRead},
		{"idle_ms", ft.IdleMS, &t.Idle},
		{"client_io_ms", ft.ClientIOMS, &t.ClientIO},
		{"upstream_connect_ms", ft.UpstreamConnectMS, &t.UpstreamConnect},
		{"upstream_response_ms", ft.UpstreamResponseMS, &t.UpstreamResponse},
	} {
		if !v.md.IsDefined("timeouts", f.key) {
			continue
		}
		if f.ms <= 0 || f.ms > maxTimeoutMS {
			v.errorf("timeouts."+f.key, "must be from 1 to %d milliseconds, got %d", maxTimeoutMS, f.ms)
			continue
		}
		*f.dst = time.Duration(f.ms) * time.Millisecond
	}
	return t
}

func (v *validator) limits(fl *fileLimits) Limits {
	l := DefaultLimits()
	for _, f := range []struct {
		key string
		n   int64
		max int64
		dst *int
	}{
		{"max_conns", fl.MaxConns, maxMaxConns, &l.MaxConns},
		{"max_header_bytes", fl.MaxHeaderBytes, maxMaxHeaderBytes, &l.MaxHeaderBytes},
	} {
		if !v.md.IsDefined("limits", f.key) {
			continue
		}
		if f.n <= 0 || f.n > f.max {
			v.errorf("limits."+f.key, "must be from 1 to %d, got %d", f.max, f.n)
			continue
		}
		*f.dst = int(f.n)
	}
	return l
}

func (v *validator) upstream(fu *fileUpstream) Upstream {
	u := DefaultUpstream()
	if v.md.IsDefined("upstream", "max_idle_per_backend") {
		if n := fu.MaxIdlePerBackend; n < 0 || n > maxMaxIdle {
			v.errorf("upstream.max_idle_per_backend", "must be from 0 to %d, got %d", maxMaxIdle, n)
		} else {
			u.MaxIdlePerBackend = int(n)
		}
	}
	if v.md.IsDefined("upstream", "idle_timeout_ms") {
		if ms := fu.IdleTimeoutMS; ms <= 0 || ms > maxTimeoutMS {
			v.errorf("upstream.idle_timeout_ms", "must be from 1 to %d milliseconds, got %d", maxTimeoutMS, ms)
		} else {
			u.IdleTimeout = time.Duration(ms) * time.Millisecond
		}
	}
	return u
}

func (v *validator) log(fl *fileLog) Log {
	l := DefaultLog()
	if v.md.IsDefined("log", "buffer_bytes") {
		if n := fl.BufferBytes; n < 0 || n > maxLogBuffer {
			v.errorf("log.buffer_bytes", "must be from 0 to %d, got %d", maxLogBuffer, n)
		} else {
			l.BufferBytes = int(n)
		}
	}
	if v.md.IsDefined("log", "flush_ms") {
		if ms := fl.FlushMS; ms <= 0 || ms > maxLogFlushMS {
			v.errorf("log.flush_ms", "must be from 1 to %d milliseconds, got %d", maxLogFlushMS, ms)
		} else {
			l.FlushInterval = time.Duration(ms) * time.Millisecond
		}
	}
	return l
}

// admin validates the [admin] table. The listener serves pprof, which
// exposes memory contents, so it may only bind a loopback address.
func (v *validator) admin(fa *fileAdmin) Admin {
	a := Admin{Address: fa.Address}
	if v.md.IsDefined("admin", "address") {
		if err := checkLoopback(fa.Address); err != nil {
			v.errorf("admin.address", "%v", err)
			a.Address = ""
		}
	}
	for _, f := range []struct {
		key string
		n   int64
		dst *int
	}{
		{"mutex_profile_fraction", fa.MutexProfileFraction, &a.MutexProfileFraction},
		{"block_profile_rate_ns", fa.BlockProfileRateNS, &a.BlockProfileRate},
	} {
		if !v.md.IsDefined("admin", f.key) {
			continue
		}
		if f.n < 0 || f.n > maxProfileRate {
			v.errorf("admin."+f.key, "must be from 0 to %d, got %d", maxProfileRate, f.n)
			continue
		}
		*f.dst = int(f.n)
	}
	return a
}

// checkLoopback checks that addr is host:port with a loopback IP or
// "localhost" as the host. Port 0 picks a free port.
func checkLoopback(addr string) error {
	if err := checkHostPort(addr, true); err != nil {
		return err
	}
	host, _, _ := net.SplitHostPort(addr)
	if host == "localhost" {
		return nil
	}
	if a, err := netip.ParseAddr(host); err == nil && a.Unmap().IsLoopback() {
		return nil
	}
	return fmt.Errorf("%q: host must be a loopback address such as 127.0.0.1, ::1 or localhost", addr)
}

func algorithmList() string {
	s := make([]string, len(lb.Algorithms))
	for i, a := range lb.Algorithms {
		s[i] = string(a)
	}
	return strings.Join(s, ", ")
}

// checkHostPort checks a host:port address. A listener may leave the host
// empty (all interfaces) and use port 0 (any free port); a backend may not.
func checkHostPort(addr string, listener bool) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		var ae *net.AddrError
		if errors.As(err, &ae) {
			err = errors.New(ae.Err)
		}
		return fmt.Errorf("%q is not host:port: %v", addr, err)
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || (n == 0 && !listener) {
		lo := 1
		if listener {
			lo = 0
		}
		return fmt.Errorf("%q: port %q is not a number from %d to 65535", addr, port, lo)
	}
	if host == "" {
		if listener {
			return nil
		}
		return fmt.Errorf("%q: host is empty", addr)
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return nil
	}
	if err := checkHostname(host); err != nil {
		return fmt.Errorf("%q: %v", addr, err)
	}
	return nil
}

// normaliseRouteHost validates a route's host and returns it in the form
// NormaliseHost gives request hosts.
func normaliseRouteHost(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("is required")
	}
	h := strings.ToLower(strings.TrimSuffix(raw, "."))
	if inner, ok := strings.CutPrefix(h, "["); ok {
		inner, ok = strings.CutSuffix(inner, "]")
		if a, err := netip.ParseAddr(inner); !ok || err != nil || !a.Is6() {
			return "", fmt.Errorf("%q is not a bracketed IPv6 address", raw)
		}
		return inner, nil
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return h, nil
	}
	if strings.Contains(h, ":") {
		return "", fmt.Errorf("%q must not include a port", raw)
	}
	if err := checkHostname(h); err != nil {
		return "", fmt.Errorf("%q: %v", raw, err)
	}
	return h, nil
}

// checkHostname checks DNS name syntax: dot-separated labels of 1 to 63
// letters, digits, '-' or '_', not starting or ending with '-', at most 253
// characters in all. '_' is not valid in a hostname but Docker service
// names may use it.
func checkHostname(h string) error {
	if len(h) > 253 {
		return errors.New("hostname is longer than 253 characters")
	}
	for label := range strings.SplitSeq(h, ".") {
		if label == "" || len(label) > 63 {
			return errors.New("hostname labels must be 1 to 63 characters")
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("hostname label %q starts or ends with '-'", label)
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return fmt.Errorf("hostname contains %q", c)
			}
		}
	}
	return nil
}

func validPoolName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// validToken reports whether s is an HTTP token (RFC 9110 section 5.6.2),
// the syntax of a header name.
func validToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		if !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			return false
		}
	}
	return true
}

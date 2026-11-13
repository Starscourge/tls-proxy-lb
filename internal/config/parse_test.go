package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/Starscourge/tls-proxy-lb/internal/lb"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

const fullConfig = `
default_pool = "pool_b"

[[listener]]
address = "0.0.0.0:8080"
mode    = "http"

[[listener]]
address = "127.0.0.1:0"

[[listener]]
address       = "127.0.0.1:8081"
mode          = "redirect"
redirect_port = 8443

[[route]]
host = "A.Test."
pool = "pool_a"
cert = "certs/a.test.crt"
key  = "certs/a.test.key"

[[route]]
host = "[::1]"
pool = "pool_b"

[pool.pool_a]
algorithm = "least_conn"
backends  = [
  { address = "10.0.0.11:8000" },
  { address = "10.0.0.12:8000" },
]

[pool.pool_b]
algorithm = "consistent_hash"
hash_key  = "header:x-user-id"
backends  = [{ address = "backend_3:8000" }]

[pool.pool_c]
backends = [{ address = "[::1]:9000" }]

[tls]
default_cert = "certs/default.crt"
default_key  = "/etc/proxy/default.key"

[timeouts]
handshake_ms         = 1500
header_read_ms       = 2500
upstream_connect_ms  = 300
upstream_response_ms = 4000
idle_ms              = 30000
client_io_ms         = 15000

[limits]
max_conns        = 500
max_header_bytes = 8192

[upstream]
max_idle_per_backend = 16
idle_timeout_ms      = 9000

[admin]
address                = "127.0.0.1:9090"
mutex_profile_fraction = 100
block_profile_rate_ns  = 10000

[log]
buffer_bytes = 0
flush_ms     = 250
`

func TestParseFull(t *testing.T) {
	c, err := Parse("test.toml", []byte(fullConfig))
	if err != nil {
		t.Fatal(err)
	}
	want := &Config{
		Source: "test.toml",
		Listeners: []Listener{
			{Address: "0.0.0.0:8080", Mode: ModeHTTP},
			{Address: "127.0.0.1:0", Mode: ModeHTTPS},
			{Address: "127.0.0.1:8081", Mode: ModeRedirect, RedirectPort: 8443},
		},
		Routes: []Route{
			{Host: "a.test", Pool: "pool_a", Cert: "certs/a.test.crt", Key: "certs/a.test.key"},
			{Host: "::1", Pool: "pool_b"},
		},
		DefaultPool: "pool_b",
		Pools: []Pool{
			{Name: "pool_a", Algorithm: lb.LeastConn, Backends: []string{"10.0.0.11:8000", "10.0.0.12:8000"}},
			{Name: "pool_b", Algorithm: lb.ConsistentHash, HashHeader: "X-User-Id", Backends: []string{"backend_3:8000"}},
			{Name: "pool_c", Algorithm: lb.RoundRobin, Backends: []string{"[::1]:9000"}},
		},
		TLS: TLS{DefaultCert: "certs/default.crt", DefaultKey: "/etc/proxy/default.key"},
		Timeouts: Timeouts{
			Handshake:        1500 * time.Millisecond,
			HeaderRead:       2500 * time.Millisecond,
			Idle:             30 * time.Second,
			ClientIO:         15 * time.Second,
			UpstreamConnect:  300 * time.Millisecond,
			UpstreamResponse: 4 * time.Second,
		},
		Limits:   Limits{MaxConns: 500, MaxHeaderBytes: 8192},
		Upstream: Upstream{MaxIdlePerBackend: 16, IdleTimeout: 9 * time.Second},
		Admin:    Admin{Address: "127.0.0.1:9090", MutexProfileFraction: 100, BlockProfileRate: 10000},
		Log:      Log{BufferBytes: 0, FlushInterval: 250 * time.Millisecond},
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("got\n%+v\nwant\n%+v", c, want)
	}
}

const minimal = `
[[listener]]
address = ":8080"
mode = "http"

[[route]]
host = "a.test"
pool = "p"

[pool.p]
backends = [{ address = "127.0.0.1:8000" }]
`

func TestParseDefaults(t *testing.T) {
	c, err := Parse("min.toml", []byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeouts != DefaultTimeouts() || c.Limits != DefaultLimits() || c.Upstream != DefaultUpstream() || c.Admin != (Admin{}) || c.Log != DefaultLog() {
		t.Fatalf("timeouts %+v limits %+v upstream %+v admin %+v log %+v, want defaults", c.Timeouts, c.Limits, c.Upstream, c.Admin, c.Log)
	}
	// max_idle_per_backend = 0 turns reuse off; admin may bind ::1 or
	// localhost, port 0 included.
	for _, extra := range []string{
		"[upstream]\nmax_idle_per_backend = 0\n",
		"[admin]\naddress = \"[::1]:0\"\n",
		"[admin]\naddress = \"localhost:6060\"\n",
	} {
		if _, err := Parse("ok.toml", []byte(minimal+extra)); err != nil {
			t.Fatalf("%s: %v", extra, err)
		}
	}
	if c.Pools[0].Algorithm != lb.RoundRobin || c.DefaultPool != "" {
		t.Fatalf("pool %+v default %q", c.Pools[0], c.DefaultPool)
	}
	// Explicit hash_key = "client_ip" is the same as leaving it out.
	c, err = Parse("ch.toml", []byte(strings.Replace(minimal, "[pool.p]", "[pool.p]\nalgorithm = \"consistent_hash\"\nhash_key = \"client_ip\"", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if c.Pools[0].HashHeader != "" {
		t.Fatalf("client_ip gave header %q", c.Pools[0].HashHeader)
	}
}

// Several listeners on port 0 each get their own free port.
func TestParseListenersOnPortZero(t *testing.T) {
	src := "[[listener]]\naddress = \"127.0.0.1:0\"\nmode = \"http\"\n" + strings.Replace(minimal, ":8080", "127.0.0.1:0", 1)
	c, err := Parse("z.toml", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Listeners) != 2 {
		t.Fatalf("listeners %v", c.Listeners)
	}
}

// A default_pool alone, with no routes, is a valid catch-all config.
func TestParseDefaultPoolOnly(t *testing.T) {
	src := "default_pool = \"p\"\n" + strings.Replace(minimal, "[[route]]\nhost = \"a.test\"\npool = \"p\"\n", "", 1)
	if _, err := Parse("d.toml", []byte(src)); err != nil {
		t.Fatal(err)
	}
}

const base = `
[[listener]]
address = ":8080"
mode = "http"
`

const pool = `
[pool.p]
backends = [{ address = "127.0.0.1:8000" }]
`

const route = `
[[route]]
host = "a.test"
pool = "p"
`

// TestParseRejects: each invalid file is rejected with an error that names
// the field at fault.
func TestParseRejects(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string // expected in the error, after "bad.toml: "
	}{
		{"syntax", base + "[pool.p\n", `line 6 (last key "listener"): expected '.' or ']' to end table name`},
		{"wrong type", base + route + pool + "[timeouts]\nidle_ms = \"60s\"\n", `line 13 (last key "timeouts.idle_ms"): incompatible types`},
		{"unknown top-level key", "listen = \":80\"\n" + base + route + pool, "listen: unknown field"},
		{"unknown pool key", base + route + "[pool.p]\nalgoritm = \"least_conn\"\nbackends = [{ address = \"a:1\" }]\n", "pool.p.algoritm: unknown field"},
		{"unknown backend key", base + route + "[pool.p]\nbackends = [{ adress = \"a:1\" }]\n", "pool.p.backends.adress: unknown field"},
		{"unknown table", base + route + pool + "[timeout]\nidle_ms = 5\n", "timeout.idle_ms: unknown field"},
		{"no listener", route + pool, "listener: at least one [[listener]] is required"},
		{"listener no address", "[[listener]]\n" + route + pool, "listener[0].address: is required"},
		{"listener no port", "[[listener]]\naddress = \"0.0.0.0\"\n" + route + pool, `listener[0].address: "0.0.0.0" is not host:port: missing port in address`},
		{"listener named port", "[[listener]]\naddress = \":http\"\n" + route + pool, `listener[0].address: ":http": port "http" is not a number from 0 to 65535`},
		{"listener duplicate", base + base + route + pool, `listener[1].address: ":8080" is also listener[0]`},
		{"no pool", base + route, "pool: at least one [pool.<name>] is required"},
		{"pool name", base + "[[route]]\nhost = \"a.test\"\npool = \"p q\"\n[pool.\"p q\"]\nbackends = [{ address = \"a:1\" }]\n", `pool.p q: pool name "p q" may use only`},
		{"no backends", base + route + "[pool.p]\nalgorithm = \"round_robin\"\n", "pool.p.backends: at least one backend is required"},
		{"empty backends", base + route + "[pool.p]\nbackends = []\n", "pool.p.backends: at least one backend is required"},
		{"backend no address", base + route + "[pool.p]\nbackends = [{ address = \"a:1\" }, {}]\n", "pool.p.backends[1].address: is required"},
		{"backend no port", base + route + "[pool.p]\nbackends = [{ address = \"a:1\" }, { address = \"10.0.0.12\" }]\n", `pool.p.backends[1].address: "10.0.0.12" is not host:port: missing port in address`},
		{"backend port 0", base + route + "[pool.p]\nbackends = [{ address = \"a:0\" }]\n", `pool.p.backends[0].address: "a:0": port "0" is not a number from 1 to 65535`},
		{"backend port range", base + route + "[pool.p]\nbackends = [{ address = \"a:65536\" }]\n", `pool.p.backends[0].address: "a:65536": port "65536"`},
		{"backend no host", base + route + "[pool.p]\nbackends = [{ address = \":8000\" }]\n", `pool.p.backends[0].address: ":8000": host is empty`},
		{"backend bad host", base + route + "[pool.p]\nbackends = [{ address = \"a..b:1\" }]\n", `pool.p.backends[0].address: "a..b:1": hostname labels`},
		{"backend duplicate", base + route + "[pool.p]\nbackends = [{ address = \"a:1\" }, { address = \"a:1\" }]\n", `pool.p.backends[1].address: "a:1" is also backends[0]`},
		{"algorithm", base + route + "[pool.p]\nalgorithm = \"random\"\nbackends = [{ address = \"a:1\" }]\n", `pool.p.algorithm: "random" is not one of round_robin, least_conn, consistent_hash`},
		{"hash_key without hashing", base + route + "[pool.p]\nhash_key = \"client_ip\"\nbackends = [{ address = \"a:1\" }]\n", `pool.p.hash_key: is only valid with algorithm = "consistent_hash"`},
		{"hash_key form", base + route + "[pool.p]\nalgorithm = \"consistent_hash\"\nhash_key = \"cookie:id\"\nbackends = [{ address = \"a:1\" }]\n", `pool.p.hash_key: "cookie:id" must be "client_ip" or "header:<Header-Name>"`},
		{"hash_key header", base + route + "[pool.p]\nalgorithm = \"consistent_hash\"\nhash_key = \"header:X User\"\nbackends = [{ address = \"a:1\" }]\n", `pool.p.hash_key: "X User" is not a valid header name`},
		{"hash_key empty header", base + route + "[pool.p]\nalgorithm = \"consistent_hash\"\nhash_key = \"header:\"\nbackends = [{ address = \"a:1\" }]\n", `pool.p.hash_key: "" is not a valid header name`},
		{"no route or default", base + pool, "route: at least one [[route]] or a default_pool is required"},
		{"route no host", base + "[[route]]\npool = \"p\"\n" + pool, "route[0].host: is required"},
		{"route host with port", base + "[[route]]\nhost = \"a.test:8443\"\npool = \"p\"\n" + pool, `route[0].host: "a.test:8443" must not include a port`},
		{"route bad host", base + "[[route]]\nhost = \"a test\"\npool = \"p\"\n" + pool, `route[0].host: "a test": hostname contains ' '`},
		{"route bad brackets", base + "[[route]]\nhost = \"[10.0.0.1]\"\npool = \"p\"\n" + pool, `route[0].host: "[10.0.0.1]" is not a bracketed IPv6 address`},
		{"route duplicate after normalising", base + route + "[[route]]\nhost = \"A.TEST.\"\npool = \"p\"\n" + pool, `route[1].host: "a.test" is also route[0]`},
		{"route no pool", base + "[[route]]\nhost = \"a.test\"\n" + pool, "route[0].pool: is required"},
		{"route unknown pool", base + "[[route]]\nhost = \"a.test\"\npool = \"q\"\n" + pool, `route[0].pool: no pool named "q"`},
		{"default pool unknown", "default_pool = \"q\"\n" + base + route + pool, `default_pool: no pool named "q"`},
		{"timeout zero", base + route + pool + "[timeouts]\nidle_ms = 0\n", "timeouts.idle_ms: must be from 1 to 86400000 milliseconds, got 0"},
		{"timeout negative", base + route + pool + "[timeouts]\nheader_read_ms = -5\n", "timeouts.header_read_ms: must be from 1"},
		{"timeout huge", base + route + pool + "[timeouts]\nupstream_response_ms = 86400001\n", "timeouts.upstream_response_ms: must be from 1"},
		{"max_conns zero", base + route + pool + "[limits]\nmax_conns = 0\n", "limits.max_conns: must be from 1 to 10000000, got 0"},
		{"max_header_bytes huge", base + route + pool + "[limits]\nmax_header_bytes = 1073741824\n", "limits.max_header_bytes: must be from 1 to 16777216"},
		{"handshake zero", base + route + pool + "[timeouts]\nhandshake_ms = 0\n", "timeouts.handshake_ms: must be from 1"},
		{"max_idle negative", base + route + pool + "[upstream]\nmax_idle_per_backend = -1\n", "upstream.max_idle_per_backend: must be from 0 to 100000, got -1"},
		{"upstream idle zero", base + route + pool + "[upstream]\nidle_timeout_ms = 0\n", "upstream.idle_timeout_ms: must be from 1 to 86400000 milliseconds, got 0"},
		{"unknown upstream key", base + route + pool + "[upstream]\nmax_idle = 5\n", "upstream.max_idle: unknown field"},
		{"admin public", base + route + pool + "[admin]\naddress = \"0.0.0.0:9090\"\n", `admin.address: "0.0.0.0:9090": host must be a loopback address`},
		{"admin all interfaces", base + route + pool + "[admin]\naddress = \":9090\"\n", `admin.address: ":9090": host must be a loopback address`},
		{"admin hostname", base + route + pool + "[admin]\naddress = \"proxy:9090\"\n", `admin.address: "proxy:9090": host must be a loopback address`},
		{"admin no port", base + route + pool + "[admin]\naddress = \"127.0.0.1\"\n", `admin.address: "127.0.0.1" is not host:port`},
		{"admin negative rate", base + route + pool + "[admin]\naddress = \"127.0.0.1:0\"\nblock_profile_rate_ns = -1\n", "admin.block_profile_rate_ns: must be from 0 to 1000000000, got -1"},
		{"log buffer negative", base + route + pool + "[log]\nbuffer_bytes = -1\n", "log.buffer_bytes: must be from 0 to 16777216, got -1"},
		{"log flush zero", base + route + pool + "[log]\nflush_ms = 0\n", "log.flush_ms: must be from 1 to 60000 milliseconds, got 0"},
		{"listener mode", "[[listener]]\naddress = \":80\"\nmode = \"tls\"\n" + route + pool, `listener[0].mode: "tls" is not one of https, http, redirect`},
		{"redirect_port without redirect", "[[listener]]\naddress = \":80\"\nmode = \"http\"\nredirect_port = 443\n" + route + pool, `listener[0].redirect_port: is only valid with mode = "redirect"`},
		{"redirect_port zero", "[[listener]]\naddress = \":80\"\nmode = \"redirect\"\nredirect_port = 0\n" + route + pool, "listener[0].redirect_port: must be from 1 to 65535, got 0"},
		{"redirect_port range", "[[listener]]\naddress = \":80\"\nmode = \"redirect\"\nredirect_port = 70000\n" + route + pool, "listener[0].redirect_port: must be from 1 to 65535, got 70000"},
		{"unknown listener key", "[[listener]]\naddress = \":80\"\nmode = \"http\"\ntls = true\n" + route + pool, "listener.tls: unknown field"},
		{"unknown tls key", base + route + pool + "[tls]\nmin_version = \"1.3\"\n", "tls.min_version: unknown field"},
		{"route cert without key", base + "[[route]]\nhost = \"a.test\"\npool = \"p\"\ncert = \"a.crt\"\n" + pool, "route[0].key: is required when cert is set"},
		{"route key without cert", base + "[[route]]\nhost = \"a.test\"\npool = \"p\"\nkey = \"a.key\"\n" + pool, "route[0].cert: is required when key is set"},
		{"default key without cert", base + route + pool + "[tls]\ndefault_key = \"d.key\"\n", "tls.default_cert: is required when default_key is set"},
		{"https route without cert", "[[listener]]\naddress = \":443\"\n" + route + pool, "route[0].cert: is required: an https listener serves this host and [tls] has no default_cert"},
		{"https without any cert", "default_pool = \"p\"\n[[listener]]\naddress = \":80\"\nmode = \"http\"\n[[listener]]\naddress = \":443\"\nmode = \"https\"\n" + pool, `listener[1].mode: "https" needs a certificate`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse("bad.toml", []byte(tc.src))
			if err == nil {
				t.Fatalf("accepted:\n%s\ngot %+v", tc.src, c)
			}
			if !strings.Contains(err.Error(), "bad.toml: "+tc.want) {
				t.Fatalf("error\n%v\nwant it to contain\nbad.toml: %s", err, tc.want)
			}
		})
	}
}

// TestParseReportsEveryError: problems are collected, not stopped at the
// first, so one -check-config run shows them all.
func TestParseReportsEveryError(t *testing.T) {
	src := "default_pool = \"nope\"\n" + base +
		"[[route]]\nhost = \"a.test:1\"\npool = \"p\"\n" +
		"[pool.p]\nalgorithm = \"x\"\nbackends = [{ address = \"a\" }]\n" +
		"[timeouts]\nidle_ms = 0\n"
	_, err := Parse("bad.toml", []byte(src))
	var fields []string
	for _, e := range err.(interface{ Unwrap() []error }).Unwrap() {
		var fe *FieldError
		if !errors.As(e, &fe) {
			t.Fatalf("%v is not a *FieldError", e)
		}
		fields = append(fields, fe.Field)
	}
	want := []string{"timeouts.idle_ms", "pool.p.algorithm", "pool.p.backends[0].address", "route[0].host", "default_pool"}
	if !reflect.DeepEqual(fields, want) {
		t.Fatalf("fields %q, want %q", fields, want)
	}
}

// TestParseTLS: listener modes default to https, redirect listeners default
// to port 443, certificate paths are resolved against the config file's
// directory, and a default certificate covers routes without their own.
func TestParseTLS(t *testing.T) {
	src := `
default_pool = "p"

[[listener]]
address = ":443"

[[listener]]
address = ":80"
mode = "redirect"

[[route]]
host = "a.test"
pool = "p"
cert = "certs/a.crt"
key  = "/abs/a.key"

[[route]]
host = "b.test"
pool = "p"

[tls]
default_cert = "d.crt"
default_key  = "d.key"
` + pool
	c, err := Parse("/etc/proxy/proxy.toml", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	wantL := []Listener{{Address: ":443", Mode: ModeHTTPS}, {Address: ":80", Mode: ModeRedirect, RedirectPort: 443}}
	if !reflect.DeepEqual(c.Listeners, wantL) {
		t.Errorf("listeners %+v, want %+v", c.Listeners, wantL)
	}
	if r := c.Routes[0]; r.Cert != "/etc/proxy/certs/a.crt" || r.Key != "/abs/a.key" {
		t.Errorf("route paths %q %q", r.Cert, r.Key)
	}
	if c.TLS != (TLS{DefaultCert: "/etc/proxy/d.crt", DefaultKey: "/etc/proxy/d.key"}) {
		t.Errorf("tls %+v", c.TLS)
	}
	if c.Timeouts.Handshake != 5*time.Second {
		t.Errorf("handshake timeout %v, want the 5s default", c.Timeouts.Handshake)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy.toml")
	if err := os.WriteFile(path, []byte(minimal), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.HasPrefix(err.Error(), path+": ") {
		t.Fatalf("error %v, want it prefixed with the path", err)
	}
	if _, err := Load(filepath.Join(dir, "missing.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
}

func TestTimeoutsValidate(t *testing.T) {
	if err := DefaultTimeouts().Validate(); err != nil {
		t.Fatal(err)
	}
	to := DefaultTimeouts()
	to.ClientIO = 0
	if err := to.Validate(); err == nil || !strings.Contains(err.Error(), "ClientIO") {
		t.Fatalf("got %v", err)
	}
}

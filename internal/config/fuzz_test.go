package config

import (
	"os"
	"strings"
	"testing"
)

// FuzzConfig runs arbitrary bytes through the config parser and, when they
// parse, the snapshot builder. The config file is trusted input, but a
// typo must give an error, never a panic, a hang or a config that passes
// validation and then fails to build. Configs that name certificate files
// skip the snapshot step: loading them would read whatever path the fuzzer
// wrote, /dev/zero included.
func FuzzConfig(f *testing.F) {
	f.Add([]byte(fullConfig))
	f.Add([]byte(minimal))
	if b, err := os.ReadFile("../../lab/proxy.toml"); err == nil {
		f.Add(b)
	}
	f.Add([]byte("[[listener]]\naddress = \"127.0.0.1:0\"\nmode = \"http\"\n[pool.p]\nbackends = [{ address = \"a:1\" }]\ndefault_pool = \"p\"\n"))
	f.Add([]byte("[limits]\nmax_conns = -1\n[timeouts]\nidle_ms = 99999999999999\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := Parse("fuzz.toml", data)
		if err != nil {
			if !strings.HasPrefix(err.Error(), "fuzz.toml") {
				t.Fatalf("error does not name the file: %v", err)
			}
			return
		}
		if len(c.Listeners) == 0 {
			t.Fatal("a valid config with no listener")
		}
		if err := c.Timeouts.Validate(); err != nil {
			t.Fatalf("a valid config with bad timeouts: %v", err)
		}
		if c.TLS.DefaultCert != "" {
			return
		}
		for _, r := range c.Routes {
			if r.Cert != "" {
				return
			}
		}
		s, err := NewSnapshot(c, Runtime{})
		if err != nil {
			t.Fatalf("Parse accepted a config NewSnapshot rejects: %v\n%s", err, data)
		}
		for _, r := range c.Routes {
			if s.Lookup(r.Host) == nil {
				t.Fatalf("route %q has no target", r.Host)
			}
		}
	})
}

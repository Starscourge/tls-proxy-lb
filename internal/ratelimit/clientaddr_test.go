package ratelimit

import (
	"net/netip"
	"testing"
)

func mustTrusted(t *testing.T, list ...string) Trusted {
	t.Helper()
	tr, err := ParseTrusted(list)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestClientAddr(t *testing.T) {
	trusted := mustTrusted(t, "10.0.0.0/24", "2001:db8:ffff::/48", "192.0.2.99")
	for _, tc := range []struct {
		name string
		peer string
		xff  []string
		want string
	}{
		{"untrusted peer, no header", "198.51.100.1", nil, "198.51.100.1"},
		{"untrusted peer, header ignored", "198.51.100.1", []string{"203.0.113.5"}, "198.51.100.1"},
		{"untrusted peer, spoofed trusted address ignored", "198.51.100.1", []string{"10.0.0.5"}, "198.51.100.1"},
		{"trusted peer, no header", "10.0.0.1", nil, "10.0.0.1"},
		{"trusted peer, empty header", "10.0.0.1", []string{""}, "10.0.0.1"},
		{"trusted peer, one hop", "10.0.0.1", []string{"203.0.113.5"}, "203.0.113.5"},
		{"trusted peer, mapped peer", "::ffff:10.0.0.1", []string{"203.0.113.5"}, "203.0.113.5"},
		{"single trusted address", "192.0.2.99", []string{"203.0.113.5"}, "203.0.113.5"},
		{"rightmost untrusted wins over client-written left part", "10.0.0.1",
			[]string{"1.1.1.1, 2.2.2.2, 203.0.113.5"}, "203.0.113.5"},
		{"trusted hops skipped", "10.0.0.1",
			[]string{"1.1.1.1, 203.0.113.5, 10.0.0.7, 10.0.0.8"}, "203.0.113.5"},
		{"across header lines", "10.0.0.1",
			[]string{"1.1.1.1, 203.0.113.5", "10.0.0.7"}, "203.0.113.5"},
		{"all trusted gives the leftmost", "10.0.0.1",
			[]string{"10.0.0.9, 10.0.0.8"}, "10.0.0.9"},
		{"spaces, tabs and empty entries", "10.0.0.1",
			[]string{" ,\t203.0.113.5 , ,10.0.0.7,"}, "203.0.113.5"},
		{"ports ignored", "10.0.0.1",
			[]string{"203.0.113.5:4711, 10.0.0.7:80"}, "203.0.113.5"},
		{"IPv6 with port", "10.0.0.1",
			[]string{"[2001:db8:1::5]:443"}, "2001:db8:1::5"},
		{"IPv6 trusted hop", "2001:db8:ffff:1::1",
			[]string{"2001:db8:1::5, 2001:db8:ffff:2::2"}, "2001:db8:1::5"},
		{"mapped entry", "10.0.0.1",
			[]string{"::ffff:203.0.113.5"}, "203.0.113.5"},
		{"garbage before the client gives the peer", "10.0.0.1",
			[]string{"203.0.113.5, unknown"}, "10.0.0.1"},
		{"garbage left of the client is never read", "10.0.0.1",
			[]string{"unknown, 203.0.113.5"}, "203.0.113.5"},
		{"obfuscated identifier gives the peer", "10.0.0.1",
			[]string{"_hidden"}, "10.0.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ClientAddr(netip.MustParseAddr(tc.peer), tc.xff, trusted)
			if got != netip.MustParseAddr(tc.want) {
				t.Fatalf("ClientAddr(%s, %q) = %s, want %s", tc.peer, tc.xff, got, tc.want)
			}
		})
	}
}

func TestClientAddrNoTrusted(t *testing.T) {
	peer := netip.MustParseAddr("10.0.0.1")
	if got := ClientAddr(peer, []string{"203.0.113.5"}, nil); got != peer {
		t.Fatalf("with no trusted proxies the header was used: %s", got)
	}
}

func TestParseTrusted(t *testing.T) {
	tr := mustTrusted(t, "10.0.0.1/24", "192.0.2.7", "::ffff:198.51.100.0/120", "2001:db8::1")
	want := []string{"10.0.0.0/24", "192.0.2.7/32", "198.51.100.0/24", "2001:db8::1/128"}
	if len(tr) != len(want) {
		t.Fatalf("got %v", tr)
	}
	for i, w := range want {
		if tr[i] != netip.MustParsePrefix(w) {
			t.Errorf("[%d] = %s, want %s", i, tr[i], w)
		}
	}
	if !tr.Contains(netip.MustParseAddr("::ffff:198.51.100.200")) {
		t.Error("mapped address not matched against IPv4 prefix")
	}
	for _, bad := range []string{"10.0.0.0/33", "not-an-ip", "10.0.0/8", ""} {
		if _, err := ParseTrusted([]string{bad}); err == nil {
			t.Errorf("ParseTrusted(%q) accepted", bad)
		}
	}
}

func TestClientAddrAllocs(t *testing.T) {
	trusted := mustTrusted(t, "10.0.0.0/8")
	peer := netip.MustParseAddr("10.0.0.1")
	xff := []string{"1.1.1.1, 2001:db8::5, 10.0.0.7"}
	if n := testing.AllocsPerRun(1000, func() { ClientAddr(peer, xff, trusted) }); n != 0 {
		t.Fatalf("ClientAddr allocates %v per call", n)
	}
}

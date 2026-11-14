package httpx

import (
	"net/http"
	"reflect"
	"testing"
)

func TestRemoveHopByHop(t *testing.T) {
	h := http.Header{
		"Connection":          {"keep-alive, X-Secret", " x-other "},
		"Keep-Alive":          {"timeout=5"},
		"Proxy-Connection":    {"keep-alive"},
		"Proxy-Authorization": {"Basic Zm9v"},
		"Proxy-Authenticate":  {"Basic"},
		"Te":                  {"trailers"},
		"Trailer":             {"X-Checksum"},
		"Transfer-Encoding":   {"chunked"},
		"Upgrade":             {"websocket"},
		"X-Secret":            {"1"},
		"X-Other":             {"2"},
		"X-Kept":              {"3"},
		"Content-Type":        {"text/plain"},
		"Authorization":       {"Bearer t"},
	}
	RemoveHopByHop(h)
	want := http.Header{
		"X-Kept":        {"3"},
		"Content-Type":  {"text/plain"},
		"Authorization": {"Bearer t"},
	}
	if !reflect.DeepEqual(h, want) {
		t.Fatalf("got %v\nwant %v", h, want)
	}
}

func TestRemoveHopByHopEmptyTokens(t *testing.T) {
	h := http.Header{"Connection": {",, ,"}, "X-A": {"1"}}
	RemoveHopByHop(h)
	if len(h) != 1 || h.Get("X-A") != "1" {
		t.Fatalf("got %v", h)
	}
}

func TestSetForwarded(t *testing.T) {
	cases := []struct {
		name    string
		trusted bool
		prior   []string
		want    string
	}{
		{"none", false, nil, "192.0.2.1"},
		{"untrusted, one", false, []string{"203.0.113.5"}, "192.0.2.1"},
		{"untrusted, several", false, []string{"203.0.113.5", "198.51.100.2, 10.0.0.1"}, "192.0.2.1"},
		{"trusted, none", true, nil, "192.0.2.1"},
		{"trusted, one", true, []string{"203.0.113.5"}, "203.0.113.5, 192.0.2.1"},
		{"trusted, several", true, []string{"203.0.113.5", "198.51.100.2, 10.0.0.1"}, "203.0.113.5, 198.51.100.2, 10.0.0.1, 192.0.2.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{"X-Forwarded-Proto": {"https"}, "Forwarded": {"for=203.0.113.5"}}
			if tc.prior != nil {
				h["X-Forwarded-For"] = tc.prior
			}
			SetForwarded(h, Forwarding{ClientIP: "192.0.2.1", Proto: "http", Trusted: tc.trusted})
			if got := h["X-Forwarded-For"]; len(got) != 1 || got[0] != tc.want {
				t.Fatalf("X-Forwarded-For = %q, want %q", got, tc.want)
			}
			if _, ok := h["Forwarded"]; ok != tc.trusted {
				t.Fatalf("Forwarded kept %v, want %v", ok, tc.trusted)
			}
			if got := h["X-Forwarded-Proto"]; len(got) != 1 || got[0] != "http" {
				t.Fatalf("X-Forwarded-Proto = %q, want [http]", got)
			}
		})
	}
}

package integration

import (
	"bytes"
	"io"
	"net/http"
	"testing"
)

// TestThreeBackendsDirect is the Phase 0 check: the harness starts three
// backends and each answers directly, identifying itself and logging the
// request.
func TestThreeBackendsDirect(t *testing.T) {
	backends := StartBackends(t, 3, BackendOptions{})
	c := NewClient(t)

	seen := map[string]bool{}
	for _, b := range backends {
		if seen[b.Addr] {
			t.Fatalf("two backends share address %s", b.Addr)
		}
		seen[b.Addr] = true

		resp, err := c.Get(b.URL + "/")
		if err != nil {
			t.Fatalf("%s: %v", b.Name(), err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("%s: read body: %v", b.Name(), err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d", b.Name(), resp.StatusCode)
		}
		if got := resp.Header.Get("X-Backend"); got != b.Name() {
			t.Errorf("%s: X-Backend = %q", b.Name(), got)
		}
		if want := "hello from " + b.Name() + "\n"; string(body) != want {
			t.Errorf("%s: body = %q, want %q", b.Name(), body, want)
		}
		reqs := b.Requests()
		if len(reqs) != 1 || reqs[0].Path != "/" || reqs[0].Status != http.StatusOK {
			t.Errorf("%s: request log = %+v", b.Name(), reqs)
		}
	}
}

func TestBackendEchoDirect(t *testing.T) {
	b := StartBackend(t, "echo", BackendOptions{})
	c := NewClient(t)
	payload := bytes.Repeat([]byte{0, 1, 2, 0xff}, 1<<18)
	resp, err := c.Post(b.URL+"/echo", "application/octet-stream", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo returned %d bytes, want %d identical bytes", len(got), len(payload))
	}
}

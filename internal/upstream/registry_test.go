package upstream

import (
	"testing"
	"time"
)

func TestRegistrySharesPoolsByAddress(t *testing.T) {
	r := NewRegistry(Options{MaxIdle: 2, IdleTimeout: time.Minute})
	defer r.Close()
	a, b := r.Pool("10.0.0.1:80"), r.Pool("10.0.0.1:80")
	if a != b || a == r.Pool("10.0.0.2:80") {
		t.Fatal("pools not keyed by address")
	}
	if !a.Reuse() || a.Addr() != "10.0.0.1:80" {
		t.Fatalf("pool reuse %v addr %q", a.Reuse(), a.Addr())
	}
}

// The reaper closes connections past the idle timeout without any Get.
func TestRegistryReaper(t *testing.T) {
	s := newServer(t)
	r := NewRegistry(Options{MaxIdle: 4, IdleTimeout: 40 * time.Millisecond})
	defer r.Close()
	p := r.Pool(s.addr)
	c := get(t, p)
	p.Put(c)
	deadline := time.Now().Add(2 * time.Second)
	for p.Idle() > 0 {
		if time.Now().After(deadline) {
			t.Fatal("reaper did not close the idle connection")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !isClosed(c) {
		t.Fatal("reaped connection still open")
	}
}

// Close stops the reaper (goleak checks it in TestMain) and closes idle
// connections; a pool asked for afterwards keeps nothing.
func TestRegistryClose(t *testing.T) {
	s := newServer(t)
	r := NewRegistry(Options{MaxIdle: 4, IdleTimeout: time.Minute})
	p := r.Pool(s.addr)
	c := get(t, p)
	p.Put(c)
	r.Close()
	r.Close()
	if p.Idle() != 0 || !isClosed(c) {
		t.Fatal("Close left an idle connection open")
	}
	late := r.Pool("127.0.0.1:1")
	if late.Idle() != 0 || !late.closed {
		t.Fatal("pool created after Close is not closed")
	}
}

// With reuse off there is no reaper to stop.
func TestRegistryReuseOff(t *testing.T) {
	r := NewRegistry(Options{})
	if r.Pool("a:1").Reuse() {
		t.Fatal("reuse on with MaxIdle 0")
	}
	r.Close()
}

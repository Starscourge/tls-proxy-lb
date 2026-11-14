package upstream

import (
	"context"
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

func TestRegistryPoolsSorted(t *testing.T) {
	r := NewRegistry(Options{})
	defer r.Close()
	r.Pool("10.0.0.2:80")
	r.Pool("10.0.0.1:80")
	ps := r.Pools()
	if len(ps) != 2 || ps[0].Addr() != "10.0.0.1:80" || ps[1].Addr() != "10.0.0.2:80" {
		t.Fatalf("pools %v", ps)
	}
}

// Retain closes the pools of removed addresses and keeps the rest; a
// connection in use from a removed pool is closed when it is put back, and
// the address gets a new pool if it returns.
func TestRegistryRetain(t *testing.T) {
	s1, s2 := newServer(t), newServer(t)
	r := NewRegistry(Options{MaxIdle: 4, IdleTimeout: time.Minute})
	defer r.Close()
	p1, p2 := r.Pool(s1.addr), r.Pool(s2.addr)
	idle1, idle2 := get(t, p1), get(t, p2)
	inUse := get(t, p1)
	p1.Put(idle1)
	p2.Put(idle2)

	r.Retain([]string{s2.addr})
	if r.Lookup(s1.addr) != nil || r.Lookup(s2.addr) != p2 {
		t.Fatal("Retain kept the wrong pools")
	}
	if !isClosed(idle1) || isClosed(idle2) || p2.Idle() != 1 {
		t.Fatal("Retain closed the wrong idle connections")
	}
	p1.Put(inUse)
	if !isClosed(inUse) || p1.Idle() != 0 {
		t.Fatal("a connection put back to a removed pool was parked")
	}
	if again := r.Pool(s1.addr); again == p1 || again.closed {
		t.Fatal("a returning address did not get a new, open pool")
	}
	if got := r.Pools(); len(got) != 2 {
		t.Fatalf("%d pools after the address came back, want 2", len(got))
	}
}

// Lookup never creates a pool.
func TestRegistryLookup(t *testing.T) {
	r := NewRegistry(Options{MaxIdle: 1, IdleTimeout: time.Minute})
	defer r.Close()
	if r.Lookup("10.0.0.1:80") != nil || len(r.Pools()) != 0 {
		t.Fatal("Lookup created a pool")
	}
}

// The reaper also stops when the registry's context ends (goleak checks
// it); Close afterwards still closes the pools.
func TestRegistryContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := NewRegistryContext(ctx, Options{MaxIdle: 1, IdleTimeout: time.Minute})
	cancel()
	select {
	case <-r.done:
	case <-time.After(2 * time.Second):
		t.Fatal("reaper still running after its context ended")
	}
	r.Close()
}

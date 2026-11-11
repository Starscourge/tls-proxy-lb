package conn

import (
	"net"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func pipeConn(t *testing.T) net.Conn {
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	return a
}

func TestTrackerCap(t *testing.T) {
	tr := NewTracker(2)
	c1, ok1 := tr.Add(pipeConn(t))
	_, ok2 := tr.Add(pipeConn(t))
	_, ok3 := tr.Add(pipeConn(t))
	if !ok1 || !ok2 || ok3 {
		t.Fatalf("adds = %v %v %v, want true true false", ok1, ok2, ok3)
	}
	if tr.Len() != 2 || tr.Accepted() != 2 || tr.Rejected() != 1 {
		t.Fatalf("len %d accepted %d rejected %d", tr.Len(), tr.Accepted(), tr.Rejected())
	}
	tr.Remove(c1)
	tr.Remove(c1) // a second Remove is harmless
	if _, ok := tr.Add(pipeConn(t)); !ok {
		t.Fatal("slot not freed by Remove")
	}
}

func TestTrackerNoCap(t *testing.T) {
	tr := NewTracker(0)
	for range 100 {
		if _, ok := tr.Add(pipeConn(t)); !ok {
			t.Fatal("uncapped tracker refused a connection")
		}
	}
}

func TestTrackerListAndState(t *testing.T) {
	tr := NewTracker(0)
	a, _ := tr.Add(pipeConn(t))
	b, _ := tr.Add(pipeConn(t))
	b.SetState(StateActive)
	if a.ID() == b.ID() {
		t.Fatal("IDs not unique")
	}
	states := map[uint64]State{}
	for _, ci := range tr.List() {
		states[ci.ID] = ci.State
	}
	if len(states) != 2 || states[a.ID()] != StateNew || states[b.ID()] != StateActive {
		t.Fatalf("List states = %v", states)
	}
	tr.Remove(a)
	if a.State() != StateClosed || len(tr.List()) != 1 {
		t.Fatalf("after Remove: state %v, list %d", a.State(), len(tr.List()))
	}
}

// TestTrackerCloseAll: CloseAll closes open connections, which unblocks
// their readers, refuses new ones, and Wait returns once all are removed.
func TestTrackerCloseAll(t *testing.T) {
	tr := NewTracker(0)
	var wg sync.WaitGroup
	for range 5 {
		tc, _ := tr.Add(pipeConn(t))
		wg.Go(func() {
			defer tr.Remove(tc)
			tc.Read(make([]byte, 1)) // blocks until closed
		})
	}
	tr.CloseAll()
	done := make(chan struct{})
	go func() { tr.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after CloseAll")
	}
	wg.Wait()
	if _, ok := tr.Add(pipeConn(t)); ok {
		t.Fatal("Add succeeded after CloseAll")
	}
}

func TestTrackerConcurrent(t *testing.T) {
	tr := NewTracker(50)
	var wg sync.WaitGroup
	for range 200 {
		wg.Go(func() {
			if tc, ok := tr.Add(pipeConn(t)); ok {
				tc.SetState(StateActive)
				_ = tr.List()
				tr.Remove(tc)
			}
		})
	}
	wg.Wait()
	if tr.Len() != 0 || tr.Accepted()+tr.Rejected() != 200 {
		t.Fatalf("len %d, accepted+rejected %d", tr.Len(), tr.Accepted()+tr.Rejected())
	}
}

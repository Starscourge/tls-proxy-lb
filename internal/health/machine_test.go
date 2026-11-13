package health

import (
	"strings"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// TestMachine feeds scripted sequences of check results ('+' success, '-'
// failure) and checks the verdict after each, written as 'H' or 'U'. The
// flapping cases are the point: results that alternate, or runs one short of
// a threshold, must not move the verdict.
func TestMachine(t *testing.T) {
	cases := []struct {
		name       string
		rise, fall int
		start      bool
		results    string
		want       string
	}{
		{"steady healthy", 2, 3, true, "++++", "HHHH"},
		{"falls after fall failures", 2, 3, true, "---", "HHU"},
		{"rises after rise successes", 2, 3, false, "++", "UH"},
		{"alternating from healthy", 2, 3, true, "-+-+-+-+-+", "HHHHHHHHHH"},
		{"alternating from unhealthy", 2, 3, false, "+-+-+-+-+-", "UUUUUUUUUU"},
		{"two failures then success, repeated", 2, 3, true, "--+--+--+--+", "HHHHHHHHHHHH"},
		{"one success then failure, repeated", 2, 3, false, "+-+--+-+", "UUUUUUUU"},
		{"falls, then flaps, then recovers", 2, 3, true, "---+-+-++", "HHUUUUUUH"},
		{"recovers, then needs a full fall run", 2, 3, false, "++--+---", "UHHHHHHU"},
		{"fall 1 rise 1 follows every result", 1, 1, true, "-+-+", "UHUH"},
		{"thresholds below 1 act as 1", 0, 0, true, "-+", "UH"},
		{"long fall", 1, 5, true, "----+-----", "HHHHHHHHHU"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMachine(tc.rise, tc.fall, tc.start)
			var got strings.Builder
			changes := 0
			prev := tc.start
			for _, r := range tc.results {
				healthy, changed := m.Observe(r == '+')
				if changed != (healthy != prev) {
					t.Fatalf("changed = %v but verdict went %v -> %v", changed, prev, healthy)
				}
				if changed {
					changes++
				}
				prev = healthy
				if healthy {
					got.WriteByte('H')
				} else {
					got.WriteByte('U')
				}
			}
			if got.String() != tc.want {
				t.Fatalf("results %s: verdicts %s, want %s", tc.results, got.String(), tc.want)
			}
			if m.Healthy() != prev {
				t.Fatalf("Healthy() = %v after %s", m.Healthy(), got.String())
			}
		})
	}
}

func TestMachineRunsAndReset(t *testing.T) {
	m := NewMachine(2, 3, true)
	m.Observe(false)
	m.Observe(false)
	if ok, fail := m.Runs(); ok != 0 || fail != 2 {
		t.Fatalf("runs %d/%d, want 0/2", ok, fail)
	}
	m.Observe(true)
	if ok, fail := m.Runs(); ok != 1 || fail != 0 {
		t.Fatalf("runs %d/%d, want 1/0", ok, fail)
	}
	m.Reset(false)
	if ok, fail := m.Runs(); m.Healthy() || ok != 0 || fail != 0 {
		t.Fatalf("after reset: healthy %v runs %d/%d", m.Healthy(), ok, fail)
	}
	// After a reset to unhealthy, rise successes are needed again.
	if h, _ := m.Observe(true); h {
		t.Fatal("healthy after one success, rise is 2")
	}
	if h, changed := m.Observe(true); !h || !changed {
		t.Fatal("not healthy after rise successes")
	}
}

package health

// Machine turns a stream of active check results into a healthy or
// unhealthy verdict with hysteresis: a healthy backend becomes unhealthy
// after fall consecutive failures, an unhealthy one healthy after rise
// consecutive successes. A result that agrees with the current verdict
// clears the opposing run, so results that alternate never reach either
// threshold and the verdict does not oscillate.
//
// A Machine belongs to one prober goroutine and is not safe for concurrent
// use.
type Machine struct {
	rise, fall int
	healthy    bool
	// okRun and failRun count the current run of successes or failures.
	// At most one is non-zero.
	okRun, failRun int
}

// NewMachine returns a machine with the given thresholds (each at least 1)
// and starting verdict.
func NewMachine(rise, fall int, healthy bool) Machine {
	return Machine{rise: max(rise, 1), fall: max(fall, 1), healthy: healthy}
}

// Observe records one check result and returns the verdict after it, and
// whether the verdict changed.
func (m *Machine) Observe(ok bool) (healthy, changed bool) {
	if ok {
		m.okRun++
		m.failRun = 0
		if !m.healthy && m.okRun >= m.rise {
			m.healthy, changed = true, true
		}
	} else {
		m.failRun++
		m.okRun = 0
		if m.healthy && m.failRun >= m.fall {
			m.healthy, changed = false, true
		}
	}
	return m.healthy, changed
}

// Reset sets the verdict and clears both runs.
func (m *Machine) Reset(healthy bool) {
	m.healthy, m.okRun, m.failRun = healthy, 0, 0
}

// Healthy returns the current verdict.
func (m *Machine) Healthy() bool { return m.healthy }

// Runs returns the current runs of consecutive successes and failures.
func (m *Machine) Runs() (ok, fail int) { return m.okRun, m.failRun }

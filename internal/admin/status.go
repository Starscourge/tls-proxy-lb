package admin

import (
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/Starscourge/tls-proxy-lb/internal/config"
	"github.com/Starscourge/tls-proxy-lb/internal/health"
	"github.com/Starscourge/tls-proxy-lb/internal/upstream"
)

// StatusSource is the state /status reports. Health and Conns may be nil.
type StatusSource struct {
	Config *config.Store
	Health *health.Checker
	Conns  *upstream.Registry
}

// Status is the /status document.
type Status struct {
	Time time.Time `json:"time"`
	// Pools lists each pool and the state of its backends.
	Pools []PoolStatus `json:"pools"`
	// Backends lists each backend address once, with its health and
	// connection pool, whatever pools it is in.
	Backends []BackendStatus `json:"backends"`
}

// PoolStatus is one pool in /status.
type PoolStatus struct {
	Name      string `json:"name"`
	Algorithm string `json:"algorithm"`
	// Healthy is how many of the pool's backends may be picked.
	Healthy  int                 `json:"healthy"`
	Backends []PoolBackendStatus `json:"backends"`
}

// PoolBackendStatus is one backend of a pool in /status.
type PoolBackendStatus struct {
	Address  string `json:"address"`
	State    string `json:"state"`
	InFlight int64  `json:"in_flight"`
}

// BackendStatus is one backend address in /status.
type BackendStatus struct {
	health.Status
	IdleConns int    `json:"idle_conns"`
	Dials     uint64 `json:"dials"`
	Reuses    uint64 `json:"reuses"`
}

// Snapshot builds the /status document.
func (src StatusSource) Snapshot() Status {
	snap := src.Config.Load()
	st := Status{Time: time.Now(), Pools: []PoolStatus{}, Backends: []BackendStatus{}}
	var addrs []string
	for _, p := range snap.Pools() {
		ps := PoolStatus{Name: p.Name, Algorithm: string(p.Algorithm), Healthy: len(p.Healthy())}
		for _, b := range p.Backends {
			ps.Backends = append(ps.Backends, PoolBackendStatus{Address: b.Addr, State: b.Health.State().String(), InFlight: b.InFlight()})
			if !slices.Contains(addrs, b.Addr) {
				addrs = append(addrs, b.Addr)
			}
		}
		st.Pools = append(st.Pools, ps)
	}
	slices.Sort(addrs)
	targets := map[string]*health.Target{}
	if src.Health != nil {
		for _, t := range src.Health.Targets() {
			targets[t.Addr()] = t
		}
	}
	conns := map[string]*upstream.Pool{}
	if src.Conns != nil {
		for _, p := range src.Conns.Pools() {
			conns[p.Addr()] = p
		}
	}
	for _, a := range addrs {
		bs := BackendStatus{Status: health.Status{Address: a, State: health.Healthy.String()}}
		if t, ok := targets[a]; ok {
			bs.Status = t.Status()
		}
		if p, ok := conns[a]; ok {
			bs.IdleConns, bs.Dials, bs.Reuses = p.Idle(), p.Dials(), p.Reuses()
		}
		st.Backends = append(st.Backends, bs)
	}
	return st
}

// ServeHTTP serves the document as JSON.
func (src StatusSource) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(src.Snapshot())
}

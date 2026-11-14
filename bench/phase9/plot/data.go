package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Request is one line from bench/phase9/demo.js.
type Request struct {
	StartMS   int64   // wall clock, epoch milliseconds
	Backend   string  // X-Backend, empty without a response
	Status    int     // 0 when there was no response
	LatencyMS float64 // the whole exchange, as the client saw it
	Error     int     // k6 error code, 0 for none
}

// OK reports whether the request succeeded: a 200 and no error.
func (r Request) OK() bool { return r.Status == 200 && r.Error == 0 }

// open opens path, through gzip when it ends in .gz.
func open(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(path, ".gz") {
		return f, nil
	}
	z, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return struct {
		io.Reader
		io.Closer
	}{z, f}, nil
}

// ReadRequests reads start_ms,backend,status,latency_ms,error lines,
// sorted by start time.
func ReadRequests(path string) ([]Request, error) {
	f, err := open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseRequests(f, path)
}

func parseRequests(r io.Reader, name string) ([]Request, error) {
	var out []Request
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) != 5 {
			return nil, fmt.Errorf("%s:%d: %d fields, want 5", name, n, len(f))
		}
		var q Request
		var errs [4]error
		q.StartMS, errs[0] = strconv.ParseInt(f[0], 10, 64)
		q.Backend = f[1]
		q.Status, errs[1] = strconv.Atoi(f[2])
		q.LatencyMS, errs[2] = strconv.ParseFloat(f[3], 64)
		q.Error, errs[3] = strconv.Atoi(f[4])
		for _, e := range errs {
			if e != nil {
				return nil, fmt.Errorf("%s:%d: %w", name, n, e)
			}
		}
		out = append(out, q)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no requests", name)
	}
	slices.SortStableFunc(out, func(a, b Request) int { return int(a.StartMS - b.StartMS) })
	return out, nil
}

// ReadEvents reads "epoch_ms name" lines; # starts a comment.
func ReadEvents(path string) (map[string]int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseEvents(string(b), path)
}

func parseEvents(s, name string) (map[string]int64, error) {
	ev := map[string]int64{}
	for n, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ts, ev2, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("%s:%d: want \"epoch_ms name\"", name, n+1)
		}
		ms, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", name, n+1, err)
		}
		ev[strings.TrimSpace(ev2)] = ms
	}
	return ev, nil
}

// HealthEvent is one line of the proxy's health log.
type HealthEvent struct {
	MS      int64
	Msg     string // "backend ejected", "backend unhealthy", "backend healthy", ...
	Backend string
}

// ReadHealth reads the proxy's JSON log lines about backend health.
func ReadHealth(path string) ([]HealthEvent, error) {
	f, err := open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseHealth(f, path)
}

func parseHealth(r io.Reader, name string) ([]HealthEvent, error) {
	var out []HealthEvent
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		var l struct {
			Time    time.Time `json:"time"`
			Msg     string    `json:"msg"`
			Backend string    `json:"backend"`
		}
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", name, n, err)
		}
		if !strings.HasPrefix(l.Msg, "backend ") {
			continue
		}
		out = append(out, HealthEvent{l.Time.UnixMilli(), l.Msg, l.Backend})
	}
	return out, sc.Err()
}

// FirstHealth returns the first event for backend at or after from whose
// message is one of msgs.
func FirstHealth(evs []HealthEvent, backend string, from int64, msgs ...string) (HealthEvent, bool) {
	for _, e := range evs {
		if e.Backend == backend && e.MS >= from && slices.Contains(msgs, e.Msg) {
			return e, true
		}
	}
	return HealthEvent{}, false
}

// Percentile returns the q-th percentile (0..100) of v by nearest rank; v
// is sorted in place. NaN for no values.
func Percentile(v []float64, q float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	slices.Sort(v)
	k := int(math.Ceil(q/100*float64(len(v)))) - 1
	return v[max(0, min(k, len(v)-1))]
}

// Second is one second of a run: requests that started in it.
type Second struct {
	OK, Failed int
	ByBackend  map[string]int // successful requests per backend
	Latency    []float64      // every request's latency, ms
}

// BySecond bins requests by start time into seconds from t0; the result
// covers [t0, last start].
func BySecond(reqs []Request, t0 int64) []Second {
	if len(reqs) == 0 {
		return nil
	}
	n := int((reqs[len(reqs)-1].StartMS-t0)/1000) + 1
	out := make([]Second, n)
	for i := range out {
		out[i].ByBackend = map[string]int{}
	}
	for _, r := range reqs {
		i := int((r.StartMS - t0) / 1000)
		if i < 0 || i >= n {
			continue
		}
		s := &out[i]
		if r.OK() {
			s.OK++
			s.ByBackend[r.Backend]++
		} else {
			s.Failed++
		}
		s.Latency = append(s.Latency, r.LatencyMS)
	}
	return out
}

// Window counts and summarises the requests that started in [from, to).
type Window struct {
	Requests, Failed int
	ByBackend        map[string]int
	P50, P99, Max    float64
	Over             map[float64]int // requests slower than each threshold, ms
}

// Summarise returns the window [from, to) of reqs, counting requests
// slower than each threshold in ms.
func Summarise(reqs []Request, from, to int64, thresholds ...float64) Window {
	w := Window{ByBackend: map[string]int{}, Over: map[float64]int{}}
	var lat []float64
	for _, r := range reqs {
		if r.StartMS < from || r.StartMS >= to {
			continue
		}
		w.Requests++
		if !r.OK() {
			w.Failed++
		} else {
			w.ByBackend[r.Backend]++
		}
		lat = append(lat, r.LatencyMS)
		w.Max = math.Max(w.Max, r.LatencyMS)
		for _, t := range thresholds {
			if r.LatencyMS > t {
				w.Over[t]++
			}
		}
	}
	w.P50 = Percentile(lat, 50)
	w.P99 = Percentile(lat, 99)
	return w
}

// Backends returns the backend names seen in successful requests, sorted.
func Backends(reqs []Request) []string {
	seen := map[string]bool{}
	for _, r := range reqs {
		if r.OK() && r.Backend != "" {
			seen[r.Backend] = true
		}
	}
	var out []string
	for b := range seen {
		out = append(out, b)
	}
	slices.Sort(out)
	return out
}

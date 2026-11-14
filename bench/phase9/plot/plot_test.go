package main

import (
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func TestParseRequests(t *testing.T) {
	in := "1000,backend1,200,1.5,0\n\n900,,0,1000.25,1211\n"
	got, err := parseRequests(strings.NewReader(in), "x")
	if err != nil {
		t.Fatal(err)
	}
	want := []Request{
		{900, "", 0, 1000.25, 1211},
		{1000, "backend1", 200, 1.5, 0},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v (sorted by start)", got, want)
	}
	if got[0].OK() || !got[1].OK() {
		t.Fatal("OK: want false for a k6 error, true for a 200")
	}
	for _, bad := range []string{"1,b,200,1\n", "x,b,200,1,0\n", ""} {
		if _, err := parseRequests(strings.NewReader(bad), "x"); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

func TestParseEvents(t *testing.T) {
	ev, err := parseEvents("# comment\n100 kill\n\n200 start\n", "e")
	if err != nil {
		t.Fatal(err)
	}
	if ev["kill"] != 100 || ev["start"] != 200 || len(ev) != 2 {
		t.Fatalf("got %v", ev)
	}
	if _, err := parseEvents("kill\n", "e"); err == nil {
		t.Fatal("want an error for a line without a time")
	}
}

func TestParseHealth(t *testing.T) {
	in := `{"time":"2026-11-14T11:07:15.599Z","level":"WARN","msg":"backend ejected","backend":"10.0.0.12:8000"}
{"time":"2026-11-14T11:07:16Z","level":"INFO","msg":"access","backend":"10.0.0.12:8000"}
{"time":"2026-11-14T11:07:36.579Z","level":"INFO","msg":"backend healthy","backend":"10.0.0.12:8000"}
`
	evs, err := parseHealth(strings.NewReader(in), "h")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 (access lines skipped)", len(evs))
	}
	e, ok := FirstHealth(evs, "10.0.0.12:8000", 0, "backend healthy")
	if !ok || e.MS != 1794654456579 {
		t.Fatalf("FirstHealth = %v, %v", e, ok)
	}
	if _, ok := FirstHealth(evs, "10.0.0.12:8000", e.MS+1, "backend healthy"); ok {
		t.Fatal("FirstHealth found an event before from")
	}
}

func TestPercentile(t *testing.T) {
	v := make([]float64, 100)
	for i := range v {
		v[i] = float64(100 - i) // 100..1, unsorted
	}
	for _, c := range []struct{ q, want float64 }{{50, 50}, {99, 99}, {100, 100}, {0, 1}} {
		if got := Percentile(v, c.q); got != c.want {
			t.Errorf("p%v = %v, want %v", c.q, got, c.want)
		}
	}
	if !math.IsNaN(Percentile(nil, 50)) {
		t.Error("p50 of nothing: want NaN")
	}
}

func TestTicks(t *testing.T) {
	for _, c := range []struct {
		lo, hi float64
		n      int
		want   string
	}{
		{0, 1000, 5, "[0 200 400 600 800 1000]"},
		{0, 37, 5, "[0 10 20 30 40]"},
		{0, 0.9, 4, "[0 0.25 0.5 0.75 1]"},
	} {
		got, top := ticks(c.lo, c.hi, c.n)
		if fmt.Sprint(got) != c.want || top != got[len(got)-1] {
			t.Errorf("ticks(%v, %v, %d) = %v, %v; want %s", c.lo, c.hi, c.n, got, top, c.want)
		}
	}
}

func TestNum(t *testing.T) {
	for v, want := range map[float64]string{0: "0", 0.25: "0.25", 1.5: "1.5", 250: "250", 1500: "1.5k", 86814: "86.8k", 2.5e6: "2.5M"} {
		if got := num(v); got != want {
			t.Errorf("num(%v) = %q, want %q", v, got, want)
		}
	}
}

// killRun is a synthetic run at 100 req/s over three backends for 30 s:
// b2 dies at 10 s, is ejected at 10.2 s (two requests fail in between),
// comes back at 20 s and is re-admitted at 21 s.
func killRun() ([]Request, map[string]int64, []HealthEvent) {
	var reqs []Request
	names := []string{"b1", "b2", "b3"}
	for i := range 3000 {
		ms := int64(i * 10)
		r := Request{StartMS: ms, Backend: names[i%3], Status: 200, LatencyMS: 1}
		down := ms >= 10000 && ms < 21000
		if down && r.Backend == "b2" {
			if ms < 10070 {
				// Detection: k6 sees an error after a second.
				r = Request{StartMS: ms, LatencyMS: 1000, Error: 1211}
			} else {
				r.Backend = "b1"
			}
		}
		reqs = append(reqs, r)
	}
	ev := map[string]int64{"kill": 10000, "start": 20000}
	health := []HealthEvent{
		{5000, "backend healthy", "addr2"}, // before the kill: ignored
		{10200, "backend ejected", "addr2"},
		{21000, "backend healthy", "addr2"},
	}
	return reqs, ev, health
}

func TestAnalyseKill(t *testing.T) {
	reqs, ev, health := killRun()
	k, err := AnalyseKill(reqs, ev, health, "addr2", "b2")
	if err != nil {
		t.Fatal(err)
	}
	if k.Eject != 10200 || k.Readmit != 21000 || k.EjectMsg != "backend ejected" {
		t.Fatalf("eject %d (%s), readmit %d", k.Eject, k.EjectMsg, k.Readmit)
	}
	if k.Detection.Failed == 0 || k.Detection.Failed != k.All.Failed {
		t.Fatalf("detection failed %d of %d failed", k.Detection.Failed, k.All.Failed)
	}
	if k.FailedAfterEjection != 0 || k.VictimRequestsWhenOut != 0 {
		t.Fatalf("after ejection: failed %d, victim served %d", k.FailedAfterEjection, k.VictimRequestsWhenOut)
	}
	if k.Detection.Over[100] != k.Detection.Failed {
		t.Fatalf("slow requests in detection %d, want %d", k.Detection.Over[100], k.Detection.Failed)
	}
	var out strings.Builder
	WriteKill(&out, k, "b2")
	for _, want := range []string{"time to eject 200 ms", "time to re-admit 1000 ms", "failed after ejection             0"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary lacks %q:\n%s", want, out.String())
		}
	}
	checkSVG(t, KillFigure(reqs, k, "b2").SVG())

	// No ejection in the log is an error, not a zero.
	if _, err := AnalyseKill(reqs, ev, health[:1], "addr2", "b2"); err == nil {
		t.Fatal("want an error when the backend never left rotation")
	}
}

func TestCheckTick(t *testing.T) {
	if err := CheckTick(10000, 12); err != nil {
		t.Fatal(err)
	}
	if err := CheckTick(10005, 0); err != nil {
		t.Fatal(err)
	}
	err := CheckTick(10833, 33)
	if err == nil || !strings.Contains(err.Error(), "+8.3%") {
		t.Fatalf("tick 10833: got %v, want an error naming +8.3%%", err)
	}
}

func TestCheckStep(t *testing.T) {
	if err := CheckStep(1e9, 1e9+50e6); err != nil {
		t.Fatal(err)
	}
	if err := CheckStep(1e9, 1e9-2.5e9); err == nil {
		t.Fatal("want an error for a 2.5 s step")
	}
}

func TestClockNow(t *testing.T) {
	a, err := offset()
	if err != nil {
		t.Fatal(err)
	}
	b, err := offset()
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckStep(a, b); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tick(); err != nil {
		t.Fatal(err)
	}
}

func TestBarChartSVG(t *testing.T) {
	f := Figure{Title: "t <&>", Panels: []Panel{BarChart{
		Title:      "bars",
		Categories: []string{"a", "b"},
		Series: []BarSeries{
			{Name: "x", Class: Slot(0), Values: []float64{1, 2}, Lo: []float64{0.5, 2}, Hi: []float64{1.5, 2}},
			{Name: "y", Class: Slot(1), Values: []float64{3, math.NaN()}},
		},
		Label: func(s, c int, v float64) string { return num(v) },
	}}}
	svg := f.SVG()
	checkSVG(t, svg)
	if strings.Count(svg, "<path class=\"s") != 3 {
		t.Fatalf("want 3 bars (the NaN one left out):\n%s", svg)
	}
}

// checkSVG fails unless s is well-formed XML with an <svg> root.
func checkSVG(t *testing.T, s string) {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(s))
	root := ""
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("SVG is not well-formed: %v", err)
		}
		if se, ok := tok.(xml.StartElement); ok && root == "" {
			root = se.Name.Local
		}
	}
	if root != "svg" {
		t.Fatalf("root element %q, want svg", root)
	}
}

func TestBySecondDropsThePartialLastSecond(t *testing.T) {
	var reqs []Request
	for ms := int64(0); ms <= 2050; ms += 10 { // two full seconds and 50 ms
		reqs = append(reqs, Request{StartMS: ms, Backend: "b", Status: 200})
	}
	secs := BySecond(reqs, 0)
	if len(secs) != 2 || secs[0].OK != 100 || secs[1].OK != 100 {
		t.Fatalf("got %d seconds, first %+v", len(secs), secs[0].OK)
	}
}

// slowRun: 100 req/s over three backends for 30 s; b2 is slow (250 ms)
// from 10 s to 20 s. With eject set, b2 is ejected at 10.1 s and gets no
// requests after that until 21 s.
func slowRun(eject bool) ([]Request, map[string]int64, []HealthEvent) {
	var reqs []Request
	names := []string{"b1", "b2", "b3"}
	for i := range 3000 {
		ms := int64(i * 10)
		r := Request{StartMS: ms, Backend: names[i%3], Status: 200, LatencyMS: 1}
		if r.Backend == "b2" && ms >= 10000 && ms < 21000 {
			switch {
			case eject && ms >= 10100:
				r.Backend = "b1"
			case ms < 20000:
				r.LatencyMS = 250
			}
		}
		reqs = append(reqs, r)
	}
	ev := map[string]int64{"fault": 10000, "clear": 20000}
	var health []HealthEvent
	if eject {
		health = []HealthEvent{{10100, "backend ejected", "addr2"}, {21000, "backend healthy", "addr2"}}
	}
	return reqs, ev, health
}

func TestAnalyseSlow(t *testing.T) {
	reqs, ev, health := slowRun(false)
	rr, err := AnalyseSlow("rr", reqs, ev, health, "addr2")
	if err != nil {
		t.Fatal(err)
	}
	if rr.Eject != 0 || share(rr.During, "b2") < 33 || rr.During.P99 != 250 {
		t.Fatalf("rr: eject %d, share %.1f, p99 %v", rr.Eject, share(rr.During, "b2"), rr.During.P99)
	}
	if rr.Before.Requests != 800 {
		t.Fatalf("before: %d requests, want 800 (warm-up left out)", rr.Before.Requests)
	}

	reqs, ev, health = slowRun(true)
	ej, err := AnalyseSlow("lc-eject", reqs, ev, health, "addr2")
	if err != nil {
		t.Fatal(err)
	}
	if ej.Eject != 10100 || ej.Readmit != 21000 {
		t.Fatalf("eject %d, readmit %d", ej.Eject, ej.Readmit)
	}
	if share(ej.DuringEjected, "b2") != 0 || ej.DuringEjected.P99 != 1 {
		t.Fatalf("ejected window: share %.1f, p99 %v", share(ej.DuringEjected, "b2"), ej.DuringEjected.P99)
	}

	var out strings.Builder
	WriteSlow(&out, []SlowRun{rr, ej}, "b2")
	for _, want := range []string{"rr         b2 stayed in rotation", "out 100 ms after the fault (backend ejected), back 1000 ms after it cleared"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary lacks %q:\n%s", want, out.String())
		}
	}
	checkSVG(t, SlowFigure([]SlowRun{rr, ej}, "netem delay", "b2").SVG())

	// An ejection after the fault cleared is not one the fault caused.
	health[0].MS = 20500
	late, _ := AnalyseSlow("x", reqs, ev, health, "addr2")
	if late.Eject != 0 {
		t.Fatalf("eject at %d counted, after the fault cleared", late.Eject)
	}
}

const wrkOut = `Running 15s test @ https://default.test:8443/1k
  8 threads and 200 connections
  Thread Stats   Avg      Stdev     Max   +/- Stdev
    Latency     4.21ms    2.48ms  88.18ms   91.02%
    Req/Sec     6.04k   512.33     7.11k    70.33%
  Latency Distribution
     50%  211.00us
     75%    1.45ms
     90%    3.86ms
     99%    1.02s
  720661 requests in 15.00s, 834.12MB read
  Socket errors: connect 0, read 3, write 0, timeout 2
  Non-2xx or 3xx responses: 7
Requests/sec:  48044.72
Transfer/sec:     55.61MB
`

const wrk2Out = `Running 40s test @ https://default.test:8443/1k
  8 threads and 200 connections
  Thread calibration: mean lat.: 1.497ms, rate sampling interval: 10ms
  Thread Stats   Avg      Stdev     Max   +/- Stdev
    Latency     1.48ms  506.36us   6.52ms   71.08%
  Latency Distribution (HdrHistogram - Recorded Latency)
 50.000%    1.43ms
 75.000%    1.79ms
 90.000%    2.10ms
 99.000%    2.96ms
 99.900%    3.83ms
 99.990%    6.52ms
100.000%    6.52ms

  Detailed Percentile spectrum:
       Value   Percentile   TotalCount 1/(1-Percentile)

       0.512     0.000000            1         1.00
       6.519     1.000000         3900          inf
#[Mean    =        1.477, StdDeviation   =        0.506]
----------------------------------------------------------
  1344004 requests in 40.00s, 3.32MB read
Requests/sec:  33600.10
`

func TestParseWrk(t *testing.T) {
	w, err := ParseWrk(strings.NewReader(wrkOut))
	if err != nil {
		t.Fatal(err)
	}
	if w.RPS != 48044.72 || w.Requests != 720661 || w.Errors != 12 {
		t.Fatalf("rps %v requests %d errors %d", w.RPS, w.Requests, w.Errors)
	}
	if w.P50 != 0.211 || w.P90 != 3.86 || w.P99 != 1020 || !math.IsNaN(w.P999) {
		t.Fatalf("percentiles %v %v %v %v", w.P50, w.P90, w.P99, w.P999)
	}
	w2, err := ParseWrk(strings.NewReader(wrk2Out))
	if err != nil {
		t.Fatal(err)
	}
	if w2.RPS != 33600.10 || w2.Requests != 1344004 || w2.P99 != 2.96 || w2.P999 != 3.83 || w2.Errors != 0 {
		t.Fatalf("wrk2: %+v", w2)
	}
	if _, err := ParseWrk(strings.NewReader("unable to connect to default.test:8443 Connection refused\n")); err == nil {
		t.Fatal("want an error for a run without Requests/sec")
	}
}

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func fanOut(outs ...string) string {
	var b strings.Builder
	for i, o := range outs {
		fmt.Fprintf(&b, "==== process %d of %d\n%s", i+1, len(outs), o)
	}
	return b.String()
}

func near(got, want, tol float64) bool { return math.Abs(got-want) <= tol*want }

func TestParseWrkProcesses(t *testing.T) {
	fast, slow := readTestdata(t, "wrk2.txt"), readTestdata(t, "wrk2-slow.txt")
	one, err := ParseWrk(strings.NewReader(fast))
	if err != nil {
		t.Fatal(err)
	}
	// The merge of one spectrum is wrk2's own percentiles.
	_, sp, _ := parseWrkOne(fast)
	for _, c := range []struct{ p, want float64 }{{50, one.P50}, {90, one.P90}, {99, one.P99}, {99.9, one.P999}} {
		if got := mergedPercentile([][]specPoint{sp}, c.p); !near(got, c.want, 0.02) {
			t.Errorf("p%v of one spectrum: %v, wrk2 says %v", c.p, got, c.want)
		}
	}

	// Two identical processes: everything summed, the percentiles kept.
	two, err := ParseWrk(strings.NewReader(fanOut(fast, fast)))
	if err != nil {
		t.Fatal(err)
	}
	if two.RPS != 2*one.RPS || two.Requests != 2*one.Requests || two.Errors != 0 {
		t.Fatalf("sums: %+v of %+v", two, one)
	}
	if !near(two.P50, one.P50, 0.02) || !near(two.P99, one.P99, 0.02) || !near(two.P999, one.P999, 0.02) {
		t.Fatalf("percentiles of two copies %+v, of one %+v", two, one)
	}

	// A fast and a slow process: each percentile between theirs, and the
	// slow one's tail dominates the merged p99.
	s1, _ := ParseWrk(strings.NewReader(slow))
	mix, err := ParseWrk(strings.NewReader(fanOut(fast, slow)))
	if err != nil {
		t.Fatal(err)
	}
	if mix.P50 < one.P50 || mix.P50 > s1.P50 || mix.P99 < one.P99 || mix.P99 > s1.P99 {
		t.Fatalf("merged p50 %v p99 %v outside fast %v/%v and slow %v/%v", mix.P50, mix.P99, one.P50, one.P99, s1.P50, s1.P99)
	}
	if mix.P99 < 10*one.P99 {
		t.Fatalf("merged p99 %v: the slow process's 10%% tail is 5%% of all, so p99 is in it", mix.P99)
	}

	// wrk prints no spectrum: summed, no percentiles.
	w, err := ParseWrk(strings.NewReader(fanOut(wrkOut, wrkOut)))
	if err != nil {
		t.Fatal(err)
	}
	if w.RPS != 2*48044.72 || w.Errors != 24 || !math.IsNaN(w.P99) {
		t.Fatalf("two wrk processes: %+v", w)
	}

	// A failed process fails the run, by number.
	if _, err := ParseWrk(strings.NewReader(fanOut(wrkOut, "unable to connect\n"))); err == nil || !strings.Contains(err.Error(), "process 2 of 2") {
		t.Fatalf("err %v, want process 2 of 2 named", err)
	}
}

func TestReadBench(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, s string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	meta := "wall_ms=15000\ncpu_usec=60000000\nmem_peak_bytes=52428800\n"
	for _, d := range []string{"proxy", "nginx"} {
		for i, rps := range []string{"40000", "50000", "45000"} {
			out := strings.Replace(wrkOut, "48044.72", rps, 1)
			if d == "nginx" {
				out = strings.Replace(wrkOut, "48044.72", "50000", 1)
			}
			m := meta
			if d == "proxy" {
				m += "gc_cpu_s=3\ngo_busy_cpu_s=60\n"
			}
			write(fmt.Sprintf("raw/%s/ka-1k-c200/tput-%d.txt", d, i+1), out)
			write(fmt.Sprintf("raw/%s/ka-1k-c200/tput-%d.meta", d, i+1), m)
			write(fmt.Sprintf("raw/%s/ka-1k-c200/lat-%d.txt", d, i+1), wrk2Out)
			write(fmt.Sprintf("raw/%s/ka-1k-c200/lat-%d.meta", d, i+1), meta+"rate=31500\n")
		}
	}
	cells, err := ReadBench(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cells) != 2 || cells[0].DUT != "proxy" || cells[0].Conns != 200 || len(cells[0].Tput) != 3 || len(cells[0].Lat) != 3 {
		t.Fatalf("cells: %+v", cells)
	}
	if r := Ratio(cells, "ka-1k", 200); r != 90 {
		t.Fatalf("ratio %v, want 90 (median 45000 of 50000)", r)
	}
	p := cells[0].Tput[0]
	if p.Cores() != 4 || p.GCShare() != 5 || !math.IsNaN(cells[1].Tput[0].GCShare()) {
		t.Fatalf("cores %v gc %v", p.Cores(), p.GCShare())
	}
	var out strings.Builder
	WriteBench(&out, cells)
	for _, want := range []string{"45000 (40000-50000)", "90.0", "2.96 (2.96-2.96)", "31500"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("tables lack %q:\n%s", want, out.String())
		}
	}
	tput, lat := BenchFigures(cells, "test machine")
	checkSVG(t, tput.SVG())
	checkSVG(t, lat.SVG())
	if !strings.Contains(tput.SVG(), "200 connections: 90% of nginx") {
		t.Error("throughput graph lacks the ratio")
	}
}

func TestVariants(t *testing.T) {
	mk := func(name string, rps float64, p99 float64, gc float64) Cell {
		var c Cell
		c.DUT, c.Scenario, c.Conns = "proxy", name, 200
		for range 3 {
			c.Tput = append(c.Tput, Run{WrkResult{RPS: rps, Requests: 1000}, Meta{"wall_ms": 1000, "cpu_usec": 4e6, "mem_peak_bytes": 32 << 20, "gc_cpu_s": gc, "go_busy_cpu_s": 100}})
			c.Lat = append(c.Lat, Run{WrkResult{RPS: rps * 0.7, Requests: 700, P50: 1, P99: p99, P999: 2 * p99}, Meta{"wall_ms": 1000, "cpu_usec": 7e4, "rate": 35000}})
		}
		return c
	}
	cells := []Cell{mk("gogc-200", 55000, 4, 1.5), mk("gogc-100", 50000, 5, 3)}
	orderVariants(cells, []string{"gogc-100", "gogc-200"})
	if cells[0].Scenario != "gogc-100" {
		t.Fatalf("order: %s first", cells[0].Scenario)
	}
	var out strings.Builder
	WriteVariants(&out, cells)
	for _, want := range []string{"GOGC=100 (default)", "+10.0%", "-20.0%", "35000 req/s"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("table lacks %q:\n%s", want, out.String())
		}
	}
	checkSVG(t, VariantsFigure(cells, "t", "m").SVG())
	if variantLabel("gogc-off-limit-256mib") != "GOGC=off, GOMEMLIMIT=256MiB" || variantLabel("before") != "before" {
		t.Error("variantLabel")
	}
}

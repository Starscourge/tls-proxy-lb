package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// WrkResult is what bench.sh needs from one wrk or wrk2 run.
type WrkResult struct {
	RPS      float64
	Requests int64
	Errors   int64 // socket errors and non-2xx/3xx responses
	// Latency percentiles in ms; NaN when the output has none (wrk prints
	// no 99.9th).
	P50, P90, P99, P999 float64
}

var (
	reWrkPct    = regexp.MustCompile(`^\s*(\d+(?:\.\d+)?)%\s+(\d+(?:\.\d+)?)(us|ms|s|m)\s*$`)
	reRequests  = regexp.MustCompile(`^\s*(\d+) requests in`)
	reSocketErr = regexp.MustCompile(`Socket errors: connect (\d+), read (\d+), write (\d+), timeout (\d+)`)
	reNon2xx    = regexp.MustCompile(`Non-2xx or 3xx responses: (\d+)`)
)

// ParseWrk reads the output of a wrk or wrk2 run with --latency: of one
// process, or of several run side by side (benchlib.sh's fan-out, each
// process's output under a "==== process I of N" line). Several are
// summed (requests per second, requests, errors), and their percentiles
// are those of the merged wrk2 spectra: NaN when a part has none (wrk).
func ParseWrk(r io.Reader) (WrkResult, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return WrkResult{}, err
	}
	parts := splitProcesses(string(b))
	if len(parts) == 1 {
		res, _, err := parseWrkOne(parts[0])
		return res, err
	}
	nan := math.NaN()
	sum := WrkResult{P50: nan, P90: nan, P99: nan, P999: nan}
	var spectra [][]specPoint
	for i, part := range parts {
		w, sp, err := parseWrkOne(part)
		if err != nil {
			return sum, fmt.Errorf("process %d of %d: %w", i+1, len(parts), err)
		}
		sum.RPS += w.RPS
		sum.Requests += w.Requests
		sum.Errors += w.Errors
		if len(sp) > 0 {
			spectra = append(spectra, sp)
		}
	}
	if len(spectra) == len(parts) {
		sum.P50 = mergedPercentile(spectra, 50)
		sum.P90 = mergedPercentile(spectra, 90)
		sum.P99 = mergedPercentile(spectra, 99)
		sum.P999 = mergedPercentile(spectra, 99.9)
	}
	return sum, nil
}

var reProcess = regexp.MustCompile(`^==== process \d+ of \d+$`)

// splitProcesses splits a fan-out's output at its process lines; output
// without them is one part.
func splitProcesses(s string) []string {
	var parts []string
	var cur strings.Builder
	marked := false
	for line := range strings.Lines(s) {
		if reProcess.MatchString(strings.TrimRight(line, "\n")) {
			if marked {
				parts = append(parts, cur.String())
			}
			cur.Reset()
			marked = true
			continue
		}
		cur.WriteString(line)
	}
	return append(parts, cur.String())
}

// specPoint is a line of wrk2's detailed percentile spectrum: Count
// requests took at most Value ms.
type specPoint struct {
	Value float64
	Count float64
}

var reSpectrum = regexp.MustCompile(`^\s*(\d+\.\d+)\s+(\d+\.\d+)\s+(\d+)\s+(?:\d+\.\d+|inf)\s*$`)

func parseWrkOne(out string) (WrkResult, []specPoint, error) {
	res := WrkResult{P50: math.NaN(), P90: math.NaN(), P99: math.NaN(), P999: math.NaN()}
	var spec []specPoint
	seenRPS := false
	for line := range strings.Lines(out) {
		line = strings.TrimRight(line, "\n")
		// The spectrum's lines carry no % sign, so only the summary's
		// percentiles match here.
		if m := reWrkPct.FindStringSubmatch(line); m != nil {
			p, _ := strconv.ParseFloat(m[1], 64)
			v, _ := strconv.ParseFloat(m[2], 64)
			v *= map[string]float64{"us": 0.001, "ms": 1, "s": 1000, "m": 60000}[m[3]]
			switch p {
			case 50:
				res.P50 = v
			case 90:
				res.P90 = v
			case 99:
				res.P99 = v
			case 99.9:
				res.P999 = v
			}
			continue
		}
		if m := reSpectrum.FindStringSubmatch(line); m != nil {
			v, _ := strconv.ParseFloat(m[1], 64)
			n, _ := strconv.ParseFloat(m[3], 64)
			spec = append(spec, specPoint{v, n})
			continue
		}
		if m := reRequests.FindStringSubmatch(line); m != nil {
			res.Requests, _ = strconv.ParseInt(m[1], 10, 64)
		}
		if m := reSocketErr.FindStringSubmatch(line); m != nil {
			for _, s := range m[1:] {
				n, _ := strconv.ParseInt(s, 10, 64)
				res.Errors += n
			}
		}
		if m := reNon2xx.FindStringSubmatch(line); m != nil {
			n, _ := strconv.ParseInt(m[1], 10, 64)
			res.Errors += n
		}
		if v, ok := strings.CutPrefix(line, "Requests/sec:"); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				return res, nil, fmt.Errorf("parsing requests per second: %w", err)
			}
			res.RPS, seenRPS = f, true
		}
	}
	if !seenRPS {
		return res, nil, errors.New("no Requests/sec line: the run failed")
	}
	return res, spec, nil
}

// countAt is how many of a spectrum's requests took at most v ms,
// interpolated linearly between its lines.
func countAt(sp []specPoint, v float64) float64 {
	if v < sp[0].Value {
		return 0
	}
	for i := 1; i < len(sp); i++ {
		if v < sp[i].Value {
			a, b := sp[i-1], sp[i]
			return a.Count + (b.Count-a.Count)*(v-a.Value)/(b.Value-a.Value)
		}
	}
	return sp[len(sp)-1].Count
}

// mergedPercentile is the p-th percentile, in ms, of the requests of all
// the spectra together: the least latency at which their summed counts,
// interpolated between the spectra's lines, reach p% of the total. It is
// as fine as the spectra, which wrk2 prints at five lines per halving of
// the remaining percentile.
func mergedPercentile(spectra [][]specPoint, p float64) float64 {
	var values []float64
	total := 0.0
	for _, sp := range spectra {
		total += sp[len(sp)-1].Count
		for _, pt := range sp {
			values = append(values, pt.Value)
		}
	}
	slices.Sort(values)
	values = slices.Compact(values)
	at := func(v float64) float64 {
		n := 0.0
		for _, sp := range spectra {
			n += countAt(sp, v)
		}
		return n
	}
	target := p / 100 * total
	prev, prevN := 0.0, 0.0
	for _, v := range values {
		n := at(v)
		if n >= target {
			if n == prevN {
				return v
			}
			return prev + (v-prev)*(target-prevN)/(n-prevN)
		}
		prev, prevN = v, n
	}
	return values[len(values)-1]
}

// Meta is a run's key=value measurements from bench.sh.
type Meta map[string]float64

func readMeta(path string) (Meta, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := Meta{}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", path, k, err)
		}
		m[k] = f
	}
	return m, nil
}

// Run is one measured run.
type Run struct {
	Wrk  WrkResult
	Meta Meta
}

// Cores is the device under test's average CPU use over the run, in cores.
func (r Run) Cores() float64 { return r.Meta["cpu_usec"] / (r.Meta["wall_ms"] * 1000) }

// CPUPerReq is its CPU time per request, µs.
func (r Run) CPUPerReq() float64 { return r.Meta["cpu_usec"] / float64(r.Wrk.Requests) }

// GCShare is the garbage collector's share of the Go process's CPU, in
// percent; NaN for nginx.
func (r Run) GCShare() float64 {
	if r.Meta["go_busy_cpu_s"] == 0 {
		return math.NaN()
	}
	return 100 * r.Meta["gc_cpu_s"] / r.Meta["go_busy_cpu_s"]
}

// Cell is one device under test in one scenario at one concurrency.
type Cell struct {
	DUT, Scenario string
	Conns         int
	Tput, Lat     []Run
}

// Stat is the median and the spread of one number across runs.
type Stat struct{ Med, Min, Max float64 }

func stat(runs []Run, f func(Run) float64) Stat {
	var v []float64
	for _, r := range runs {
		if x := f(r); !math.IsNaN(x) {
			v = append(v, x)
		}
	}
	if len(v) == 0 {
		return Stat{math.NaN(), math.NaN(), math.NaN()}
	}
	slices.Sort(v)
	// The median of an even count is the lower middle, as bench.sh takes it.
	return Stat{v[(len(v)-1)/2], v[0], v[len(v)-1]}
}

func errorsOf(runs []Run) int64 {
	var n int64
	for _, r := range runs {
		n += r.Wrk.Errors
	}
	return n
}

// The order things are reported in; anything else sorts after.
var (
	scenarioOrder = []string{"ka-1k", "new-1k", "ka-100k"}
	scenarioTitle = map[string]string{
		"ka-1k":   "1 KB, keep-alive",
		"new-1k":  "1 KB, new TLS connection per request",
		"ka-100k": "100 KB, keep-alive",
	}
	dutOrder = []string{"proxy", "nginx"}
	dutName  = map[string]string{"proxy": "tls-proxy-lb", "nginx": "nginx"}
)

func orderOf(list []string, s string) int {
	if i := slices.Index(list, s); i >= 0 {
		return i
	}
	return len(list)
}

// ReadBench reads DIR/raw/DUT/SCENARIO-cCONNS/{tput,lat}-N.{txt,meta}.
func ReadBench(dir string) ([]Cell, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "raw", "*", "*-c*"))
	if err != nil {
		return nil, err
	}
	var cells []Cell
	for _, p := range paths {
		name := filepath.Base(p)
		i := strings.LastIndex(name, "-c")
		conns, err := strconv.Atoi(name[i+2:])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		c := Cell{DUT: filepath.Base(filepath.Dir(p)), Scenario: name[:i], Conns: conns}
		for _, kind := range []string{"tput", "lat"} {
			txts, _ := filepath.Glob(filepath.Join(p, kind+"-*.txt"))
			slices.Sort(txts)
			for _, t := range txts {
				f, err := os.Open(t)
				if err != nil {
					return nil, err
				}
				w, err := ParseWrk(f)
				f.Close()
				if err != nil {
					return nil, fmt.Errorf("%s: %w", t, err)
				}
				m, err := readMeta(strings.TrimSuffix(t, ".txt") + ".meta")
				if err != nil {
					return nil, err
				}
				if kind == "tput" {
					c.Tput = append(c.Tput, Run{w, m})
				} else {
					c.Lat = append(c.Lat, Run{w, m})
				}
			}
		}
		cells = append(cells, c)
	}
	if len(cells) == 0 {
		return nil, fmt.Errorf("%s: no results under raw/", dir)
	}
	slices.SortFunc(cells, func(a, b Cell) int {
		if d := orderOf(scenarioOrder, a.Scenario) - orderOf(scenarioOrder, b.Scenario); d != 0 {
			return d
		}
		if a.Scenario != b.Scenario {
			return strings.Compare(a.Scenario, b.Scenario)
		}
		if a.Conns != b.Conns {
			return a.Conns - b.Conns
		}
		return orderOf(dutOrder, a.DUT) - orderOf(dutOrder, b.DUT)
	})
	return cells, nil
}

func find(cells []Cell, dut, sc string, conns int) (Cell, bool) {
	for _, c := range cells {
		if c.DUT == dut && c.Scenario == sc && c.Conns == conns {
			return c, true
		}
	}
	return Cell{}, false
}

func rpsOf(r Run) float64 { return r.Wrk.RPS }

// Ratio is the proxy's median throughput as a percentage of nginx's in
// the same scenario and concurrency; NaN without both.
func Ratio(cells []Cell, sc string, conns int) float64 {
	p, ok1 := find(cells, "proxy", sc, conns)
	n, ok2 := find(cells, "nginx", sc, conns)
	if !ok1 || !ok2 {
		return math.NaN()
	}
	return 100 * stat(p.Tput, rpsOf).Med / stat(n.Tput, rpsOf).Med
}

func fmtStat(s Stat, f string) string {
	if math.IsNaN(s.Med) {
		return "-"
	}
	return fmt.Sprintf(f+" ("+f+"-"+f+")", s.Med, s.Min, s.Max)
}

func fmtF(v float64, f string) string {
	if math.IsNaN(v) {
		return "-"
	}
	return fmt.Sprintf(f, v)
}

// WriteBench prints the throughput and latency tables.
func WriteBench(w io.Writer, cells []Cell) {
	fmt.Fprintln(w, "Throughput: wrk at full speed; median of the runs (min-max)")
	fmt.Fprintf(w, "%-8s %6s %-13s %28s %8s %7s %9s %6s %7s\n", "scenario", "conns", "", "req/s", "%nginx", "cores", "peak MB", "GC %", "errors")
	for _, c := range cells {
		ratio := "-"
		if c.DUT == "proxy" {
			ratio = fmtF(Ratio(cells, c.Scenario, c.Conns), "%.1f")
		}
		fmt.Fprintf(w, "%-8s %6d %-13s %28s %8s %7s %9s %6s %7d\n", c.Scenario, c.Conns, dutName[c.DUT],
			fmtStat(stat(c.Tput, rpsOf), "%.0f"), ratio,
			fmtF(stat(c.Tput, Run.Cores).Med, "%.2f"),
			// mem_peak_bytes is the cell's peak so far: the last run's is the
			// peak of them all.
			fmtF(stat(c.Tput, func(r Run) float64 { return r.Meta["mem_peak_bytes"] / (1 << 20) }).Max, "%.1f"),
			fmtF(stat(c.Tput, Run.GCShare).Med, "%.1f"), errorsOf(c.Tput))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Latency: wrk2 at 70% of the device's own median throughput; ms, median of the runs (min-max)")
	fmt.Fprintf(w, "%-8s %6s %-13s %8s %8s %22s %22s %8s %7s\n", "scenario", "conns", "", "rate", "p50", "p99", "p99.9", "µs/req", "errors")
	for _, c := range cells {
		if len(c.Lat) == 0 {
			continue
		}
		fmt.Fprintf(w, "%-8s %6d %-13s %8.0f %8s %22s %22s %8s %7d\n", c.Scenario, c.Conns, dutName[c.DUT],
			c.Lat[0].Meta["rate"],
			fmtF(stat(c.Lat, func(r Run) float64 { return r.Wrk.P50 }).Med, "%.2f"),
			fmtStat(stat(c.Lat, func(r Run) float64 { return r.Wrk.P99 }), "%.2f"),
			fmtStat(stat(c.Lat, func(r Run) float64 { return r.Wrk.P999 }), "%.2f"),
			fmtF(stat(c.Lat, Run.CPUPerReq).Med, "%.1f"), errorsOf(c.Lat))
	}
}

// benchPanels draws one bar panel per scenario: categories are the
// concurrencies, one bar per device under test.
func benchPanels(cells []Cell, title func(sc string) string, cat func(sc string, conns int) string,
	runs func(Cell) []Run, f func(Run) float64, suffix string) []Panel {
	var scs []string
	conns := map[string][]int{}
	for _, c := range cells {
		if !slices.Contains(scs, c.Scenario) {
			scs = append(scs, c.Scenario)
		}
		if !slices.Contains(conns[c.Scenario], c.Conns) {
			conns[c.Scenario] = append(conns[c.Scenario], c.Conns)
		}
	}
	var panels []Panel
	for _, sc := range scs {
		bc := BarChart{Title: title(sc), YTickSuffix: suffix, Height: 230}
		for _, n := range conns[sc] {
			bc.Categories = append(bc.Categories, cat(sc, n))
		}
		for di, d := range dutOrder {
			s := BarSeries{Name: dutName[d], Class: Slot(di)}
			for _, n := range conns[sc] {
				c, ok := find(cells, d, sc, n)
				st := Stat{math.NaN(), math.NaN(), math.NaN()}
				if ok {
					st = stat(runs(c), f)
				}
				s.Values = append(s.Values, st.Med)
				s.Lo = append(s.Lo, st.Min)
				s.Hi = append(s.Hi, st.Max)
			}
			bc.Series = append(bc.Series, s)
		}
		bc.Label = func(_, _ int, v float64) string { return num(v) }
		panels = append(panels, bc)
	}
	return panels
}

// BenchFigures returns the throughput and latency graphs.
func BenchFigures(cells []Cell, machine string) (Figure, Figure) {
	tput := Figure{
		Title:    "Throughput against nginx: median requests per second of the runs, whiskers min to max",
		Subtitle: "wrk at full speed; " + machine,
		Panels: benchPanels(cells,
			func(sc string) string { return scenarioTitle[sc] },
			func(sc string, n int) string {
				if r := Ratio(cells, sc, n); !math.IsNaN(r) {
					return fmt.Sprintf("%d connections: %.0f%% of nginx", n, r)
				}
				return fmt.Sprintf("%d connections", n)
			},
			func(c Cell) []Run { return c.Tput }, rpsOf, ""),
	}
	lat := Figure{
		Title:    "p99 latency at 70% of each one's own maximum: median of the runs in ms, whiskers min to max",
		Subtitle: "wrk2 at a constant rate (coordinated omission corrected); " + machine,
		Panels: benchPanels(cells,
			func(sc string) string { return scenarioTitle[sc] },
			func(sc string, n int) string { return fmt.Sprintf("%d connections", n) },
			func(c Cell) []Run { return c.Lat }, func(r Run) float64 { return r.Wrk.P99 }, " ms"),
	}
	return tput, lat
}

// machineLine is the short hardware note for the graphs, from env.txt.
func machineLine(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "env.txt"))
	if err != nil {
		return "same machine"
	}
	var cpu string
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "cpu:"); ok {
			cpu = strings.TrimSpace(v)
		}
	}
	return "4 CPUs each, client and servers on one machine (" + cpu + "), so the numbers are relative"
}

// cmdBench: plot bench -dir DIR writes DIR/bench-throughput.svg and
// DIR/bench-latency.svg and prints the tables.
func cmdBench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	dir := fs.String("dir", "", "bench.sh's results directory")
	fs.Parse(args)
	if *dir == "" {
		fs.Usage()
		return errors.New("missing -dir")
	}
	cells, err := ReadBench(*dir)
	if err != nil {
		return err
	}
	WriteBench(os.Stdout, cells)
	tput, lat := BenchFigures(cells, machineLine(*dir))
	if err := os.WriteFile(filepath.Join(*dir, "bench-throughput.svg"), []byte(tput.SVG()), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(*dir, "bench-latency.svg"), []byte(lat.SVG()), 0o644)
}

// cmdWrk prints, per wrk output file, its requests per second (a whole
// number) and its p99 in ms ("NaN" without one), for the scripts' progress
// lines and medians.
func cmdWrk(files []string) error {
	if len(files) == 0 {
		return errors.New("usage: plot wrk FILE [FILE]")
	}
	for _, name := range files {
		f, err := os.Open(name)
		if err != nil {
			return err
		}
		w, err := ParseWrk(f)
		f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		fmt.Printf("%d %.2f\n", int64(w.RPS), w.P99)
	}
	return nil
}

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// SlowRun is one run of experiment C: one proxy config, one fault.
type SlowRun struct {
	Name          string
	Reqs          []Request
	T0            int64
	Fault, Clear  int64
	Eject         int64 // 0: never left rotation during the fault
	EjectMsg      string
	Readmit       int64 // the first time back after Eject; 0: not left, or never came back
	Ejections     int   // times it left rotation during the fault
	Before        Window
	During        Window // [fault, clear)
	DuringEjected Window // [eject, min(readmit, clear)): out of rotation, the fault on
	After         Window // [clear, end]
}

// warmupMS is left out of the baseline windows: k6's first second opens
// its connections and handshakes, which is k6 starting, not the proxy.
const warmupMS = 2000

// AnalyseSlow splits a run at the fault, the ejection (if any) and the
// clearing of the fault.
func AnalyseSlow(name string, reqs []Request, ev map[string]int64, health []HealthEvent, victim string) (SlowRun, error) {
	r := SlowRun{Name: name, Reqs: reqs, T0: reqs[0].StartMS}
	var ok bool
	if r.Fault, ok = ev["fault"]; !ok {
		return r, fmt.Errorf("%s: events: no fault", name)
	}
	if r.Clear, ok = ev["clear"]; !ok {
		return r, fmt.Errorf("%s: events: no clear", name)
	}
	// An ejection that runs out while the fault is still on can end in
	// re-admission, and another ejection, before the fault clears.
	for from := r.Fault; ; {
		e, ok := FirstHealth(health, victim, from, "backend ejected", "backend unhealthy")
		if !ok || e.MS >= r.Clear {
			break
		}
		r.Ejections++
		h, back := FirstHealth(health, victim, e.MS+1, "backend healthy")
		if r.Eject == 0 {
			r.Eject, r.EjectMsg = e.MS, e.Msg
			if back {
				r.Readmit = h.MS
			}
		}
		if !back {
			break
		}
		from = h.MS + 1
	}
	end := reqs[len(reqs)-1].StartMS + 1
	thr := []float64{100, 1000}
	r.Before = Summarise(reqs, r.T0+warmupMS, r.Fault, thr...)
	r.During = Summarise(reqs, r.Fault, r.Clear, thr...)
	if r.Eject != 0 {
		out := r.Clear
		if r.Readmit != 0 {
			out = min(out, r.Readmit)
		}
		r.DuringEjected = Summarise(reqs, r.Eject, out, thr...)
	}
	r.After = Summarise(reqs, r.Clear, end, thr...)
	return r, nil
}

// share is backend's share of w's successful requests, in percent.
func share(w Window, backend string) float64 {
	ok := w.Requests - w.Failed
	if ok == 0 {
		return 0
	}
	return 100 * float64(w.ByBackend[backend]) / float64(ok)
}

// WriteSlow prints one table row per run and window.
func WriteSlow(w io.Writer, runs []SlowRun, name string) {
	fmt.Fprintf(w, "%-10s %-22s %9s %7s %8s %9s %9s %10s %8s\n", "config", "window", "requests", "failed", name+"%", "p50 ms", "p99 ms", "max ms", ">100ms")
	for _, r := range runs {
		rows := []struct {
			n string
			w Window
		}{{"before the fault", r.Before}, {"fault on", r.During}}
		if r.Eject != 0 {
			rows = append(rows, struct {
				n string
				w Window
			}{"fault on, out", r.DuringEjected})
		}
		rows = append(rows, struct {
			n string
			w Window
		}{"after the fault", r.After})
		for _, row := range rows {
			fmt.Fprintf(w, "%-10s %-22s %9d %7d %8.1f %9.2f %9.2f %10.1f %8d\n", r.Name, row.n, row.w.Requests, row.w.Failed,
				share(row.w, name), nan0(row.w.P50), nan0(row.w.P99), row.w.Max, row.w.Over[100])
		}
	}
	fmt.Fprintln(w)
	for _, r := range runs {
		switch {
		case r.Eject == 0:
			fmt.Fprintf(w, "%-10s %s stayed in rotation\n", r.Name, name)
		default:
			fmt.Fprintf(w, "%-10s %s out %d ms after the fault (%s), %s%s\n", r.Name, name, r.Eject-r.Fault, r.EjectMsg, r.readmitText(), r.againText())
		}
	}
	fmt.Fprintf(w, "\n(the first %d s of each run, k6 starting up, are left out of \"before the fault\")\n", warmupMS/1000)
}

// readmitText says when the backend came back after its first ejection.
func (r SlowRun) readmitText() string {
	switch {
	case r.Readmit == 0:
		return "not back by the end"
	case r.Readmit < r.Clear:
		return fmt.Sprintf("back %d ms later with the fault still on", r.Readmit-r.Eject)
	default:
		return fmt.Sprintf("back %d ms after it cleared", r.Readmit-r.Clear)
	}
}

func (r SlowRun) againText() string {
	if r.Ejections < 2 {
		return ""
	}
	return fmt.Sprintf("; out %d times in all while the fault was on", r.Ejections)
}

// SlowFigure draws p99 per second for every run, then each run's
// successful requests per second by backend, stacked.
func SlowFigure(runs []SlowRun, fault, name string) Figure {
	var p99s []Series
	var panels []Panel
	xmax := 0.0
	for _, r := range runs {
		xmax = max(xmax, float64(len(BySecond(r.Reqs, r.T0))))
	}
	for i, r := range runs {
		x := func(ms int64) float64 { return float64(ms-r.T0) / 1000 }
		secs := BySecond(r.Reqs, r.T0)
		p := Series{Name: r.Name, Class: Slot(i)}
		for j, s := range secs {
			if len(s.Latency) > 0 {
				p.Points = append(p.Points, Point{float64(j) + 0.5, Percentile(s.Latency, 99)})
			}
		}
		p99s = append(p99s, p)

		backends := Backends(r.Reqs)
		per := make([]Series, len(backends))
		for k, b := range backends {
			per[k] = Series{Name: b, Class: Slot(k)}
			for j, s := range secs {
				per[k].Points = append(per[k].Points, Point{float64(j) + 0.5, float64(s.ByBackend[b])})
			}
		}
		var marks []VMark
		if r.Eject != 0 {
			marks = append(marks, VMark{x(r.Eject), fmt.Sprintf("%s ejected +%d ms", name, r.Eject-r.Fault)})
			if r.Readmit != 0 {
				l := fmt.Sprintf("re-admitted +%d ms", r.Readmit-r.Clear)
				if r.Readmit < r.Clear {
					l = "re-admitted, fault on"
				}
				marks = append(marks, VMark{x(r.Readmit), l})
			}
		}
		panels = append(panels, LineChart{
			Title:  fmt.Sprintf("%s: successful requests per second by backend (stacked); %s share with the fault on %.1f%%", r.Name, name, share(r.During, name)),
			YLabel: "req/s", XMax: xmax, Height: 200, Series: per, Stacked: true, Marks: marks,
			Bands: []Band{{x(r.Fault), x(r.Clear), fault}}, XTickSuffix: " s",
		})
	}
	first := runs[0]
	x := func(ms int64) float64 { return float64(ms-first.T0) / 1000 }
	var parts []string
	for _, r := range runs {
		parts = append(parts, fmt.Sprintf("%s %.1f ms", r.Name, r.During.P99))
	}
	panels = append([]Panel{LineChart{
		Title:  "p99 latency per second, by config (log scale)",
		YLabel: "ms", XMax: xmax, YMin: 0.1, LogY: true, Height: 260, Series: p99s,
		Bands: []Band{{x(first.Fault), x(first.Clear), fault + " on " + name}}, XTickSuffix: " s",
	}}, panels...)
	panels[len(panels)-1] = withXLabel(panels[len(panels)-1], "time since the load started")
	rate := float64(first.Before.Requests) / (float64(first.Fault-first.T0-warmupMS) / 1000)
	return Figure{
		Title:    fmt.Sprintf("Slow a backend: %s on %s from %.0f s to %.0f s", fault, name, x(first.Fault), x(first.Clear)),
		Subtitle: fmt.Sprintf("k6 at a constant %.0f req/s, three backends; p99 with the fault on: %s", roundTo(rate, 10), strings.Join(parts, ", ")),
		Panels:   panels,
	}
}

func withXLabel(p Panel, l string) Panel {
	if c, ok := p.(LineChart); ok {
		c.XLabel = l
		c.Height += 14
		return c
	}
	return p
}

func roundTo(v, m float64) float64 { return float64(int64(v/m+0.5)) * m }

// cmdSlow: plot slow -fault F -backend ADDR -name NAME -svg OUT name=dir...
// where each dir holds requests.csv.gz, events.txt and proxy-health.log.
func cmdSlow(args []string) error {
	fs := flag.NewFlagSet("slow", flag.ExitOnError)
	fault := fs.String("fault", "", "the fault, as the graph names it")
	victim := fs.String("backend", "", "the slowed backend's address in the health log")
	name := fs.String("name", "", "the slowed backend's X-Backend name")
	svg := fs.String("svg", "", "write the graph here")
	fs.Parse(args)
	if *fault == "" || *victim == "" || *name == "" || *svg == "" || fs.NArg() == 0 {
		fs.Usage()
		return errors.New("missing flags or runs (name=dir ...)")
	}
	var runs []SlowRun
	for _, a := range fs.Args() {
		n, dir, ok := strings.Cut(a, "=")
		if !ok {
			return fmt.Errorf("run %q: want name=dir", a)
		}
		reqs, err := ReadRequests(filepath.Join(dir, "requests.csv.gz"))
		if err != nil {
			return err
		}
		ev, err := ReadEvents(filepath.Join(dir, "events.txt"))
		if err != nil {
			return err
		}
		health, err := ReadHealth(filepath.Join(dir, "proxy-health.log"))
		if err != nil {
			return err
		}
		r, err := AnalyseSlow(n, reqs, ev, health, *victim)
		if err != nil {
			return err
		}
		runs = append(runs, r)
	}
	WriteSlow(os.Stdout, runs, *name)
	return os.WriteFile(*svg, []byte(SlowFigure(runs, *fault, *name).SVG()), 0o644)
}

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
)

// KillResult is experiment B's numbers.
type KillResult struct {
	T0                    int64 // first request, epoch ms
	Kill, Eject           int64
	Start, Readmit        int64
	EjectMsg              string // which health event ejected it
	Before, Detection     Window // [t0, kill), [kill, eject)
	Out, After            Window // [eject, readmit), [readmit, end]
	All                   Window
	FailedAfterEjection   int // failures that started at or after the ejection
	VictimRequestsWhenOut int // successful requests to the victim while it was out
}

// AnalyseKill splits the run at the kill, the ejection, the restart and
// the re-admission. victim is the backend's address in the health log,
// name its X-Backend.
func AnalyseKill(reqs []Request, ev map[string]int64, health []HealthEvent, victim, name string) (KillResult, error) {
	var k KillResult
	var ok bool
	if k.Kill, ok = ev["kill"]; !ok {
		return k, errors.New("events: no kill")
	}
	if k.Start, ok = ev["start"]; !ok {
		return k, errors.New("events: no start")
	}
	e, ok := FirstHealth(health, victim, k.Kill, "backend ejected", "backend unhealthy")
	if !ok {
		return k, fmt.Errorf("health log: %s never left rotation after the kill", victim)
	}
	k.Eject, k.EjectMsg = e.MS, e.Msg
	h, ok := FirstHealth(health, victim, k.Start, "backend healthy")
	if !ok {
		return k, fmt.Errorf("health log: %s never returned after the start", victim)
	}
	k.Readmit = h.MS
	k.T0 = reqs[0].StartMS
	end := reqs[len(reqs)-1].StartMS + 1
	thr := []float64{100, 1000}
	k.Before = Summarise(reqs, k.T0+warmupMS, k.Kill, thr...)
	k.Detection = Summarise(reqs, k.Kill, k.Eject, thr...)
	k.Out = Summarise(reqs, k.Eject, k.Readmit, thr...)
	k.After = Summarise(reqs, k.Readmit, end, thr...)
	k.All = Summarise(reqs, k.T0, end, thr...)
	k.FailedAfterEjection = k.Out.Failed + k.After.Failed
	k.VictimRequestsWhenOut = k.Out.ByBackend[name]
	return k, nil
}

// WriteKill prints the summary.
func WriteKill(w io.Writer, k KillResult, name string) {
	s := func(ms int64) float64 { return float64(ms-k.T0) / 1000 }
	fmt.Fprintf(w, "requests             %d, failed %d\n", k.All.Requests, k.All.Failed)
	fmt.Fprintf(w, "%s killed           at %.3f s\n", name, s(k.Kill))
	fmt.Fprintf(w, "out of rotation      at %.3f s (%s): time to eject %d ms\n", s(k.Eject), k.EjectMsg, k.Eject-k.Kill)
	fmt.Fprintf(w, "%s started          at %.3f s\n", name, s(k.Start))
	fmt.Fprintf(w, "back in rotation     at %.3f s: time to re-admit %d ms\n", s(k.Readmit), k.Readmit-k.Start)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%-22s %9s %7s %9s %9s %10s %9s %9s\n", "window", "requests", "failed", "p50 ms", "p99 ms", "max ms", ">100ms", ">1s")
	for _, r := range []struct {
		n string
		w Window
	}{
		{"before the kill", k.Before},
		{"detection", k.Detection},
		{"out of rotation", k.Out},
		{"after re-admission", k.After},
	} {
		fmt.Fprintf(w, "%-22s %9d %7d %9.2f %9.2f %10.1f %9d %9d\n", r.n, r.w.Requests, r.w.Failed,
			nan0(r.w.P50), nan0(r.w.P99), r.w.Max, r.w.Over[100], r.w.Over[1000])
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "failed during detection           %d\n", k.Detection.Failed)
	fmt.Fprintf(w, "failed after ejection             %d\n", k.FailedAfterEjection)
	fmt.Fprintf(w, "served by %s while out of rotation %d\n", name, k.VictimRequestsWhenOut)
	fmt.Fprintf(w, "\n(the first %d s, k6 starting up, are left out of \"before the kill\")\n", warmupMS/1000)
}

func nan0(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return v
}

// KillFigure draws successes and errors, each backend's requests and the
// latency per second, with the four events marked.
func KillFigure(reqs []Request, k KillResult, name string) Figure {
	secs := BySecond(reqs, k.T0)
	x := func(ms int64) float64 { return float64(ms-k.T0) / 1000 }
	xmax := float64(len(secs))
	marks := []VMark{
		{x(k.Kill), name + " killed"},
		{x(k.Eject), fmt.Sprintf("ejected +%d ms", k.Eject-k.Kill)},
		{x(k.Start), name + " started"},
		{x(k.Readmit), fmt.Sprintf("re-admitted +%d ms", k.Readmit-k.Start)},
	}
	band := []Band{{x(k.Kill), x(k.Start), name + " down"}}

	var ok, failed Series
	ok = Series{Name: "succeeded", Class: "neutral"}
	failed = Series{Name: "failed", Class: "err"}
	var p50, p99, pmax Series
	p50 = Series{Name: "p50", Class: Slot(0)}
	p99 = Series{Name: "p99", Class: Slot(1)}
	pmax = Series{Name: "max", Class: "neutral", Dash: true}
	backends := Backends(reqs)
	per := make([]Series, len(backends))
	for i, b := range backends {
		per[i] = Series{Name: b, Class: Slot(i)}
	}
	for i, s := range secs {
		t := float64(i) + 0.5
		ok.Points = append(ok.Points, Point{t, float64(s.OK)})
		failed.Points = append(failed.Points, Point{t, float64(s.Failed)})
		for j, b := range backends {
			per[j].Points = append(per[j].Points, Point{t, float64(s.ByBackend[b])})
		}
		lat := s.Latency
		if len(lat) == 0 {
			continue
		}
		p50.Points = append(p50.Points, Point{t, Percentile(lat, 50)})
		p99.Points = append(p99.Points, Point{t, Percentile(lat, 99)})
		pmax.Points = append(pmax.Points, Point{t, lat[len(lat)-1]})
	}
	rate := float64(k.All.Requests) / xmax
	return Figure{
		Title: fmt.Sprintf("Kill a backend under load: %s, %d failed of %d requests", name, k.All.Failed, k.All.Requests),
		Subtitle: fmt.Sprintf("k6 at a constant %.0f req/s; docker kill at %.0f s, docker start at %.0f s; ejected %d ms after the kill, re-admitted %d ms after the start",
			math.Round(rate/10)*10, x(k.Kill), x(k.Start), k.Eject-k.Kill, k.Readmit-k.Start),
		Panels: []Panel{
			LineChart{Title: "Requests per second, by outcome", YLabel: "req/s", XMax: xmax, Series: []Series{ok, failed}, Marks: marks, Bands: band, XTickSuffix: " s"},
			LineChart{Title: "Successful requests per second, by backend (stacked)", YLabel: "req/s", XMax: xmax, Series: per, Stacked: true, Marks: marks, Bands: band, XTickSuffix: " s"},
			LineChart{Title: "Latency by second of request start (log scale)", YLabel: "ms", XMax: xmax, YMin: 0.1, LogY: true,
				Series: []Series{p50, p99, pmax}, Marks: marks, Bands: band, XTickSuffix: " s", XLabel: "time since the load started"},
		},
	}
}

func cmdKill(args []string) error {
	fs := flag.NewFlagSet("kill", flag.ExitOnError)
	reqPath := fs.String("requests", "", "per-request CSV from demo.js (.gz allowed)")
	evPath := fs.String("events", "", "events file: epoch_ms kill, epoch_ms start")
	healthPath := fs.String("health", "", "the proxy's health log lines")
	victim := fs.String("backend", "", "the killed backend's address in the health log")
	name := fs.String("name", "", "the killed backend's X-Backend name")
	svg := fs.String("svg", "", "write the graph here")
	fs.Parse(args)
	if *reqPath == "" || *evPath == "" || *healthPath == "" || *victim == "" || *name == "" || *svg == "" {
		fs.Usage()
		return errors.New("missing flags")
	}
	reqs, err := ReadRequests(*reqPath)
	if err != nil {
		return err
	}
	ev, err := ReadEvents(*evPath)
	if err != nil {
		return err
	}
	health, err := ReadHealth(*healthPath)
	if err != nil {
		return err
	}
	k, err := AnalyseKill(reqs, ev, health, *victim, *name)
	if err != nil {
		return err
	}
	WriteKill(os.Stdout, k, *name)
	if err := os.WriteFile(*svg, []byte(KillFigure(reqs, k, *name).SVG()), 0o644); err != nil {
		return err
	}
	if k.FailedAfterEjection > 0 {
		return fmt.Errorf("%d requests failed after the ejection", k.FailedAfterEjection)
	}
	if k.VictimRequestsWhenOut > 0 {
		return fmt.Errorf("%s served %d requests while out of rotation", *name, k.VictimRequestsWhenOut)
	}
	return nil
}

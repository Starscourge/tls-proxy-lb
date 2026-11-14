package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Variants are cells of one device in one scenario that differ by
// something else, named in place of the scenario: GOGC settings (gc.sh),
// or builds before and after a change (hotspots.sh).

// variantLabel names a variant for people: gogc-off-limit-256mib is
// "GOGC=off, GOMEMLIMIT=256MiB".
func variantLabel(v string) string {
	switch {
	case v == "gogc-off-limit-256mib":
		return "GOGC=off, GOMEMLIMIT=256MiB"
	case strings.HasPrefix(v, "gogc-"):
		l := "GOGC=" + strings.TrimPrefix(v, "gogc-")
		if l == "GOGC=100" {
			l += " (default)"
		}
		return l
	}
	return v
}

// orderVariants sorts cells by order, a list of variant names; the rest
// keep their place after them.
func orderVariants(cells []Cell, order []string) {
	slices.SortStableFunc(cells, func(a, b Cell) int {
		return orderOf(order, a.Scenario) - orderOf(order, b.Scenario)
	})
}

func p99Of(r Run) float64  { return r.Wrk.P99 }
func p999Of(r Run) float64 { return r.Wrk.P999 }
func memMB(r Run) float64  { return r.Meta["mem_peak_bytes"] / (1 << 20) }

// WriteVariants prints one row per variant, with throughput and p99
// against the first.
func WriteVariants(w io.Writer, cells []Cell) {
	base := cells[0]
	baseRPS := stat(base.Tput, rpsOf).Med
	baseP99 := stat(base.Lat, p99Of).Med
	fmt.Fprintf(w, "%-30s %26s %7s %7s %20s %7s %8s %6s %7s %8s %7s\n",
		"", "req/s", "vs 1st", "p50", "p99", "vs 1st", "p99.9", "GC %", "µs/req", "peak MB", "errors")
	for _, c := range cells {
		rps := stat(c.Tput, rpsOf)
		p99 := stat(c.Lat, p99Of)
		fmt.Fprintf(w, "%-30s %26s %+6.1f%% %7s %20s %+6.1f%% %8s %6s %7s %8s %7d\n", variantLabel(c.Scenario),
			fmtStat(rps, "%.0f"), 100*(rps.Med/baseRPS-1),
			fmtF(stat(c.Lat, func(r Run) float64 { return r.Wrk.P50 }).Med, "%.2f"),
			fmtStat(p99, "%.2f"), 100*(p99.Med/baseP99-1),
			fmtF(stat(c.Lat, p999Of).Med, "%.2f"),
			fmtF(stat(c.Tput, Run.GCShare).Med, "%.2f"),
			fmtF(stat(c.Lat, Run.CPUPerReq).Med, "%.1f"),
			fmtF(stat(c.Tput, memMB).Max, "%.1f"),
			errorsOf(c.Tput)+errorsOf(c.Lat))
	}
	if len(base.Lat) > 0 {
		fmt.Fprintf(w, "\nlatency runs at %.0f req/s for every variant (70%% of the first one's median throughput)\n", base.Lat[0].Meta["rate"])
	}
}

// VariantsFigure draws throughput, p99, GC share and peak memory, one bar
// per variant.
func VariantsFigure(cells []Cell, title, machine string) Figure {
	var cats []string
	for _, c := range cells {
		cats = append(cats, variantLabel(c.Scenario))
	}
	panel := func(t, suffix string, runs func(Cell) []Run, f func(Run) float64, useMax bool) Panel {
		s := BarSeries{Name: "tls-proxy-lb", Class: Slot(0)}
		for _, c := range cells {
			st := stat(runs(c), f)
			v := st.Med
			if useMax {
				v, st.Min = st.Max, st.Max
			}
			s.Values = append(s.Values, v)
			s.Lo = append(s.Lo, st.Min)
			s.Hi = append(s.Hi, st.Max)
		}
		return BarChart{Title: t, YTickSuffix: suffix, Height: 200, Categories: cats, Series: []BarSeries{s},
			Label: func(_, _ int, v float64) string { return num(v) }}
	}
	tput := func(c Cell) []Run { return c.Tput }
	lat := func(c Cell) []Run { return c.Lat }
	rate := ""
	if len(cells[0].Lat) > 0 {
		rate = fmt.Sprintf(" at %s req/s", num(cells[0].Lat[0].Meta["rate"]))
	}
	return Figure{
		Title:    title,
		Subtitle: "1 KB over 200 keep-alive connections; median of the runs, whiskers min to max; " + machine,
		Panels: []Panel{
			panel("Throughput, requests per second (wrk at full speed)", "", tput, rpsOf, false),
			panel("p99 latency"+rate+", the same rate for every variant (wrk2)", " ms", lat, p99Of, false),
			panel("Garbage collector's share of the proxy's CPU, at full speed", "%", tput, Run.GCShare, false),
			panel("Peak memory of the container", " MB", tput, memMB, true),
		},
	}
}

// cmdVariants: plot variants -dir DIR -title T -svg NAME [-order a,b,...]
func cmdVariants(args []string) error {
	fs := flag.NewFlagSet("variants", flag.ExitOnError)
	dir := fs.String("dir", "", "gc.sh's or hotspots.sh's results directory")
	title := fs.String("title", "", "the graph's title")
	svg := fs.String("svg", "", "the graph's file name, in -dir")
	order := fs.String("order", "before,after", "variant order, comma-separated; the first is the baseline")
	fs.Parse(args)
	if *dir == "" || *title == "" || *svg == "" {
		fs.Usage()
		return errors.New("missing flags")
	}
	cells, err := ReadBench(*dir)
	if err != nil {
		return err
	}
	orderVariants(cells, strings.Split(*order, ","))
	if math.IsNaN(stat(cells[0].Tput, rpsOf).Med) {
		return fmt.Errorf("%s: the first variant has no throughput runs", cells[0].Scenario)
	}
	WriteVariants(os.Stdout, cells)
	return os.WriteFile(filepath.Join(*dir, *svg), []byte(VariantsFigure(cells, *title, machineLine(*dir)).SVG()), 0o644)
}

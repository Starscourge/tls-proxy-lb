package main

import (
	"fmt"
	"html"
	"math"
	"slices"
	"strings"
)

// The SVG layer: a figure is a title and a column of panels, each panel a
// line chart (time series) or a bar chart (benchmark cells). It is written
// for one job, graphs in a README, so it has no interaction beyond <title>
// tooltips, and it styles itself with classes whose colours switch with
// prefers-color-scheme (the viewer's OS setting; GitHub shows an SVG in an
// <img>, so its own theme does not reach in).
//
// Colours are the reference data-viz palette: categorical slots in fixed
// order (s1 blue, s2 orange, s3 aqua, s4 yellow, ...), the status red for
// errors, and recessive ink for axes, grid and text.

const style = `
svg { font-family: system-ui, -apple-system, "Segoe UI", sans-serif; font-size: 12px; }
.bg { fill: #fcfcfb; } .ink { fill: #0b0b0b; } .ink2 { fill: #52514e; } .muted { fill: #898781; }
.grid { stroke: #e1e0d9; stroke-width: 1; } .axis { stroke: #c3c2b7; stroke-width: 1; }
.band { fill: #0b0b0b; fill-opacity: 0.05; }
.mark { stroke: #52514e; stroke-width: 1; stroke-dasharray: 4 3; }
.line { fill: none; stroke-width: 2; stroke-linejoin: round; stroke-linecap: round; }
.thin { stroke-width: 1.25; } .dash { stroke-dasharray: 5 3; }
.s1 { stroke: #2a78d6; fill: #2a78d6; } .s2 { stroke: #eb6834; fill: #eb6834; }
.s3 { stroke: #1baf7a; fill: #1baf7a; } .s4 { stroke: #eda100; fill: #eda100; }
.s5 { stroke: #e87ba4; fill: #e87ba4; } .s6 { stroke: #008300; fill: #008300; }
.s7 { stroke: #6250d6; fill: #6250d6; } .s8 { stroke: #e34948; fill: #e34948; }
.neutral { stroke: #898781; fill: #898781; } .err { stroke: #d03b3b; fill: #d03b3b; }
.line.s1, .line.s2, .line.s3, .line.s4, .line.s5, .line.s6, .line.s7, .line.s8, .line.neutral, .line.err { fill: none; }
.whisker { stroke: #52514e; stroke-width: 1.25; }
.area { stroke: none; fill-opacity: 0.85; } .gap { fill: none; stroke: #fcfcfb; stroke-width: 2; }
@media (prefers-color-scheme: dark) {
  .bg { fill: #1a1a19; } .ink { fill: #f0efec; } .ink2 { fill: #c3c2b7; }
  .grid { stroke: #2c2c2a; } .axis { stroke: #383835; }
  .band { fill: #f0efec; fill-opacity: 0.07; } .mark { stroke: #c3c2b7; }
  .s1 { stroke: #3987e5; fill: #3987e5; } .s2 { stroke: #d95926; fill: #d95926; }
  .s3 { stroke: #199e70; fill: #199e70; } .s4 { stroke: #c98500; fill: #c98500; }
  .s5 { stroke: #d55181; fill: #d55181; } .s6 { stroke: #0ca30c; fill: #0ca30c; }
  .s7 { stroke: #9085e9; fill: #9085e9; } .s8 { stroke: #e66767; fill: #e66767; }
  .whisker { stroke: #c3c2b7; } .gap { stroke: #1a1a19; }
}
`

// Slot returns the class of categorical slot i, counted from 0.
func Slot(i int) string { return fmt.Sprintf("s%d", i%8+1) }

// Figure is a titled column of panels, written as one SVG document.
type Figure struct {
	Title    string
	Subtitle string
	Width    int // default 960
	Panels   []Panel
}

// Panel is one chart in a figure.
type Panel interface {
	height() int
	render(b *strings.Builder, x, y, w int)
}

// SVG renders the figure.
func (f Figure) SVG() string {
	w := f.Width
	if w == 0 {
		w = 960
	}
	top := 40
	if f.Subtitle != "" {
		top += 18
	}
	h := top
	for _, p := range f.Panels {
		h += p.height()
	}
	h += 8
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img">`+"\n", w, h, w, h)
	fmt.Fprintf(&b, "<title>%s</title>\n<style>%s</style>\n", esc(f.Title), style)
	fmt.Fprintf(&b, `<rect class="bg" x="0" y="0" width="%d" height="%d" rx="6"/>`+"\n", w, h)
	fmt.Fprintf(&b, `<text class="ink" x="16" y="26" font-size="16" font-weight="600">%s</text>`+"\n", esc(f.Title))
	if f.Subtitle != "" {
		fmt.Fprintf(&b, `<text class="ink2" x="16" y="46">%s</text>`+"\n", esc(f.Subtitle))
	}
	y := top
	for _, p := range f.Panels {
		p.render(&b, 0, y, w)
		y += p.height()
	}
	b.WriteString("</svg>\n")
	return b.String()
}

func esc(s string) string { return html.EscapeString(s) }

// Point is one (x, y) sample. A NaN Y is a gap in the line.
type Point struct{ X, Y float64 }

// Series is one line.
type Series struct {
	Name   string
	Class  string // Slot(i), "neutral" or "err"
	Dash   bool
	Points []Point
}

// VMark is a vertical event line, labelled at the top of the plot.
type VMark struct {
	X     float64
	Label string
}

// Band shades the plot between two x values.
type Band struct {
	X0, X1 float64
	Label  string
}

// LineChart is a time-series panel. Y ranges from YMin to YMax (YMax 0:
// from the data); LogY plots it on a log10 scale from YMin, which must then
// be positive.
type LineChart struct {
	Title       string
	XLabel      string
	YLabel      string
	Height      int // default 240
	XMin, XMax  float64
	YMin, YMax  float64
	LogY        bool
	Series      []Series
	Marks       []VMark
	Bands       []Band
	NoEndLabels bool // skip the direct labels at line ends
	// Stacked draws the series as stacked areas, each on the ones before
	// it. The series must share their X values.
	Stacked     bool
	YTickSuffix string
	XTickSuffix string
}

const (
	padL = 72
	padR = 128
)

func (c LineChart) height() int {
	if c.Height == 0 {
		return 240
	}
	return c.Height
}

// stack returns the series' running totals, in order: each series' Y is
// the sum of its own and every earlier series' at the same index.
func stack(series []Series) []Series {
	out := make([]Series, len(series))
	for i, s := range series {
		out[i] = s
		out[i].Points = slices.Clone(s.Points)
		if i == 0 {
			continue
		}
		for j := range out[i].Points {
			if j < len(out[i-1].Points) {
				out[i].Points[j].Y += out[i-1].Points[j].Y
			}
		}
	}
	return out
}

func (c LineChart) render(b *strings.Builder, x0, y0, w int) {
	h := c.height()
	raw := c.Series
	if c.Stacked {
		c.Series = stack(c.Series)
	}
	// Panel title and legend row, then marks' labels, then the plot.
	px0, px1 := float64(x0+padL), float64(x0+w-padR)
	py0, py1 := float64(y0+46), float64(y0+h-36)
	fmt.Fprintf(b, `<text class="ink" x="%d" y="%d" font-weight="600">%s</text>`+"\n", x0+16, y0+14, esc(c.Title))
	// Legend: the series when there are two or more, then the bands.
	lx := float64(x0 + 16)
	if len(c.Series) >= 2 {
		for _, s := range c.Series {
			dash := ""
			if s.Dash {
				dash = " dash"
			}
			if c.Stacked {
				fmt.Fprintf(b, `<rect class="%s" x="%.1f" y="%d" width="12" height="12" rx="2"/>`, s.Class, lx+2, y0+21)
			} else {
				fmt.Fprintf(b, `<line class="line %s%s" x1="%.1f" y1="%d" x2="%.1f" y2="%d"/>`, s.Class, dash, lx, y0+27, lx+16, y0+27)
			}
			fmt.Fprintf(b, `<text class="ink2" x="%.1f" y="%d">%s</text>`+"\n", lx+21, y0+31, esc(s.Name))
			lx += 21 + 7.2*float64(len([]rune(s.Name))) + 18
		}
	}
	for _, bd := range c.Bands {
		if bd.Label == "" {
			continue
		}
		fmt.Fprintf(b, `<rect class="band" x="%.1f" y="%d" width="16" height="12" rx="2"/><rect class="mark" fill="none" x="%.1f" y="%d" width="16" height="12" rx="2"/>`, lx, y0+21, lx, y0+21)
		fmt.Fprintf(b, `<text class="ink2" x="%.1f" y="%d">%s</text>`+"\n", lx+21, y0+31, esc(bd.Label))
		lx += 21 + 7.2*float64(len([]rune(bd.Label))) + 18
	}

	ymin, ymax := c.YMin, c.YMax
	if ymax == 0 {
		for _, s := range c.Series {
			for _, p := range s.Points {
				if !math.IsNaN(p.Y) && p.Y > ymax {
					ymax = p.Y
				}
			}
		}
		if !c.LogY {
			ymax += (ymax - ymin) * 0.1 // headroom for the marks' labels
		}
		if ymax <= ymin {
			ymax = ymin + 1
		}
	}
	var yticks []float64
	if c.LogY {
		lo, hi := math.Floor(math.Log10(ymin)), math.Ceil(math.Log10(ymax))
		if hi <= lo {
			hi = lo + 1
		}
		ymin, ymax = math.Pow(10, lo), math.Pow(10, hi)
		for e := lo; e <= hi; e++ {
			yticks = append(yticks, math.Pow(10, e))
		}
	} else {
		yticks, ymax = ticks(ymin, ymax, 5)
	}
	sx := func(v float64) float64 { return px0 + (v-c.XMin)/(c.XMax-c.XMin)*(px1-px0) }
	sy := func(v float64) float64 {
		if c.LogY {
			v = math.Max(v, ymin)
			return py1 - (math.Log10(v)-math.Log10(ymin))/(math.Log10(ymax)-math.Log10(ymin))*(py1-py0)
		}
		return py1 - (v-ymin)/(ymax-ymin)*(py1-py0)
	}

	for _, bd := range c.Bands {
		a, z := sx(math.Max(bd.X0, c.XMin)), sx(math.Min(bd.X1, c.XMax))
		fmt.Fprintf(b, `<rect class="band" x="%.1f" y="%.1f" width="%.1f" height="%.1f"><title>%s</title></rect>`+"\n", a, py0, z-a, py1-py0, esc(bd.Label))
	}
	for _, t := range yticks {
		y := sy(t)
		fmt.Fprintf(b, `<line class="grid" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`, px0, y, px1, y)
		fmt.Fprintf(b, `<text class="muted" x="%.1f" y="%.1f" text-anchor="end">%s%s</text>`+"\n", px0-6, y+4, num(t), c.YTickSuffix)
	}
	xt, _ := ticks(c.XMin, c.XMax, 8)
	for _, t := range xt {
		if t > c.XMax {
			continue
		}
		x := sx(t)
		fmt.Fprintf(b, `<line class="axis" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`, x, py1, x, py1+4)
		fmt.Fprintf(b, `<text class="muted" x="%.1f" y="%.1f" text-anchor="middle">%s%s</text>`+"\n", x, py1+17, num(t), c.XTickSuffix)
	}
	fmt.Fprintf(b, `<line class="axis" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`+"\n", px0, py1, px1, py1)
	if c.XLabel != "" {
		fmt.Fprintf(b, `<text class="muted" x="%.1f" y="%.1f" text-anchor="end">%s</text>`+"\n", px1, py1+32, esc(c.XLabel))
	}
	if c.YLabel != "" {
		fmt.Fprintf(b, `<text class="muted" x="%.1f" y="%.1f" text-anchor="middle" transform="rotate(-90 %.1f %.1f)">%s</text>`+"\n",
			float64(x0+16), (py0+py1)/2, float64(x0+16), (py0+py1)/2, esc(c.YLabel))
	}

	// Event marks: labels alternate between two rows so close events stay
	// readable.
	for i, m := range c.Marks {
		if m.X < c.XMin || m.X > c.XMax {
			continue
		}
		x := sx(m.X)
		fmt.Fprintf(b, `<line class="mark" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`, x, py0-2, x, py1)
		ly := py0 - 6
		if i%2 == 1 {
			ly = py0 + 10
		}
		fmt.Fprintf(b, `<text class="ink2" x="%.1f" y="%.1f" font-size="11">%s</text>`+"\n", x+4, ly, esc(m.Label))
	}

	type end struct {
		y    float64
		name string
	}
	var ends []end
	if c.Stacked {
		// Areas from the previous total up to this one, a surface-coloured
		// line along each top as the gap between them, a label in the
		// middle of each band at its right end.
		for i, s := range c.Series {
			var d strings.Builder
			for j, p := range s.Points {
				cmd := "L"
				if j == 0 {
					cmd = "M"
				}
				fmt.Fprintf(&d, "%s%.1f %.1f ", cmd, sx(p.X), sy(p.Y))
			}
			for j := len(s.Points) - 1; j >= 0; j-- {
				base := 0.0
				if i > 0 && j < len(c.Series[i-1].Points) {
					base = c.Series[i-1].Points[j].Y
				}
				fmt.Fprintf(&d, "L%.1f %.1f ", sx(s.Points[j].X), sy(base))
			}
			fmt.Fprintf(b, `<path class="area %s" d="%sZ"><title>%s</title></path>`+"\n", s.Class, d.String(), esc(s.Name))
		}
		for _, s := range c.Series[:len(c.Series)-1] {
			var d strings.Builder
			for j, p := range s.Points {
				cmd := "L"
				if j == 0 {
					cmd = "M"
				}
				fmt.Fprintf(&d, "%s%.1f %.1f ", cmd, sx(p.X), sy(p.Y))
			}
			fmt.Fprintf(b, `<path class="gap" d="%s"/>`+"\n", strings.TrimSpace(d.String()))
		}
		for i, s := range c.Series {
			if n := len(s.Points); n > 0 && len(raw[i].Points) == n && raw[i].Points[n-1].Y > 0 {
				ends = append(ends, end{sy(s.Points[n-1].Y - raw[i].Points[n-1].Y/2), s.Name})
			}
		}
	}
	for _, s := range c.Series {
		if c.Stacked {
			break
		}
		var d strings.Builder
		pen := false
		var last Point
		have := false
		for _, p := range s.Points {
			if math.IsNaN(p.Y) || p.X < c.XMin || p.X > c.XMax {
				pen = false
				continue
			}
			cmd := "L"
			if !pen {
				cmd = "M"
			}
			fmt.Fprintf(&d, "%s%.1f %.1f ", cmd, sx(p.X), sy(p.Y))
			pen, last, have = true, p, true
		}
		dash := ""
		if s.Dash {
			dash = " dash"
		}
		fmt.Fprintf(b, `<path class="line %s%s" d="%s"><title>%s</title></path>`+"\n", s.Class, dash, strings.TrimSpace(d.String()), esc(s.Name))
		if have {
			ends = append(ends, end{sy(last.Y), s.Name})
		}
	}
	if !c.NoEndLabels && len(ends) >= 2 && len(ends) <= 4 {
		slices.SortFunc(ends, func(a, b end) int { return int(a.y - b.y) })
		for i := 1; i < len(ends); i++ {
			if ends[i].y-ends[i-1].y < 13 {
				ends[i].y = ends[i-1].y + 13
			}
		}
		for _, e := range ends {
			fmt.Fprintf(b, `<text class="ink2" x="%.1f" y="%.1f">%s</text>`+"\n", px1+8, e.y+4, esc(e.name))
		}
	}
}

// BarChart is grouped bars: one group per category, one bar per series in
// each group, with a whisker from Lo to Hi when they differ from the value.
type BarChart struct {
	Title       string
	YLabel      string
	Height      int // default 260
	Categories  []string
	Series      []BarSeries
	YTickSuffix string
	// Label formats the value printed above each bar; nil prints none.
	Label func(series, category int, v float64) string
}

// BarSeries is one bar per category.
type BarSeries struct {
	Name   string
	Class  string
	Values []float64 // per category; NaN leaves the bar out
	Lo, Hi []float64 // optional spread per category
}

func (c BarChart) height() int {
	if c.Height == 0 {
		return 260
	}
	return c.Height
}

func (c BarChart) render(b *strings.Builder, x0, y0, w int) {
	h := c.height()
	px0, px1 := float64(x0+padL), float64(x0+w-24)
	py0, py1 := float64(y0+50), float64(y0+h-40)
	fmt.Fprintf(b, `<text class="ink" x="%d" y="%d" font-weight="600">%s</text>`+"\n", x0+16, y0+14, esc(c.Title))
	if len(c.Series) >= 2 {
		lx := float64(x0 + 16)
		for _, s := range c.Series {
			fmt.Fprintf(b, `<rect class="%s" x="%.1f" y="%d" width="12" height="12" rx="2"/>`, s.Class, lx, y0+21)
			fmt.Fprintf(b, `<text class="ink2" x="%.1f" y="%d">%s</text>`+"\n", lx+17, y0+31, esc(s.Name))
			lx += 17 + 7.2*float64(len([]rune(s.Name))) + 18
		}
	}
	ymax := 0.0
	for _, s := range c.Series {
		for i, v := range s.Values {
			if !math.IsNaN(v) {
				ymax = math.Max(ymax, v)
			}
			if i < len(s.Hi) && !math.IsNaN(s.Hi[i]) {
				ymax = math.Max(ymax, s.Hi[i])
			}
		}
	}
	if ymax == 0 {
		ymax = 1
	}
	yt, ymax := ticks(0, ymax*1.08, 5)
	sy := func(v float64) float64 { return py1 - v/ymax*(py1-py0) }
	for _, t := range yt {
		y := sy(t)
		fmt.Fprintf(b, `<line class="grid" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`, px0, y, px1, y)
		fmt.Fprintf(b, `<text class="muted" x="%.1f" y="%.1f" text-anchor="end">%s%s</text>`+"\n", px0-6, y+4, num(t), c.YTickSuffix)
	}
	if c.YLabel != "" {
		fmt.Fprintf(b, `<text class="muted" x="%.1f" y="%.1f" text-anchor="middle" transform="rotate(-90 %.1f %.1f)">%s</text>`+"\n",
			float64(x0+16), (py0+py1)/2, float64(x0+16), (py0+py1)/2, esc(c.YLabel))
	}
	n := len(c.Categories)
	gw := (px1 - px0) / float64(n)
	ns := float64(len(c.Series))
	bw := math.Min(28, (gw*0.7)/ns)
	for ci, cat := range c.Categories {
		gx := px0 + gw*float64(ci) + gw/2
		fmt.Fprintf(b, `<text class="ink2" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`+"\n", gx, py1+17, esc(cat))
		for si, s := range c.Series {
			if ci >= len(s.Values) || math.IsNaN(s.Values[ci]) {
				continue
			}
			v := s.Values[ci]
			bx := gx - bw*ns/2 - (ns - 1) + float64(si)*(bw+2)
			top := sy(v)
			// Rounded data end, square at the baseline.
			r := math.Min(4, bw/2)
			if py1-top < r {
				r = (py1 - top) / 2
			}
			fmt.Fprintf(b, `<path class="%s" d="M%.1f %.1f V%.1f Q%.1f %.1f %.1f %.1f H%.1f Q%.1f %.1f %.1f %.1f V%.1f Z"><title>%s, %s: %s</title></path>`+"\n",
				s.Class, bx, py1, top+r, bx, top, bx+r, top, bx+bw-r, bx+bw, top, bx+bw, top+r, py1,
				esc(s.Name), esc(cat), num(v))
			labelY := top - 4
			if ci < len(s.Lo) && ci < len(s.Hi) && !math.IsNaN(s.Lo[ci]) && (s.Lo[ci] != v || s.Hi[ci] != v) {
				cx := bx + bw/2
				lo, hi := sy(s.Lo[ci]), sy(s.Hi[ci])
				fmt.Fprintf(b, `<line class="whisker" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`, cx, lo, cx, hi)
				fmt.Fprintf(b, `<line class="whisker" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`, cx-3, hi, cx+3, hi)
				fmt.Fprintf(b, `<line class="whisker" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`+"\n", cx-3, lo, cx+3, lo)
				labelY = math.Min(labelY, hi-4)
			}
			if c.Label != nil {
				if l := c.Label(si, ci, v); l != "" {
					fmt.Fprintf(b, `<text class="ink2" x="%.1f" y="%.1f" text-anchor="middle" font-size="10">%s</text>`+"\n", bx+bw/2, labelY, esc(l))
				}
			}
		}
	}
	fmt.Fprintf(b, `<line class="axis" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`+"\n", px0, py1, px1, py1)
}

// ticks returns about n round tick values from lo up to past hi, and the
// last one, which becomes the axis maximum.
func ticks(lo, hi float64, n int) ([]float64, float64) {
	if hi <= lo {
		hi = lo + 1
	}
	raw := (hi - lo) / float64(n)
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	step := mag
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if m*mag >= raw {
			step = m * mag
			break
		}
	}
	var out []float64
	start := math.Floor(lo/step) * step
	for v := start; ; v += step {
		v = math.Round(v/step) * step
		out = append(out, v)
		if v >= hi-step*1e-9 {
			return out, v
		}
	}
}

// num formats a tick or value compactly: 1500 as 1.5k, 0.25 as 0.25.
func num(v float64) string {
	a := math.Abs(v)
	switch {
	case a >= 1e6:
		return trim(fmt.Sprintf("%.2f", v/1e6)) + "M"
	case a >= 1e4:
		return trim(fmt.Sprintf("%.1f", v/1e3)) + "k"
	case a >= 1000:
		return trim(fmt.Sprintf("%.2f", v/1e3)) + "k"
	case a >= 100 || v == math.Trunc(v):
		return fmt.Sprintf("%.0f", v)
	case a >= 1:
		return trim(fmt.Sprintf("%.2f", v))
	default:
		return trim(fmt.Sprintf("%.3f", v))
	}
}

func trim(s string) string {
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

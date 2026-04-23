package e2e_test

// svgchart.go – dependency-free SVG bar/line chart generator for HTML reports.
//
// Charts are fully self-contained SVG markup; no JavaScript or external CDN is
// required.  The generated SVG can be embedded directly in any HTML file.

import (
	"fmt"
	"math"
	"strings"
)

// ── types ──────────────────────────────────────────────────────────────────────

// SVGSeries describes one data series inside a chart.
type SVGSeries struct {
	Name  string
	Color string // CSS colour string, e.g. "#38bdf8"
	Data  []float64
}

// ── palette ───────────────────────────────────────────────────────────────────

var svgPalette = []string{
	"#38bdf8", // sky-400
	"#818cf8", // indigo-400
	"#f472b6", // pink-400
	"#4ade80", // green-400
	"#fb923c", // orange-400
	"#a78bfa", // violet-400
	"#facc15", // yellow-400
	"#f87171", // red-400
}

// PaletteColor returns a palette colour by index (wraps around).
func PaletteColor(i int) string { return svgPalette[i%len(svgPalette)] }

// ── helpers ───────────────────────────────────────────────────────────────────

func niceMax(v float64) float64 {
	if v <= 0 {
		return 1
	}
	exp := math.Pow(10, math.Floor(math.Log10(v)))
	norm := v / exp
	switch {
	case norm <= 1:
		norm = 1
	case norm <= 2:
		norm = 2
	case norm <= 5:
		norm = 5
	default:
		norm = 10
	}
	return norm * exp
}

func fmtVal(v float64) string {
	if v >= 1000 {
		return fmt.Sprintf("%.0f", v)
	}
	if v >= 100 {
		return fmt.Sprintf("%.1f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

// ── SVGBarChart ───────────────────────────────────────────────────────────────

// SVGBarChart generates a grouped bar chart and returns the SVG markup.
//
// Parameters:
//
//	title   – chart title shown at the top
//	labels  – x-axis category labels (one per data point)
//	series  – one or more data series; colours are taken from SVGSeries.Color
//	          (leave empty to use the default palette)
//	yUnit   – unit string appended to y-axis tick labels, e.g. "ms" or "MB"
func SVGBarChart(title string, labels []string, series []SVGSeries, yUnit string) string {
	// ── layout constants ──────────────────────────────────────────────────────
	const (
		svgW      = 560
		svgH      = 320
		padLeft   = 60
		padRight  = 20
		padTop    = 44
		padBottom = 60
		legendH   = 20
		tickCount = 5
	)

	n := len(labels)
	if n == 0 || len(series) == 0 {
		return "<svg></svg>"
	}

	// Assign default colours.
	for i := range series {
		if series[i].Color == "" {
			series[i].Color = PaletteColor(i)
		}
	}

	// ── find max value ────────────────────────────────────────────────────────
	maxVal := 0.0
	for _, s := range series {
		for _, v := range s.Data {
			if v > maxVal {
				maxVal = v
			}
		}
	}
	yMax := niceMax(maxVal * 1.05)

	// ── geometry ──────────────────────────────────────────────────────────────
	plotW := float64(svgW - padLeft - padRight)
	plotH := float64(svgH - padTop - padBottom)

	groupW := plotW / float64(n)
	numSeries := len(series)
	barPad := groupW * 0.15
	barW := (groupW - barPad*2) / float64(numSeries)
	if barW < 3 {
		barW = 3
	}

	var b strings.Builder

	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" `+
		`width="%d" height="%d" style="background:#1e293b;border-radius:8px;overflow:visible">`,
		svgW, svgH, svgW, svgH)

	// ── title ─────────────────────────────────────────────────────────────────
	fmt.Fprintf(&b, `<text x="%d" y="20" text-anchor="middle" `+
		`font-family="system-ui,sans-serif" font-size="13" font-weight="600" fill="#e2e8f0">%s</text>`,
		svgW/2, escapeXML(title))

	// ── y-axis grid lines & ticks ─────────────────────────────────────────────
	for i := 0; i <= tickCount; i++ {
		frac := float64(i) / float64(tickCount)
		yVal := yMax * frac
		yPx := float64(padTop) + plotH - frac*plotH

		// grid line
		fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" `+
			`stroke="#334155" stroke-width="1"/>`,
			padLeft, yPx, svgW-padRight, yPx)

		// tick label
		lbl := fmtVal(yVal)
		if yUnit != "" {
			lbl += yUnit
		}
		fmt.Fprintf(&b, `<text x="%d" y="%.1f" text-anchor="end" dominant-baseline="middle" `+
			`font-family="system-ui,sans-serif" font-size="10" fill="#64748b">%s</text>`,
			padLeft-4, yPx, escapeXML(lbl))
	}

	// ── bars ──────────────────────────────────────────────────────────────────
	for gi, lbl := range labels {
		groupX := float64(padLeft) + float64(gi)*groupW + barPad

		for si, s := range series {
			if gi >= len(s.Data) {
				continue
			}
			v := s.Data[gi]

			bH := (v / yMax) * plotH
			if bH < 0 {
				bH = 0
			}
			bX := groupX + float64(si)*barW
			bY := float64(padTop) + plotH - bH

			col := s.Color
			fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" `+
				`fill="%s" fill-opacity="0.8" rx="2"/>`,
				bX, bY, barW-1, bH, col)

			// value label on top of bar
			if bH > 14 {
				fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle" `+
					`font-family="system-ui,sans-serif" font-size="8" fill="#e2e8f0">%s</text>`,
					bX+barW/2-0.5, bY+10, fmtVal(v))
			}
		}

		// x-axis label (rotated if too wide)
		lblX := float64(padLeft) + float64(gi)*groupW + groupW/2
		lblY := float64(padTop) + plotH + 14
		short := lbl
		if len(short) > 18 {
			short = short[:16] + "…"
		}
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle" `+
			`transform="rotate(-30,%.1f,%.1f)" `+
			`font-family="system-ui,sans-serif" font-size="9" fill="#94a3b8">%s</text>`,
			lblX, lblY, lblX, lblY, escapeXML(short))
	}

	// ── legend ────────────────────────────────────────────────────────────────
	if numSeries > 1 {
		legendY := float64(svgH) - 12
		totalLegW := 0.0
		for _, s := range series {
			totalLegW += float64(len(s.Name))*6.5 + 24
		}
		lx := (float64(svgW) - totalLegW) / 2
		for _, s := range series {
			fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="12" height="10" fill="%s" rx="2"/>`,
				lx, legendY-8, s.Color)
			fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-family="system-ui,sans-serif" `+
				`font-size="10" fill="#cbd5e1">%s</text>`,
				lx+14, legendY, escapeXML(s.Name))
			lx += float64(len(s.Name))*6.5 + 24
		}
	}

	// ── axes ──────────────────────────────────────────────────────────────────
	// left axis
	fmt.Fprintf(&b, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#475569" stroke-width="1"/>`,
		padLeft, padTop, padLeft, padTop+int(plotH))
	// bottom axis
	fmt.Fprintf(&b, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#475569" stroke-width="1"/>`,
		padLeft, padTop+int(plotH), svgW-padRight, padTop+int(plotH))

	b.WriteString(`</svg>`)
	return b.String()
}

// ── SVGLineChart ──────────────────────────────────────────────────────────────

// SVGLineChart generates a multi-line chart and returns the SVG markup.
func SVGLineChart(title string, labels []string, series []SVGSeries, yUnit string) string {
	const (
		svgW      = 560
		svgH      = 320
		padLeft   = 60
		padRight  = 20
		padTop    = 44
		padBottom = 60
		tickCount = 5
	)

	n := len(labels)
	if n == 0 || len(series) == 0 {
		return "<svg></svg>"
	}

	for i := range series {
		if series[i].Color == "" {
			series[i].Color = PaletteColor(i)
		}
	}

	maxVal := 0.0
	for _, s := range series {
		for _, v := range s.Data {
			if v > maxVal {
				maxVal = v
			}
		}
	}
	yMax := niceMax(maxVal * 1.05)

	plotW := float64(svgW - padLeft - padRight)
	plotH := float64(svgH - padTop - padBottom)

	toX := func(i int) float64 {
		if n <= 1 {
			return float64(padLeft) + plotW/2
		}
		return float64(padLeft) + float64(i)/float64(n-1)*plotW
	}
	toY := func(v float64) float64 {
		return float64(padTop) + plotH - (v/yMax)*plotH
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" `+
		`width="%d" height="%d" style="background:#1e293b;border-radius:8px;overflow:visible">`,
		svgW, svgH, svgW, svgH)

	fmt.Fprintf(&b, `<text x="%d" y="20" text-anchor="middle" `+
		`font-family="system-ui,sans-serif" font-size="13" font-weight="600" fill="#e2e8f0">%s</text>`,
		svgW/2, escapeXML(title))

	for i := 0; i <= tickCount; i++ {
		frac := float64(i) / float64(tickCount)
		yVal := yMax * frac
		yPx := float64(padTop) + plotH - frac*plotH
		fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" stroke="#334155" stroke-width="1"/>`,
			padLeft, yPx, svgW-padRight, yPx)
		lbl := fmtVal(yVal)
		if yUnit != "" {
			lbl += yUnit
		}
		fmt.Fprintf(&b, `<text x="%d" y="%.1f" text-anchor="end" dominant-baseline="middle" `+
			`font-family="system-ui,sans-serif" font-size="10" fill="#64748b">%s</text>`,
			padLeft-4, yPx, escapeXML(lbl))
	}

	// x-axis labels
	for i, lbl := range labels {
		lx := toX(i)
		ly := float64(padTop) + plotH + 14
		short := lbl
		if len(short) > 12 {
			short = short[:10] + "…"
		}
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle" `+
			`transform="rotate(-30,%.1f,%.1f)" `+
			`font-family="system-ui,sans-serif" font-size="9" fill="#94a3b8">%s</text>`,
			lx, ly, lx, ly, escapeXML(short))
	}

	// lines + dots
	for _, s := range series {
		if len(s.Data) == 0 {
			continue
		}
		var pts []string
		for i, v := range s.Data {
			if i >= n {
				break
			}
			pts = append(pts, fmt.Sprintf("%.1f,%.1f", toX(i), toY(v)))
		}
		fmt.Fprintf(&b, `<polyline points="%s" fill="none" stroke="%s" stroke-width="2"/>`,
			strings.Join(pts, " "), s.Color)
		for i, v := range s.Data {
			if i >= n {
				break
			}
			fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="4" fill="%s"/>`, toX(i), toY(v), s.Color)
		}
	}

	// legend
	if len(series) > 1 {
		legendY := float64(svgH) - 12
		totalW := 0.0
		for _, s := range series {
			totalW += float64(len(s.Name))*6.5 + 24
		}
		lx := (float64(svgW) - totalW) / 2
		for _, s := range series {
			fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="12" height="3" fill="%s" rx="1"/>`,
				lx, legendY-4, s.Color)
			fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-family="system-ui,sans-serif" `+
				`font-size="10" fill="#cbd5e1">%s</text>`,
				lx+14, legendY, escapeXML(s.Name))
			lx += float64(len(s.Name))*6.5 + 24
		}
	}

	fmt.Fprintf(&b, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#475569" stroke-width="1"/>`,
		padLeft, padTop, padLeft, padTop+int(plotH))
	fmt.Fprintf(&b, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#475569" stroke-width="1"/>`,
		padLeft, padTop+int(plotH), svgW-padRight, padTop+int(plotH))

	b.WriteString(`</svg>`)
	return b.String()
}

func escapeXML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return s
}

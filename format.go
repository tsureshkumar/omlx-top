package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

var (
	cDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	cHead   = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	cTitle  = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("39")).Bold(true)
	cGreen  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	cYellow = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	cRed    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	cBlue   = lipgloss.NewStyle().Foreground(lipgloss.Color("75"))
	cMag    = lipgloss.NewStyle().Foreground(lipgloss.Color("170"))
	cBold   = lipgloss.NewStyle().Bold(true)
	cSel    = lipgloss.NewStyle().Reverse(true)
	cColHdr = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Underline(true)
)

func badge(bg, text string) string {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color(bg)).Bold(true).Render(text)
}

// fit pads or truncates s to exactly w cells.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) > w {
		s = ansi.Truncate(s, w, "…")
	}
	if pw := w - lipgloss.Width(s); pw > 0 {
		s += strings.Repeat(" ", pw)
	}
	return s
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return string(r[:n-1]) + "…"
}

// modelName shortens a model id from the left, where the shared org prefix is.
func modelName(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	if _, after, ok := strings.Cut(s, "--"); ok {
		s = after
	}
	return trunc(s, n)
}

func lvlColor(p float64) lipgloss.Style {
	switch {
	case p >= 90:
		return cRed
	case p >= 75:
		return cYellow
	}
	return cGreen
}

func bar(frac float64, w int, st lipgloss.Style) string {
	if w < 1 {
		return ""
	}
	frac = min(1, max(0, frac))
	full := frac * float64(w)
	n := int(full)
	parts := []string{"", "▏", "▎", "▍", "▌", "▋", "▊", "▉"}
	s := strings.Repeat("█", n)
	rest := w - n
	if rest > 0 {
		if p := int((full - float64(n)) * 8); p > 0 {
			s += parts[p]
			rest--
		}
	}
	return st.Render(s) + cDim.Render(strings.Repeat("·", rest))
}

func meter(label string, pct float64, w int, extra string) string {
	if pct < 0 {
		return cBold.Render(label) + cDim.Render(" n/a ") + extra
	}
	st := lvlColor(pct)
	return cBold.Render(label) + "[" + bar(pct/100, w, st) + "]" + st.Render(fmt.Sprintf("%3.0f%%", pct)) + " " + extra
}

// spark draws the last w values scaled to top (or to their maximum when top is 0).
// Negative values mean "no data".
func spark(v []float64, w int, top float64) string {
	if w < 1 {
		return ""
	}
	if len(v) > w {
		v = v[len(v)-w:]
	}
	if top <= 0 {
		for _, x := range v {
			top = max(top, x)
		}
	}
	levels := []rune("▁▂▃▄▅▆▇█")
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", w-len(v)))
	for _, x := range v {
		switch {
		case x < 0:
			b.WriteRune(' ')
		case x == 0 || top <= 0:
			b.WriteRune('▁')
		default:
			// Any activity at all is drawn above the zero line.
			i := min(7, max(1, int(x/top*7+0.5)))
			b.WriteRune(levels[i])
		}
	}
	return b.String()
}

func fmtBytes(b float64) string {
	switch {
	case b >= 1<<40:
		return fmt.Sprintf("%.1fT", b/(1<<40))
	case b >= 1<<30:
		return fmt.Sprintf("%.1fG", b/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0fM", b/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0fK", b/(1<<10))
	}
	return fmt.Sprintf("%.0fB", b)
}

func fmtDur(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds())
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func fmtNum(n float64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.1fB", n/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1fM", n/1e6)
	case n >= 1e4:
		return fmt.Sprintf("%.1fk", n/1e3)
	}
	return fmt.Sprintf("%.0f", n)
}

// opt formats v, or "-" when it is zero (unknown).
func opt(v float64, f func(float64) string) string {
	if v <= 0 {
		return "-"
	}
	return f(v)
}

func fmtSec(f float64) string  { return fmtDur(secs(f)) }
func fmtRate(f float64) string { return fmt.Sprintf("%.1f", f) }

func stamp(t, now time.Time) string {
	if now.Sub(t) > 24*time.Hour {
		return t.Format("Jan02 15:04")
	}
	return t.Format("15:04:05")
}

func section(title string, w int, right string) string {
	t := cHead.Render("─ " + title + " ")
	r := ""
	if right != "" {
		r = " " + cDim.Render(trunc(right, max(0, w-lipgloss.Width(t)-6))) + " "
	}
	fill := max(0, w-lipgloss.Width(t)-lipgloss.Width(r)-1)
	return t + cDim.Render(strings.Repeat("─", fill)) + r + cDim.Render("─")
}

package main

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	vOverview = iota
	vRequests
	vModels
	vCache
	vEvents
	vUsage
	nViews
)

var viewNames = [nViews]string{"overview", "requests", "models", "cache", "events", "usage"}

type tickMsg time.Time

type model struct {
	st     *State
	every  time.Duration
	w, h   int
	view   int
	sel    int // selected row or scroll offset in the current view
	week   bool
	help   bool
	paused bool
	frozen string
}

func (m model) Init() tea.Cmd { return tick(m.every) }

func tick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// rows is the number of selectable rows in the current view.
func (m model) rows() int {
	m.st.mu.RLock()
	defer m.st.mu.RUnlock()
	switch m.view {
	case vOverview:
		return len(Analyze(m.st, time.Now()))
	case vRequests:
		return len(m.st.Recent)
	case vModels:
		return len(m.st.Models)
	case vEvents:
		return len(m.st.Events)
	}
	return 0
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case tickMsg:
		return m, tick(m.every)
	case tea.KeyMsg:
		switch k := msg.String(); k {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "esc":
			if m.help {
				m.help = false
			} else {
				m.view, m.sel = vOverview, 0
			}
		case "1", "2", "3", "4", "5", "6":
			m.view, m.sel, m.help = int(k[0]-'1'), 0, false
		case "tab", "right", "l":
			m.view, m.sel = (m.view+1)%nViews, 0
		case "shift+tab", "left", "h":
			m.view, m.sel = (m.view+nViews-1)%nViews, 0
		case "j", "down":
			m.sel = min(m.sel+1, max(0, m.rows()-1))
		case "k", "up":
			m.sel = max(m.sel-1, 0)
		case "pgdown", "ctrl+d":
			m.sel = min(m.sel+10, max(0, m.rows()-1))
		case "pgup", "ctrl+u":
			m.sel = max(m.sel-10, 0)
		case "g", "home":
			m.sel = 0
		case "u":
			m.week = !m.week
		case "r":
			m.st.reset()
			m.sel = 0
		case "p", " ":
			m.paused = !m.paused
		case "?":
			m.help = !m.help
		}
	}
	// A paused screen is redrawn only when the user does something.
	if _, tickOnly := msg.(tickMsg); m.paused && !tickOnly {
		m.frozen = m.render()
	}
	return m, nil
}

func (m model) View() string {
	if m.paused && m.frozen != "" {
		return m.frozen
	}
	return m.render()
}

// page collects exactly-sized lines.
type page struct {
	w, h  int
	lines []string
}

func (p *page) add(s ...string) {
	for _, x := range s {
		if len(p.lines) < p.h {
			p.lines = append(p.lines, fit(x, p.w))
		}
	}
}

func (p *page) room() int { return p.h - len(p.lines) }

func (m model) render() string {
	w, h := m.w, m.h
	if w <= 0 || h <= 0 {
		w, h = 120, 40
	}
	if m.help {
		return helpText(w, h)
	}
	m.st.mu.RLock()
	defer m.st.mu.RUnlock()
	now := time.Now()
	st := m.st
	findings := Analyze(st, now)

	p := &page{w: w, h: h - 1} // last line is the footer
	m.header(p, st, now)
	m.system(p, st, now)
	p.add(tabs(m.view, findings))
	switch m.view {
	case vOverview:
		m.overview(p, st, findings, now)
	case vRequests:
		m.requests(p, st, now)
	case vModels:
		m.models(p, st, now)
	case vCache:
		m.cache(p, st, now)
	case vEvents:
		m.events(p, st, now)
	case vUsage:
		m.usage(p, st)
	}
	for p.room() > 0 {
		p.add("")
	}
	foot := " q quit  1-6/tab views  j/k scroll  p pause  r reset  u usage range  ? help"
	return strings.Join(append(p.lines, fit(cDim.Render(foot), w)), "\n")
}

func tabs(cur int, f []Finding) string {
	var out []string
	for i, n := range viewNames {
		label := fmt.Sprintf(" %d %s ", i+1, n)
		if i == cur {
			out = append(out, cTitle.Render(label))
		} else {
			out = append(out, cDim.Render(label))
		}
	}
	var crit, warn int
	for _, x := range f {
		switch x.Sev {
		case sevCrit:
			crit++
		case sevWarn:
			warn++
		}
	}
	s := strings.Join(out, " ")
	if crit > 0 {
		s += "  " + badge("196", fmt.Sprintf(" %d CRITICAL ", crit))
	}
	if warn > 0 {
		s += "  " + badge("220", fmt.Sprintf(" %d WARNING ", warn))
	}
	return s
}

func (m model) header(p *page, st *State, now time.Time) {
	status := cDim.Render("○ connecting")
	switch {
	case st.Up && st.Health != nil && st.Health.Status == "loading":
		status = cYellow.Render("● loading")
	case st.Up:
		status = cGreen.Render("● up")
	case st.NotOMLX:
		status = cRed.Render("● not oMLX")
	case st.UpErr != "":
		status = cRed.Render("● down")
	}
	ver := ""
	if st.Version != "" {
		ver = " " + st.Version
	}
	hdr := cTitle.Render(" omlx-top ") + " " + cBold.Render("oMLX"+ver) + " " + status + " " + cDim.Render(st.Cfg.URL)
	if st.Up {
		hdr += cDim.Render(" api " + fmtDur(st.APILat))
		if c := st.snapshot(); c != nil {
			hdr += cDim.Render("  uptime " + fmtSec(c.Uptime))
		}
		switch {
		case st.AdminOK:
			hdr += cDim.Render("  data ") + cGreen.Render("full")
		case st.Status != nil:
			hdr += cDim.Render("  data ") + cYellow.Render("basic")
		case st.AccessErr != "":
			hdr += cDim.Render("  data ") + cYellow.Render("health+log only")
		}
	}
	clock := now.Format("15:04:05")
	if m.paused {
		clock = cYellow.Render("PAUSED ") + clock
	}
	pad := max(1, p.w-lipgloss.Width(hdr)-lipgloss.Width(clock))
	p.add(hdr + strings.Repeat(" ", pad) + clock)
}

func (m model) system(p *page, st *State, now time.Time) {
	s := st.Sys
	bw := min(24, max(6, (p.w-92)/4))
	memPct, cpu, gpu := -1.0, -1.0, -1.0
	if s.MemTotal > 0 {
		memPct = 100 * s.MemUsed / s.MemTotal
	}
	if s.OK {
		cpu, gpu = s.CPU, s.GPUUtil
	}
	used, ceil := st.omlxMemory()
	omPct := -1.0
	omText := ""
	if ceil > 0 {
		omPct = 100 * used / ceil
		omText = cDim.Render(fmtBytes(used) + "/" + fmtBytes(ceil))
		if a := st.Activity; a != nil && a.Pressure.Level != "" {
			c := cGreen
			switch a.Pressure.Level {
			case "ok":
			case "hard":
				c = cRed
			default:
				c = cYellow
			}
			omText += " " + c.Render(a.Pressure.Level)
		}
	}
	meters := []string{
		meter("CPU", cpu, bw, ""),
		meter("GPU", gpu, bw, ""),
		meter("RAM", memPct, bw, cDim.Render(fmtBytes(s.MemUsed)+"/"+fmtBytes(s.MemTotal))),
		meter("oMLX", omPct, bw, omText),
	}
	if p.w >= 100 {
		p.add(strings.Join(meters, " "))
	} else {
		p.add(meters[0]+" "+meters[1], meters[2], meters[3])
	}
	swap := fmtBytes(s.SwapUsed) + "/" + fmtBytes(s.SwapTotal)
	if s.SwapOutRate >= swapWarnRate {
		swap = cRed.Render(swap + " out " + fmtBytes(s.SwapOutRate) + "/s")
	} else if s.SwapInRate >= swapWarnRate {
		swap = cYellow.Render(swap + " in " + fmtBytes(s.SwapInRate) + "/s")
	}
	l := cDim.Render("swap ") + swap + cDim.Render("  wired ") + fmtBytes(s.Wired) + cDim.Render("  compressed ") + fmtBytes(s.Compress) +
		cDim.Render("  load ") + fmt.Sprintf("%.1f %.1f %.1f", s.Load[0], s.Load[1], s.Load[2])
	if s.GPUMem > 0 {
		l += cDim.Render("  gpu mem ") + fmtBytes(s.GPUMem)
	}
	if len(s.OMLX) > 0 {
		l += cDim.Render("  omlx procs ") + fmt.Sprintf("%d, cpu %.0f%%, rss %s", len(s.OMLX), s.OMLXCPU, fmtBytes(s.OMLXRSS))
	}
	if pw := st.Power; now.Sub(pw.T) < 15*time.Second {
		l += cDim.Render("  gpu ") + fmt.Sprintf("%.0f°C %.1fW", pw.GPUTemp, pw.GPUWatts)
		if pw.GPUMHz > 0 {
			l += fmt.Sprintf(" %.0fMHz", pw.GPUMHz)
		}
		if pw.SysWatts > 0 {
			l += cDim.Render("  sys ") + fmt.Sprintf("%.0fW", pw.SysWatts)
		}
	}
	if s.Chip != "" {
		l += cDim.Render("  " + s.Chip)
	}
	if s.Err != "" {
		l += "  " + cRed.Render(s.Err)
	}
	p.add(l)
}

func helpText(w, h int) string {
	lines := []string{
		cTitle.Render(" omlx-top help "),
		"",
		cHead.Render("Keys"),
		"  q / ctrl-c      quit                    p / space   pause screen",
		"  1-6 tab ← →     switch view             j/k ↑/↓     select or scroll (pgup/pgdn, g = top)",
		"  r               reset local history     u           usage view: today / 7 days",
		"  ?               toggle this help        esc         back to overview",
		"",
		cHead.Render("Views"),
		"  overview   bottlenecks with evidence and a suggested fix, live requests, throughput, where time goes",
		"  requests   in-flight requests and finished ones: queue wait, time to first token, speed, cache reuse",
		"  models     discovered and loaded models, size, context, idle/TTL, load times",
		"  cache      SSD and hot (RAM) KV cache tiers and per-model counters",
		"  events     warnings, errors, loads, evictions and memory events from the server log",
		"  usage      stored hourly history (today or 7 days)",
		"",
		cHead.Render("Reading the numbers"),
		"  TTFT       time to first token; includes queueing and prefill",
		"  QUEUE/PRE  observed by polling, so only for requests seen while omlx-top runs",
		"  data full  admin API reachable.  basic: only totals (no per-request detail)",
		"",
		cHead.Render("Sources"),
		"  /health, /api/status, /v1/models/status, /admin/api/{activity,stats,usage} (all read-only GETs),",
		"  the server log, vm_stat/sysctl/ioreg/ps, and macmon when installed",
		"",
		cDim.Render("press ? or esc to close"),
	}
	for i := range lines {
		lines[i] = fit(lines[i], w)
	}
	if h > 0 && len(lines) > h {
		lines = lines[:h]
	}
	return strings.Join(lines, "\n")
}

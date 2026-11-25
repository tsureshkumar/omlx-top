package main

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

func sevBadge(sev int) string {
	switch sev {
	case sevCrit:
		return badge("196", " CRIT ")
	case sevWarn:
		return badge("220", " WARN ")
	}
	return badge("75", " INFO ")
}

// scroll returns the slice bounds that keep row sel visible among n rows.
func scroll(sel, n, rows int) (from, to int) {
	if rows < 1 || n == 0 {
		return 0, 0
	}
	sel = min(max(sel, 0), n-1)
	from = max(0, min(sel-rows+1, n-rows))
	return from, min(n, from+rows)
}

// ---------- overview ----------

func (m model) overview(p *page, st *State, f []Finding, now time.Time) {
	m.bottlenecks(p, st, f, max(4, p.room()/3))
	m.live(p, st, now, max(3, p.room()/3))
	m.throughput(p, st)
	m.timeGoes(p, st)
	if p.room() >= 3 {
		recentTable(p, st, now, p.room(), 0, false)
	}
}

func (m model) bottlenecks(p *page, st *State, f []Finding, rows int) {
	p.add(section("BOTTLENECKS", p.w, "j/k: select for details"))
	if len(f) == 0 {
		switch {
		case !st.Up:
			p.add(cDim.Render("  waiting for the server…"))
		case !st.AdminOK:
			p.add(cDim.Render("  nothing detected, but without admin data many checks cannot run"))
		default:
			p.add(cGreen.Render("  ✓ no bottleneck detected"))
		}
		return
	}
	sel := min(m.sel, len(f)-1)
	if m.view != vOverview {
		sel = 0
	}
	// The selected finding is expanded with its fix.
	extra := 0
	if f[sel].Fix != "" {
		extra = 1
	}
	from, to := scroll(sel, len(f), max(1, rows-extra-1))
	for i := from; i < to; i++ {
		x := f[i]
		line := fmt.Sprintf(" %s %s %s  %s", sevBadge(x.Sev), cDim.Render(fmt.Sprintf("%-8s", x.Area)), cBold.Render(x.Title), cDim.Render(x.Detail))
		if i == sel {
			line = fmt.Sprintf("▸%s %s %s  %s", sevBadge(x.Sev), cDim.Render(fmt.Sprintf("%-8s", x.Area)), cBold.Render(x.Title), x.Detail)
		}
		p.add(line)
		if i == sel && x.Fix != "" {
			p.add("                 " + cYellow.Render("→ "+x.Fix))
		}
	}
	if hidden := len(f) - (to - from); hidden > 0 {
		p.add(cDim.Render(fmt.Sprintf("  … %d more", hidden)))
	}
}

func liveList(st *State) []*LiveReq {
	var l []*LiveReq
	for _, r := range st.Live {
		l = append(l, r)
	}
	order := map[string]int{"generate": 0, "prefill": 1, "queued": 2}
	slices.SortFunc(l, func(a, b *LiveReq) int {
		return cmp.Or(order[a.Phase]-order[b.Phase], a.FirstSeen.Compare(b.FirstSeen), cmp.Compare(a.QueuePos, b.QueuePos), strings.Compare(a.ID, b.ID))
	})
	return l
}

func liveLine(r *LiveReq, w int, now time.Time) string {
	nw := min(34, max(18, w/4))
	name := cBold.Render(fit(modelName(r.Model, nw), nw))
	id := cDim.Render(fit(short(r.ID), 8))
	bw := min(30, max(8, w-nw-80))
	switch r.Phase {
	case "prefill":
		frac := 0.0
		if r.Total > 0 {
			frac = r.Processed / r.Total
		}
		extra := ""
		if r.ETA > 0 {
			extra += "  eta " + fmtSec(r.ETA)
		}
		if r.CachedTok > 0 {
			extra += cGreen.Render("  cached " + fmtNum(r.CachedTok))
		}
		if r.Detail != "" {
			extra += cDim.Render("  " + r.Detail)
		}
		if idle := now.Sub(r.Progress); idle > 10*time.Second {
			extra += cRed.Render("  no progress " + fmtDur(idle))
		}
		return fmt.Sprintf(" %s %s %s [%s] %s %s/%s  %s%s", badge("220", " PREFILL  "), name, id, bar(frac, bw, cYellow),
			cYellow.Render(fmt.Sprintf("%3.0f%%", 100*frac)), fmtNum(r.Processed), fmtNum(r.Total),
			cBold.Render(fmt.Sprintf("%.0f tok/s", r.PreTPS)), extra)
	case "generate":
		prog := strings.Repeat(" ", bw+2) + "    "
		if r.MaxTok > 0 {
			frac := r.OutTok / r.MaxTok
			prog = "[" + bar(frac, bw, cGreen) + "] " + cDim.Render(fmt.Sprintf("%3.0f%%", 100*min(1, frac)))
		}
		extra := cDim.Render("  prompt " + opt(r.PromptTok, fmtNum))
		if r.CachedTok > 0 {
			extra += cGreen.Render("  cached " + fmtNum(r.CachedTok))
		}
		extra += cDim.Render("  " + fmtSec(r.GenSec))
		if r.IdleAge >= 3 {
			extra += cRed.Render("  no token " + fmtSec(r.IdleAge))
		}
		return fmt.Sprintf(" %s %s %s %s %s out  %s%s", badge("42", " GENERATE "), name, id, prog, fmtNum(r.OutTok),
			cGreen.Bold(true).Render(fmt.Sprintf("%.1f tok/s", r.TPS)), extra)
	}
	return fmt.Sprintf(" %s %s %s %s", badge("170", " QUEUED   "), name, id,
		cDim.Render(fmt.Sprintf("position %.0f  waiting %s  prompt %s", r.QueuePos, fmtSec(r.QueueSec), opt(r.PromptTok, fmtNum))))
}

func (m model) live(p *page, st *State, now time.Time, rows int) {
	_, _, active, waiting, _ := st.liveTotals()
	right := fmt.Sprintf("%d active, %d waiting", active, waiting)
	if mx := st.Cfg.MaxConcurrent; mx > 0 {
		right += fmt.Sprintf(" / %d slots", mx)
	}
	p.add(section("LIVE REQUESTS", p.w, right))
	n := 0
	if a := st.Activity; a != nil {
		for _, am := range a.Models {
			if !am.Loading {
				continue
			}
			line := fmt.Sprintf(" %s %s %s", badge("75", " LOADING  "), cBold.Render(am.ID), cDim.Render("for "+fmtSec(am.LoadElapsed)))
			if am.LoadEstimate > 0 {
				line += " [" + bar(min(0.99, am.LoadElapsed/am.LoadEstimate), 20, cBlue) + "] " + cDim.Render("estimated "+fmtSec(am.LoadEstimate))
			}
			p.add(line)
			n++
		}
	}
	l := liveList(st)
	for i, r := range l {
		if n >= rows-1 && len(l)-i > 1 {
			p.add(cDim.Render(fmt.Sprintf("  … %d more", len(l)-i)))
			return
		}
		p.add(liveLine(r, p.w, now))
		n++
	}
	if n > 0 {
		return
	}
	switch {
	case !st.Up:
		p.add(" " + badge("196", " DOWN "))
	case st.AdminOK:
		var loaded []string
		if st.Activity != nil {
			for _, am := range st.Activity.Models {
				loaded = append(loaded, am.ID)
			}
		}
		msg := "no models loaded"
		if len(loaded) > 0 {
			msg = "warm: " + strings.Join(loaded, ", ")
		}
		p.add(" " + badge("244", " IDLE ") + " " + cDim.Render(msg))
	case active+waiting > 0:
		p.add(cDim.Render("  requests are running; per-request detail needs admin access"))
	default:
		p.add(cDim.Render("  no requests"))
	}
}

func series(st *State, f func(Sample) float64) (v []float64, cur, peak float64) {
	v = make([]float64, len(st.History))
	for i, s := range st.History {
		v[i] = f(s)
		peak = max(peak, v[i])
	}
	if len(v) > 0 {
		cur = v[len(v)-1]
	}
	return
}

func (m model) throughput(p *page, st *State) {
	span := ""
	sw := max(10, p.w-52)
	if n := len(st.History); n > 1 {
		span = "last " + fmtDur(st.History[n-1].T.Sub(st.History[max(0, n-sw)].T))
	}
	p.add(section("THROUGHPUT", p.w, span))
	row := func(label string, style lipgloss.Style, f func(Sample) float64, top float64, format func(cur, peak float64) string) {
		v, cur, peak := series(st, f)
		p.add(fmt.Sprintf(" %s %s %s", cBold.Render(fit(label, 12)), style.Render(spark(v, sw, top)), format(cur, peak)))
	}
	rate := func(cur, peak float64) string {
		return fmt.Sprintf("%7.1f tok/s %s", cur, cDim.Render(fmt.Sprintf("peak %.0f", peak)))
	}
	row("decode", cGreen, func(s Sample) float64 { return s.DecTPS }, 0, rate)
	row("prefill", cYellow, func(s Sample) float64 { return s.PreTPS }, 0, rate)
	row("requests", cMag, func(s Sample) float64 { return float64(s.Active + s.Waiting) }, 0, func(cur, peak float64) string {
		return fmt.Sprintf("%7.0f       %s", cur, cDim.Render(fmt.Sprintf("peak %.0f", peak)))
	})
	row("gpu", cBlue, func(s Sample) float64 { return s.GPU }, 100, func(cur, _ float64) string {
		return fmt.Sprintf("%7s", pctOrDash(cur))
	})
	if _, ceil := st.omlxMemory(); ceil > 0 {
		row("oMLX mem", cHead, func(s Sample) float64 { return s.OMLXMem }, ceil, func(cur, peak float64) string {
			return fmt.Sprintf("%7s       %s", opt(cur, fmtBytes), cDim.Render("peak "+opt(peak, fmtBytes)))
		})
	}
}

func (m model) timeGoes(p *page, st *State) {
	pre, gen, reqs, scope := st.timeShare()
	p.add(section("WHERE TIME GOES", p.w, scope))
	tot := pre + gen
	if tot <= 0 {
		p.add(cDim.Render("  no completed requests yet"))
		return
	}
	bw := max(1, min(100, p.w-4))
	n := int(pre/tot*float64(bw) + 0.5)
	p.add("  " + cYellow.Render(strings.Repeat("█", n)) + cGreen.Render(strings.Repeat("█", bw-n)))
	l := "  " + cYellow.Render("■ first token ") + fmt.Sprintf("%s %.0f%%", fmtSec(pre), 100*pre/tot) +
		"   " + cGreen.Render("■ generation ") + fmt.Sprintf("%s %.0f%%", fmtSec(gen), 100*gen/tot) + cDim.Render("   requests ") + fmtNum(reqs)
	if c := st.snapshot(); c != nil && c.Requests > 0 {
		l += cDim.Render("   session avg: prefill ") + fmt.Sprintf("%.0f tok/s", c.PrefillTPS) + cDim.Render("  decode ") +
			fmt.Sprintf("%.1f tok/s", c.DecodeTPS) + cDim.Render("  cache reuse ") + fmt.Sprintf("%.0f%%", c.CacheEff)
	}
	p.add(l)
}

// ---------- requests ----------

func recentTable(p *page, st *State, now time.Time, rows, sel int, selectable bool) {
	p.add(section(fmt.Sprintf("FINISHED REQUESTS (%d)", len(st.Recent)), p.w, "newest first"))
	nw := min(40, max(16, p.w-92))
	p.add(cColHdr.Render(fit(fmt.Sprintf(" %-11s %-5s %s %7s %7s %5s %6s %7s %7s %7s %7s %7s", "TIME", "API", fit("MODEL", nw),
		"PROMPT", "CACHED", "REUSE", "OUT", "QUEUE", "TTFT", "TOTAL", "TOK/S", "FINISH"), p.w)))
	if len(st.Recent) == 0 {
		p.add(cDim.Render(" none yet"))
		return
	}
	n := len(st.Recent)
	from, to := scroll(sel, n, rows-2)
	for i := from; i < to; i++ {
		r := st.Recent[n-1-i]
		reuse := "-"
		if r.PromptTok > 0 && r.CachedTok > 0 {
			reuse = fmt.Sprintf("%.0f%%", 100*min(1, r.CachedTok/r.PromptTok))
		}
		ttft := opt(r.TTFT, fmtSec)
		if r.TTFT == 0 && r.PreSec > 0 {
			ttft = "~" + fmtSec(r.QueueSec+r.PreSec)
		}
		finish := r.Finish
		if !r.Logged {
			finish = "?"
		}
		line := fmt.Sprintf(" %-11s %-5s %s %7s %7s %5s %6s %7s %7s %7s %7s %7s", stamp(r.End, now), r.API, fit(modelName(r.Model, nw), nw),
			opt(r.PromptTok, fmtNum), opt(r.CachedTok, fmtNum), reuse, fmtNum(r.OutTok), opt(r.QueueSec, fmtSec), ttft,
			opt(r.TotalSec, fmtSec), opt(r.TPS, fmtRate), trunc(finish, 7))
		if selectable && i == sel {
			line = cSel.Render(fit(line, p.w))
		}
		p.add(line)
	}
}

func (m model) requests(p *page, st *State, now time.Time) {
	m.live(p, st, now, max(3, p.room()/3))
	errs := 0
	for _, g := range st.Signals {
		if g.Kind == "http-error" {
			errs++
		}
	}
	errRows := 0
	if errs > 0 {
		errRows = min(errs, 5) + 1
	}
	recentTable(p, st, now, max(3, p.room()-errRows), m.sel, true)
	if errs > 0 {
		p.add(section(fmt.Sprintf("HTTP ERRORS (%d)", errs), p.w, ""))
		for i := len(st.Signals) - 1; i >= 0 && p.room() > 0; i-- {
			if g := st.Signals[i]; g.Kind == "http-error" {
				p.add(" " + cDim.Render(stamp(g.T, now)) + " " + cYellow.Render(g.Detail))
			}
		}
	}
}

// ---------- models ----------

func (m model) models(p *page, st *State, now time.Time) {
	act := map[string]ActiveModel{}
	if st.Activity != nil {
		for _, a := range st.Activity.Models {
			act[a.ID] = a
		}
	}
	var loaded int
	var mem float64
	for _, x := range st.Models {
		if x.Loaded {
			loaded++
			mem += cmp.Or(x.ActualSize, x.EstSize)
		}
	}
	p.add(section(fmt.Sprintf("MODELS (%d, %d loaded, %s)", len(st.Models), loaded, fmtBytes(mem)), p.w, "j/k select"))
	if len(st.Models) == 0 {
		p.add(cDim.Render("  model list unavailable (server down or API access refused)"))
	}
	nw := min(48, max(20, p.w-76))
	p.add(cColHdr.Render(fit(fmt.Sprintf(" %s %-8s %-10s %-7s %8s %8s %4s %4s %8s %8s", fit("MODEL", nw), "STATE", "ENGINE", "FORMAT", "SIZE", "CTX", "ACT", "WAIT", "IDLE", "TTL"), p.w)))
	sel := min(m.sel, max(0, len(st.Models)-1))
	from, to := scroll(sel, len(st.Models), max(3, p.room()-8))
	for i := from; i < to; i++ {
		x := st.Models[i]
		a, live := act[x.ID]
		state, sc := "-", cDim
		switch {
		case x.Loading || a.Loading:
			state, sc = "loading", cBlue
		case live && a.Active > 0:
			state, sc = "busy", cGreen
		case x.Loaded:
			state, sc = "loaded", cBold
		}
		if x.Pinned {
			state += "*"
		}
		idle, ttl := "-", "-"
		if a.Idle != nil && *a.Idle > 0 {
			idle = fmtSec(*a.Idle)
		}
		if a.TTL != nil {
			ttl = fmtSec(*a.TTL)
		}
		actN, waitN := "-", "-"
		if live {
			actN, waitN = fmtNum(a.Active), fmtNum(a.WaitingN)
		}
		line := fmt.Sprintf(" %s %s %-10s %-7s %8s %8s %4s %4s %8s %8s", fit(modelName(x.ID, nw), nw), sc.Render(fit(state, 8)), trunc(x.Engine, 10), trunc(x.Format, 7),
			opt(cmp.Or(x.ActualSize, x.EstSize), fmtBytes), opt(cmp.Or(x.CtxMax, x.CtxModel), fmtNum), actN, waitN, idle, ttl)
		if i == sel {
			line = cSel.Render(fit(line, p.w))
		}
		p.add(line)
	}
	if len(st.Models) > 0 {
		x := st.Models[sel]
		p.add(section("DETAIL · "+x.ID, p.w, "* = pinned"))
		kv := func(k, v string) string { return cDim.Render(k+" ") + v + "  " }
		l := "  " + kv("type", cmp.Or(x.Type, "-")) + kv("alias", cmp.Or(x.Alias, "-")) + kv("estimated", opt(x.EstSize, fmtBytes)) +
			kv("actual", opt(x.ActualSize, fmtBytes)) + kv("model ctx", opt(x.CtxModel, fmtNum)) + kv("max tokens", opt(x.MaxTokens, fmtNum))
		if x.LastAccess > 0 {
			l += kv("last used", agoAt(time.Unix(int64(x.LastAccess), 0), now))
		}
		p.add(l)
		l = "  " + kv("median decode", opt(st.baselineTPS(x.ID), func(f float64) string { return fmt.Sprintf("%.1f tok/s", f) }))
		if u := st.Usage["7d"]; u != nil {
			for _, r := range u.Models {
				if r.ModelID == x.ID {
					l += kv("7d requests", fmtNum(r.Requests)) + kv("tokens in/out", fmtNum(r.PromptTok)+"/"+fmtNum(r.OutTok)) +
						kv("cache reuse", fmt.Sprintf("%.0f%%", 100*r.CacheEff))
					if r.PrefillTPS != nil {
						l += kv("prefill", fmt.Sprintf("%.0f tok/s", *r.PrefillTPS))
					}
					if r.DecodeTPS != nil {
						l += kv("decode", fmt.Sprintf("%.1f tok/s", *r.DecodeTPS))
					}
				}
			}
		}
		p.add(l)
	}
	if len(st.Loads) > 0 && p.room() >= 2 {
		p.add(section("MODEL LOADS", p.w, "from the server log"))
		for i := len(st.Loads) - 1; i >= 0 && p.room() > 0; i-- {
			r := st.Loads[i]
			d := "-"
			if r.Dur > 0 {
				d = fmtDur(r.Dur)
			}
			p.add(fmt.Sprintf(" %-11s %-48s %9s %s", stamp(r.T, now), modelName(r.Model, 48), r.Size, cDim.Render("load time ")+d))
		}
	}
}

// ---------- cache ----------

func (m model) cache(p *page, st *State, now time.Time) {
	p.add(section("KV CACHE TIERS", p.w, "dir "+st.Cfg.CacheDir))
	bw := min(60, max(10, p.w-50))
	var rc *RuntimeCache
	if st.Stats != nil {
		rc = st.Stats.Cache
	}
	tier := func(label string, used, mx float64, extra string) {
		if mx <= 0 {
			p.add(fmt.Sprintf(" %s %s %s", cBold.Render(fit(label, 10)), cDim.Render("off or unlimited"), cDim.Render("  used "+fmtBytes(used)+"  "+extra)))
			return
		}
		p.add(fmt.Sprintf(" %s [%s] %s/%s %s", cBold.Render(fit(label, 10)), bar(used/mx, bw, lvlColor(100*used/mx)), fmtBytes(used), fmtBytes(mx), cDim.Render(extra)))
	}
	if rc == nil {
		why := "cache statistics need admin access"
		if st.AdminOK {
			why = cmp.Or(st.StatsErr, "waiting for cache statistics…")
		}
		p.add(cDim.Render("  " + why))
	} else {
		tier("hot (RAM)", rc.HotSize, rc.HotMax, fmtNum(rc.HotEntries)+" blocks")
		tier("SSD", rc.Size, rc.MaxSize, fmtNum(rc.Files)+" files")
	}
	if st.Sys.DiskFree >= 0 && !st.Sys.T.IsZero() {
		p.add(cDim.Render(" disk free ") + fmtBytes(st.Sys.DiskFree))
	}
	if c := st.snapshot(); c != nil {
		p.add(fmt.Sprintf(" %s [%s] %.1f%% %s", cBold.Render(fit("reuse", 10)), bar(c.CacheEff/100, bw, cGreen), c.CacheEff,
			cDim.Render(fmtNum(c.CachedTok)+" of "+fmtNum(c.PromptTok)+" prompt tokens since server start")))
	}
	if st.Restores > 0 {
		p.add(cDim.Render(" log: ") + fmt.Sprintf("%d prefix restores, %s tokens restored", st.Restores, fmtNum(st.RestoredTk)))
	}
	if st.SpecDecode != "" {
		p.add(cDim.Render(" speculative decode accepted ") + st.SpecDecode)
	}
	if rc != nil {
		p.add(section(fmt.Sprintf("PER MODEL (%d loaded)", len(rc.Models)), p.w, "cumulative since the model was loaded"))
		nw := min(40, max(18, p.w-86))
		if len(rc.Models) == 0 {
			p.add(cDim.Render("  no loaded model with a paged cache"))
		} else {
			p.add(cColHdr.Render(fit(fmt.Sprintf(" %s %8s %7s %7s %5s %7s %7s %7s %7s %6s %6s %6s", fit("MODEL", nw), "BLOCKS", "HITS", "MISSES", "HIT%", "HOT HIT", "LOADS", "SAVES", "EVICT", "DROPS", "INLINE", "ERRORS"), p.w)))
		}
		for _, c := range rc.Models {
			hit := "-"
			if t := c.Hits + c.Misses; t > 0 {
				hit = fmt.Sprintf("%.0f%%", 100*c.Hits/t)
			}
			red := func(v float64) string {
				s := fmt.Sprintf("%6s", fmtNum(v))
				if v > 0 {
					return cRed.Render(s)
				}
				return cDim.Render(s)
			}
			p.add(fmt.Sprintf(" %s %8s %7s %7s %5s %7s %7s %7s %7s %s %s %s", fit(modelName(c.ID, nw), nw), fmtNum(c.IndexedBlocks), fmtNum(c.Hits), fmtNum(c.Misses), hit,
				fmtNum(c.HotHits), fmtNum(c.Loads), fmtNum(c.Saves), fmtNum(c.Evictions), red(c.WriteDrops), red(c.InlineWrites), red(c.Errors)))
		}
	}
	if p.room() >= 2 {
		p.add(section("CACHE ACTIVITY", p.w, "recent symptoms"))
		n := 0
		for i := len(st.Signals) - 1; i >= 0 && p.room() > 0; i-- {
			g := st.Signals[i]
			if g.Kind == "reprefill" || strings.HasPrefix(g.Kind, "ssd-") {
				p.add(" " + cDim.Render(stamp(g.T, now)+" "+fit(g.Kind, 14)) + g.Detail)
				n++
			}
		}
		if n == 0 {
			p.add(cDim.Render("  none"))
		}
	}
}

// ---------- events ----------

func (m model) events(p *page, st *State, now time.Time) {
	right := st.Cfg.LogPath
	if !st.LogLast.IsZero() {
		right += "  last line " + agoAt(st.LogLast, now)
	}
	p.add(section(fmt.Sprintf("EVENTS (%d)", len(st.Events)), p.w, right))
	if st.LogErr != "" {
		p.add(cRed.Render(" log error: " + st.LogErr + "  (use -log PATH)"))
	}
	if len(st.Events) == 0 {
		p.add(cDim.Render("  none"))
		return
	}
	n := len(st.Events)
	sel := min(m.sel, n-1)
	// The selected message is shown wrapped underneath.
	from, to := scroll(sel, n, max(1, p.room()-5))
	for i := from; i < to; i++ {
		e := st.Events[n-1-i]
		c := cDim
		switch e.Level {
		case "WARN":
			c = cYellow
		case "ERROR":
			c = cRed
		}
		rep := ""
		if e.N > 1 {
			rep = cDim.Render(fmt.Sprintf(" ×%d", e.N))
		}
		line := fmt.Sprintf(" %s %s %s%s", cDim.Render(fmt.Sprintf("%-11s", stamp(e.T, now))), c.Render(fmt.Sprintf("%-5s", e.Level)), trunc(e.Msg, max(10, p.w-26)), rep)
		if i == sel {
			line = cSel.Render(fit(fmt.Sprintf(" %-11s %-5s %s", stamp(e.T, now), e.Level, trunc(e.Msg, max(10, p.w-26))), p.w))
		}
		p.add(line)
	}
	p.add(section("SELECTED", p.w, ""))
	r := []rune(st.Events[n-1-sel].Msg)
	for width := max(10, p.w-2); len(r) > 0 && p.room() > 0; {
		k := min(width, len(r))
		p.add(" " + string(r[:k]))
		r = r[k:]
	}
}

// ---------- usage ----------

func (m model) usage(p *page, st *State) {
	rng, title := "today", "TODAY"
	if m.week {
		rng, title = "7d", "LAST 7 DAYS"
	}
	p.add(section("USAGE · "+title, p.w, "u: switch range"))
	u := st.Usage[rng]
	switch {
	case u == nil:
		p.add(cDim.Render("  usage history needs admin access (refreshed every 30s)"))
		return
	case !u.Enabled:
		p.add(cDim.Render("  usage history is disabled in the oMLX settings"))
		return
	}
	t := u.Totals
	f := func(v *float64, format string) string {
		if v == nil {
			return "-"
		}
		return fmt.Sprintf(format, *v)
	}
	p.add(fmt.Sprintf(" requests %s   prompt %s   cached %s (%.0f%%)   output %s   prefill %s   decode %s   avg request %s",
		cBold.Render(fmtNum(t.Requests)), cBold.Render(fmtNum(t.PromptTok)), fmtNum(t.CachedTok), 100*t.CacheEff, cBold.Render(fmtNum(t.OutTok)),
		f(t.PrefillTPS, "%.0f tok/s"), f(t.DecodeTPS, "%.1f tok/s"), f(t.AvgReqSec, "%.1fs")))

	rows, label := u.Hourly, "BY HOUR"
	if m.week {
		rows, label = u.Daily, "BY DAY"
	}
	p.add(section(label+" (tokens)", p.w, ""))
	var top float64
	for _, r := range rows {
		top = max(top, r.PromptTok+r.OutTok)
	}
	bw := min(70, max(10, p.w-60))
	// Keep room for the model table below.
	if maxRows := max(3, p.room()-len(u.Models)-3); len(rows) > maxRows {
		rows = rows[len(rows)-maxRows:]
	}
	if len(rows) == 0 || top == 0 {
		p.add(cDim.Render("  no traffic in this range"))
	}
	for _, r := range rows {
		if top == 0 {
			break
		}
		name := r.Date
		if !m.week {
			name = time.Unix(int64(r.Hour), 0).Format("15:00")
		}
		frac := (r.PromptTok + r.OutTok) / top
		p.add(fmt.Sprintf(" %-10s %s %8s %s", clean(name), bar(frac, bw, cBlue), fmtNum(r.PromptTok+r.OutTok),
			cDim.Render(fmt.Sprintf("%s req  decode %s", fmtNum(r.Requests), f(r.DecodeTPS, "%.1f t/s")))))
	}
	if p.room() >= 3 {
		p.add(section("BY MODEL", p.w, ""))
		nw := min(48, max(20, p.w-70))
		p.add(cColHdr.Render(fit(fmt.Sprintf(" %s %7s %9s %9s %6s %9s %9s %8s", fit("MODEL", nw), "REQS", "PROMPT", "OUTPUT", "CACHE", "PRE t/s", "DEC t/s", "AVG REQ"), p.w)))
		for _, r := range u.Models {
			p.add(fmt.Sprintf(" %s %7s %9s %9s %5.0f%% %9s %9s %8s", fit(modelName(r.ModelID, nw), nw), fmtNum(r.Requests), fmtNum(r.PromptTok), fmtNum(r.OutTok),
				100*r.CacheEff, f(r.PrefillTPS, "%.0f"), f(r.DecodeTPS, "%.1f"), f(r.AvgReqSec, "%.1fs")))
		}
	}
}

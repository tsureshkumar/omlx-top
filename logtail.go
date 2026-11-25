package main

import (
	"bufio"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// tailFile follows path (handling truncation and daily rotation), feeding lines to the parser.
func tailFile(path string, backlog int64, st *State) {
	first := true
	setErr := func(e string) {
		st.mu.Lock()
		st.LogErr = e
		st.mu.Unlock()
	}
	for {
		f, err := os.Open(path)
		if err != nil {
			setErr(shortErr(err))
			time.Sleep(time.Second)
			continue
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			setErr(shortErr(err))
			time.Sleep(time.Second)
			continue
		}
		var off int64
		if first && fi.Size() > backlog {
			off = fi.Size() - backlog
		}
		f.Seek(off, io.SeekStart)
		r := bufio.NewReaderSize(f, 256<<10)
		if off > 0 {
			r.ReadString('\n') // skip the partial line
		}
		setErr("")
		var partial string
		for {
			line, err := r.ReadString('\n')
			if err == nil {
				st.feedLog(partial + strings.TrimRight(line, "\r\n"))
				partial = ""
				continue
			}
			if partial += line; len(partial) > 1<<20 {
				partial = ""
			}
			// EOF: check for rotation or truncation.
			time.Sleep(150 * time.Millisecond)
			cur, _ := f.Seek(0, io.SeekCurrent)
			nfi, serr := os.Stat(path)
			if serr != nil {
				continue
			}
			if !os.SameFile(fi, nfi) || nfi.Size() < cur-int64(r.Buffered()) {
				// Drain anything written to the old file before the rotation.
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						break
					}
					st.feedLog(partial + strings.TrimRight(line, "\r\n"))
					partial = ""
				}
				break
			}
		}
		f.Close()
		first = false
	}
}

var (
	// 2026-11-25 08:14:25,047 - omlx.scheduler - INFO - [-] - message
	reLine = regexp.MustCompile(`^(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d,\d{3}) - ([\w.]+) - (\w+) - (?:\[[^\]]*\] - )?(.*)$`)

	reDone       = regexp.MustCompile(`^(Chat completion|Completion|Anthropic message|Responses API): model=(.+?), (\d+) tokens in ([\d.]+)s \(([\d.]+) tok/s`)
	rePrompt     = regexp.MustCompile(`\bprompt: (\d+)`)
	reFinish     = regexp.MustCompile(`\bfinish_reason=(\w+)`)
	reTTFT       = regexp.MustCompile(`\bstream_model_ttft=([\d.]+)s`)
	reRestore    = regexp.MustCompile(`^Prefix cache restore for ([\w-]+): source=\w+ cached=(\d+) suffix=(\d+)`)
	reReprefill  = regexp.MustCompile(`^prefix cache: request \S+ re-prefills (\d+) of (\d+) tokens \(reused (\d+)\)`)
	reThrottle   = regexp.MustCompile(`^Prefill throttled for \S+: chunk (\d+) -> (\d+) \(usage ([\d.]+)GB vs sizing target ([\d.]+)GB`)
	rePressure   = regexp.MustCompile(`^Memory pressure level: (\w+) -> (\w+) \((.*)\)`)
	reLoading    = regexp.MustCompile(`^Loading model(?: as LM \(force_lm=True\))?: (.+)$`)
	reLoaded     = regexp.MustCompile(`^Loaded model: (\S+) \(actual: ([\d.]+\w+)`)
	reUnloaded   = regexp.MustCompile(`^Unloaded (?:\w+ )?model: ([^\s,]+)`)
	reEvict      = regexp.MustCompile(`^Evicting (?:idle model |model )?'([^']+)'`)
	reMetalCap   = regexp.MustCompile(`^Metal cap \(([^,]+), .*is below the oMLX static ceiling \(([^)]+)\)`)
	reWiredLimit = regexp.MustCompile(`iogpu\.wired_limit_mb=(\d+)`)
	reSpec       = regexp.MustCompile(`^vlm_mtp stats: .*accepted=(\d+/\d+ \([\d.]+%\))`)
	reHTTP       = regexp.MustCompile(`^(GET|POST|PUT|DELETE|PATCH|HEAD) (\S+) → (\d{3})`)
)

func num(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

// signalRules classify messages that indicate a performance problem.
var signalRules = []struct{ kind, prefix, contains string }{
	{kind: "requeue", prefix: "Memory pressure: requeued"},
	{kind: "requeue", prefix: "Hard memory pressure", contains: "requeueing"},
	{kind: "requeue", prefix: "Requeued ", contains: "memory pressure"},
	{kind: "mem-abort", prefix: "Hard memory pressure", contains: "abort"},
	{kind: "mem-abort", prefix: "Emergency memory pressure"},
	{kind: "mem-abort", prefix: "Aborted ", contains: "memory pressure"},
	{kind: "mem-abort", contains: "exhausted", prefix: "Prefill for "},
	{kind: "reject", prefix: "Preflight rejected"},
	{kind: "reject", prefix: "Preflight safety-cap rejected"},
	{kind: "reject", prefix: "Prefill capacity rejected"},
	{kind: "reject", contains: "prefill rejected"},
	{kind: "ssd-saturated", contains: "write queue saturated"},
	{kind: "ssd-disk", prefix: "SSD cache disk"},
	{kind: "ssd-error", prefix: "Failed to load block from SSD cache"},
	{kind: "ssd-error", prefix: "Cache corruption detected"},
	{kind: "admission-stall", contains: "admission stalled"},
	{kind: "disconnect", prefix: "Client disconnected"},
}

// feedLog parses one server log line.
func (s *State) feedLog(line string) {
	m := reLine.FindStringSubmatch(line)
	if m == nil {
		return // traceback continuation or foreign output
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05,000", m[1], time.Local)
	if err != nil {
		return
	}
	logger, level, msg := m[2], m[3], clean(m[4])
	if level == "WARNING" {
		level = "WARN"
	}
	if level == "CRITICAL" {
		level = "ERROR"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.LogLines++
	s.LogLast = t
	if strings.HasPrefix(logger, "httpx") || strings.HasPrefix(logger, "httpcore") {
		return
	}

	if d := reDone.FindStringSubmatch(msg); d != nil {
		r := &Req{End: t, API: map[string]string{"Chat completion": "chat", "Completion": "compl",
			"Anthropic message": "msgs", "Responses API": "resp"}[d[1]],
			Model: d[2], OutTok: num(d[3]), TotalSec: num(d[4]), TPS: num(d[5])}
		if x := rePrompt.FindStringSubmatch(msg); x != nil {
			r.PromptTok = num(x[1])
		}
		if x := reFinish.FindStringSubmatch(msg); x != nil {
			r.Finish = x[1]
			if r.Finish == "length" {
				s.signal(t, "truncated", r.Model)
			}
		}
		if x := reTTFT.FindStringSubmatch(msg); x != nil {
			r.TTFT = num(x[1])
		}
		s.addLogged(r)
		return
	}
	if x := reRestore.FindStringSubmatch(msg); x != nil {
		s.Restores++
		s.RestoredTk += num(x[2])
		if len(s.cacheRes) > 500 {
			clear(s.cacheRes)
		}
		s.cacheRes[x[1]] = num(x[2])
		if r := s.Live[x[1]]; r != nil {
			r.CachedTok = max(r.CachedTok, num(x[2]))
		}
		return
	}
	if x := reReprefill.FindStringSubmatch(msg); x != nil {
		d := "re-prefilled " + x[1] + " of " + x[2] + " tokens (reused " + x[3] + ")"
		s.signal(t, "reprefill", d)
		s.event(t, "INFO", "prompt prefix diverged: "+d)
		return
	}
	if x := reThrottle.FindStringSubmatch(msg); x != nil {
		d := "chunk " + x[1] + "→" + x[2] + " at " + x[3] + "GB (target " + x[4] + "GB)"
		s.signal(t, "throttle", d)
		s.event(t, "WARN", "prefill throttled: "+d)
		return
	}
	if x := rePressure.FindStringSubmatch(msg); x != nil {
		lv := "WARN"
		if x[2] == "ok" {
			lv = "INFO"
		} else {
			s.signal(t, "pressure", x[1]+" → "+x[2]+" ("+x[3]+")")
		}
		s.event(t, lv, msg)
		return
	}
	if x := reEvict.FindStringSubmatch(msg); x != nil {
		s.signal(t, "evict", x[1])
		s.event(t, "WARN", msg)
		return
	}
	if x := reLoading.FindStringSubmatch(msg); x != nil && logger == "omlx.engine_pool" {
		s.loading[x[1]] = t
		s.event(t, "INFO", msg)
		return
	}
	if x := reLoaded.FindStringSubmatch(msg); x != nil && logger == "omlx.engine_pool" {
		rec := LoadRec{T: t, Model: x[1], Size: x[2]}
		if b, ok := s.loading[x[1]]; ok {
			rec.Dur = t.Sub(b)
			delete(s.loading, x[1])
		}
		s.Loads = tail(append(s.Loads, rec), maxLoads)
		s.signal(t, "load", x[1])
		if rec.Dur > 0 {
			msg += " in " + fmtDur(rec.Dur)
		}
		s.event(t, "INFO", msg)
		return
	}
	if reUnloaded.MatchString(msg) && logger == "omlx.engine_pool" {
		s.event(t, "INFO", msg)
		return
	}
	if x := reMetalCap.FindStringSubmatch(msg); x != nil {
		s.MetalCap = "Metal cap " + x[1] + " is below the oMLX static ceiling " + x[2]
		s.MetalCapB = num(strings.TrimSuffix(x[1], "GB")) * (1 << 30)
		if w := reWiredLimit.FindStringSubmatch(msg); w != nil {
			s.MetalCapMB = int(num(w[1]))
		}
		s.event(t, "WARN", msg)
		return
	}
	if x := reSpec.FindStringSubmatch(msg); x != nil {
		s.SpecDecode = x[1]
		return
	}
	// A restart invalidates what earlier runs reported about the setup.
	if logger == "omlx.server" && strings.HasPrefix(msg, "Server initialized with") {
		s.MetalCap, s.MetalCapMB, s.SpecDecode = "", 0, ""
		clear(s.loading)
		s.event(t, "INFO", msg)
		return
	}
	if x := reHTTP.FindStringSubmatch(msg); x != nil {
		s.signal(t, "http-error", x[1]+" "+x[2]+" → "+x[3])
	}
	matched := false
	for _, r := range signalRules {
		if strings.HasPrefix(msg, r.prefix) && strings.Contains(msg, r.contains) {
			s.signal(t, r.kind, msg)
			matched = true
			break
		}
	}
	if matched || level == "WARN" || level == "ERROR" {
		s.event(t, level, msg)
	}
}

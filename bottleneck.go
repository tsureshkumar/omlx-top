package main

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	sevInfo = iota + 1
	sevWarn
	sevCrit
)

// Finding is one diagnosed bottleneck or risk.
type Finding struct {
	Sev    int
	Area   string
	Title  string
	Detail string // the evidence
	Fix    string // what to try
}

// Thresholds. All are heuristics.
const (
	window          = 10 * time.Minute // how long a logged symptom stays relevant
	swapWarnRate    = 2 << 20          // bytes/s swapped out, averaged
	swapCritRate    = 20 << 20
	swapSamples     = 10
	queueSamples    = 5
	idleGPUSamples  = 10
	idleGPUPct      = 30.0
	stallDecodeSec  = 15.0
	stallPrefill    = 30 * time.Second
	slowDecodeRatio = 0.5
	lowCacheEffPct  = 20.0
	minCachePrompt  = 20000.0
	prefillShare    = 0.6
	evictThrash     = 3
	hotGPUTemp      = 95.0
	staleAfter      = 6 * time.Second
	clientErrors    = 3
	lowDiskBytes    = 10 << 30
)

// lastN returns the last n history samples, or nil if there are fewer or they are stale.
func (s *State) lastN(n int, now time.Time) []Sample {
	if len(s.History) < n {
		return nil
	}
	h := s.History[len(s.History)-n:]
	if now.Sub(h[n-1].T) > 30*time.Second {
		return nil
	}
	return h
}

func all(h []Sample, f func(Sample) bool) bool {
	return len(h) > 0 && !slices.ContainsFunc(h, func(x Sample) bool { return !f(x) })
}

// baselineTPS is the median decode speed of the model's logged requests.
func (s *State) baselineTPS(model string) float64 {
	var v []float64
	for _, r := range s.Recent {
		if r.Logged && r.Model == model && r.OutTok >= 32 && r.TPS > 0 {
			v = append(v, r.TPS)
		}
	}
	if len(v) < 3 {
		return 0
	}
	slices.Sort(v)
	return v[len(v)/2]
}

// timeShare returns the seconds spent in prefill and generation today (or this session).
func (s *State) timeShare() (pre, gen, reqs float64, scope string) {
	if u := s.Usage["today"]; u != nil && u.Totals.PrefillSec+u.Totals.GenSec > 0 {
		return u.Totals.PrefillSec, u.Totals.GenSec, u.Totals.Requests, "today"
	}
	if c := s.snapshot(); c != nil {
		// Durations are recovered from the average speeds the server reports.
		if c.PrefillTPS > 0 {
			pre = max(0, c.PromptTok-c.CachedTok) / c.PrefillTPS
		}
		if c.DecodeTPS > 0 {
			gen = c.OutTok / c.DecodeTPS
		}
		return pre, gen, c.Requests, "since server start"
	}
	var n float64
	for _, r := range s.Recent {
		if r.Logged && r.TTFT > 0 {
			pre += r.TTFT
			gen += max(0, r.TotalSec-r.TTFT)
			n++
		}
	}
	return pre, gen, n, "logged streaming requests"
}

// Analyze diagnoses the current state, most severe first. Caller holds a read lock.
func Analyze(s *State, now time.Time) []Finding {
	var out []Finding
	add := func(sev int, area, title, detail, fix string) {
		out = append(out, Finding{sev, area, title, detail, fix})
	}
	since := now.Add(-window)
	count := func(kind string) (int, Signal) { return s.signalsSince(kind, since) }

	// ---- availability ----
	if !s.Up && s.UpErr != "" {
		if s.NotOMLX {
			add(sevCrit, "server", "Port answers, but it is not oMLX", s.Cfg.URL,
				"Quit the other server (e.g. Ollama on 11434), change the oMLX port, or use -host")
		} else {
			add(sevCrit, "server", "oMLX is not reachable", s.UpErr+" at "+s.Cfg.URL, "Start it with `omlx start`, or use -host")
		}
	}
	if s.Up && s.Health != nil && s.Health.Status == "loading" {
		add(sevInfo, "server", "Server is still preloading pinned models", "/health reports loading", "")
	}
	if s.Up && !s.AdminOK && s.AccessErr != "" {
		add(sevWarn, "access", "Limited data: no live request detail", s.AccessErr+"; key: "+s.Cfg.KeySource,
			"Pass the main API key with -api-key or $OMLX_API_KEY")
	}
	if s.AdminOK && !s.ActAt.IsZero() && now.Sub(s.ActAt) > staleAfter {
		add(sevWarn, "server", "Live request data is stale", "last update "+agoAt(s.ActAt, now)+" "+s.APIErr,
			"The admin API is not answering; the server may be overloaded")
	}
	if s.LogErr != "" {
		add(sevInfo, "access", "Server log is not readable", s.LogErr+": "+s.Cfg.LogPath, "Memory, eviction and error checks need it; use -log PATH")
	}
	if s.Up && s.APILat > time.Second {
		add(sevWarn, "server", "Server responds slowly", "/health took "+fmtDur(s.APILat),
			"The event loop is blocked or the machine is overloaded; check swap and CPU")
	}

	// ---- memory ----
	used, ceil := s.omlxMemory()
	level := ""
	if s.Activity != nil {
		level = s.Activity.Pressure.Level
	}
	switch {
	case level != "" && level != "ok":
		sev := sevWarn
		if level == "hard" {
			sev = sevCrit
		}
		add(sev, "memory", "oMLX memory pressure: "+level, fmt.Sprintf("%s used of %s ceiling", fmtBytes(used), fmtBytes(ceil)),
			"Reduce context length or concurrency, unload idle models, or use a smaller quantization")
	case ceil > 0 && used/ceil >= 0.9:
		add(sevWarn, "memory", "oMLX is close to its memory ceiling", fmt.Sprintf("%s of %s (%.0f%%)", fmtBytes(used), fmtBytes(ceil), 100*used/ceil),
			"Long prompts may be throttled or rejected; unload a model or shorten context")
	}
	if h := s.lastN(swapSamples, now); h != nil {
		var sum float64
		for _, x := range h {
			sum += x.SwapOutRate
		}
		if avg := sum / float64(len(h)); avg >= swapWarnRate {
			sev := sevWarn
			if avg >= swapCritRate {
				sev = sevCrit
			}
			add(sev, "memory", "macOS is swapping", fmt.Sprintf("%s/s swapped out, %s swap in use", fmtBytes(avg), fmtBytes(s.Sys.SwapUsed)),
				"Swapping stalls prefill and decode: close apps or load a smaller model"+s.Sys.largest())
		}
	}
	switch s.Sys.Pressure {
	case 4:
		add(sevCrit, "memory", "macOS memory pressure is critical", fmt.Sprintf("%s of %s in use", fmtBytes(s.Sys.MemUsed), fmtBytes(s.Sys.MemTotal)), "Free memory now"+s.Sys.largest())
	case 2:
		add(sevWarn, "memory", "macOS memory pressure is elevated", fmt.Sprintf("%s of %s in use", fmtBytes(s.Sys.MemUsed), fmtBytes(s.Sys.MemTotal)), "Close unused apps"+s.Sys.largest())
	}
	if s.Sys.OllamaRSS > 1<<30 {
		add(sevWarn, "memory", "Ollama is competing for memory", fmtBytes(s.Sys.OllamaRSS)+" resident (Metal buffers may add more)",
			"`ollama stop <model>` or quit Ollama while using oMLX")
	}
	if n, last := count("mem-abort"); n > 0 {
		add(sevCrit, "memory", fmt.Sprintf("Requests aborted for memory (%d)", n), agoAt(last.T, now)+": "+last.Detail,
			"Lower max_concurrent_requests or context length, or raise the memory guard tier")
	}
	if n, last := count("requeue"); n > 0 {
		add(sevWarn, "memory", fmt.Sprintf("Requests requeued for memory (%d)", n), agoAt(last.T, now)+": "+last.Detail,
			"Work is being redone; lower concurrency or context length")
	}
	if n, last := count("throttle"); n > 0 {
		add(sevWarn, "memory", fmt.Sprintf("Prefill is being throttled (%d)", n), agoAt(last.T, now)+": "+last.Detail,
			"Chunks shrink to stay under the ceiling; reduce context or free memory")
	}
	if n, last := count("reject"); n > 0 {
		add(sevWarn, "memory", "Prompts are being rejected by the memory guard", agoAt(last.T, now)+": "+last.Detail,
			"Shorten the prompt, unload other models, or pick a less strict memory_guard_tier")
	}
	if n, last := count("pressure"); n > 0 && (level == "" || level == "ok") {
		add(sevInfo, "memory", fmt.Sprintf("Memory pressure occurred recently (%d)", n), agoAt(last.T, now)+": "+last.Detail, "")
	}
	// Only worth raising when the ceiling currently in force actually reaches the cap.
	if s.MetalCap != "" && s.Sys.WiredLimit <= 0 && ceil > 0 && ceil >= s.MetalCapB {
		fix := ""
		if s.MetalCapMB > 0 {
			fix = fmt.Sprintf("sudo sysctl iogpu.wired_limit_mb=%d (resets on reboot; leave headroom for macOS)", s.MetalCapMB)
		}
		add(sevInfo, "memory", "GPU memory is capped below the oMLX ceiling", s.MetalCap, fix)
	}

	// ---- models ----
	if n, last := count("evict"); n >= evictThrash {
		add(sevWarn, "models", fmt.Sprintf("Model churn: %d evictions in %s", n, fmtDur(window)), "latest: "+last.Detail,
			"The models in use do not fit together; pin the hot one, use smaller quants, or stagger clients")
	}
	var busyModels []string
	if a := s.Activity; a != nil {
		for _, m := range a.Models {
			if m.Loading {
				add(sevInfo, "models", "Loading "+m.ID, "for "+fmtDur(secs(m.LoadElapsed))+"; requests for it wait", "Pin frequently used models to avoid cold loads")
			}
			if m.Active > 0 {
				busyModels = append(busyModels, m.ID)
			}
		}
	}

	// ---- queue and live requests ----
	_, _, active, waiting, loading := s.liveTotals()
	if h := s.lastN(queueSamples, now); all(h, func(x Sample) bool { return x.Waiting > 0 }) {
		d := fmt.Sprintf("%d waiting, %d active", waiting, active)
		fix := "Requests arrive faster than they finish"
		if mx := s.Cfg.MaxConcurrent; mx > 0 {
			d += fmt.Sprintf(" (max_concurrent_requests %d)", mx)
			if active >= mx {
				fix = "At the concurrency limit: raise max_concurrent_requests only if memory allows"
			}
		}
		add(sevWarn, "queue", "Requests are queueing", d, fix)
	}
	ids := make([]string, 0, len(s.Live))
	for id := range s.Live {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		r := s.Live[id]
		switch {
		case r.missed > 0:
		case r.Phase == "generate" && r.IdleAge >= stallDecodeSec:
			add(sevCrit, "stall", "Generation stalled on "+r.Model, fmt.Sprintf("request %s: no token for %s", short(id), fmtDur(secs(r.IdleAge))),
				"Check swap and memory pressure; see the Events view")
		case r.Phase == "prefill" && now.Sub(r.Progress) >= stallPrefill:
			add(sevWarn, "stall", "Prefill not progressing on "+r.Model, fmt.Sprintf("request %s: stuck at %s/%s for %s", short(id), fmtNum(r.Processed), fmtNum(r.Total), fmtDur(now.Sub(r.Progress))),
				"Usually memory pressure, swap, or contention with another request")
		case r.Phase == "generate" && r.OutTok >= 64:
			if base := s.baselineTPS(r.Model); base > 0 && r.TPS < base*slowDecodeRatio {
				fix := "Check swap, temperature and other GPU users"
				if active > 1 {
					fix = fmt.Sprintf("%d requests share the GPU; lower concurrency if single-request latency matters", active)
				}
				add(sevWarn, "speed", "Decode is slower than usual on "+r.Model, fmt.Sprintf("%.1f tok/s now vs %.1f median", r.TPS, base), fix)
			}
		}
	}
	if h := s.lastN(idleGPUSamples, now); !loading && all(h, func(x Sample) bool { return x.Busy && !x.LoadingModel && x.GPU >= 0 && x.GPU < idleGPUPct }) {
		add(sevWarn, "gpu", "GPU mostly idle while requests are active", fmt.Sprintf("under %.0f%% for %d samples (%s)", idleGPUPct, idleGPUSamples, strings.Join(busyModels, ", ")),
			"Something else is the limit: SSD cache I/O, swap, tokenization, or a slow client")
	}
	if now.Sub(s.Power.T) < 15*time.Second && s.Power.GPUTemp >= hotGPUTemp {
		add(sevWarn, "thermal", "GPU is running hot", fmt.Sprintf("%.0f°C", s.Power.GPUTemp), "Thermal throttling likely; improve cooling or enable High Power Mode")
	}

	// ---- cache ----
	if !s.Cfg.CacheEnabled {
		add(sevWarn, "cache", "Prefix cache is disabled", "cache.enabled is false in settings.json", "Enable it so repeated prompts skip prefill")
	}
	reN, reLast := count("reprefill")
	c := s.snapshot()
	switch {
	case s.Cfg.CacheEnabled && c != nil && c.Requests >= 5 && c.PromptTok >= minCachePrompt && c.CacheEff < lowCacheEffPct:
		add(sevWarn, "cache", "Low prompt cache reuse", fmt.Sprintf("%.0f%% of %s prompt tokens came from cache", c.CacheEff, fmtNum(c.PromptTok)),
			"Keep the start of prompts stable (no timestamps or reordered tools before the history)")
	case reN >= 3:
		add(sevWarn, "cache", fmt.Sprintf("Prompt prefixes keep diverging (%d)", reN), "latest "+reLast.Detail,
			"A client changes early prompt content between turns, forcing re-prefill")
	}
	if n, last := count("ssd-error"); n > 0 {
		add(sevWarn, "cache", fmt.Sprintf("SSD cache errors (%d)", n), agoAt(last.T, now)+": "+last.Detail, "Check the disk; clearing the SSD cache may help")
	}
	if n, last := count("ssd-saturated"); n > 0 {
		add(sevWarn, "cache", fmt.Sprintf("SSD cache writes cannot keep up (%d)", n), agoAt(last.T, now)+": "+last.Detail,
			"Inline writes stall inference; enable or enlarge the hot cache, or use a faster disk")
	}
	if n, last := count("ssd-disk"); n > 0 {
		add(sevWarn, "cache", "SSD cache is limited by disk space", agoAt(last.T, now)+": "+last.Detail, "Free disk space or lower ssd_cache_max_size")
	}
	if s.Cfg.CacheEnabled && !s.Sys.T.IsZero() && s.Sys.DiskFree >= 0 && s.Sys.DiskFree < lowDiskBytes {
		add(sevWarn, "cache", "Cache disk is nearly full", fmtBytes(s.Sys.DiskFree)+" free", "Free space or move ssd_cache_dir")
	}
	if s.Stats != nil && s.Stats.Cache != nil {
		rc := s.Stats.Cache
		if rc.Unavailable {
			add(sevWarn, "cache", "SSD cache directory is unavailable", rc.Dir, "Check the volume is mounted, then restart oMLX")
		}
		if n, _ := count("ssd-evict"); n > 0 && rc.MaxSize > 0 && rc.Size/rc.MaxSize > 0.95 {
			add(sevInfo, "cache", "SSD cache is full and evicting", fmt.Sprintf("%s of %s", fmtBytes(rc.Size), fmtBytes(rc.MaxSize)), "Raise ssd_cache_max_size if older sessions should stay cached")
		}
		var loads float64
		for _, m := range rc.Models {
			loads += m.Loads
		}
		if s.Cfg.CacheEnabled && rc.HotMax == 0 && (s.Cfg.HotCacheMax == "0" || s.Cfg.HotCacheMax == "") && loads > 0 {
			add(sevInfo, "cache", "Hot (RAM) cache is off", fmt.Sprintf("%s block loads came from SSD", fmtNum(loads)),
				"With spare memory, set hot_cache_max_size (e.g. 8GB) to restore from RAM")
		}
	}

	// ---- workload ----
	if pre, gen, reqs, scope := s.timeShare(); reqs >= 3 && pre+gen > 30 && pre/(pre+gen) > prefillShare {
		add(sevWarn, "prefill", "Prompt processing dominates", fmt.Sprintf("%.0f%% of model time is time-to-first-token (%s, includes queueing)", 100*pre/(pre+gen), scope),
			"Improve cache reuse, trim context, or try ANE/speculative prefill if the model supports it")
	}
	if n, last := count("truncated"); n >= 2 {
		add(sevInfo, "workload", fmt.Sprintf("Replies cut off at max tokens (%d)", n), "latest model: "+last.Detail, "Raise max_tokens if replies should complete")
	}
	if n, last := count("http-error"); n >= clientErrors {
		fix := "See the requests view"
		switch {
		case strings.HasSuffix(last.Detail, "401"):
			fix = "A client is sending a missing or wrong API key"
		case strings.HasSuffix(last.Detail, "404"):
			fix = "A client asks for a path or model id that does not exist; compare with the models view"
		}
		add(sevWarn, "clients", fmt.Sprintf("Requests are failing (%d)", n), agoAt(last.T, now)+": "+last.Detail, fix)
	}
	if n, last := count("admission-stall"); n > 0 {
		add(sevWarn, "queue", fmt.Sprintf("Admission stalled (%d)", n), agoAt(last.T, now)+": "+last.Detail, "The scheduler is waiting for memory or cache writes")
	}

	slices.SortStableFunc(out, func(a, b Finding) int { return b.Sev - a.Sev })
	return out
}

func secs(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }

func agoAt(t, now time.Time) string { return fmtDur(max(0, now.Sub(t))) + " ago" }

func short(id string) string {
	if r := []rune(id); len(r) > 8 {
		return string(r[:8])
	}
	return id
}

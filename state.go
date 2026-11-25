package main

import (
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
)

// LiveReq is one request followed across activity polls.
type LiveReq struct {
	ID, Model string
	Phase     string // queued, prefill, generate
	FirstSeen time.Time
	LastSeen  time.Time

	QueueSec   float64 // longest observed time in the waiting queue
	PrefillSec float64
	GenSec     float64

	PromptTok float64
	CachedTok float64
	Processed float64
	Total     float64
	PreTPS    float64
	ETA       float64
	Detail    string
	QueuePos  float64

	OutTok    float64
	MaxTok    float64
	TPS       float64
	IdleAge   float64 // seconds since the last generated token
	Progress  time.Time
	missed    int
	sawDecode bool
}

// Req is a finished request, from the server log and/or live observation.
type Req struct {
	End       time.Time
	API       string
	Model     string
	PromptTok float64
	CachedTok float64
	OutTok    float64
	TotalSec  float64
	TTFT      float64
	QueueSec  float64
	PreSec    float64
	TPS       float64
	Finish    string
	Logged    bool // a completion line was found in the server log
	Seen      bool // observed in flight
}

// Event is a notable server log line.
type Event struct {
	T     time.Time
	Level string // INFO, WARN, ERROR
	Msg   string
	N     int // consecutive repeats
}

// Signal is a timestamped performance symptom used by the analyzer.
type Signal struct {
	T      time.Time
	Kind   string
	Detail string
}

type LoadRec struct {
	T     time.Time
	Model string
	Dur   time.Duration
	Size  string
}

// Sample is one point of the history sparklines.
type Sample struct {
	T                  time.Time
	PreTPS, DecTPS     float64
	Active, Waiting    int
	GPU                float64 // -1 unknown
	OMLXMem            float64 // -1 unknown
	SwapOutRate        float64 // bytes/s
	Busy, LoadingModel bool
}

const (
	maxRecent  = 300
	maxEvents  = 500
	maxSignals = 600
	maxHistory = 600
	maxLoads   = 50
)

type State struct {
	mu  sync.RWMutex
	Cfg Config

	Health  *Health
	Up      bool
	UpErr   string
	NotOMLX bool
	APILat  time.Duration
	LastOK  time.Time

	timeouts int // consecutive /health timeouts tolerated

	AdminOK   bool
	AccessErr string
	APIErr    string

	Status   *Status
	Version  string
	Stats    *AdminStats
	StatsErr string
	Models   []ModelInfo
	Usage    map[string]*Usage

	Activity *Activity
	ActAt    time.Time
	Live     map[string]*LiveReq

	Sys   SysInfo
	Power PowerInfo

	Recent  []*Req // oldest first
	Events  []Event
	Signals []Signal
	Loads   []LoadRec
	History []Sample

	loading    map[string]time.Time
	cacheRes   map[string]float64 // request id -> cached tokens, from the log
	prevCache  map[string]CacheCounters
	MetalCapMB int // suggested iogpu.wired_limit_mb from the log; 0 = no warning
	MetalCap   string
	MetalCapB  float64 // the cap in bytes
	SpecDecode string
	Restores   int
	RestoredTk float64

	LogErr   string
	LogLast  time.Time
	LogLines int
	Started  time.Time
}

func NewState(cfg Config) *State {
	return &State{Cfg: cfg, Live: map[string]*LiveReq{}, Usage: map[string]*Usage{},
		loading: map[string]time.Time{}, cacheRes: map[string]float64{}, prevCache: map[string]CacheCounters{},
		Started: time.Now()}
}

// clean makes server-provided text safe to print on a terminal.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' {
			return ' '
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	if r := []rune(s); len(r) > 400 {
		s = string(r[:400]) + "…"
	}
	return s
}

func tail[T any](s []T, n int) []T {
	if len(s) > n {
		return slices.Clone(s[len(s)-n:])
	}
	return s
}

// caller holds lock
func (s *State) event(t time.Time, level, msg string) {
	if n := len(s.Events); n > 0 && s.Events[n-1].Msg == msg && s.Events[n-1].Level == level {
		s.Events[n-1].N++
		s.Events[n-1].T = t
		return
	}
	s.Events = tail(append(s.Events, Event{T: t, Level: level, Msg: msg, N: 1}), maxEvents)
}

// caller holds lock
func (s *State) signal(t time.Time, kind, detail string) {
	s.Signals = tail(append(s.Signals, Signal{t, kind, detail}), maxSignals)
}

// signalsSince returns how many signals of kind happened after t, and the latest.
func (s *State) signalsSince(kind string, t time.Time) (n int, last Signal) {
	for _, g := range s.Signals {
		if g.Kind == kind && g.T.After(t) {
			n++
			if !g.T.Before(last.T) {
				last = g
			}
		}
	}
	return
}

func (s *State) setHealth(h *Health, err error, lat time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.APILat = lat
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() && s.Up && s.timeouts < 2 {
		s.timeouts++
		return
	}
	s.timeouts = 0
	if err != nil {
		if s.Up {
			s.event(time.Now(), "ERROR", "server unreachable: "+clean(shortErr(err)))
		}
		s.Up, s.Health, s.UpErr = false, nil, clean(shortErr(err))
		s.NotOMLX = errors.Is(err, errNotOMLX)
		// Everything live is stale once the server is gone.
		s.Activity, s.AdminOK, s.Stats, s.Status, s.Models = nil, false, nil, nil, nil
		s.Live = map[string]*LiveReq{}
		s.prevCache = map[string]CacheCounters{}
		return
	}
	if !s.Up && !s.LastOK.IsZero() {
		s.event(time.Now(), "INFO", "server reachable again")
	}
	h.DefaultModel = clean(h.DefaultModel)
	s.Up, s.Health, s.UpErr, s.NotOMLX, s.LastOK = true, h, "", false, time.Now()
}

// setAccess records whether the admin API accepts our credentials.
func (s *State) setAccess(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case err == nil:
		s.AdminOK, s.AccessErr = true, ""
	case isAuthErr(err):
		s.AdminOK, s.Activity, s.Stats = false, nil, nil
		s.AccessErr = "admin API rejected the credentials (the main API key is required)"
		s.Live = map[string]*LiveReq{}
	default:
		s.APIErr = clean(shortErr(err))
		return
	}
	s.APIErr = ""
}

func (s *State) hasAdmin() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.AdminOK
}

func (s *State) setStatus(st *Status, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if isAuthErr(err) {
			s.AccessErr = "the API rejected the credentials; showing /health and log data only"
			s.Status = nil
		}
		return
	}
	st.Version = clean(st.Version)
	s.Status, s.Version = st, st.Version
}

func (s *State) setModels(m []ModelInfo) {
	for i := range m {
		m[i].ID, m[i].Alias = clean(m[i].ID), clean(m[i].Alias)
		m[i].Engine, m[i].Type, m[i].Format = clean(m[i].Engine), clean(m[i].Type), clean(m[i].Format)
	}
	slices.SortStableFunc(m, func(a, b ModelInfo) int {
		if a.Loaded != b.Loaded {
			if a.Loaded {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.ID), strings.ToLower(b.ID))
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Models = m
}

func (s *State) setUsage(rng string, u *Usage) {
	for i := range u.Models {
		u.Models[i].ModelID = clean(u.Models[i].ModelID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Usage[rng] = u
}

func (s *State) setStats(st *AdminStats, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.StatsErr = clean(shortErr(err))
		return
	}
	s.StatsErr, s.Stats = "", st
	if st.Cache == nil {
		return
	}
	st.Cache.Dir = clean(st.Cache.Dir)
	now := time.Now()
	for i := range st.Cache.Models {
		c := &st.Cache.Models[i]
		c.ID = clean(c.ID)
		if p, ok := s.prevCache[c.ID]; ok {
			grew := func(now_, before float64, kind, what string) {
				if now_ > before {
					s.signal(now, kind, c.ID+": "+what+" +"+fmtNum(now_-before))
				}
			}
			grew(c.Errors, p.Errors, "ssd-error", "SSD cache errors")
			grew(c.WriteDrops, p.WriteDrops, "ssd-saturated", "SSD write drops")
			grew(c.InlineWrites, p.InlineWrites, "ssd-saturated", "inline SSD writes")
			grew(c.Evictions, p.Evictions, "ssd-evict", "SSD evictions")
		}
		s.prevCache[c.ID] = *c
	}
}

// setActivity updates the in-flight requests and retires the ones that vanished.
func (s *State) setActivity(a *Activity, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Before(s.ActAt) {
		return
	}
	a.Pressure.Level = clean(a.Pressure.Level)
	s.Activity, s.ActAt = a, now
	seen := map[string]bool{}
	get := func(id, model string) *LiveReq {
		r := s.Live[id]
		if r == nil {
			r = &LiveReq{ID: id, Model: model, FirstSeen: now, Progress: now}
			s.Live[id] = r
		}
		seen[id] = true
		r.LastSeen, r.missed = now, 0
		return r
	}
	for i := range a.Models {
		m := &a.Models[i]
		m.ID = clean(m.ID)
		for j := range m.Waiting {
			w := &m.Waiting[j]
			w.ID = clean(w.ID)
			r := get(w.ID, m.ID)
			r.Phase, r.QueuePos = "queued", w.Position
			r.QueueSec = max(r.QueueSec, w.Elapsed)
			r.PromptTok = max(r.PromptTok, w.PromptTok)
		}
		for j := range m.Prefilling {
			p := &m.Prefilling[j]
			p.ID, p.Detail, p.Phase = clean(p.ID), clean(p.Detail), clean(p.Phase)
			r := get(p.ID, m.ID)
			if r.Phase != "prefill" || p.Processed != r.Processed {
				r.Progress = now
			}
			r.Phase, r.Processed, r.Total, r.PreTPS, r.ETA = "prefill", p.Processed, p.Total, p.Speed, p.ETA
			r.Detail = p.Detail
			if r.Detail == "" && p.Phase != "prefill" {
				r.Detail = p.Phase
			}
			r.PrefillSec = max(r.PrefillSec, p.Elapsed)
			r.PromptTok = max(r.PromptTok, p.PromptTok, p.Total+p.CachedTok)
			r.CachedTok = max(r.CachedTok, p.CachedTok)
		}
		for j := range m.Generating {
			g := &m.Generating[j]
			g.ID = clean(g.ID)
			r := get(g.ID, m.ID)
			r.Phase, r.sawDecode = "generate", true
			r.OutTok, r.TPS, r.MaxTok = g.Tokens, g.TPS, g.MaxTokens
			r.GenSec = max(r.GenSec, g.Elapsed)
			r.PromptTok = max(r.PromptTok, g.PromptTok)
			r.IdleAge = 0
			if g.IdleAge != nil {
				r.IdleAge = *g.IdleAge
			}
			r.Progress = now
		}
	}
	for id, r := range s.Live {
		if seen[id] {
			if c, ok := s.cacheRes[id]; ok {
				r.CachedTok = max(r.CachedTok, c)
			}
			continue
		}
		// Two misses in a row: a request can be briefly absent between phases.
		if r.missed++; r.missed >= 2 {
			delete(s.Live, id)
			delete(s.cacheRes, id)
			s.finishLive(r)
		}
	}
}

const matchWindow = 8 * time.Second

func sameRequest(model string, end time.Time, out float64, other *Req) bool {
	d := end.Sub(other.End)
	if d < 0 {
		d = -d
	}
	// The live view lags the last tokens, so it can only see fewer of them.
	return d < matchWindow && other.Model == model && out <= other.OutTok
}

// caller holds lock
func (s *State) finishLive(l *LiveReq) {
	for i := len(s.Recent) - 1; i >= 0 && i >= len(s.Recent)-30; i-- {
		r := s.Recent[i]
		if r.Logged && !r.Seen && sameRequest(l.Model, l.LastSeen, l.OutTok, r) {
			mergeLive(r, l)
			return
		}
	}
	r := &Req{End: l.LastSeen, Model: l.Model, API: "seen", OutTok: l.OutTok, PromptTok: l.PromptTok,
		TotalSec: l.LastSeen.Sub(l.FirstSeen).Seconds(), TPS: l.TPS}
	mergeLive(r, l)
	s.Recent = tail(append(s.Recent, r), maxRecent)
}

func mergeLive(r *Req, l *LiveReq) {
	r.Seen = true
	r.QueueSec, r.PreSec = l.QueueSec, l.PrefillSec
	r.CachedTok = max(r.CachedTok, l.CachedTok)
	if r.PromptTok == 0 {
		r.PromptTok = l.PromptTok
	}
}

// addLogged records a completion parsed from the log. Caller holds lock.
func (s *State) addLogged(r *Req) {
	r.Logged = true
	for i := len(s.Recent) - 1; i >= 0 && i >= len(s.Recent)-30; i-- {
		o := s.Recent[i]
		if !o.Logged && o.Seen && sameRequest(o.Model, o.End, o.OutTok, r) {
			r.Seen, r.QueueSec, r.PreSec = true, o.QueueSec, o.PreSec
			r.CachedTok = max(r.CachedTok, o.CachedTok)
			if r.PromptTok == 0 {
				r.PromptTok = o.PromptTok
			}
			s.Recent[i] = r
			return
		}
	}
	s.Recent = tail(append(s.Recent, r), maxRecent)
}

// liveTotals sums the current speeds and request counts. Caller holds lock.
func (s *State) liveTotals() (preTPS, decTPS float64, active, waiting int, loading bool) {
	if a := s.Activity; a != nil {
		for _, m := range a.Models {
			loading = loading || m.Loading
			for _, p := range m.Prefilling {
				preTPS += p.Speed
			}
			for _, g := range m.Generating {
				decTPS += g.TPS
			}
		}
		return preTPS, decTPS, int(a.Active), int(a.Waiting), loading
	}
	if st := s.Status; st != nil {
		return 0, 0, int(st.Active), int(st.Waiting), st.Loading > 0
	}
	return
}

// record appends one history sample. Caller holds lock.
func (s *State) record(now time.Time) {
	sm := Sample{T: now, GPU: -1, OMLXMem: -1, SwapOutRate: s.Sys.SwapOutRate}
	sm.PreTPS, sm.DecTPS, sm.Active, sm.Waiting, sm.LoadingModel = s.liveTotals()
	sm.Busy = sm.Active > 0
	if s.Sys.OK {
		sm.GPU = s.Sys.GPUUtil
	}
	if cur, _ := s.omlxMemory(); cur > 0 {
		sm.OMLXMem = cur
	}
	s.History = tail(append(s.History, sm), maxHistory)
}

// omlxMemory returns oMLX's memory use and ceiling from the best available source.
func (s *State) omlxMemory() (used, ceiling float64) {
	switch {
	case s.Activity != nil && s.Activity.MemMax > 0:
		return s.Activity.MemUsed, s.Activity.MemMax
	case s.Status != nil && s.Status.MemMax > 0:
		return s.Status.MemUsed, s.Status.MemMax
	case s.Health != nil && s.Health.Pool != nil:
		return s.Health.Pool.ModelMemory, s.Health.Pool.Ceiling
	}
	return 0, 0
}

// snapshot returns the session counters from whichever endpoint has them.
func (s *State) snapshot() *Snapshot {
	if s.Stats != nil {
		return &s.Stats.Snapshot
	}
	if s.Status != nil {
		return &s.Status.Snapshot
	}
	return nil
}

func (s *State) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Recent, s.Events, s.Signals, s.Loads, s.History = nil, nil, nil, nil, nil
	s.Restores, s.RestoredTk = 0, 0
	s.Started = time.Now()
}

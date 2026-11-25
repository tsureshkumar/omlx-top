package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ---- oMLX API types ----
// Numbers are float64 because the server mixes ints, floats and nulls.

// Health is GET /health (unauthenticated).
type Health struct {
	Status       string `json:"status"` // healthy, loading
	DefaultModel string `json:"default_model"`
	Pool         *struct {
		ModelCount  float64 `json:"model_count"`
		LoadedCount float64 `json:"loaded_count"`
		Ceiling     float64 `json:"final_ceiling"`
		ModelMemory float64 `json:"current_model_memory"`
	} `json:"engine_pool"`
}

// Snapshot is the session counters shared by /api/status and /admin/api/stats.
type Snapshot struct {
	Requests   float64 `json:"total_requests"`
	PromptTok  float64 `json:"total_prompt_tokens"`
	OutTok     float64 `json:"total_completion_tokens"`
	CachedTok  float64 `json:"total_cached_tokens"`
	CacheEff   float64 `json:"cache_efficiency"` // percent
	PrefillTPS float64 `json:"avg_prefill_tps"`
	DecodeTPS  float64 `json:"avg_generation_tps"`
	Uptime     float64 `json:"uptime_seconds"`
}

// Status is GET /api/status (any API key).
type Status struct {
	Snapshot
	Version      string   `json:"version"`
	LoadedModels []string `json:"loaded_models"`
	Active       float64  `json:"active_requests"`
	Waiting      float64  `json:"waiting_requests"`
	Loading      float64  `json:"models_loading"`
	MemUsed      float64  `json:"model_memory_used"`
	MemMax       float64  `json:"model_memory_max"`
}

type Waiting struct {
	ID        string  `json:"request_id"`
	Position  float64 `json:"queue_position"`
	Elapsed   float64 `json:"elapsed_seconds"`
	PromptTok float64 `json:"prompt_tokens"`
}

type Prefilling struct {
	ID        string  `json:"request_id"`
	Processed float64 `json:"processed"`
	Total     float64 `json:"total"`
	Speed     float64 `json:"speed"`
	ETA       float64 `json:"eta"`
	Elapsed   float64 `json:"elapsed"`
	Phase     string  `json:"phase"`
	Detail    string  `json:"detail"`
	CachedTok float64 `json:"cached_tokens"`
	PromptTok float64 `json:"prompt_tokens"`
}

type Generating struct {
	ID        string   `json:"request_id"`
	Elapsed   float64  `json:"elapsed_seconds"`
	Tokens    float64  `json:"generated_tokens"`
	TPS       float64  `json:"tokens_per_second"`
	IdleAge   *float64 `json:"last_activity_age_seconds"`
	PromptTok float64  `json:"prompt_tokens"`
	MaxTokens float64  `json:"max_tokens"`
}

// ActiveModel is a loaded or loading model with its in-flight requests.
type ActiveModel struct {
	ID           string       `json:"id"`
	EstSize      float64      `json:"estimated_size"`
	ActualSize   float64      `json:"actual_size"`
	Pinned       bool         `json:"pinned"`
	Loading      bool         `json:"is_loading"`
	LoadElapsed  float64      `json:"loading_elapsed_seconds"`
	LoadEstimate float64      `json:"loading_estimated_seconds"`
	Active       float64      `json:"active_requests"`
	WaitingN     float64      `json:"waiting_requests"`
	Waiting      []Waiting    `json:"waiting"`
	Prefilling   []Prefilling `json:"prefilling"`
	Generating   []Generating `json:"generating"`
	Idle         *float64     `json:"idle_seconds"`
	TTL          *float64     `json:"ttl_remaining_seconds"`
}

type MemPressure struct {
	Enabled bool    `json:"enabled"`
	Current float64 `json:"current_bytes"`
	Soft    float64 `json:"soft_bytes"`
	Hard    float64 `json:"hard_bytes"`
	Level   string  `json:"pressure_level"`
}

type Activity struct {
	Models   []ActiveModel `json:"models"`
	MemUsed  float64       `json:"model_memory_used"`
	MemMax   float64       `json:"model_memory_max"`
	Pressure MemPressure   `json:"memory_pressure"`
	Active   float64       `json:"total_active_requests"`
	Waiting  float64       `json:"total_waiting_requests"`
}

// CacheCounters are the cumulative paged-cache counters of one loaded model.
type CacheCounters struct {
	ID             string  `json:"id"`
	BlockSize      float64 `json:"block_size"`
	IndexedBlocks  float64 `json:"indexed_blocks"`
	Files          float64 `json:"num_files"`
	Size           float64 `json:"total_size_bytes"`
	MaxSize        float64 `json:"max_size_bytes"`
	HotMax         float64 `json:"hot_cache_max_bytes"`
	HotSize        float64 `json:"hot_cache_size_bytes"`
	HotEntries     float64 `json:"hot_cache_entries"`
	Hits           float64 `json:"hits"`
	Misses         float64 `json:"misses"`
	Evictions      float64 `json:"evictions"`
	Saves          float64 `json:"saves"`
	Persisted      float64 `json:"saves_persisted"`
	Loads          float64 `json:"loads"`
	Errors         float64 `json:"errors"`
	WriteDrops     float64 `json:"ssd_write_drops"`
	InlineWrites   float64 `json:"ssd_inline_write_fallbacks"`
	HotHits        float64 `json:"hot_cache_hits"`
	HotEvictions   float64 `json:"hot_cache_evictions"`
	HotPromotions  float64 `json:"hot_cache_promotions"`
	PartialSkips   float64 `json:"partial_block_skips"`
	PartialSkipped float64 `json:"partial_tokens_skipped"`
}

type RuntimeCache struct {
	Dir         string          `json:"ssd_cache_dir"`
	Unavailable bool            `json:"ssd_cache_unavailable"`
	Files       float64         `json:"total_num_files"`
	Size        float64         `json:"total_size_bytes"`
	MaxSize     float64         `json:"disk_max_bytes"`
	HotMax      float64         `json:"hot_cache_max_bytes"`
	HotSize     float64         `json:"hot_cache_size_bytes"`
	HotEntries  float64         `json:"hot_cache_entries"`
	Models      []CacheCounters `json:"models"`
}

// AdminStats is GET /admin/api/stats. The response also contains the server's
// API key; it has no field here so it is discarded while decoding.
type AdminStats struct {
	Snapshot
	Engines map[string]struct {
		Version string `json:"version"`
	} `json:"engines"`
	Cache *RuntimeCache `json:"runtime_cache"`
}

// ModelInfo is one entry of GET /v1/models/status.
type ModelInfo struct {
	ID         string  `json:"id"`
	Alias      string  `json:"model_alias"`
	Loaded     bool    `json:"loaded"`
	Loading    bool    `json:"is_loading"`
	EstSize    float64 `json:"estimated_size"`
	ActualSize float64 `json:"actual_size"`
	Pinned     bool    `json:"pinned"`
	Engine     string  `json:"engine_type"`
	Type       string  `json:"model_type"`
	Format     string  `json:"model_format"`
	CtxModel   float64 `json:"model_context_length"`
	CtxMax     float64 `json:"max_context_window"`
	MaxTokens  float64 `json:"max_tokens"`
	LastAccess float64 `json:"last_access"`
}

type UsageRow struct {
	ModelID    string   `json:"model_id"`
	Hour       float64  `json:"timestamp_hour"`
	Date       string   `json:"date"`
	Requests   float64  `json:"requests"`
	PromptTok  float64  `json:"prompt_tokens"`
	OutTok     float64  `json:"completion_tokens"`
	CachedTok  float64  `json:"cached_tokens"`
	PrefillSec float64  `json:"prefill_seconds"`
	GenSec     float64  `json:"generation_seconds"`
	CacheEff   float64  `json:"cache_efficiency"` // fraction
	PrefillTPS *float64 `json:"prefill_tps"`
	DecodeTPS  *float64 `json:"generation_tps"`
	AvgReqSec  *float64 `json:"average_request_seconds"`
}

// Usage is GET /admin/api/usage.
type Usage struct {
	Range   string     `json:"range"`
	Enabled bool       `json:"enabled"`
	Totals  UsageRow   `json:"totals"`
	Models  []UsageRow `json:"models"`
	Daily   []UsageRow `json:"daily"`
	Hourly  []UsageRow `json:"hourly"`
}

// ---- client ----

type httpError struct {
	status int
	path   string
}

func (e *httpError) Error() string { return fmt.Sprintf("%s: HTTP %d", e.path, e.status) }

func isAuthErr(err error) bool {
	var he *httpError
	return errors.As(err, &he) && (he.status == 401 || he.status == 403 || he.status == 429)
}

// API is a read-only oMLX client: it only ever sends GET requests.
type API struct {
	base string
	key  string
	c    *http.Client
}

func NewAPI(base, key string) *API {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // the key goes to the server directly, never through a proxy
	return &API{base: base, key: key, c: &http.Client{
		Transport: tr,
		// Never follow redirects: the key must not travel anywhere else.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

const maxBody = 16 << 20

func (a *API) do(path string, auth bool, timeout time.Duration) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+path, nil)
	if err != nil {
		return 0, nil, err
	}
	if auth && a.key != "" {
		req.Header.Set("Authorization", "Bearer "+a.key)
	}
	resp, err := a.c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	return resp.StatusCode, b, err
}

func (a *API) get(path string, timeout time.Duration, v any) error {
	code, b, err := a.do(path, true, timeout)
	if err != nil {
		return err
	}
	if code != 200 {
		return &httpError{code, path}
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

var errNotOMLX = errors.New("a server answers on this port but it is not oMLX (is Ollama using it?)")

// health identifies the server. No key is sent.
func (a *API) health() (*Health, error) {
	code, b, err := a.do("/health", false, 3*time.Second)
	if err != nil {
		return nil, err
	}
	var h Health
	if json.Unmarshal(b, &h) != nil || (code != 200 && code != 503) || (h.Status != "healthy" && h.Status != "loading") {
		return nil, errNotOMLX
	}
	return &h, nil
}

// gate holds back authenticated calls until the server is confirmed to be oMLX
// and backs off after a rejected key, so the server log is not flooded.
type gate struct {
	mu       sync.Mutex
	isOMLX   bool
	until    map[string]time.Time
	disabled time.Duration
}

func newGate() *gate { return &gate{until: map[string]time.Time{}, disabled: time.Minute} }

func (g *gate) set(ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.isOMLX = ok
}

func (g *gate) allow(group string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.isOMLX && time.Now().After(g.until[group])
}

func (g *gate) result(group string, err error) {
	if isAuthErr(err) {
		g.mu.Lock()
		g.until[group] = time.Now().Add(g.disabled)
		g.mu.Unlock()
	}
}

func every(d time.Duration, f func()) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		f()
		<-t.C
	}
}

// Poller feeds State from the HTTP API.
type Poller struct {
	a    *API
	st   *State
	g    *gate
	poll time.Duration

	lastStats, lastStatus, lastModels, lastUsage time.Time
}

func NewPoller(a *API, st *State, poll time.Duration) *Poller {
	return &Poller{a: a, st: st, g: newGate(), poll: poll}
}

// live refreshes health and in-flight requests; it runs at the poll interval.
func (p *Poller) live() {
	t0 := time.Now()
	h, err := p.a.health()
	p.g.set(err == nil)
	p.st.setHealth(h, err, time.Since(t0))
	if err != nil {
		return
	}
	if p.g.allow("admin") {
		var r struct {
			Active *Activity `json:"active_models"`
		}
		t0 = time.Now()
		err := p.a.get("/admin/api/activity", 3*time.Second, &r)
		p.g.result("admin", err)
		p.st.setAccess(err)
		if err == nil && r.Active != nil {
			p.st.setActivity(r.Active, t0)
		}
	}
}

// slow refreshes everything that changes slowly or is expensive to produce.
func (p *Poller) slow() {
	now := time.Now()
	due := func(last *time.Time, d time.Duration) bool {
		if now.Sub(*last) < d {
			return false
		}
		*last = now
		return true
	}
	admin := p.st.hasAdmin()
	// /api/status carries the version, and is the fallback without admin access.
	statusEvery := time.Minute
	if !admin {
		statusEvery = p.poll
	}
	if p.g.allow("api") && due(&p.lastStatus, statusEvery) {
		var s Status
		err := p.a.get("/api/status", 5*time.Second, &s)
		p.g.result("api", err)
		p.st.setStatus(&s, err)
	}
	if p.g.allow("api") && due(&p.lastModels, 10*time.Second) {
		var m struct {
			Models []ModelInfo `json:"models"`
		}
		err := p.a.get("/v1/models/status", 5*time.Second, &m)
		p.g.result("api", err)
		if err == nil {
			p.st.setModels(m.Models)
		}
	}
	if admin && p.g.allow("admin") && due(&p.lastStats, max(3*time.Second, p.poll)) {
		var s AdminStats
		err := p.a.get("/admin/api/stats", 10*time.Second, &s)
		p.g.result("admin", err)
		p.st.setStats(&s, err)
	}
	if admin && p.g.allow("admin") && due(&p.lastUsage, 30*time.Second) {
		for _, rng := range []string{"today", "7d"} {
			var u Usage
			err := p.a.get("/admin/api/usage?include_details=true&range="+rng, 5*time.Second, &u)
			p.g.result("admin", err)
			if err != nil {
				break
			}
			p.st.setUsage(rng, &u)
		}
	}
}

func (p *Poller) Run() {
	go every(p.poll, p.live)
	every(p.poll, p.slow)
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i > 0 && len(s) > 60 {
		s = s[i+2:]
	}
	return s
}

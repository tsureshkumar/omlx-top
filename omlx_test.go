package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// ---- config ----

func TestConfig(t *testing.T) {
	home := t.TempDir()
	base := filepath.Join(home, "data")
	os.MkdirAll(base, 0o755)
	os.WriteFile(filepath.Join(base, "settings.json"), []byte(`{
		"server":{"host":"0.0.0.0","port":11434},
		"auth":{"api_key":"secret-main"},
		"scheduler":{"max_concurrent_requests":8},
		"cache":{"enabled":true,"hot_cache_max_size":"0","ssd_cache_dir":null},
		"logging":{"log_dir":null}}`), 0o600)
	boot := filepath.Join(home, "Library", "Application Support", "oMLX")
	os.MkdirAll(boot, 0o755)
	os.WriteFile(filepath.Join(boot, "base-path"), []byte(base+"\n"), 0o644)
	noEnv := func(string) string { return "" }

	c, err := loadConfig("", "", "", "", noEnv, home)
	if err != nil {
		t.Fatal(err)
	}
	if c.Base != base || c.URL != "http://127.0.0.1:11434" || c.APIKey != "secret-main" || c.MaxConcurrent != 8 {
		t.Fatalf("unexpected config: base=%q url=%q source=%q max=%d", c.Base, c.URL, c.KeySource, c.MaxConcurrent)
	}
	if c.LogPath != filepath.Join(base, "logs", "server.log") || c.CacheDir != filepath.Join(base, "cache") {
		t.Fatalf("paths: %q %q", c.LogPath, c.CacheDir)
	}

	// The local key must never be offered to another machine or another port.
	for _, host := range []string{"example.com:11434", "192.168.1.50:11434", "localhost:9999", "http://10.0.0.2"} {
		c, err := loadConfig("", host, "", "", noEnv, home)
		if err != nil {
			t.Fatal(err)
		}
		if c.APIKey != "" {
			t.Errorf("%s: settings.json key would be sent", host)
		}
	}
	for _, host := range []string{"localhost", "127.0.0.1:11434", "[::1]:11434"} {
		if c, _ := loadConfig("", host, "", "", noEnv, home); c.APIKey != "secret-main" {
			t.Errorf("%s: expected the local key, got source %q url %q", host, c.KeySource, c.URL)
		}
	}
	// An explicit key is honoured for any host.
	if c, _ := loadConfig("", "example.com", "mine", "", noEnv, home); c.APIKey != "mine" || c.URL != "http://example.com:11434" {
		t.Errorf("explicit key: %q %q", c.KeySource, c.URL)
	}
}

// ---- client ----

type fakeServer struct {
	mu     sync.Mutex
	routes map[string]string
	status map[string]int
	authed []string
	calls  map[string]int
}

func (f *fakeServer) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	f.calls[p]++
	if r.Method != http.MethodGet {
		return nil, fmt.Errorf("monitor must be read-only, got %s", r.Method)
	}
	if r.Header.Get("Authorization") != "" {
		f.authed = append(f.authed, p)
	}
	body, ok := f.routes[p]
	code := f.status[p]
	if code == 0 {
		code = 200
		if !ok {
			code = 404
		}
	}
	return &http.Response{StatusCode: code, Header: http.Header{"Location": {"http://evil.example/"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

const healthJSON = `{"status":"healthy","default_model":"mlx-community--Qwen3.8-27B-4bit","engine_pool":{"model_count":3,"loaded_count":1,"final_ceiling":86114770944,"current_model_memory":16500000000},"mcp":null}`

const activityJSON = `{"models":[{"id":"mlx-community--Qwen3.8-27B-4bit","estimated_size":16000000000,"actual_size":16500000000,"pinned":false,"is_loading":false,"loading_elapsed_seconds":null,"loading_estimated_seconds":null,"active_requests":2,"waiting_requests":1,
 "waiting":[{"request_id":"cccccccc-3","queue_position":1,"elapsed_seconds":4.2,"prompt_tokens":900}],
 "activities":[],
 "prefilling":[{"request_id":"bbbbbbbb-2","processed":12288,"total":40000,"speed":812.5,"eta":34.1,"elapsed":15.1,"phase":"prefill","detail":null,"cached_tokens":2048}],
 "generating":[{"request_id":"aaaaaaaa-1","elapsed_seconds":12.5,"generated_tokens":412,"tokens_per_second":33.4,"last_activity_age_seconds":0.1,"prompt_tokens":8000,"max_tokens":4096}],
 "request_abort":true,"idle_seconds":0.0,"ttl_remaining_seconds":null,"cluster":null}],
 "model_memory_used":52000000000,"model_memory_max":86114770944,
 "memory_pressure":{"enabled":true,"current_bytes":52000000000,"soft_bytes":73000000000,"hard_bytes":81000000000,"current_formatted":"48.4GB","pressure_level":"ok"},
 "total_active_requests":2,"total_waiting_requests":1}`

const statsJSON = `{"total_tokens_served":150000,"total_cached_tokens":9000,"cache_efficiency":7.5,"total_prompt_tokens":120000,"total_completion_tokens":30000,"total_requests":12,"avg_prefill_tps":650.0,"avg_generation_tps":31.0,"uptime_seconds":5400.5,
 "host":"0.0.0.0","port":11434,"api_key":"LEAKED-KEY","cli_prefix":"omlx","engines":{"mlx":{"name":"mlx","version":"0.30.0","commit":null,"url":null}},
 "active_models":%s,
 "runtime_cache":{"base_path":"/x","ssd_cache_dir":"/x/cache","ssd_cache_unavailable":false,"total_num_files":321,"total_size_bytes":5000000000,"disk_max_bytes":100000000000,"hot_cache_max_bytes":0,"hot_cache_size_bytes":0,"hot_cache_entries":0,"effective_block_sizes":[2048],
  "models":[{"id":"mlx-community--Qwen3.8-27B-4bit","block_size":2048,"indexed_blocks":300,"indexed_blocks_display":"300","has_sub_block_cache":false,"num_files":321,"total_size_bytes":5000000000,"max_size_bytes":100000000000,"hits":40,"misses":12,"evictions":0,"saves":321,"saves_persisted":321,"loads":40,"errors":0,"ssd_write_drops":0,"ssd_inline_write_fallbacks":2}]}}`

const statusJSON = `{"status":"ok","version":"0.8.3","uptime_seconds":5400.5,"models_discovered":3,"models_loaded":1,"models_loading":0,"loaded_models":["a"],"total_requests":12,"active_requests":2,"waiting_requests":1,"total_prompt_tokens":120000,"total_completion_tokens":30000,"total_cached_tokens":9000,"cache_efficiency":7.5,"avg_prefill_tps":650.0,"avg_generation_tps":31.0,"model_memory_used":16500000000,"model_memory_max":null}`

const modelsJSON = `{"final_ceiling":86114770944,"models":[{"id":"mlx-community--Qwen3.8-27B-4bit","loaded":true,"is_loading":false,"estimated_size":16000000000,"actual_size":16500000000,"pinned":false,"engine_type":"vlm","model_type":"vlm","model_format":"mlx","model_context_length":262144,"max_context_window":65535,"max_tokens":65535,"last_access":1795600000.5},
 {"id":"mlx-community--parakeet-tdt-0.6b-v3","loaded":false,"estimated_size":2500000000,"engine_type":"audio_stt","model_type":"audio_stt","last_access":null}]}`

const usageJSON = `{"range":"today","enabled":true,"totals":{"requests":12,"prompt_tokens":120000,"completion_tokens":30000,"cached_tokens":9000,"prefill_seconds":400,"generation_seconds":200,"cache_efficiency":0.075,"prefill_tps":277.5,"generation_tps":150,"average_request_seconds":50},
 "models":[{"model_id":"mlx-community--Qwen3.8-27B-4bit","requests":12,"prompt_tokens":120000,"completion_tokens":30000,"cache_efficiency":0.075,"generation_tps":null,"prefill_tps":null,"average_request_seconds":null}],
 "daily":[{"date":"2026-11-25","requests":12,"prompt_tokens":120000,"completion_tokens":30000}],
 "hourly":[{"timestamp_hour":1795600800,"requests":5,"prompt_tokens":50000,"completion_tokens":10000,"generation_tps":30.5},{"timestamp_hour":1795604400,"requests":7,"prompt_tokens":70000,"completion_tokens":20000}]}`

func newFake() *fakeServer {
	return &fakeServer{calls: map[string]int{}, status: map[string]int{}, routes: map[string]string{
		"/health": healthJSON, "/api/status": statusJSON, "/v1/models/status": modelsJSON,
		"/admin/api/activity": `{"active_models":` + activityJSON + `}`,
		"/admin/api/stats":    fmt.Sprintf(statsJSON, activityJSON),
		"/admin/api/usage":    usageJSON,
	}}
}

func poller(f *fakeServer) (*Poller, *State) {
	st := NewState(Config{URL: "http://127.0.0.1:11434", MaxConcurrent: 2, CacheEnabled: true, HotCacheMax: "0", KeySource: "settings.json"})
	a := NewAPI(st.Cfg.URL, "k")
	a.c.Transport = f
	return NewPoller(a, st, time.Second), st
}

func TestPollFullAccess(t *testing.T) {
	f := newFake()
	p, st := poller(f)
	p.live()
	p.slow()
	if !st.Up || !st.AdminOK || st.Version != "0.8.3" || len(st.Models) != 2 || st.Stats == nil || st.Usage["7d"] == nil {
		t.Fatalf("up=%v admin=%v version=%q models=%d stats=%v apiErr=%q statsErr=%q", st.Up, st.AdminOK, st.Version, len(st.Models), st.Stats != nil, st.APIErr, st.StatsErr)
	}
	if len(st.Live) != 3 {
		t.Fatalf("live requests = %d", len(st.Live))
	}
	if r := st.Live["bbbbbbbb-2"]; r.Phase != "prefill" || r.PreTPS != 812.5 || r.CachedTok != 2048 {
		t.Fatalf("prefill row: %+v", r)
	}
	if st.Stats.OutTok != 30000 || st.Stats.Cache.Models[0].InlineWrites != 2 {
		t.Fatalf("stats not decoded: %+v", st.Stats.Snapshot)
	}
	if b, _ := json.Marshal(st.Stats); strings.Contains(string(b), "LEAKED-KEY") {
		t.Fatal("the server API key was retained")
	}
	for _, path := range f.authed {
		if path == "/health" {
			t.Fatal("key sent to /health")
		}
	}
}

func TestKeyNotSentToForeignServer(t *testing.T) {
	f := &fakeServer{calls: map[string]int{}, status: map[string]int{}, routes: map[string]string{"/health": "Ollama is running"}}
	p, st := poller(f)
	p.live()
	p.slow()
	if len(f.authed) != 0 {
		t.Fatalf("key sent to a server that is not oMLX: %v", f.authed)
	}
	if st.Up || !st.NotOMLX {
		t.Fatalf("up=%v notOMLX=%v", st.Up, st.NotOMLX)
	}
	if fs := Analyze(st, time.Now()); len(fs) == 0 || !strings.Contains(fs[0].Title, "not oMLX") {
		t.Fatalf("findings: %+v", fs)
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	f := newFake()
	f.status["/api/status"] = 302
	p, _ := poller(f)
	var s Status
	if err := p.a.get("/api/status", time.Second, &s); err == nil || f.calls["/"] != 0 {
		t.Fatalf("redirect followed: err=%v calls=%v", err, f.calls)
	}
}

func TestRejectedKeyBacksOffAndFallsBack(t *testing.T) {
	f := newFake()
	f.status["/admin/api/activity"] = 401
	p, st := poller(f)
	for range 5 {
		p.live()
		p.slow()
		p.lastStatus = time.Time{}
	}
	if n := f.calls["/admin/api/activity"] + f.calls["/admin/api/stats"] + f.calls["/admin/api/usage"]; n != 1 {
		t.Fatalf("admin API called %d times after a rejection", n)
	}
	if st.AdminOK || st.Status == nil {
		t.Fatalf("admin=%v status=%v", st.AdminOK, st.Status)
	}
	if _, _, active, waiting, _ := st.liveTotals(); active != 2 || waiting != 1 {
		t.Fatalf("basic totals: %d %d", active, waiting)
	}
}

func TestServerGoesAway(t *testing.T) {
	f := newFake()
	p, st := poller(f)
	p.live()
	f.routes = map[string]string{}
	p.live()
	if st.Up || st.Activity != nil || len(st.Live) != 0 {
		t.Fatal("stale data kept after the server went away")
	}
}

type timeout struct{}

func (timeout) Error() string   { return "timeout" }
func (timeout) Timeout() bool   { return true }
func (timeout) Temporary() bool { return true }

func TestSlowHealthKeepsData(t *testing.T) {
	f := newFake()
	p, st := poller(f)
	p.live()
	for range 2 {
		st.setHealth(nil, timeout{}, 3*time.Second)
	}
	if !st.Up || len(st.Live) != 3 {
		t.Fatal("two slow health checks must not drop live data")
	}
	st.setHealth(nil, timeout{}, 3*time.Second)
	if st.Up {
		t.Fatal("repeated timeouts mean the server is down")
	}
}

func TestRequeueIsNotAbort(t *testing.T) {
	st := NewState(Config{})
	st.feedLog(logLine(time.Now(), "omlx.process_memory_enforcer", "WARNING", "Hard memory pressure: storing and requeueing 1 request(s) on 'm' instead of aborting"))
	since := time.Now().Add(-time.Minute)
	if a, _ := st.signalsSince("mem-abort", since); a != 0 {
		t.Fatal("counted as an abort")
	}
	if r, _ := st.signalsSince("requeue", since); r != 1 {
		t.Fatal("not counted as a requeue")
	}
}

// ---- log ----

func logLine(t time.Time, logger, level, msg string) string {
	return fmt.Sprintf("%s - %s - %s - [-] - %s", t.Format("2006-01-02 15:04:05,000"), logger, level, msg)
}

// Messages follow the format strings in oMLX 0.8.3.
var sampleLog = []struct{ logger, level, msg string }{
	{"omlx.server", "INFO", "Server initialized with 6 models"},
	{"omlx.process_memory_enforcer", "WARNING", "Metal cap (107.5GB, Apple max_recommended_working_set_size) is below the oMLX static ceiling (120.0GB); leaving Apple's default Metal cap active because iogpu.wired_limit_mb is unset. Raise it with: sudo sysctl iogpu.wired_limit_mb=122880"},
	{"httpx2", "INFO", `HTTP Request: GET https://huggingface.co/api "HTTP/1.1 500 Error"`},
	{"omlx.engine_pool", "INFO", "Loading model: Qwen3.8-27B-4bit"},
	{"omlx.engine_pool", "INFO", "Loaded model: Qwen3.8-27B-4bit (actual: 15.42GB, local estimate: 15.00GB, full model: 15.00GB, total: 15.42GB)"},
	{"omlx.scheduler", "INFO", "Prefix cache restore for abc-123: source=paged cached=4096 suffix=100 blocks=2 lookup=0.100ms reconstruct=2.000ms promote=True"},
	{"omlx.scheduler", "INFO", "prefix cache: request abc-124 re-prefills 9000 of 10000 tokens (reused 1000); closest stored sequence x shares the first 1100 of 9000 comparable tokens before diverging"},
	{"omlx.scheduler", "INFO", "Prefill throttled for abc-124: chunk 2048 -> 256 (usage 70.10GB vs sizing target 72.00GB, soft ceiling 80.20GB, per_token=120.0KB, floor=32). Prefill runs slower until usage drops. reduce context length."},
	{"omlx.process_memory_enforcer", "INFO", "Memory pressure level: ok -> soft (current=70.1GB, soft=68.2GB, hard=76.2GB, ceiling=80.2GB)"},
	{"omlx.engine_pool", "INFO", "Evicting idle model 'a' for prefill headroom on 'b' (request=abc, projected=1GB > target=1GB)"},
	{"omlx.engine_pool", "INFO", "Evicting 'c' to fit 'd' under the admission soft target (3GB > 2GB)"},
	{"omlx.process_memory_enforcer", "WARNING", "Evicting model 'e' (pressure=soft)"},
	{"omlx.scheduler", "WARNING", "Memory pressure: requeued 2 request(s) in 0.4s; tokens kept in the SSD cache: abc=100"},
	{"omlx.scheduler", "WARNING", "Preflight rejected (90000 tokens, cached=0, request_id=zz): too large"},
	{"omlx.server", "WARNING", "POST /v1/chat/completions → 400: prompt too long"},
	{"omlx.server", "INFO", "Chat completion: model=Qwen3.8-27B-4bit, 500 tokens in 20.00s (31.2 tok/s), prompt: 9000, finish_reason=length, max_tokens=500, request_max_tokens=500, stream_model_ttft=4.00s, stream_visible_ttft=unavailable"},
	{"omlx.server", "INFO", "Anthropic message: model=Qwen3.8-27B-4bit, 100 tokens in 5.00s (20.0 tok/s)"},
	{"omlx.scheduler", "INFO", "vlm_mtp stats: request=a finish=stop rounds=10 accepted=30/40 (75.0%) tokens_per_round=4.00 emitted=40 block_size=4"},
}

func TestLogParser(t *testing.T) {
	st := NewState(Config{})
	base := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	for i, l := range sampleLog {
		st.feedLog(logLine(base.Add(time.Duration(i)*time.Second), l.logger, l.level, l.msg))
	}
	st.feedLog("Traceback (most recent call last):")
	since := base.Add(-time.Second)
	for kind, want := range map[string]int{"throttle": 1, "pressure": 1, "evict": 3, "requeue": 1, "reject": 1, "reprefill": 1, "http-error": 1, "truncated": 1, "load": 1} {
		if n, _ := st.signalsSince(kind, since); n != want {
			t.Errorf("signal %s = %d, want %d", kind, n, want)
		}
	}
	if len(st.Recent) != 2 {
		t.Fatalf("completions = %d", len(st.Recent))
	}
	r := st.Recent[0]
	if r.API != "chat" || r.Model != "Qwen3.8-27B-4bit" || r.OutTok != 500 || r.PromptTok != 9000 || r.TTFT != 4 || r.TPS != 31.2 || r.Finish != "length" || !r.End.Equal(base.Add(15*time.Second)) {
		t.Errorf("completion: %+v", r)
	}
	if st.Recent[1].API != "msgs" || st.Recent[1].PromptTok != 0 {
		t.Errorf("anthropic completion: %+v", st.Recent[1])
	}
	if len(st.Loads) != 1 || st.Loads[0].Dur != time.Second || st.Loads[0].Size != "15.42GB" {
		t.Errorf("loads: %+v", st.Loads)
	}
	if st.MetalCapMB != 122880 || st.Restores != 1 || st.RestoredTk != 4096 || st.SpecDecode != "30/40 (75.0%)" {
		t.Errorf("metal=%d restores=%d %v spec=%q", st.MetalCapMB, st.Restores, st.RestoredTk, st.SpecDecode)
	}
	for _, e := range st.Events {
		if strings.Contains(e.Msg, "huggingface") {
			t.Error("download noise became an event")
		}
	}
}

func TestTerminalEscapesRemoved(t *testing.T) {
	st := NewState(Config{})
	st.feedLog(logLine(time.Now(), "omlx.server", "WARNING", "GET /\x1b]0;pwned\x07\x1b[2J → 404: nope‮"))
	if len(st.Events) != 1 || strings.ContainsAny(st.Events[0].Msg, "\x1b\x07‮") {
		t.Fatalf("unsafe event: %+v", st.Events)
	}
}

func TestTailFollowsRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	write := func(msg string, flag int) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|flag, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		fmt.Fprintln(f, logLine(time.Now(), "omlx.server", "WARNING", msg))
	}
	write("first", os.O_APPEND)
	st := NewState(Config{})
	go tailFile(path, 1<<20, st)
	wait := func(n int) {
		t.Helper()
		for range 100 {
			st.mu.RLock()
			got := len(st.Events)
			st.mu.RUnlock()
			if got >= n {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("did not see %d events", n)
	}
	wait(1)
	write("second", os.O_APPEND)
	wait(2)
	os.Rename(path, path+".2026-11-24")
	write("third", os.O_APPEND)
	wait(3)
}

// ---- request tracking ----

func act(prefill, gen []string, waiting []string) *Activity {
	m := ActiveModel{ID: "m"}
	for _, id := range waiting {
		m.Waiting = append(m.Waiting, Waiting{ID: id, Position: 1, Elapsed: 3, PromptTok: 1000})
	}
	for _, id := range prefill {
		m.Prefilling = append(m.Prefilling, Prefilling{ID: id, Processed: 100, Total: 600, Speed: 200, Elapsed: 2, CachedTok: 400})
	}
	for _, id := range gen {
		m.Generating = append(m.Generating, Generating{ID: id, Tokens: 90, TPS: 30, Elapsed: 3})
	}
	return &Activity{Models: []ActiveModel{m}}
}

func TestLifecycleMergesWithLog(t *testing.T) {
	for _, logFirst := range []bool{true, false} {
		st := NewState(Config{})
		t0 := time.Now()
		st.setActivity(act(nil, nil, []string{"r"}), t0)
		st.setActivity(act([]string{"r"}, nil, nil), t0.Add(time.Second))
		st.setActivity(act(nil, nil, nil), t0.Add(2*time.Second)) // gap between phases
		if len(st.Live) != 1 || len(st.Recent) != 0 {
			t.Fatal("one missed poll must not finish a request")
		}
		st.setActivity(act(nil, []string{"r"}, nil), t0.Add(3*time.Second))
		logIt := func() {
			st.feedLog(logLine(t0.Add(4*time.Second), "omlx.server", "INFO", "Chat completion: model=m, 100 tokens in 4.00s (30.0 tok/s), prompt: 1000, finish_reason=stop"))
		}
		if logFirst {
			logIt()
		}
		st.setActivity(act(nil, nil, nil), t0.Add(4*time.Second))
		st.setActivity(act(nil, nil, nil), t0.Add(5*time.Second))
		if !logFirst {
			logIt()
		}
		if len(st.Live) != 0 || len(st.Recent) != 1 {
			t.Fatalf("logFirst=%v live=%d recent=%d", logFirst, len(st.Live), len(st.Recent))
		}
		r := st.Recent[0]
		if !r.Logged || !r.Seen || r.QueueSec != 3 || r.PreSec != 2 || r.CachedTok != 400 || r.OutTok != 100 {
			t.Fatalf("logFirst=%v merged: %+v", logFirst, r)
		}
	}
}

// ---- analyzer ----

func titles(f []Finding) string {
	var s []string
	for _, x := range f {
		s = append(s, x.Title)
	}
	return strings.Join(s, " | ")
}

func healthy(now time.Time) *State {
	st := NewState(Config{CacheEnabled: true, MaxConcurrent: 2})
	st.Up, st.AdminOK = true, true
	st.Health = &Health{Status: "healthy"}
	st.Activity = &Activity{MemUsed: 10 << 30, MemMax: 80 << 30, Pressure: MemPressure{Level: "ok"}}
	st.Sys = SysInfo{OK: true, T: now, GPUUtil: 90, Pressure: 1, DiskFree: 500 << 30, WiredLimit: -1}
	for i := range 20 {
		st.History = append(st.History, Sample{T: now.Add(time.Duration(i-19) * time.Second), GPU: 90})
	}
	return st
}

func TestAnalyzeHealthy(t *testing.T) {
	now := time.Now()
	if f := Analyze(healthy(now), now); len(f) != 0 {
		t.Fatalf("false positives: %s", titles(f))
	}
}

func TestAnalyzeRules(t *testing.T) {
	now := time.Now()
	hist := func(f func(*Sample)) func(*State) {
		return func(s *State) {
			for i := range s.History {
				f(&s.History[i])
			}
		}
	}
	signals := func(kind string, n int) func(*State) {
		return func(s *State) {
			for range n {
				s.signal(now.Add(-time.Minute), kind, "x")
			}
		}
	}
	live := func(r LiveReq) func(*State) {
		return func(s *State) {
			if r.Progress.IsZero() {
				r.Progress = now
			}
			s.Live["id"] = &r
		}
	}
	cases := []struct {
		want  string
		setup func(*State)
	}{
		{"not reachable", func(s *State) { s.Up, s.UpErr = false, "refused" }},
		{"Limited data", func(s *State) { s.AdminOK, s.AccessErr = false, "rejected" }},
		{"memory pressure: hard", func(s *State) { s.Activity.Pressure.Level = "hard" }},
		{"close to its memory ceiling", func(s *State) { s.Activity.MemUsed = 75 << 30 }},
		{"swapping", hist(func(x *Sample) { x.SwapOutRate = 50 << 20 })},
		{"pressure is critical", func(s *State) { s.Sys.Pressure = 4 }},
		{"Ollama is competing", func(s *State) { s.Sys.OllamaRSS = 8 << 30 }},
		{"queueing", hist(func(x *Sample) { x.Waiting = 3 })},
		{"GPU mostly idle", hist(func(x *Sample) { x.Busy, x.GPU = true, 4 })},
		{"throttled", signals("throttle", 1)},
		{"requeued", signals("requeue", 1)},
		{"aborted", signals("mem-abort", 1)},
		{"rejected", signals("reject", 1)},
		{"Model churn", signals("evict", 3)},
		{"diverging", signals("reprefill", 3)},
		{"cannot keep up", signals("ssd-saturated", 1)},
		{"cut off", signals("truncated", 2)},
		{"Low prompt cache reuse", func(s *State) { s.Status = &Status{Snapshot: Snapshot{Requests: 9, PromptTok: 90000, CacheEff: 3}} }},
		{"Prompt processing dominates", func(s *State) {
			s.Usage["today"] = &Usage{Totals: UsageRow{Requests: 9, PrefillSec: 90, GenSec: 10}}
		}},
		{"nearly full", func(s *State) { s.Sys.DiskFree = 1 << 30 }},
		{"GPU is running hot", func(s *State) { s.Power = PowerInfo{T: now, GPUTemp: 101} }},
		{"Requests are failing", signals("http-error", 3)},
		{"data is stale", func(s *State) { s.ActAt = now.Add(-time.Minute) }},
		{"log is not readable", func(s *State) { s.LogErr = "denied" }},
		{"capped below", func(s *State) { s.MetalCap, s.MetalCapMB, s.MetalCapB = "cap", 1000, 60<<30 }},
		{"Generation stalled", live(LiveReq{ID: "id", Model: "m", Phase: "generate", IdleAge: 40})},
		{"Prefill not progressing", live(LiveReq{ID: "id", Model: "m", Phase: "prefill", Progress: now.Add(-time.Minute)})},
		{"slower than usual", func(s *State) {
			for range 3 {
				s.Recent = append(s.Recent, &Req{Logged: true, Model: "m", OutTok: 200, TPS: 40})
			}
			live(LiveReq{ID: "id", Model: "m", Phase: "generate", OutTok: 300, TPS: 8})(s)
		}},
	}
	for _, c := range cases {
		s := healthy(now)
		c.setup(s)
		if got := titles(Analyze(s, now)); !strings.Contains(got, c.want) {
			t.Errorf("want %q, got %q", c.want, got)
		}
	}
	// A cap above the ceiling in force is not a bottleneck.
	quiet := healthy(now)
	quiet.MetalCap, quiet.MetalCapB = "cap", 107<<30
	if f := Analyze(quiet, now); len(f) != 0 {
		t.Errorf("non-binding Metal cap reported: %s", titles(f))
	}
	// Old symptoms expire.
	s := healthy(now)
	s.signal(now.Add(-time.Hour), "throttle", "x")
	if f := Analyze(s, now); len(f) != 0 {
		t.Errorf("stale signal reported: %s", titles(f))
	}
}

// ---- system parsers ----

func TestParseSystem(t *testing.T) {
	var si SysInfo
	parseVMStat("Mach Virtual Memory Statistics: (page size of 16384 bytes)\nAnonymous pages: 1000.\nPages purgeable: 100.\nPages wired down: 200.\nPages occupied by compressor: 50.\nSwapins: 7.\nSwapouts: 9.\n", &si)
	if si.MemUsed != 1150*16384 || si.SwapOuts != 9 || si.SwapIns != 7 {
		t.Fatalf("vm_stat: %+v", si)
	}
	ps := `  1  10.0  1000 01:00 omlx-server
  2   1.0  2000 01:00 /Applications/oMLX.app/Contents/MacOS/omlx-cli serve
  3   5.0  3000 01:00 ./omlx-top
  4   2.0  4000 01:00 /usr/local/bin/ollama serve
  5   2.0  1000 01:00 /Applications/Ollama.app/Contents/MacOS/Ollama
  6  50.0  9000 01:00 /Applications/Google Chrome.app/Contents/MacOS/Google Chrome
  7   0.0   100 01:00 vim /Users/x/omlx/README.md
`
	si = SysInfo{}
	total := parsePS(ps, 99, &si)
	if len(si.OMLX) != 2 || si.OMLXRSS != 3000*1024 || si.OllamaRSS != 5000*1024 || total != 70 {
		t.Fatalf("ps: omlx=%+v ollama=%v total=%v", si.OMLX, si.OllamaRSS, total)
	}
	if si.Others[0].Name != "Google Chrome" || si.Others[1].Name != "Ollama" {
		t.Fatalf("others: %+v", si.Others)
	}
	for _, line := range []string{
		`{"temp":{"cpu_temp_avg":55,"gpu_temp_avg":61},"gpu_power":12.5,"gpu_freq_mhz":1398,"sys_power":30}`,
		`{"temp":{"cpu_temp_avg":55,"gpu_temp_avg":61},"gpu_power":12.5,"gpu_usage":[1398,0.9],"sys_power":30}`,
	} {
		if p, ok := parseMacmon([]byte(line)); !ok || p.GPUTemp != 61 || p.GPUMHz != 1398 || p.GPUWatts != 12.5 {
			t.Fatalf("macmon: %+v %v", p, ok)
		}
	}
	if _, ok := parseMacmon([]byte(`Error: "Failed to create subscription"`)); ok {
		t.Fatal("accepted an error line")
	}
}

// ---- rendering ----

// demoState is a populated state used to check layout.
func demoState() *State {
	f := newFake()
	p, st := poller(f)
	p.live()
	p.slow()
	now := time.Now()
	for i, l := range sampleLog {
		st.feedLog(logLine(now.Add(time.Duration(i-len(sampleLog))*time.Second), l.logger, l.level, l.msg))
	}
	st.Cfg.CacheDir, st.Cfg.LogPath = "~/.omlx/cache", "~/.omlx/logs/server.log"
	st.Sys = SysInfo{OK: true, T: now, Chip: "Apple M5 Max", NCPU: 18, CPU: 22, GPUUtil: 97, MemTotal: 128 << 30, MemUsed: 80 << 30,
		Wired: 55 << 30, SwapTotal: 4 << 30, SwapUsed: 1 << 30, Pressure: 1, DiskFree: 700 << 30, WiredLimit: -1, OMLX: []Proc{{PID: 1}}, OMLXRSS: 3 << 30, OMLXCPU: 80}
	st.Power = PowerInfo{T: now, GPUTemp: 71, GPUWatts: 42, SysWatts: 80, GPUMHz: 1398}
	for i := range 200 {
		st.History = append(st.History, Sample{T: now.Add(time.Duration(i-199) * time.Second), DecTPS: float64(20 + i%17), PreTPS: float64((i % 40) * 30),
			Active: 1 + i%3, Waiting: i % 2, GPU: float64(60 + i%40), OMLXMem: float64(40<<30 + i<<26), Busy: true})
	}
	return st
}

func TestRenderFits(t *testing.T) {
	states := map[string]*State{"empty": NewState(Config{}), "full": demoState()}
	sizes := [][2]int{{200, 60}, {120, 40}, {80, 24}, {40, 12}, {10, 3}, {3, 20}, {1, 1}}
	for name, st := range states {
		for _, sz := range sizes {
			for v := range nViews {
				for _, sel := range []int{0, 3, 1000} {
					for _, week := range []bool{false, true} {
						out := model{st: st, w: sz[0], h: sz[1], view: v, sel: sel, week: week}.render()
						lines := strings.Split(out, "\n")
						if len(lines) != sz[1] {
							t.Fatalf("%s %s %v: %d lines", name, viewNames[v], sz, len(lines))
						}
						for i, l := range lines {
							if lw := lipgloss.Width(l); lw > sz[0] {
								t.Fatalf("%s %s %v line %d: width %d: %q", name, viewNames[v], sz, i, lw, ansi.Strip(l))
							}
						}
					}
				}
			}
			help := model{st: st, w: sz[0], h: sz[1], help: true}.render()
			if n := strings.Count(help, "\n") + 1; n > sz[1] {
				t.Fatalf("help too tall at %v: %d", sz, n)
			}
		}
	}
	if dir := os.Getenv("OMLX_TOP_DUMP"); dir != "" {
		for v := range nViews {
			out := model{st: states["full"], w: 160, h: 48, view: v}.render()
			os.WriteFile(filepath.Join(dir, viewNames[v]+".txt"), []byte(ansi.Strip(out)), 0o644)
		}
	}
}

func TestKeys(t *testing.T) {
	var m tea.Model = model{st: demoState(), w: 120, h: 40}
	press := func(keys ...string) model {
		for _, k := range keys {
			msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
			switch k {
			case "tab":
				msg = tea.KeyMsg{Type: tea.KeyTab}
			case "esc":
				msg = tea.KeyMsg{Type: tea.KeyEsc}
			}
			m, _ = m.Update(msg)
		}
		return m.(model)
	}
	if got := press("3"); got.view != vModels {
		t.Fatalf("view %d", got.view)
	}
	if got := press("j", "j", "j", "j"); got.sel != 1 {
		t.Fatalf("selection must stop at the last model, got %d", got.sel)
	}
	if got := press("tab"); got.view != vCache || got.sel != 0 {
		t.Fatalf("tab: %d %d", got.view, got.sel)
	}
	if got := press("p"); !got.paused || got.frozen == "" {
		t.Fatal("not paused")
	}
	if got := press("5"); !strings.Contains(ansi.Strip(got.View()), "EVENTS (") {
		t.Fatal("paused screen did not follow the view change")
	}
	if got := press("p", "r"); got.paused || len(got.st.Events) != 0 {
		t.Fatal("resume/reset failed")
	}
	if got := press("?", "esc"); got.help {
		t.Fatal("help not closed")
	}
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}); cmd == nil {
		t.Fatal("q does not quit")
	}
}

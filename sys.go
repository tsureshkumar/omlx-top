package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Proc struct {
	PID  int
	CPU  float64
	RSS  float64
	Age  string
	Name string
}

type SysInfo struct {
	OK        bool
	Err       string
	Chip      string
	NCPU      int
	CPU       float64 // 0..100, -1 unknown
	Load      [3]float64
	MemTotal  float64
	MemUsed   float64
	Wired     float64
	Compress  float64
	SwapUsed  float64
	SwapTotal float64
	SwapIns   float64 // cumulative pages
	SwapOuts  float64 // cumulative pages
	PageSize  float64
	// computed from successive samples, bytes/s
	SwapInRate  float64
	SwapOutRate float64
	Pressure    int     // macOS memory pressure: 1 normal, 2 warning, 4 critical, 0 unknown
	WiredLimit  int     // iogpu.wired_limit_mb, -1 unknown
	GPUUtil     float64 // -1 unknown
	GPUMem      float64
	DiskFree    float64 // on the SSD cache volume, -1 unknown

	OMLX      []Proc
	OMLXCPU   float64
	OMLXRSS   float64
	Others    []Proc  // largest unrelated processes, by memory, grouped by app
	OllamaRSS float64 // other local LLM server competing for the same memory
	T         time.Time
}

// largest names the biggest non-oMLX apps by resident memory.
func (si SysInfo) largest() string {
	var l []string
	for _, p := range si.Others {
		if p.RSS >= 1<<30 && len(l) < 3 {
			l = append(l, p.Name+" "+fmtBytes(p.RSS))
		}
	}
	if len(l) == 0 {
		return ""
	}
	return " (largest other apps: " + strings.Join(l, ", ") + ")"
}

// sampleSys runs forever, refreshing system stats and the history.
func sampleSys(st *State, every time.Duration) {
	var prev SysInfo
	for {
		si := sampleOS()
		si.T = time.Now()
		si.DiskFree = diskFree(st.Cfg.CacheDir)
		if out, err := exec.Command("ps", "-A", "-o", "pid=,pcpu=,rss=,etime=,command=").Output(); err != nil {
			if si.Err == "" {
				si.Err = "ps: " + shortErr(err)
			}
		} else {
			total := parsePS(string(out), os.Getpid(), &si)
			if si.CPU < 0 && si.NCPU > 0 {
				si.CPU = min(100, total/float64(si.NCPU))
			}
		}
		if dt := si.T.Sub(prev.T).Seconds(); prev.OK && si.OK && dt > 0 {
			si.SwapInRate = max(0, si.SwapIns-prev.SwapIns) * si.PageSize / dt
			si.SwapOutRate = max(0, si.SwapOuts-prev.SwapOuts) * si.PageSize / dt
		}
		prev = si
		st.mu.Lock()
		st.Sys = si
		st.record(si.T)
		st.mu.Unlock()
		time.Sleep(every)
	}
}

// parseVMStat fills the memory fields from `vm_stat` output.
func parseVMStat(out string, si *SysInfo) {
	page := 16384.0
	vals := map[string]float64{}
	for i, ln := range strings.Split(out, "\n") {
		if i == 0 {
			if _, rest, ok := strings.Cut(ln, "page size of "); ok {
				if f := strings.Fields(rest); len(f) > 0 && num(f[0]) > 0 {
					page = num(f[0])
				}
			}
			continue
		}
		if k, v, ok := strings.Cut(ln, ":"); ok {
			vals[strings.TrimSpace(k)] = num(strings.TrimSuffix(strings.TrimSpace(v), "."))
		}
	}
	// Used = app + wired + compressed, as Activity Monitor counts it.
	app := vals["Anonymous pages"] - vals["Pages purgeable"]
	if vals["Anonymous pages"] == 0 {
		app = vals["Pages active"]
	}
	si.PageSize = page
	si.Wired = vals["Pages wired down"] * page
	si.Compress = vals["Pages occupied by compressor"] * page
	si.MemUsed = app*page + si.Wired + si.Compress
	si.SwapIns, si.SwapOuts = vals["Swapins"], vals["Swapouts"]
}

// isOMLXProc reports whether a command line belongs to the oMLX server.
func isOMLXProc(fields []string) bool {
	exe := strings.ToLower(filepath.Base(fields[0]))
	if exe == "omlx-top" {
		return false
	}
	cmd := strings.Join(fields, " ")
	if strings.HasPrefix(exe, "omlx") || exe == "splash-server" || strings.Contains(cmd, "/oMLX.app/") {
		return true
	}
	return strings.Contains(exe, "python") && (strings.Contains(cmd, "-m omlx") || strings.Contains(cmd, "/omlx "))
}

// appName gives a short name for a command line: the app bundle or the executable.
func appName(cmd string) string {
	if i := strings.Index(cmd, ".app/"); i > 0 {
		return filepath.Base(cmd[:i])
	}
	f := strings.Fields(cmd)
	return filepath.Base(f[0])
}

// parsePS fills the process fields and returns the summed %CPU of all processes.
func parsePS(out string, self int, si *SysInfo) (total float64) {
	others := map[string]*Proc{}
	for _, ln := range strings.Split(out, "\n") {
		f := strings.Fields(ln)
		if len(f) < 5 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		cpu, rss := num(f[1]), num(f[2])*1024
		total += cpu
		if pid == self {
			continue
		}
		cmd := clean(strings.Join(f[4:], " "))
		if cmd == "" {
			continue
		}
		name := appName(cmd)
		if isOMLXProc(strings.Fields(cmd)) {
			si.OMLX = append(si.OMLX, Proc{pid, cpu, rss, f[3], name})
			si.OMLXCPU += cpu
			si.OMLXRSS += rss
			continue
		}
		if strings.Contains(strings.ToLower(name), "ollama") && name != "ollama-top" {
			name = "Ollama"
			si.OllamaRSS += rss
		}
		p := others[name]
		if p == nil {
			p = &Proc{PID: pid, Name: name, Age: f[3]}
			others[name] = p
		}
		p.CPU += cpu
		p.RSS += rss
	}
	for _, p := range others {
		si.Others = append(si.Others, *p)
	}
	slices.SortFunc(si.Others, func(a, b Proc) int {
		if a.RSS != b.RSS {
			if a.RSS > b.RSS {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	if len(si.Others) > 6 {
		si.Others = si.Others[:6]
	}
	slices.SortFunc(si.OMLX, func(a, b Proc) int { return a.PID - b.PID })
	return total
}

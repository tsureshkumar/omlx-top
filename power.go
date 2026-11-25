package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"time"
)

// PowerInfo comes from the optional macmon tool (power, temperature).
type PowerInfo struct {
	T        time.Time
	CPUTemp  float64
	GPUTemp  float64
	GPUWatts float64
	CPUWatts float64
	ANEWatts float64
	SysWatts float64
	GPUMHz   float64

	// GPU load averaged over the sampling interval, 0..100.
	HasLoad   bool
	GPUScaled float64 // adjusted for clock speed, as macmon displays it
	GPUActive float64 // share of the time the GPU was doing any work
}

// powerMaxAge is how long a macmon reading stays usable.
const powerMaxAge = 15 * time.Second

// parseMacmon reads one line of `macmon pipe`, accepting old and new field names.
func parseMacmon(line []byte) (PowerInfo, bool) {
	var m struct {
		Temp *struct {
			CPU float64 `json:"cpu_temp_avg"`
			GPU float64 `json:"gpu_temp_avg"`
		} `json:"temp"`
		CPUPower float64   `json:"cpu_power"`
		GPUPower float64   `json:"gpu_power"`
		ANEPower float64   `json:"ane_power"`
		SysPower float64   `json:"sys_power"`
		GPUFreq  float64   `json:"gpu_freq_mhz"`
		Scaled   *float64  `json:"gpu_scaled_ratio"`
		Active   *float64  `json:"gpu_active_ratio"`
		GPUUsage []float64 `json:"gpu_usage"` // older releases: [MHz, ratio]
	}
	if json.Unmarshal(line, &m) != nil || m.Temp == nil {
		return PowerInfo{}, false
	}
	p := PowerInfo{T: time.Now(), CPUTemp: m.Temp.CPU, GPUTemp: m.Temp.GPU, CPUWatts: m.CPUPower,
		GPUWatts: m.GPUPower, ANEWatts: m.ANEPower, SysWatts: m.SysPower, GPUMHz: m.GPUFreq}
	if p.GPUMHz == 0 && len(m.GPUUsage) == 2 {
		p.GPUMHz = m.GPUUsage[0]
	}
	scaled, active := m.Scaled, m.Active
	if scaled == nil && len(m.GPUUsage) == 2 {
		scaled = &m.GPUUsage[1]
	}
	if scaled != nil {
		if active == nil {
			active = scaled
		}
		p.HasLoad, p.GPUScaled, p.GPUActive = true, 100**scaled, 100**active
	}
	return p, true
}

// runMacmon streams macmon until ctx is cancelled. It gives up if macmon
// repeatedly produces nothing.
func runMacmon(ctx context.Context, path string, every time.Duration, st *State) {
	for fails := 0; fails < 3 && ctx.Err() == nil; fails++ {
		cmd := exec.CommandContext(ctx, path, "pipe", "-i", strconv.Itoa(int(max(every, time.Second).Milliseconds())))
		out, err := cmd.StdoutPipe()
		if err != nil || cmd.Start() != nil {
			return
		}
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			if p, ok := parseMacmon(sc.Bytes()); ok {
				fails = 0
				st.mu.Lock()
				st.Power = p
				st.mu.Unlock()
			}
		}
		cmd.Wait()
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
}

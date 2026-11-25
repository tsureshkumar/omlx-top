//go:build darwin

package main

import (
	"encoding/binary"
	"os/exec"
	"path/filepath"
	"regexp"

	"golang.org/x/sys/unix"
)

var (
	reGPUUtil = regexp.MustCompile(`"Device Utilization %"=(\d+)`)
	reGPUMem  = regexp.MustCompile(`"In use system memory"=(\d+)`)
)

func sampleOS() SysInfo {
	si := SysInfo{OK: true, CPU: -1, GPUUtil: -1, WiredLimit: -1}
	si.Chip, _ = unix.Sysctl("machdep.cpu.brand_string")
	if n, err := unix.SysctlUint32("hw.ncpu"); err == nil {
		si.NCPU = int(n)
	}
	if m, err := unix.SysctlUint64("hw.memsize"); err == nil {
		si.MemTotal = float64(m)
	}
	if b, err := unix.SysctlRaw("vm.loadavg"); err == nil && len(b) >= 24 {
		if scale := float64(binary.LittleEndian.Uint64(b[16:24])); scale > 0 {
			for i := range 3 {
				si.Load[i] = float64(binary.LittleEndian.Uint32(b[i*4:])) / scale
			}
		}
	}
	if b, err := unix.SysctlRaw("vm.swapusage"); err == nil && len(b) >= 24 {
		si.SwapTotal = float64(binary.LittleEndian.Uint64(b[0:8]))
		si.SwapUsed = float64(binary.LittleEndian.Uint64(b[16:24]))
	}
	if p, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level"); err == nil {
		si.Pressure = int(p)
	}
	// The value is 32 or 64 bits wide depending on the macOS release.
	if b, err := unix.SysctlRaw("iogpu.wired_limit_mb"); err == nil {
		switch len(b) {
		case 4:
			si.WiredLimit = int(binary.LittleEndian.Uint32(b))
		case 8:
			si.WiredLimit = int(binary.LittleEndian.Uint64(b))
		}
	}
	if out, err := exec.Command("vm_stat").Output(); err == nil {
		parseVMStat(string(out), &si)
	} else {
		si.Err = "vm_stat: " + shortErr(err)
	}
	// GPU utilisation from IOAccelerator performance statistics (no sudo needed).
	if out, err := exec.Command("ioreg", "-r", "-d", "1", "-w", "0", "-c", "IOAccelerator").Output(); err == nil {
		if m := reGPUUtil.FindSubmatch(out); m != nil {
			si.GPUUtil = num(string(m[1]))
		}
		if m := reGPUMem.FindSubmatch(out); m != nil {
			si.GPUMem = num(string(m[1]))
		}
	}
	return si
}

// diskFree returns the bytes available on the volume holding dir, walking up to
// the first parent that exists. -1 if unknown.
func diskFree(dir string) float64 {
	for dir != "" {
		var s unix.Statfs_t
		if unix.Statfs(dir, &s) == nil {
			return float64(s.Bavail) * float64(s.Bsize)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return -1
}

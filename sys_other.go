//go:build !darwin

package main

func sampleOS() SysInfo {
	return SysInfo{CPU: -1, GPUUtil: -1, WiredLimit: -1, Err: "system stats are only supported on macOS"}
}

func diskFree(string) float64 { return -1 }

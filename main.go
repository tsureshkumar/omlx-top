// omlx-top: a terminal monitor for a local oMLX server.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

var version = "0.1.0"

func main() {
	base := flag.String("base", "", "oMLX base directory (default $OMLX_BASE_PATH, the app's setting, or ~/.omlx)")
	host := flag.String("host", "", "oMLX URL or host:port (default from settings.json)")
	key := flag.String("api-key", "", "main API key (default $OMLX_API_KEY, or settings.json for the local server)")
	logPath := flag.String("log", "", "server log to tail (default <base>/logs/server.log)")
	backlog := flag.Int64("backlog", 2<<20, "bytes of existing log to replay at startup")
	interval := flag.Duration("i", 500*time.Millisecond, "screen refresh interval")
	poll := flag.Duration("poll", time.Second, "API / system poll interval")
	useMacmon := flag.Bool("macmon", true, "show power and temperature using macmon, if installed")
	once := flag.Bool("once", false, "print one snapshot and exit (no TUI)")
	view := flag.String("view", "overview", "starting view: overview, requests, models, cache, events, usage")
	width := flag.Int("width", 160, "width for -once")
	height := flag.Int("height", 55, "height for -once")
	showVer := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVer {
		fmt.Println("omlx-top", version)
		return
	}
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "omlx-top:", err)
		os.Exit(1)
	}
	if *interval < 100*time.Millisecond || *poll < 250*time.Millisecond {
		fail(fmt.Errorf("-i must be at least 100ms and -poll at least 250ms"))
	}
	start := slices.Index(viewNames[:], *view)
	if start < 0 {
		fail(fmt.Errorf("unknown -view %q", *view))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fail(err)
	}
	cfg, err := loadConfig(*base, *host, *key, *logPath, os.Getenv, home)
	if err != nil {
		fail(err)
	}

	st := NewState(cfg)
	if cfg.SettingsErr != "" {
		st.event(time.Now(), "WARN", cfg.SettingsErr)
	}
	go tailFile(cfg.LogPath, *backlog, st)
	go NewPoller(NewAPI(cfg.URL, cfg.APIKey), st, *poll).Run()
	go sampleSys(st, *poll)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	if path, err := exec.LookPath("macmon"); *useMacmon && err == nil {
		go func() { runMacmon(ctx, path, *poll, st); close(done) }()
	} else {
		close(done)
	}
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}

	m := model{st: st, every: *interval, view: start}
	if *once {
		// Long enough for two polls, so rates and live requests are filled in.
		time.Sleep(*poll*2 + 500*time.Millisecond)
		m.w, m.h = *width, *height
		fmt.Println(m.render())
		stop()
		return
	}
	_, err = tea.NewProgram(m, tea.WithAltScreen()).Run()
	stop()
	if err != nil {
		fail(err)
	}
}

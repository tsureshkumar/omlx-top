package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config is what omlx-top needs from the local oMLX installation.
type Config struct {
	Base      string // oMLX base directory (~/.omlx)
	URL       string // server URL to monitor
	APIKey    string // never rendered or logged
	KeySource string // where the key came from, or why there is none

	LogPath       string
	CacheDir      string
	MaxConcurrent int
	HotCacheMax   string
	CacheEnabled  bool
	GuardTier     string
	SettingsErr   string
}

// settingsFile is the subset of settings.json that is read.
type settingsFile struct {
	Server struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	} `json:"server"`
	Auth struct {
		APIKey string `json:"api_key"`
	} `json:"auth"`
	Scheduler struct {
		MaxConcurrent int `json:"max_concurrent_requests"`
	} `json:"scheduler"`
	Cache struct {
		Enabled     *bool   `json:"enabled"`
		HotCacheMax string  `json:"hot_cache_max_size"`
		SSDCacheDir *string `json:"ssd_cache_dir"`
	} `json:"cache"`
	Memory struct {
		GuardTier string `json:"memory_guard_tier"`
	} `json:"memory"`
	Logging struct {
		LogDir *string `json:"log_dir"`
	} `json:"logging"`
}

type env func(string) string

func expandHome(p, home string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}

// basePath follows oMLX's own priority: env var, the macOS app's bootstrap file, ~/.omlx.
func basePath(flagBase string, getenv env, home string) string {
	if flagBase != "" {
		return expandHome(flagBase, home)
	}
	if b := getenv("OMLX_BASE_PATH"); b != "" {
		return expandHome(b, home)
	}
	boot := filepath.Join(home, "Library", "Application Support", "oMLX", "base-path")
	if b, err := os.ReadFile(boot); err == nil {
		if line, _, _ := strings.Cut(string(b), "\n"); strings.TrimSpace(line) != "" {
			return expandHome(strings.TrimSpace(line), home)
		}
	}
	return filepath.Join(home, ".omlx")
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// loadConfig resolves where the server is, which key to use and where its files live.
func loadConfig(flagBase, flagHost, flagKey, flagLog string, getenv env, home string) (Config, error) {
	c := Config{Base: basePath(flagBase, getenv, home), CacheEnabled: true}

	var s settingsFile
	if b, err := os.ReadFile(filepath.Join(c.Base, "settings.json")); err != nil {
		c.SettingsErr = "settings.json: " + shortErr(err)
	} else if err := json.Unmarshal(b, &s); err != nil {
		// A type mismatch in one field still leaves the others decoded.
		c.SettingsErr = "settings.json: " + shortErr(err)
	}

	cfgPort := s.Server.Port
	if p, err := strconv.Atoi(getenv("OMLX_PORT")); err == nil && p > 0 {
		cfgPort = p
	}
	if cfgPort == 0 {
		cfgPort = 8000
	}
	cfgHost := s.Server.Host
	if h := getenv("OMLX_HOST"); h != "" {
		cfgHost = h
	}
	cfgHost, _, _ = strings.Cut(cfgHost, ",") // a list of bind addresses: use the first
	cfgHost = strings.TrimSpace(cfgHost)
	switch cfgHost {
	case "", "0.0.0.0", "::", "*":
		cfgHost = "127.0.0.1"
	}

	target := flagHost
	if target == "" {
		target = net.JoinHostPort(cfgHost, strconv.Itoa(cfgPort))
	}
	if !strings.Contains(target, "://") {
		target = "http://" + target
	}
	u, err := url.Parse(target)
	if err != nil || u.Hostname() == "" {
		return c, fmt.Errorf("invalid -host %q", flagHost)
	}
	switch u.Hostname() {
	case "0.0.0.0", "::":
		u.Host = net.JoinHostPort("127.0.0.1", u.Port())
	}
	port := u.Port()
	if port == "" {
		port = strconv.Itoa(cfgPort)
		u.Host = net.JoinHostPort(u.Hostname(), port)
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	c.URL = strings.TrimRight(u.String(), "/")

	// The key in settings.json belongs to the local server: it is only used when
	// the target is that server (this machine, configured port).
	local := isLoopback(u.Hostname()) || u.Hostname() == cfgHost && flagHost == ""
	switch {
	case flagKey != "":
		c.APIKey, c.KeySource = flagKey, "-api-key"
	case getenv("OMLX_API_KEY") != "":
		c.APIKey, c.KeySource = getenv("OMLX_API_KEY"), "$OMLX_API_KEY"
	case s.Auth.APIKey != "" && local && port == strconv.Itoa(cfgPort):
		c.APIKey, c.KeySource = s.Auth.APIKey, "settings.json"
	case s.Auth.APIKey != "":
		c.KeySource = "settings.json key not sent: target is not the configured local server (use -api-key)"
	default:
		c.KeySource = "no API key found"
	}

	logDir := filepath.Join(c.Base, "logs")
	if s.Logging.LogDir != nil && *s.Logging.LogDir != "" {
		logDir = expandHome(*s.Logging.LogDir, home)
	}
	c.LogPath = filepath.Join(logDir, "server.log")
	if flagLog != "" {
		c.LogPath = expandHome(flagLog, home)
	}
	c.CacheDir = filepath.Join(c.Base, "cache")
	if s.Cache.SSDCacheDir != nil && *s.Cache.SSDCacheDir != "" {
		c.CacheDir = expandHome(*s.Cache.SSDCacheDir, home)
	}
	if s.Cache.Enabled != nil {
		c.CacheEnabled = *s.Cache.Enabled
	}
	c.HotCacheMax = s.Cache.HotCacheMax
	c.MaxConcurrent = s.Scheduler.MaxConcurrent
	c.GuardTier = s.Memory.GuardTier
	return c, nil
}

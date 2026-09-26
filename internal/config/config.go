// Package config reads the controller settings from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all settings.
type Config struct {
	Org        string // GITHUB_ORG
	AppID      int64  // GITHUB_APP_ID (GitHub App auth)
	AppKeyFile string // GITHUB_APP_KEY_FILE (GitHub App auth)
	// Token authenticates as a personal access token instead of a GitHub
	// App: set GITHUB_TOKEN directly, or GITHUB_TOKEN_FILE to read it from a
	// mounted secret. Mutually exclusive with the App settings above.
	Token            string
	APIURL           string        // GITHUB_API_URL
	EnvProperty      string        // ENV_PROPERTY: org custom property listing allowed envs ("" = all allowed)
	RequireProtected []string      // REQUIRE_PROTECTED: envs that may only deploy from protected branches
	ScanInterval     time.Duration // SCAN_INTERVAL
	ImageInterval    time.Duration // IMAGE_INTERVAL (0 disables automatic image updates)
	RolloutTimeout   time.Duration // ROLLOUT_TIMEOUT
	PruneEnabled     bool          // PRUNE_ENABLED
	PruneConfirm     int           // PRUNE_CONFIRMATIONS: consecutive scans before a stack is removed
	MaxPrune         int           // MAX_PRUNE: more removals than this in one scan need approval
	Concurrency      int           // CONCURRENCY
	DataDir          string        // DATA_DIR
	PolicyFile       string        // POLICY_FILE
	Socket           string        // SOCKET
	DockerConfig     string        // DOCKER_CONFIG (dir with config.json for registry auth)
	DryRun           bool          // DRY_RUN: render, check and report, but never touch the Swarm
	PrepImage        string        // PREP_IMAGE: image running `swarm-gitops prepare` on the nodes
	PrepTimeout      time.Duration // PREP_TIMEOUT
	// MetricsAddr, e.g. ":9090", starts a read-only Prometheus endpoint on
	// its own listener. Empty (default) disables it. Unlike SOCKET, this is
	// meant to be network-reachable - keep it off the public internet (no
	// Traefik router, an internal-only overlay network is enough).
	MetricsAddr string // METRICS_ADDR
}

// FromEnv loads the configuration.
func FromEnv() (*Config, error) {
	c := &Config{
		Org:              os.Getenv("GITHUB_ORG"),
		APIURL:           env("GITHUB_API_URL", "https://api.github.com"),
		EnvProperty:      env("ENV_PROPERTY", "swarm-environments"),
		RequireProtected: list(env("REQUIRE_PROTECTED", "prod")),
		DataDir:          env("DATA_DIR", "/data"),
		PolicyFile:       os.Getenv("POLICY_FILE"),
		Socket:           env("SOCKET", "/run/swarm-gitops/api.sock"),
		DockerConfig:     os.Getenv("DOCKER_CONFIG"),
		// No built-in default: it must name your own image, not any specific
		// project's. Usually the same image running this controller (see
		// deploy/stack.yml).
		PrepImage:   os.Getenv("PREP_IMAGE"),
		MetricsAddr: os.Getenv("METRICS_ADDR"),
	}
	if err := c.loadAuth(); err != nil {
		return nil, err
	}
	if c.Org == "" || c.PrepImage == "" {
		return nil, fmt.Errorf("GITHUB_ORG and PREP_IMAGE are required")
	}
	var err error
	durations := []struct {
		dst  *time.Duration
		key  string
		def  string
		zero bool
	}{
		{&c.ScanInterval, "SCAN_INTERVAL", "1m", false},
		{&c.ImageInterval, "IMAGE_INTERVAL", "2m", true},
		{&c.RolloutTimeout, "ROLLOUT_TIMEOUT", "5m", false},
		{&c.PrepTimeout, "PREP_TIMEOUT", "2m", false},
	}
	for _, d := range durations {
		if *d.dst, err = time.ParseDuration(env(d.key, d.def)); err != nil {
			return nil, fmt.Errorf("%s: %v", d.key, err)
		}
		if *d.dst < 10*time.Second && !(d.zero && *d.dst == 0) {
			return nil, fmt.Errorf("%s must be at least 10s", d.key)
		}
	}
	ints := []struct {
		dst *int
		key string
		def string
	}{
		{&c.PruneConfirm, "PRUNE_CONFIRMATIONS", "2"},
		{&c.MaxPrune, "MAX_PRUNE", "3"},
		{&c.Concurrency, "CONCURRENCY", "4"},
	}
	for _, i := range ints {
		if *i.dst, err = strconv.Atoi(env(i.key, i.def)); err != nil || *i.dst < 1 {
			return nil, fmt.Errorf("%s must be a positive number", i.key)
		}
	}
	c.PruneEnabled = env("PRUNE_ENABLED", "true") == "true"
	c.DryRun = env("DRY_RUN", "false") == "true"
	return c, nil
}

// loadAuth resolves exactly one of the two supported ways to authenticate to
// GitHub: a GitHub App (GITHUB_APP_ID + GITHUB_APP_KEY_FILE) or a personal
// access token (GITHUB_TOKEN or GITHUB_TOKEN_FILE).
func (c *Config) loadAuth() error {
	appID, appKeyFile := os.Getenv("GITHUB_APP_ID"), os.Getenv("GITHUB_APP_KEY_FILE")
	token, tokenFile := os.Getenv("GITHUB_TOKEN"), os.Getenv("GITHUB_TOKEN_FILE")
	switch {
	case appID != "" || appKeyFile != "":
		if token != "" || tokenFile != "" {
			return fmt.Errorf("set either GITHUB_APP_ID/GITHUB_APP_KEY_FILE or GITHUB_TOKEN/GITHUB_TOKEN_FILE, not both")
		}
		if appID == "" || appKeyFile == "" {
			return fmt.Errorf("GITHUB_APP_ID and GITHUB_APP_KEY_FILE must be set together")
		}
		id, err := strconv.ParseInt(appID, 10, 64)
		if err != nil {
			return fmt.Errorf("GITHUB_APP_ID: %v", err)
		}
		c.AppID, c.AppKeyFile = id, appKeyFile
		return nil
	case token != "" || tokenFile != "":
		if tokenFile != "" {
			data, err := os.ReadFile(tokenFile)
			if err != nil {
				return fmt.Errorf("GITHUB_TOKEN_FILE: %v", err)
			}
			token = strings.TrimSpace(string(data))
		}
		if token == "" {
			return fmt.Errorf("GITHUB_TOKEN_FILE is empty")
		}
		c.Token = token
		return nil
	default:
		return fmt.Errorf("set GITHUB_APP_ID/GITHUB_APP_KEY_FILE (a GitHub App) or GITHUB_TOKEN/GITHUB_TOKEN_FILE (a personal access token)")
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func list(s string) []string {
	var res []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			res = append(res, p)
		}
	}
	return res
}

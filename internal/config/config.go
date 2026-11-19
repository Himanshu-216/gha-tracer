// Package config loads service configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Himanshu-216/gha-tracer/internal/release"
)

type Config struct {
	ListenAddr   string // public: /webhook, /healthz
	InternalAddr string // private: /metrics, /api/*

	AppID         int64
	PrivateKey    []byte
	WebhookSecret []byte
	GitHubAPIURL  string // override for GHES / tests; empty = api.github.com

	OTLPEndpoint string            // e.g. http://tempo:4318/v1/traces
	OTLPHeaders  map[string]string // e.g. Authorization for hosted backends

	TagPattern   string
	BranchPrefix string
	TraceAll     bool     // trace every run, not just release refs
	TraceEvents  []string // run events to trace (default: push)

	Workers   int
	QueueSize int

	StaleAfter        time.Duration // in-progress longer than this → stale gauge
	RepollAfter       time.Duration // no events for this long → poll the API
	RepollInterval    time.Duration
	RedeliverInterval time.Duration
	RedeliverLookback time.Duration // first sweep after start looks back this far
	RecentRuns        int           // completed runs kept for /api/recent
}

func Load() (*Config, error) {
	c := &Config{
		ListenAddr:   env("LISTEN_ADDR", ":8080"),
		InternalAddr: env("INTERNAL_ADDR", ":9090"),
		GitHubAPIURL: os.Getenv("GITHUB_API_URL"),
		OTLPEndpoint: env("OTLP_ENDPOINT", "http://localhost:4318/v1/traces"),
		TagPattern:   env("TAG_PATTERN", release.DefaultTagPattern),
		BranchPrefix: env("RELEASE_BRANCH_PREFIX", release.DefaultBranchPrefix),
		TraceEvents:  list(env("TRACE_EVENTS", "push")),
		OTLPHeaders:  map[string]string{},
	}
	var errs []string
	fail := func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}

	var err error
	c.AppID, err = strconv.ParseInt(os.Getenv("GITHUB_APP_ID"), 10, 64)
	if err != nil {
		fail(fmt.Errorf("GITHUB_APP_ID: %w", err))
	}

	c.PrivateKey = []byte(os.Getenv("GITHUB_APP_PRIVATE_KEY"))
	if f := os.Getenv("GITHUB_APP_PRIVATE_KEY_FILE"); len(c.PrivateKey) == 0 && f != "" {
		c.PrivateKey, err = os.ReadFile(f)
		fail(err)
	}
	if len(c.PrivateKey) == 0 {
		fail(fmt.Errorf("GITHUB_APP_PRIVATE_KEY or GITHUB_APP_PRIVATE_KEY_FILE is required"))
	}

	c.WebhookSecret = []byte(os.Getenv("GITHUB_WEBHOOK_SECRET"))
	if len(c.WebhookSecret) == 0 {
		fail(fmt.Errorf("GITHUB_WEBHOOK_SECRET is required"))
	}

	for _, kv := range list(os.Getenv("OTLP_HEADERS")) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			fail(fmt.Errorf("OTLP_HEADERS: %q is not key=value", kv))
			continue
		}
		c.OTLPHeaders[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}

	c.TraceAll, err = strconv.ParseBool(env("TRACE_ALL_RUNS", "false"))
	fail(err)
	c.Workers, err = strconv.Atoi(env("WORKERS", "4"))
	fail(err)
	c.QueueSize, err = strconv.Atoi(env("QUEUE_SIZE", "1000"))
	fail(err)
	c.RecentRuns, err = strconv.Atoi(env("RECENT_RUNS", "1000"))
	fail(err)

	durs := []struct {
		dst  *time.Duration
		name string
		def  string
	}{
		{&c.StaleAfter, "STALE_AFTER", "2h"},
		{&c.RepollAfter, "REPOLL_AFTER", "20m"},
		{&c.RepollInterval, "REPOLL_INTERVAL", "10m"},
		{&c.RedeliverInterval, "REDELIVER_INTERVAL", "5m"},
		{&c.RedeliverLookback, "REDELIVER_LOOKBACK", "6h"},
	}
	for _, d := range durs {
		*d.dst, err = time.ParseDuration(env(d.name, d.def))
		if err != nil {
			fail(fmt.Errorf("%s: %w", d.name, err))
		}
	}

	if _, err := release.New(c.TagPattern, c.BranchPrefix); err != nil {
		fail(err)
	}
	if c.Workers < 1 || c.QueueSize < 1 {
		fail(fmt.Errorf("WORKERS and QUEUE_SIZE must be >= 1"))
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return c, nil
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func list(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

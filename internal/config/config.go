// Package config loads service configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Himanshu-216/gha-tracer/internal/tags"
)

type Config struct {
	ListenAddr   string // public: /webhook, /healthz
	InternalAddr string // private: /metrics, /healthz

	AppID         int64
	PrivateKey    []byte
	WebhookSecret []byte
	GitHubAPIURL  string // override for GHES / tests; empty = api.github.com

	TagPattern string
	RunEvents  []string // workflow_run.event values to track (default: push)

	KeepCompleted     time.Duration // finished runs stay exported this long
	RepollAfter       time.Duration // running run with no events for this long → ask the API
	RepollInterval    time.Duration
	RedeliverInterval time.Duration
	RedeliverLookback time.Duration // first sweep after start looks back this far
}

func Load() (*Config, error) {
	c := &Config{
		ListenAddr:   env("LISTEN_ADDR", ":8080"),
		InternalAddr: env("INTERNAL_ADDR", ":9090"),
		GitHubAPIURL: os.Getenv("GITHUB_API_URL"),
		TagPattern:   env("TAG_PATTERN", tags.DefaultPattern),
		RunEvents:    list(env("RUN_EVENTS", "push")),
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

	durs := []struct {
		dst  *time.Duration
		name string
		def  string
	}{
		{&c.KeepCompleted, "KEEP_COMPLETED", "1h"},
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
	if c.KeepCompleted < 5*time.Minute {
		fail(fmt.Errorf("KEEP_COMPLETED must be at least 5m so Prometheus scrapes the final status"))
	}

	if _, err := tags.New(c.TagPattern); err != nil {
		fail(err)
	}
	if len(c.RunEvents) == 0 {
		fail(fmt.Errorf("RUN_EVENTS must not be empty"))
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

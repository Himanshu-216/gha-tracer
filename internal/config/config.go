// Package config loads service configuration from environment variables.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
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
		// The path comes from the operator's deployment config, not from
		// request data, so reading an arbitrary path is intended here.
		c.PrivateKey, err = os.ReadFile(filepath.Clean(f)) // #nosec G304 G703 -- operator-supplied path
		fail(err)
	}
	if len(c.PrivateKey) == 0 {
		fail(fmt.Errorf("GITHUB_APP_PRIVATE_KEY or GITHUB_APP_PRIVATE_KEY_FILE is required"))
	}

	c.WebhookSecret = []byte(os.Getenv("GITHUB_WEBHOOK_SECRET"))
	switch {
	case len(c.WebhookSecret) == 0:
		fail(fmt.Errorf("GITHUB_WEBHOOK_SECRET is required"))
	case len(c.WebhookSecret) < MinWebhookSecretLen:
		// The secret is the only thing authenticating the public /webhook
		// endpoint; a short one can be brute-forced offline from one delivery.
		fail(fmt.Errorf("GITHUB_WEBHOOK_SECRET must be at least %d bytes (use `openssl rand -hex 32`)", MinWebhookSecretLen))
	}

	if err := checkAPIURL(c.GitHubAPIURL); err != nil {
		fail(err)
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

// MinWebhookSecretLen is the shortest GITHUB_WEBHOOK_SECRET accepted.
const MinWebhookSecretLen = 16

// checkAPIURL makes sure the App's JWT and installation tokens are never sent
// in clear text: GITHUB_API_URL must be https, except for a loopback host
// (local testing).
func checkAPIURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("GITHUB_API_URL: %w", err)
	}
	if u.Host == "" {
		return fmt.Errorf("GITHUB_API_URL: %q has no host", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
	}
	return fmt.Errorf("GITHUB_API_URL: must use https (got %q)", raw)
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

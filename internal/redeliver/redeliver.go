// Package redeliver periodically asks GitHub to redeliver App webhook
// deliveries that failed (service down, deploy, queue full). Together with the
// deterministic span IDs this replaces durable storage.
package redeliver

import (
	"context"
	"log/slog"
	"time"

	"github.com/Himanshu-216/gha-tracer/internal/github"
)

type Sweeper struct {
	api         github.API
	interval    time.Duration
	lookback    time.Duration
	maxAttempts int
	onResult    func(result string)
	log         *slog.Logger
	now         func() time.Time

	since    time.Time
	attempts map[string]int // GUID → redeliveries requested
}

func New(api github.API, interval, lookback time.Duration, onResult func(string), log *slog.Logger) *Sweeper {
	if onResult == nil {
		onResult = func(string) {}
	}
	return &Sweeper{
		api: api, interval: interval, lookback: lookback, maxAttempts: 3,
		onResult: onResult, log: log, now: time.Now, attempts: map[string]int{},
	}
}

// Run sweeps shortly after start (to recover what was missed while we were
// down) and then every interval.
func (s *Sweeper) Run(ctx context.Context) {
	select {
	case <-time.After(30 * time.Second): // let the listener come up first
	case <-ctx.Done():
		return
	}
	s.Sweep(ctx)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Sweep(ctx)
		}
	}
}

var tracked = map[string]bool{"workflow_run": true, "workflow_job": true}

// Sweep redelivers each failed delivery GUID at most maxAttempts times. A
// GUID counts as delivered once any attempt (original or redelivery) got a 2xx.
func (s *Sweeper) Sweep(ctx context.Context) {
	start := s.now()
	since := s.since
	if since.IsZero() {
		since = start.Add(-s.lookback)
	}
	since = since.Add(-time.Minute) // overlap; dedupe makes it harmless

	ds, err := s.api.Deliveries(ctx, since)
	if err != nil {
		s.onResult("list_error")
		s.log.Warn("redeliver: list deliveries failed", "err", err)
		return
	}

	ok := map[string]bool{}
	latestFailed := map[string]github.Delivery{}
	for _, d := range ds {
		if !tracked[d.Event] {
			continue
		}
		if d.StatusCode >= 200 && d.StatusCode < 300 {
			ok[d.GUID] = true
			continue
		}
		if cur, seen := latestFailed[d.GUID]; !seen || d.DeliveredAt.After(cur.DeliveredAt) {
			latestFailed[d.GUID] = d
		}
	}

	for guid, d := range latestFailed {
		if ok[guid] {
			continue
		}
		if s.attempts[guid] >= s.maxAttempts {
			if s.attempts[guid] == s.maxAttempts { // report once per GUID
				s.onResult("gave_up")
				s.log.Error("redeliver: giving up", "guid", guid, "event", d.Event, "status", d.StatusCode)
				s.attempts[guid]++
			}
			continue
		}
		s.attempts[guid]++
		if err := s.api.Redeliver(ctx, d.ID); err != nil {
			s.onResult("error")
			s.log.Warn("redeliver failed", "guid", guid, "delivery_id", d.ID, "err", err)
			continue
		}
		s.onResult("requested")
		s.log.Info("redelivery requested", "guid", guid, "event", d.Event, "action", d.Action, "status", d.StatusCode)
	}

	s.since = start
	if len(s.attempts) > 10000 {
		s.attempts = map[string]int{}
	}
}

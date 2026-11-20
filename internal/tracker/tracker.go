// Package tracker turns workflow_run webhooks for tag pushes into run state,
// and re-polls runs whose "completed" webhook never arrived.
package tracker

import (
	"context"
	"log/slog"
	"slices"
	"time"

	gh "github.com/google/go-github/v88/github"

	"github.com/Himanshu-216/gha-tracer/internal/github"
	"github.com/Himanshu-216/gha-tracer/internal/runs"
	"github.com/Himanshu-216/gha-tracer/internal/tags"
	"github.com/Himanshu-216/gha-tracer/internal/webhook"
)

type Tracker struct {
	tags   *tags.Matcher
	events []string // workflow_run.event values to track (default: push)
	store  *runs.Store
	api    github.API
	log    *slog.Logger
}

func New(m *tags.Matcher, events []string, store *runs.Store, api github.API, log *slog.Logger) *Tracker {
	return &Tracker{tags: m, events: events, store: store, api: api, log: log}
}

// Handle is the webhook sink. Applying an event is an in-memory update, so it
// runs inline and never reports the queue as full.
func (t *Tracker) Handle(ev webhook.Event) bool {
	if e, ok := ev.Payload.(*gh.WorkflowRunEvent); ok {
		t.apply(e.GetInstallation().GetID(), e.GetWorkflowRun())
	}
	return true
}

func (t *Tracker) apply(installationID int64, r *gh.WorkflowRun) {
	if r == nil || r.GetRepository().GetFullName() == "" {
		return
	}
	if !slices.Contains(t.events, r.GetEvent()) || !t.tags.Match(r.GetHeadBranch()) {
		return
	}
	run := runs.Run{
		InstallationID: installationID,
		Repo:           r.GetRepository().GetFullName(),
		Workflow:       r.GetName(),
		Tag:            r.GetHeadBranch(),
		ID:             r.GetID(),
		Attempt:        max(r.GetRunAttempt(), 1),
		Status:         runs.StatusOf(r.GetStatus(), r.GetConclusion()),
		StartedAt:      firstTime(r.RunStartedAt, r.CreatedAt),
	}
	if run.Status != runs.Running {
		run.CompletedAt = r.GetUpdatedAt().Time
	}
	t.store.Apply(run)
}

// Repoll asks the API about running runs that have been quiet for `after`;
// the answer goes through the same path as a webhook.
func (t *Tracker) Repoll(ctx context.Context, interval, after time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			t.RepollOnce(ctx, after)
		}
	}
}

func (t *Tracker) RepollOnce(ctx context.Context, after time.Duration) {
	for _, r := range t.store.Quiet(after) {
		wr, err := t.api.Run(ctx, r.InstallationID, r.Repo, r.ID, r.Attempt)
		if err != nil {
			t.log.Warn("repoll failed", "repo", r.Repo, "run_id", r.ID, "err", err)
			continue
		}
		if wr.GetStatus() == "completed" {
			t.log.Info("repoll found completed run (missed webhook)", "repo", r.Repo, "run_id", r.ID, "conclusion", wr.GetConclusion())
		}
		t.apply(r.InstallationID, wr)
	}
}

func firstTime(ts ...*gh.Timestamp) time.Time {
	for _, t := range ts {
		if t != nil && !t.IsZero() {
			return t.Time
		}
	}
	return time.Time{}
}

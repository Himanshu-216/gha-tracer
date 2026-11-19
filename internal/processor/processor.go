// Package processor is the core pipeline: it consumes webhook events, keeps
// the live view current, and on run completion fetches the jobs, builds the
// tree, exports it as a trace and records metrics.
package processor

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v88/github"

	"github.com/Himanshu-216/gha-tracer/internal/github"
	"github.com/Himanshu-216/gha-tracer/internal/ids"
	"github.com/Himanshu-216/gha-tracer/internal/live"
	"github.com/Himanshu-216/gha-tracer/internal/metrics"
	"github.com/Himanshu-216/gha-tracer/internal/release"
	"github.com/Himanshu-216/gha-tracer/internal/tree"
	"github.com/Himanshu-216/gha-tracer/internal/webhook"
)

type Exporter interface {
	Export(ctx context.Context, t tree.Tree) error
}

type Options struct {
	TraceAll    bool
	TraceEvents []string
	Workers     int
	QueueSize   int
	MaxAttempts int           // processing attempts per event (default 5)
	RetryBase   time.Duration // first retry delay, doubles each time (default 30s)
}

type Processor struct {
	opts  Options
	cls   *release.Classifier
	api   github.API
	exp   Exporter
	store *live.Store
	m     *metrics.Metrics
	log   *slog.Logger

	queue chan item
	wg    sync.WaitGroup

	mu       sync.Mutex
	building map[string]bool // run keys currently being built (dedupe concurrent completes)
}

type item struct {
	ev      webhook.Event
	attempt int
}

func New(opts Options, cls *release.Classifier, api github.API, exp Exporter, store *live.Store, m *metrics.Metrics, log *slog.Logger) *Processor {
	if opts.MaxAttempts == 0 {
		opts.MaxAttempts = 5
	}
	if opts.RetryBase == 0 {
		opts.RetryBase = 30 * time.Second
	}
	if opts.Workers == 0 {
		opts.Workers = 1
	}
	if opts.QueueSize == 0 {
		opts.QueueSize = 100
	}
	return &Processor{
		opts: opts, cls: cls, api: api, exp: exp, store: store, m: m, log: log,
		queue:    make(chan item, opts.QueueSize),
		building: map[string]bool{},
	}
}

// Enqueue is the webhook sink; it never blocks.
func (p *Processor) Enqueue(ev webhook.Event) bool {
	select {
	case p.queue <- item{ev: ev}:
		p.m.LastEvent.SetToCurrentTime()
		return true
	default:
		return false
	}
}

// Start runs the workers until ctx is cancelled; Wait blocks until they exit.
func (p *Processor) Start(ctx context.Context) {
	for range p.opts.Workers {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case it := <-p.queue:
					p.process(ctx, it)
				}
			}
		}()
	}
}

func (p *Processor) Wait() { p.wg.Wait() }

// Drain processes whatever is queued right now (used on shutdown, with a
// deadline in ctx).
func (p *Processor) Drain(ctx context.Context) {
	for {
		select {
		case it := <-p.queue:
			p.process(ctx, it)
		default:
			return
		}
	}
}

func (p *Processor) process(ctx context.Context, it item) {
	err := p.handle(ctx, it.ev)
	if err == nil {
		return
	}
	it.attempt++
	if it.attempt >= p.opts.MaxAttempts || ctx.Err() != nil {
		p.log.Error("giving up on event (the re-poll loop will retry the run later)",
			"delivery", it.ev.DeliveryID, "type", it.ev.Type, "attempts", it.attempt, "err", err)
		return
	}
	delay := p.opts.RetryBase << (it.attempt - 1)
	p.log.Warn("event failed, will retry", "delivery", it.ev.DeliveryID, "attempt", it.attempt, "in", delay, "err", err)
	time.AfterFunc(delay, func() {
		select {
		case p.queue <- it:
		default:
			p.log.Error("retry dropped: queue full", "delivery", it.ev.DeliveryID)
		}
	})
}

func (p *Processor) handle(ctx context.Context, ev webhook.Event) error {
	switch e := ev.Payload.(type) {
	case *gh.WorkflowRunEvent:
		return p.handleRun(ctx, e.GetAction(), e.GetInstallation().GetID(), e.GetWorkflowRun())
	case *gh.WorkflowJobEvent:
		p.handleJob(e)
		return nil
	}
	return nil
}

// classify decides whether a run is tracked and how it's labelled.
func (p *Processor) classify(r *gh.WorkflowRun) (release.Info, bool) {
	info, ok := p.cls.Classify(r.GetHeadBranch())
	if p.opts.TraceAll {
		if !ok {
			info = release.Info{Type: "other"}
		}
		return info, true
	}
	if !ok || !slices.Contains(p.opts.TraceEvents, r.GetEvent()) {
		return release.Info{}, false
	}
	return info, true
}

func (p *Processor) handleRun(ctx context.Context, action string, installationID int64, r *gh.WorkflowRun) error {
	if r == nil || r.GetRepository().GetFullName() == "" {
		return nil
	}
	info, tracked := p.classify(r)
	key := ids.RunKey(r.GetRepository().GetFullName(), r.GetID(), max(r.GetRunAttempt(), 1))
	if !tracked {
		if p.store.Has(key) {
			p.store.Drop(key)
		}
		return nil
	}

	lr := liveRun(key, installationID, r, info)
	if action != "completed" {
		p.store.UpsertRun(lr)
		return nil
	}
	return p.complete(ctx, installationID, r, info, lr)
}

func (p *Processor) handleJob(e *gh.WorkflowJobEvent) {
	j := e.GetWorkflowJob()
	repo := e.GetRepo().GetFullName()
	if j == nil || repo == "" {
		return
	}
	key := ids.RunKey(repo, j.GetRunID(), int(max(j.GetRunAttempt(), 1)))
	p.store.UpdateJob(key, j.GetID(), live.Job{Name: j.GetName(), Status: j.GetStatus(), Conclusion: j.GetConclusion()})
}

// complete builds and exports the tree for a finished run. Idempotent: runs
// already exported (in this process) are skipped; across restarts the
// deterministic IDs make a re-export harmless.
func (p *Processor) complete(ctx context.Context, installationID int64, r *gh.WorkflowRun, info release.Info, lr live.Run) error {
	key := lr.Key
	if p.store.IsDone(key) {
		return nil
	}
	p.mu.Lock()
	if p.building[key] {
		p.mu.Unlock()
		return nil
	}
	p.building[key] = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.building, key)
		p.mu.Unlock()
	}()

	run := github.RunFromAPI(r)
	jobs, err := p.api.Jobs(ctx, installationID, run.Repo, run.ID, run.Attempt)
	if err != nil {
		p.m.ProcessErrors.WithLabelValues("fetch_jobs").Inc()
		return err
	}
	t := tree.Build(run, jobs, info)
	if err := p.exp.Export(ctx, t); err != nil {
		p.m.EmitErrors.Inc()
		p.m.ProcessErrors.WithLabelValues("export").Inc()
		return fmt.Errorf("export %s: %w", key, err)
	}

	p.recordMetrics(run, jobs, info)
	failed := 0
	for _, j := range jobs {
		if tree.IsError(j.Conclusion) {
			failed++
		}
	}
	lr.Status = "completed"
	lr.Conclusion = run.Conclusion
	lr.CompletedAt = run.UpdatedAt
	p.store.Complete(lr, len(jobs), failed)
	p.log.Info("run exported", "run", key, "release_line", info.Line, "change_type", info.Type,
		"conclusion", run.Conclusion, "jobs", len(jobs), "trace_id", fmt.Sprintf("%x", t.TraceID))
	return nil
}

func (p *Processor) recordMetrics(run tree.Run, jobs []tree.Job, info release.Info) {
	ct := string(info.Type)
	if !run.StartedAt.IsZero() && run.UpdatedAt.After(run.StartedAt) {
		p.m.RunDuration.WithLabelValues(run.Repo, run.Workflow, ct, run.Conclusion).
			Observe(run.UpdatedAt.Sub(run.StartedAt).Seconds())
	}
	p.m.RunsCompleted.WithLabelValues(run.Repo, run.Workflow, ct, run.Conclusion).Inc()
	switch run.Conclusion {
	case "failure", "timed_out", "startup_failure":
		p.m.RunFailures.WithLabelValues(run.Repo, run.Workflow, ct).Inc()
	}
	for _, j := range jobs {
		if j.StartedAt.IsZero() {
			continue
		}
		if j.CompletedAt.After(j.StartedAt) {
			p.m.JobDuration.WithLabelValues(run.Repo, run.Workflow, j.Name, j.Conclusion).
				Observe(j.CompletedAt.Sub(j.StartedAt).Seconds())
		}
		if !j.CreatedAt.IsZero() && !j.StartedAt.Before(j.CreatedAt) {
			p.m.JobQueue.WithLabelValues(run.Repo, strings.Join(j.Labels, ",")).
				Observe(j.StartedAt.Sub(j.CreatedAt).Seconds())
		}
	}
}

// Repoll catches runs whose "completed" webhook never arrived: any active run
// with no events for `after` is fetched from the API; if it has finished it's
// pushed through the normal completion path.
func (p *Processor) Repoll(ctx context.Context, interval, after time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.repollOnce(ctx, after)
		}
	}
}

func (p *Processor) repollOnce(ctx context.Context, after time.Duration) {
	for _, lr := range p.store.Quiet(after) {
		r, err := p.api.Run(ctx, lr.InstallationID, lr.Repo, lr.RunID, lr.Attempt)
		if err != nil {
			p.m.ProcessErrors.WithLabelValues("repoll").Inc()
			p.log.Warn("repoll failed", "run", lr.Key, "err", err)
			continue
		}
		if r.GetStatus() != "completed" {
			p.store.Touch(lr.Key, r.GetStatus())
			continue
		}
		p.log.Info("repoll found completed run (missed webhook)", "run", lr.Key)
		action := "completed"
		ev := webhook.Event{DeliveryID: "repoll:" + lr.Key, Type: "workflow_run", Payload: &gh.WorkflowRunEvent{
			Action:       &action,
			WorkflowRun:  r,
			Installation: &gh.Installation{ID: &lr.InstallationID},
		}}
		select {
		case p.queue <- item{ev: ev}:
		default:
		}
	}
}

func liveRun(key string, installationID int64, r *gh.WorkflowRun, info release.Info) live.Run {
	return live.Run{
		Key:            key,
		InstallationID: installationID,
		Repo:           r.GetRepository().GetFullName(),
		Workflow:       r.GetName(),
		RunID:          r.GetID(),
		Attempt:        max(r.GetRunAttempt(), 1),
		ReleaseLine:    info.Line,
		Version:        info.Version,
		ChangeType:     string(info.Type),
		HeadBranch:     r.GetHeadBranch(),
		HeadSHA:        r.GetHeadSHA(),
		Event:          r.GetEvent(),
		Status:         r.GetStatus(),
		Conclusion:     r.GetConclusion(),
		HTMLURL:        r.GetHTMLURL(),
		StartedAt:      github.RunFromAPI(r).StartedAt,
	}
}

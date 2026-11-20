// Package runs keeps the state of tag-triggered workflow runs in memory and
// exposes it as Prometheus gauges, one series per run:
//
//	gha_tag_run_status{repo,workflow,tag,run_id}          0 running, 1 success, 2 failure, 3 cancelled, 4 timed out, 5 other
//	gha_tag_run_started_seconds{...}                      run (attempt) start, unix seconds
//	gha_tag_run_duration_seconds{...}                     elapsed so far while running, final when done
//	gha_tag_run_updated_seconds{...}                      when this process last changed the run
//
// Prometheus is the history: a finished run stays exported for `keep` and is
// then dropped here, while its samples remain queryable for the full
// Prometheus retention. A re-run keeps the same run_id, so its latest attempt
// overwrites the previous result.
package runs

import (
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type Status int

const (
	Running Status = iota
	Success
	Failure
	Cancelled
	TimedOut
	Other
)

// StatusOf maps a GitHub run status/conclusion to a Status.
func StatusOf(status, conclusion string) Status {
	if status != "completed" {
		return Running
	}
	switch conclusion {
	case "success":
		return Success
	case "failure", "startup_failure":
		return Failure
	case "cancelled":
		return Cancelled
	case "timed_out":
		return TimedOut
	}
	return Other // neutral, skipped, action_required, stale
}

type Run struct {
	InstallationID int64
	Repo           string // org/repo
	Workflow       string
	Tag            string
	ID             int64
	Attempt        int
	Status         Status
	StartedAt      time.Time
	CompletedAt    time.Time // zero while running

	updated  time.Time // last state change
	lastSeen time.Time // last webhook or successful re-poll
}

// forgetSilent: a running run nobody has heard about for this long (no
// webhook, every re-poll failing) is dropped rather than shown forever.
const forgetSilent = 24 * time.Hour

type Store struct {
	mu   sync.Mutex
	runs map[string]*Run
	keep time.Duration
	now  func() time.Time
}

func NewStore(keep time.Duration) *Store {
	return &Store{runs: map[string]*Run{}, keep: keep, now: time.Now}
}

func key(repo string, id int64) string { return repo + "#" + strconv.FormatInt(id, 10) }

// Apply records the latest known state of a run. Out-of-order events are
// ignored: an older attempt never replaces a newer one, and a late
// "in_progress" never revives a completed attempt.
func (s *Store) Apply(r Run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	k := key(r.Repo, r.ID)
	if cur, ok := s.runs[k]; ok {
		if r.Attempt < cur.Attempt || (r.Attempt == cur.Attempt && cur.Status != Running && r.Status == Running) {
			return
		}
		if r.StartedAt.IsZero() && r.Attempt == cur.Attempt {
			r.StartedAt = cur.StartedAt
		}
		if r.InstallationID == 0 {
			r.InstallationID = cur.InstallationID
		}
		r.updated = cur.updated
		if r.Status != cur.Status || r.Attempt != cur.Attempt {
			r.updated = now
		}
	} else {
		r.updated = now
	}
	r.lastSeen = now
	s.runs[k] = &r
}

// Quiet returns running runs with no news for longer than d (to re-poll).
func (s *Store) Quiet(d time.Duration) []Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var out []Run
	for _, r := range s.runs {
		if r.Status == Running && now.Sub(r.lastSeen) > d {
			out = append(out, *r)
		}
	}
	return out
}

// Len is the number of runs currently exported.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

func (s *Store) prune(now time.Time) {
	for k, r := range s.runs {
		if (r.Status != Running && now.Sub(r.updated) > s.keep) ||
			(r.Status == Running && now.Sub(r.lastSeen) > forgetSilent) {
			delete(s.runs, k)
		}
	}
}

var (
	labels      = []string{"repo", "workflow", "tag", "run_id"}
	statusDesc  = prometheus.NewDesc("gha_tag_run_status", "Tag run status: 0 running, 1 success, 2 failure, 3 cancelled, 4 timed out, 5 other.", labels, nil)
	startedDesc = prometheus.NewDesc("gha_tag_run_started_seconds", "Unix time the run (latest attempt) started.", labels, nil)
	durDesc     = prometheus.NewDesc("gha_tag_run_duration_seconds", "Run duration; elapsed time while still running.", labels, nil)
	updatedDesc = prometheus.NewDesc("gha_tag_run_updated_seconds", "Unix time this process last saw the run change.", labels, nil)
)

func (s *Store) Describe(ch chan<- *prometheus.Desc) {
	ch <- statusDesc
	ch <- startedDesc
	ch <- durDesc
	ch <- updatedDesc
}

func (s *Store) Collect(ch chan<- prometheus.Metric) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.prune(now)
	for _, r := range s.runs {
		lv := []string{r.Repo, r.Workflow, r.Tag, strconv.FormatInt(r.ID, 10)}
		ch <- prometheus.MustNewConstMetric(statusDesc, prometheus.GaugeValue, float64(r.Status), lv...)
		ch <- prometheus.MustNewConstMetric(updatedDesc, prometheus.GaugeValue, float64(r.updated.Unix()), lv...)
		if r.StartedAt.IsZero() {
			continue
		}
		end := r.CompletedAt
		if r.Status == Running || end.IsZero() {
			end = now
		}
		ch <- prometheus.MustNewConstMetric(startedDesc, prometheus.GaugeValue, float64(r.StartedAt.Unix()), lv...)
		ch <- prometheus.MustNewConstMetric(durDesc, prometheus.GaugeValue, max(end.Sub(r.StartedAt).Seconds(), 0), lv...)
	}
}

// Package live keeps an in-memory view of in-progress and recently completed
// runs. It is deliberately lossy: after a restart it refills from new webhook
// events. Finished trees live in Tempo, not here.
package live

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"
)

type Run struct {
	Key            string `json:"-"`
	InstallationID int64  `json:"-"`

	Repo        string    `json:"repo"`
	Workflow    string    `json:"workflow"`
	RunID       int64     `json:"run_id"`
	Attempt     int       `json:"attempt"`
	ReleaseLine string    `json:"release_line"`
	Version     string    `json:"version"`
	ChangeType  string    `json:"change_type"`
	HeadBranch  string    `json:"head_branch"`
	HeadSHA     string    `json:"head_sha"`
	Event       string    `json:"event"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion"`
	HTMLURL     string    `json:"url"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at,omitzero"`
	LastSeen    time.Time `json:"last_seen"`

	jobs map[int64]Job
}

type Job struct {
	Name       string
	Status     string
	Conclusion string
}

// Row is the flat JSON shape served to Grafana (Infinity).
type Row struct {
	Run
	DurationSeconds float64  `json:"duration_seconds"`
	JobsTotal       int      `json:"jobs_total"`
	JobsCompleted   int      `json:"jobs_completed"`
	JobsFailed      int      `json:"jobs_failed"`
	RunningJobs     []string `json:"running_jobs"`
}

type Store struct {
	mu        sync.Mutex
	active    map[string]*Run
	recent    []Row // newest last, capped
	maxRecent int
	done      map[string]time.Time // keys already completed (guards late events)
	doneTTL   time.Duration
	now       func() time.Time
}

func NewStore(maxRecent int) *Store {
	return &Store{
		active:    map[string]*Run{},
		maxRecent: maxRecent,
		done:      map[string]time.Time{},
		doneTTL:   24 * time.Hour,
		now:       time.Now,
	}
}

// UpsertRun records or refreshes an in-progress run. Ignored once the run has
// completed, so out-of-order "requested"/"in_progress" events can't revive it.
func (s *Store) UpsertRun(r Run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.done[r.Key]; ok {
		return
	}
	r.LastSeen = s.now()
	if cur, ok := s.active[r.Key]; ok {
		r.jobs = cur.jobs
		if r.StartedAt.IsZero() {
			r.StartedAt = cur.StartedAt
		}
	}
	if r.jobs == nil {
		r.jobs = map[int64]Job{}
	}
	s.active[r.Key] = &r
}

// UpdateJob updates a job of an active run; unknown runs are ignored.
func (s *Store) UpdateJob(runKey string, jobID int64, j Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.active[runKey]
	if !ok {
		return
	}
	r.jobs[jobID] = j
	r.LastSeen = s.now()
	if r.Status == "queued" || r.Status == "requested" || r.Status == "waiting" {
		if j.Status == "in_progress" {
			r.Status = "in_progress"
		}
	}
}

func (s *Store) Has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.active[key]
	return ok
}

func (s *Store) IsDone(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.done[key]
	return ok
}

// Complete moves a run to the recent list and marks it done. r carries the
// final state (from the completed event / API).
func (s *Store) Complete(r Run, jobsTotal, jobsFailed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if cur, ok := s.active[r.Key]; ok && r.StartedAt.IsZero() {
		r.StartedAt = cur.StartedAt
	}
	delete(s.active, r.Key)
	s.markDone(r.Key, now)
	r.LastSeen = now
	row := Row{Run: r, JobsTotal: jobsTotal, JobsCompleted: jobsTotal, JobsFailed: jobsFailed, RunningJobs: []string{}}
	if !r.StartedAt.IsZero() && !r.CompletedAt.IsZero() {
		row.DurationSeconds = r.CompletedAt.Sub(r.StartedAt).Seconds()
	}
	s.recent = append(s.recent, row)
	if over := len(s.recent) - s.maxRecent; over > 0 {
		s.recent = append([]Row(nil), s.recent[over:]...)
	}
}

// Drop forgets an active run without recording it (e.g. filtered out).
func (s *Store) Drop(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, key)
	s.markDone(key, s.now())
}

func (s *Store) markDone(key string, now time.Time) {
	s.done[key] = now
	if len(s.done)%256 == 0 {
		for k, t := range s.done {
			if now.Sub(t) > s.doneTTL {
				delete(s.done, k)
			}
		}
	}
}

// Quiet returns active runs with no events for longer than d (to re-poll).
func (s *Store) Quiet(d time.Duration) []Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var out []Run
	for _, r := range s.active {
		if now.Sub(r.LastSeen) > d {
			out = append(out, *r)
		}
	}
	return out
}

// Touch refreshes LastSeen (after a re-poll found the run still running).
func (s *Store) Touch(key, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.active[key]; ok {
		r.LastSeen = s.now()
		if status != "" {
			r.Status = status
		}
	}
}

// GaugeKey groups active runs for metrics.
type GaugeKey struct{ Repo, ChangeType string }

// Counts returns active and stale (running longer than staleAfter) counts.
func (s *Store) Counts(staleAfter time.Duration) (active, stale map[GaugeKey]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	active, stale = map[GaugeKey]int{}, map[GaugeKey]int{}
	for _, r := range s.active {
		k := GaugeKey{r.Repo, r.ChangeType}
		active[k]++
		if !r.StartedAt.IsZero() && now.Sub(r.StartedAt) > staleAfter {
			stale[k]++
		}
	}
	return active, stale
}

func (s *Store) Active() []Row {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	rows := make([]Row, 0, len(s.active))
	for _, r := range s.active {
		row := Row{Run: *r, RunningJobs: []string{}}
		if !r.StartedAt.IsZero() {
			row.DurationSeconds = now.Sub(r.StartedAt).Seconds()
		}
		for _, j := range r.jobs {
			row.JobsTotal++
			switch {
			case j.Status == "completed":
				row.JobsCompleted++
				if j.Conclusion == "failure" || j.Conclusion == "timed_out" || j.Conclusion == "cancelled" {
					row.JobsFailed++
				}
			case j.Status == "in_progress":
				row.RunningJobs = append(row.RunningJobs, j.Name)
			}
		}
		sort.Strings(row.RunningJobs)
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].StartedAt.Before(rows[j].StartedAt) })
	return rows
}

func (s *Store) Recent() []Row {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Row, len(s.recent))
	for i := range s.recent { // newest first
		out[i] = s.recent[len(s.recent)-1-i]
	}
	return out
}

// Handler serves /api/active and /api/recent with optional filters
// ?repo=&release_line=&change_type=
func (s *Store) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/active", func(w http.ResponseWriter, r *http.Request) {
		writeRows(w, filter(s.Active(), r))
	})
	mux.HandleFunc("GET /api/recent", func(w http.ResponseWriter, r *http.Request) {
		writeRows(w, filter(s.Recent(), r))
	})
	return mux
}

func filter(rows []Row, r *http.Request) []Row {
	q := r.URL.Query()
	repo, line, typ := q.Get("repo"), q.Get("release_line"), q.Get("change_type")
	out := rows[:0:0]
	for _, row := range rows {
		if (repo == "" || row.Repo == repo) && (line == "" || row.ReleaseLine == line) && (typ == "" || row.ChangeType == typ) {
			out = append(out, row)
		}
	}
	return out
}

func writeRows(w http.ResponseWriter, rows []Row) {
	w.Header().Set("Content-Type", "application/json")
	if rows == nil {
		rows = []Row{}
	}
	_ = json.NewEncoder(w).Encode(rows)
}

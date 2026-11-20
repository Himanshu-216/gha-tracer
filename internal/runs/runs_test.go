package runs

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

var t0 = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

func newStore(now *time.Time) *Store {
	s := NewStore(time.Hour)
	s.now = func() time.Time { return *now }
	return s
}

func run(attempt int, st Status) Run {
	r := Run{Repo: "acme/payments", Workflow: "Release", Tag: "v1.20.0", ID: 9001, Attempt: attempt, Status: st, StartedAt: t0}
	if st != Running {
		r.CompletedAt = t0.Add(30 * time.Minute)
	}
	return r
}

func TestStatusOf(t *testing.T) {
	cases := map[[2]string]Status{
		{"queued", ""}:                   Running,
		{"in_progress", ""}:              Running,
		{"completed", "success"}:         Success,
		{"completed", "failure"}:         Failure,
		{"completed", "startup_failure"}: Failure,
		{"completed", "cancelled"}:       Cancelled,
		{"completed", "timed_out"}:       TimedOut,
		{"completed", "skipped"}:         Other,
	}
	for in, want := range cases {
		if got := StatusOf(in[0], in[1]); got != want {
			t.Errorf("StatusOf(%q,%q) = %d, want %d", in[0], in[1], got, want)
		}
	}
}

func TestExposition(t *testing.T) {
	now := t0.Add(30 * time.Minute)
	s := newStore(&now)
	s.Apply(run(1, Success))
	want := `
# HELP gha_tag_run_status Tag run status: 0 running, 1 success, 2 failure, 3 cancelled, 4 timed out, 5 other.
# TYPE gha_tag_run_status gauge
gha_tag_run_status{repo="acme/payments",run_id="9001",tag="v1.20.0",workflow="Release"} 1
# HELP gha_tag_run_duration_seconds Run duration; elapsed time while still running.
# TYPE gha_tag_run_duration_seconds gauge
gha_tag_run_duration_seconds{repo="acme/payments",run_id="9001",tag="v1.20.0",workflow="Release"} 1800
`
	if err := testutil.CollectAndCompare(s, strings.NewReader(want), "gha_tag_run_status", "gha_tag_run_duration_seconds"); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleAndOrdering(t *testing.T) {
	now := t0
	s := newStore(&now)

	s.Apply(run(1, Running))
	now = now.Add(10 * time.Minute)
	if v := value(t, s, "gha_tag_run_duration_seconds"); v != 600 {
		t.Fatalf("running duration = %v, want 600", v)
	}

	now = now.Add(20 * time.Minute)
	s.Apply(run(1, Failure))
	s.Apply(run(1, Running)) // late in_progress must not revive it
	if v := value(t, s, "gha_tag_run_status"); v != float64(Failure) {
		t.Fatalf("status = %v, want failure", v)
	}
	if v := value(t, s, "gha_tag_run_duration_seconds"); v != 1800 {
		t.Fatalf("final duration = %v, want 1800", v)
	}

	// Re-run: attempt 2 replaces attempt 1; a late attempt-1 event is ignored.
	r2 := run(2, Success)
	r2.StartedAt = t0.Add(time.Hour)
	r2.CompletedAt = r2.StartedAt.Add(5 * time.Minute)
	now = r2.CompletedAt
	s.Apply(r2)
	s.Apply(run(1, Failure))
	if v := value(t, s, "gha_tag_run_status"); v != float64(Success) {
		t.Fatalf("status after re-run = %v, want success", v)
	}
	if s.Len() != 1 {
		t.Fatalf("len = %d, want 1 run", s.Len())
	}

	// Finished runs are dropped after `keep` (Prometheus keeps the history).
	now = now.Add(61 * time.Minute)
	if n := testutil.CollectAndCount(s); n != 0 {
		t.Fatalf("series after keep = %d, want 0", n)
	}
}

func TestQuietAndForget(t *testing.T) {
	now := t0
	s := newStore(&now)
	s.Apply(run(1, Running))
	if len(s.Quiet(20*time.Minute)) != 0 {
		t.Fatal("fresh run reported quiet")
	}
	now = now.Add(21 * time.Minute)
	if q := s.Quiet(20 * time.Minute); len(q) != 1 || q[0].ID != 9001 {
		t.Fatalf("quiet = %+v", q)
	}
	now = now.Add(forgetSilent)
	if n := testutil.CollectAndCount(s); n != 0 {
		t.Fatalf("silent running run still exported (%d series)", n)
	}
}

// value returns the single sample of one metric family.
func value(t *testing.T, s *Store, name string) float64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(s)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name && len(mf.GetMetric()) == 1 {
			return mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("no single sample for %s", name)
	return 0
}

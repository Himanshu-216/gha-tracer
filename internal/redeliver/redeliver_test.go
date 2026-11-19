package redeliver

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"testing"
	"time"

	gh "github.com/google/go-github/v88/github"

	"github.com/Himanshu-216/gha-tracer/internal/github"
	"github.com/Himanshu-216/gha-tracer/internal/tree"
)

type fakeAPI struct {
	ds          []github.Delivery
	since       []time.Time
	redelivered []int64
}

func (f *fakeAPI) Jobs(context.Context, int64, string, int64, int) ([]tree.Job, error) {
	return nil, nil
}
func (f *fakeAPI) Run(context.Context, int64, string, int64, int) (*gh.WorkflowRun, error) {
	return nil, nil
}
func (f *fakeAPI) Deliveries(_ context.Context, since time.Time) ([]github.Delivery, error) {
	f.since = append(f.since, since)
	return f.ds, nil
}
func (f *fakeAPI) Redeliver(_ context.Context, id int64) error {
	f.redelivered = append(f.redelivered, id)
	return nil
}

func TestSweep(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f := &fakeAPI{ds: []github.Delivery{
		{ID: 1, GUID: "a", Event: "workflow_run", StatusCode: 503, DeliveredAt: now.Add(-3 * time.Minute)},
		{ID: 2, GUID: "a", Event: "workflow_run", StatusCode: 0, DeliveredAt: now.Add(-2 * time.Minute)}, // latest failure for a
		{ID: 3, GUID: "b", Event: "workflow_job", StatusCode: 502, DeliveredAt: now.Add(-2 * time.Minute)},
		{ID: 4, GUID: "b", Event: "workflow_job", StatusCode: 202, DeliveredAt: now.Add(-1 * time.Minute)}, // b recovered
		{ID: 5, GUID: "c", Event: "workflow_job", StatusCode: 202, DeliveredAt: now},
		{ID: 6, GUID: "d", Event: "installation", StatusCode: 500, DeliveredAt: now}, // not ours
	}}
	results := map[string]int{}
	s := New(f, time.Minute, time.Hour, func(r string) { results[r]++ }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.now = func() time.Time { return now }

	s.Sweep(context.Background())
	if len(f.redelivered) != 1 || f.redelivered[0] != 2 {
		t.Fatalf("redelivered = %v, want [2]", f.redelivered)
	}
	if want := now.Add(-time.Hour - time.Minute); !f.since[0].Equal(want) {
		t.Fatalf("first sweep since = %v, want %v", f.since[0], want)
	}

	// Subsequent sweeps look back only to the previous sweep (+1m overlap),
	// and give up on a GUID after maxAttempts.
	for range 5 {
		s.Sweep(context.Background())
	}
	if !f.since[1].Equal(now.Add(-time.Minute)) {
		t.Fatalf("second sweep since = %v", f.since[1])
	}
	sort.Slice(f.redelivered, func(i, j int) bool { return f.redelivered[i] < f.redelivered[j] })
	if len(f.redelivered) != 3 {
		t.Fatalf("redeliveries = %d, want 3 (maxAttempts)", len(f.redelivered))
	}
	if results["gave_up"] != 1 || results["requested"] != 3 {
		t.Fatalf("results = %v", results)
	}
}

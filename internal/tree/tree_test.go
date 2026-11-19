package tree

import (
	"testing"
	"time"

	"github.com/Himanshu-216/gha-tracer/internal/ids"
	"github.com/Himanshu-216/gha-tracer/internal/release"
)

var t0 = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

func at(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }

func fixture() (Run, []Job) {
	run := Run{
		Repo: "org/repo-a", ID: 123, Attempt: 1, Workflow: "Release",
		HeadBranch: "v1.20.0", HeadSHA: "abc123", Event: "push", Conclusion: "failure",
		StartedAt: at(0), UpdatedAt: at(30),
	}
	jobs := []Job{
		{ID: 1, Name: "build (linux)", Conclusion: "success", CreatedAt: at(0), StartedAt: at(1), CompletedAt: at(5),
			Steps: []Step{
				{Number: 1, Name: "checkout", Conclusion: "success", StartedAt: at(1), CompletedAt: at(2)},
				{Number: 2, Name: "make", Conclusion: "success", StartedAt: at(2), CompletedAt: at(5)},
			}},
		{ID: 3, Name: "deploy / apply", Conclusion: "failure", CreatedAt: at(10), StartedAt: at(12), CompletedAt: at(20)},
		{ID: 2, Name: "deploy / plan", Conclusion: "success", CreatedAt: at(5), StartedAt: at(6), CompletedAt: at(10)},
		{ID: 4, Name: "notify", Conclusion: "skipped",
			Steps: []Step{{Number: 1, Name: "slack", Conclusion: "skipped"}}},
	}
	return run, jobs
}

func TestBuildHierarchy(t *testing.T) {
	run, jobs := fixture()
	rel, _ := release.MustDefault().Classify(run.HeadBranch)
	tr := Build(run, jobs, rel)

	if tr.TraceID != ids.Trace("org/repo-a#123#1") {
		t.Fatal("unexpected trace ID")
	}
	if len(tr.Links) != 0 {
		t.Fatal("attempt 1 must have no links")
	}
	r := tr.Root
	if r.Attrs["gha.release_line"] != "v1.20" || r.Attrs["gha.change_type"] != "release" {
		t.Fatalf("bad release attrs: %v", r.Attrs)
	}

	// Children sorted by start: build(1m), deploy call(6m), notify(zero-length at run end).
	names := []string{}
	for _, c := range r.Children {
		names = append(names, string(c.Kind)+":"+c.Name)
	}
	want := []string{"job:build (linux)", "call:deploy", "job:notify"}
	if len(names) != len(want) {
		t.Fatalf("children = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("children = %v, want %v", names, want)
		}
	}

	call := r.Children[1]
	if call.ParentID != r.SpanID {
		t.Error("call must parent to run")
	}
	if !call.Start.Equal(at(6)) || !call.End.Equal(at(20)) {
		t.Errorf("call span = %v..%v", call.Start, call.End)
	}
	if call.Conclusion != "failure" {
		t.Errorf("call conclusion = %q, want failure", call.Conclusion)
	}
	if call.Children[0].Name != "deploy / plan" || call.Children[0].ParentID != call.SpanID {
		t.Error("jobs under call wrong order or parent")
	}

	notify := r.Children[2]
	if !notify.Start.Equal(at(30)) || !notify.End.Equal(notify.Start) {
		t.Errorf("skipped job should be zero-length at run end, got %v..%v", notify.Start, notify.End)
	}
	if st := notify.Children[0]; !st.Start.Equal(notify.End) || st.ParentID != notify.SpanID {
		t.Error("skipped step should be zero-length at job end under its job")
	}

	build := r.Children[0]
	if q := build.Attrs["gha.queue_seconds"]; q != 60.0 {
		t.Errorf("queue_seconds = %v", q)
	}
	if _, ok := notify.Attrs["gha.queue_seconds"]; ok {
		t.Error("never-started job must not report queue time")
	}

	count := 0
	tr.Walk(func(*Node) { count++ })
	if count != 1+1+4+3 { // run + call + 4 jobs + 3 steps
		t.Errorf("node count = %d", count)
	}
}

func TestDeterministic(t *testing.T) {
	run, jobs := fixture()
	rel, _ := release.MustDefault().Classify(run.HeadBranch)
	a, b := Build(run, jobs, rel), Build(run, jobs, rel)
	var as, bs []ids.SpanID
	a.Walk(func(n *Node) { as = append(as, n.SpanID) })
	b.Walk(func(n *Node) { bs = append(bs, n.SpanID) })
	for i := range as {
		if as[i] != bs[i] {
			t.Fatal("non-deterministic span IDs")
		}
	}
}

func TestRerunLinksPreviousAttempt(t *testing.T) {
	run, jobs := fixture()
	run.Attempt = 2
	tr := Build(run, jobs, release.Info{})
	if len(tr.Links) != 1 || tr.Links[0].TraceID != ids.Trace("org/repo-a#123#1") ||
		tr.Links[0].SpanID != ids.RunSpan("org/repo-a#123#1") {
		t.Fatalf("links = %+v", tr.Links)
	}
}

func TestMatrixReusableCallsGroupSeparately(t *testing.T) {
	run, _ := fixture()
	jobs := []Job{
		{ID: 10, Name: "deploy (prod) / apply", StartedAt: at(1), CompletedAt: at(2), Conclusion: "success"},
		{ID: 11, Name: "deploy (stage) / apply", StartedAt: at(1), CompletedAt: at(3), Conclusion: "success"},
	}
	tr := Build(run, jobs, release.Info{})
	if len(tr.Root.Children) != 2 || tr.Root.Children[0].Kind != KindCall || tr.Root.Children[1].Kind != KindCall {
		t.Fatalf("expected two call groups, got %d children", len(tr.Root.Children))
	}
}

func TestMissingRunTimesFallBackToChildren(t *testing.T) {
	run, jobs := fixture()
	run.StartedAt, run.UpdatedAt = time.Time{}, time.Time{}
	tr := Build(run, jobs[:3], release.Info{})
	if !tr.Root.Start.Equal(at(1)) || !tr.Root.End.Equal(at(20)) {
		t.Errorf("root = %v..%v", tr.Root.Start, tr.Root.End)
	}
}

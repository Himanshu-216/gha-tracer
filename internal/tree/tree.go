// Package tree turns one completed workflow run plus its jobs into a
// run → reusable-call → job → step hierarchy with deterministic IDs.
//
// Reusable workflows (workflow_call) don't get their own run: their jobs show
// up in the caller's run named "<caller job> / <called job>". Jobs sharing a
// caller prefix are grouped under a synthetic "call" node.
package tree

import (
	"sort"
	"strings"
	"time"

	"github.com/Himanshu-216/gha-tracer/internal/ids"
	"github.com/Himanshu-216/gha-tracer/internal/release"
)

// Input types are deliberately decoupled from go-github; the github package
// adapts API responses into these.

type Run struct {
	Repo                string // org/repo
	ID                  int64
	Attempt             int
	Workflow            string
	HeadBranch          string
	HeadSHA             string
	Event               string
	Conclusion          string
	Actor               string
	HTMLURL             string
	StartedAt           time.Time // run_started_at
	UpdatedAt           time.Time // end time for a completed run
	ReferencedWorkflows []string  // workflow_run.referenced_workflows[].path
}

type Job struct {
	ID          int64
	Name        string
	Conclusion  string
	CreatedAt   time.Time
	StartedAt   time.Time
	CompletedAt time.Time
	Labels      []string
	RunnerName  string
	HTMLURL     string
	Steps       []Step
}

type Step struct {
	Number      int
	Name        string
	Conclusion  string
	StartedAt   time.Time
	CompletedAt time.Time
}

type Kind string

const (
	KindRun  Kind = "run"
	KindCall Kind = "call"
	KindJob  Kind = "job"
	KindStep Kind = "step"
)

type Node struct {
	Kind       Kind
	Name       string
	SpanID     ids.SpanID
	ParentID   ids.SpanID // zero for the root
	Start, End time.Time
	Conclusion string
	Attrs      map[string]any
	Children   []*Node
}

// Link points at another trace's span (used for previous run attempts).
type Link struct {
	TraceID ids.TraceID
	SpanID  ids.SpanID
}

type Tree struct {
	TraceID ids.TraceID
	Root    *Node
	Links   []Link
}

// CallSeparator is how GitHub names jobs from reusable workflows.
const CallSeparator = " / "

// Build constructs the tree. It never fails: missing timestamps (skipped or
// never-started jobs/steps) become zero-length nodes clamped into the run.
func Build(run Run, jobs []Job, rel release.Info) Tree {
	key := ids.RunKey(run.Repo, run.ID, run.Attempt)
	runSpan := ids.RunSpan(key)

	root := &Node{
		Kind:       KindRun,
		Name:       run.Workflow,
		SpanID:     runSpan,
		Start:      run.StartedAt,
		End:        run.UpdatedAt,
		Conclusion: run.Conclusion,
		Attrs: map[string]any{
			"gha.release_line":         rel.Line,
			"gha.version":              rel.Version,
			"gha.change_type":          string(rel.Type),
			"gha.head_sha":             run.HeadSHA,
			"vcs.repository.name":      run.Repo,
			"vcs.ref.head.name":        run.HeadBranch,
			"gha.workflow":             run.Workflow,
			"gha.run_id":               run.ID,
			"gha.run_attempt":          int64(run.Attempt),
			"gha.event":                run.Event,
			"gha.conclusion":           run.Conclusion,
			"gha.actor":                run.Actor,
			"gha.html_url":             run.HTMLURL,
			"gha.referenced_workflows": strings.Join(run.ReferencedWorkflows, ","),
		},
	}

	calls := map[string]*Node{}
	for _, j := range jobs {
		parent := root
		if caller, _, ok := strings.Cut(j.Name, CallSeparator); ok {
			c := calls[caller]
			if c == nil {
				c = &Node{
					Kind:     KindCall,
					Name:     caller,
					SpanID:   ids.CallSpan(key, caller),
					ParentID: runSpan,
					Attrs:    map[string]any{"gha.call": caller},
				}
				calls[caller] = c
				root.Children = append(root.Children, c)
			}
			parent = c
		}
		parent.Children = append(parent.Children, buildJob(j, parent.SpanID, root))
	}

	for _, c := range calls {
		c.Start, c.End = span(c.Children)
		c.Conclusion = worst(c.Children)
		c.Attrs["gha.conclusion"] = c.Conclusion
	}

	// Run timestamps can be missing in odd payloads; fall back to children.
	if root.Start.IsZero() || root.End.IsZero() {
		s, e := span(root.Children)
		if root.Start.IsZero() {
			root.Start = s
		}
		if root.End.IsZero() {
			root.End = e
		}
	}

	sortTree(root)

	t := Tree{TraceID: ids.Trace(key), Root: root}
	if run.Attempt > 1 {
		prev := ids.RunKey(run.Repo, run.ID, run.Attempt-1)
		t.Links = []Link{{TraceID: ids.Trace(prev), SpanID: ids.RunSpan(prev)}}
	}
	return t
}

func buildJob(j Job, parentID ids.SpanID, root *Node) *Node {
	start, end := j.StartedAt, j.CompletedAt
	if start.IsZero() { // never started (skipped / cancelled while queued)
		start = firstNonZero(j.CompletedAt, j.CreatedAt, root.End)
	}
	if end.IsZero() || end.Before(start) {
		end = start
	}

	attrs := map[string]any{
		"gha.job_id":       j.ID,
		"gha.conclusion":   j.Conclusion,
		"gha.runner_label": strings.Join(j.Labels, ","),
		"gha.runner_name":  j.RunnerName,
		"gha.html_url":     j.HTMLURL,
	}
	if !j.CreatedAt.IsZero() && !j.StartedAt.IsZero() && !j.StartedAt.Before(j.CreatedAt) {
		attrs["gha.queue_seconds"] = j.StartedAt.Sub(j.CreatedAt).Seconds()
	}

	n := &Node{
		Kind:       KindJob,
		Name:       j.Name,
		SpanID:     ids.JobSpan(j.ID),
		ParentID:   parentID,
		Start:      start,
		End:        end,
		Conclusion: j.Conclusion,
		Attrs:      attrs,
	}
	for _, s := range j.Steps {
		ss, se := s.StartedAt, s.CompletedAt
		if ss.IsZero() {
			ss = end // skipped step: zero-length at job end
		}
		if se.IsZero() || se.Before(ss) {
			se = ss
		}
		n.Children = append(n.Children, &Node{
			Kind:       KindStep,
			Name:       s.Name,
			SpanID:     ids.StepSpan(j.ID, s.Number),
			ParentID:   n.SpanID,
			Start:      ss,
			End:        se,
			Conclusion: s.Conclusion,
			Attrs: map[string]any{
				"gha.step_number": int64(s.Number),
				"gha.conclusion":  s.Conclusion,
			},
		})
	}
	return n
}

// IsError reports whether a conclusion should mark a span as failed.
func IsError(conclusion string) bool {
	switch conclusion {
	case "failure", "cancelled", "timed_out", "startup_failure", "action_required":
		return true
	}
	return false
}

var severity = map[string]int{
	"": 0, "skipped": 1, "neutral": 2, "success": 3,
	"action_required": 4, "cancelled": 5, "timed_out": 6, "startup_failure": 7, "failure": 8,
}

func worst(ns []*Node) string {
	w := ""
	for _, n := range ns {
		if severity[n.Conclusion] > severity[w] {
			w = n.Conclusion
		}
	}
	return w
}

func span(ns []*Node) (start, end time.Time) {
	for _, n := range ns {
		if start.IsZero() || (!n.Start.IsZero() && n.Start.Before(start)) {
			start = n.Start
		}
		if n.End.After(end) {
			end = n.End
		}
	}
	return start, end
}

func firstNonZero(ts ...time.Time) time.Time {
	for _, t := range ts {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

func sortTree(n *Node) {
	sort.SliceStable(n.Children, func(i, j int) bool {
		a, b := n.Children[i], n.Children[j]
		if !a.Start.Equal(b.Start) {
			return a.Start.Before(b.Start)
		}
		return a.Name < b.Name
	})
	for _, c := range n.Children {
		sortTree(c)
	}
}

// Walk visits every node depth-first, parents before children.
func (t Tree) Walk(fn func(*Node)) {
	var rec func(*Node)
	rec = func(n *Node) {
		fn(n)
		for _, c := range n.Children {
			rec(c)
		}
	}
	rec(t.Root)
}

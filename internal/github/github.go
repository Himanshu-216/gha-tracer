// Package github wraps the GitHub App APIs the service needs and adapts
// go-github types into tree types.
package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	gh "github.com/google/go-github/v88/github"

	"github.com/Himanshu-216/gha-tracer/internal/tree"
)

// API is what the rest of the service depends on (faked in tests).
type API interface {
	Jobs(ctx context.Context, installationID int64, repo string, runID int64, attempt int) ([]tree.Job, error)
	Run(ctx context.Context, installationID int64, repo string, runID int64, attempt int) (*gh.WorkflowRun, error)
	Deliveries(ctx context.Context, since time.Time) ([]Delivery, error)
	Redeliver(ctx context.Context, deliveryID int64) error
}

type Delivery struct {
	ID          int64
	GUID        string // X-GitHub-Delivery; shared by redeliveries
	Event       string
	Action      string
	StatusCode  int
	DeliveredAt time.Time
	Redelivery  bool
}

// OnCall is invoked after each API call with an endpoint name and result
// ("ok" / "error"); used for metrics.
type OnCall func(endpoint, result string)

type Client struct {
	appID   int64
	key     []byte
	baseURL string
	apps    *gh.Client
	onCall  OnCall

	mu    sync.Mutex
	insts map[int64]*gh.Client
}

func New(appID int64, privateKey []byte, baseURL string, onCall OnCall) (*Client, error) {
	atr, err := ghinstallation.NewAppsTransport(http.DefaultTransport, appID, privateKey)
	if err != nil {
		return nil, fmt.Errorf("github: app transport: %w", err)
	}
	if baseURL != "" {
		atr.BaseURL = strings.TrimRight(baseURL, "/")
	}
	if onCall == nil {
		onCall = func(string, string) {}
	}
	c := &Client{appID: appID, key: privateKey, baseURL: baseURL, onCall: onCall, insts: map[int64]*gh.Client{}}
	c.apps, err = c.newClient(atr)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) newClient(rt http.RoundTripper) (*gh.Client, error) {
	opts := []gh.ClientOptionsFunc{gh.WithHTTPClient(&http.Client{Transport: rt, Timeout: 30 * time.Second})}
	if c.baseURL != "" {
		u, err := url.Parse(strings.TrimRight(c.baseURL, "/") + "/")
		if err != nil {
			return nil, fmt.Errorf("github: base URL: %w", err)
		}
		s := u.String()
		opts = append(opts, gh.WithURLs(&s, &s))
	}
	cl, err := gh.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("github: client: %w", err)
	}
	return cl, nil
}

// inst returns a client authenticated as the given installation. Tokens are
// minted and refreshed by ghinstallation.
func (c *Client) inst(id int64) (*gh.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl, ok := c.insts[id]; ok {
		return cl, nil
	}
	tr, err := ghinstallation.New(http.DefaultTransport, c.appID, id, c.key)
	if err != nil {
		return nil, fmt.Errorf("github: installation transport: %w", err)
	}
	if c.baseURL != "" {
		tr.BaseURL = strings.TrimRight(c.baseURL, "/")
	}
	cl, err := c.newClient(tr)
	if err != nil {
		return nil, err
	}
	c.insts[id] = cl
	return cl, nil
}

func (c *Client) record(endpoint string, err error) {
	if err != nil {
		c.onCall(endpoint, "error")
	} else {
		c.onCall(endpoint, "ok")
	}
}

func (c *Client) Jobs(ctx context.Context, installationID int64, repo string, runID int64, attempt int) ([]tree.Job, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	cl, err := c.inst(installationID)
	if err != nil {
		return nil, err
	}
	var out []tree.Job
	opts := &gh.ListOptions{PerPage: 100}
	for {
		var jobs *gh.Jobs
		var resp *gh.Response
		err = withRateLimitRetry(ctx, func() error {
			var e error
			jobs, resp, e = cl.Actions.ListWorkflowJobsAttempt(ctx, owner, name, runID, int64(attempt), opts)
			return e
		})
		c.record("list_jobs", err)
		if err != nil {
			return nil, fmt.Errorf("github: list jobs %s#%d#%d: %w", repo, runID, attempt, err)
		}
		for _, j := range jobs.Jobs {
			out = append(out, JobFromAPI(j))
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// Run fetches one specific attempt of a run.
func (c *Client) Run(ctx context.Context, installationID int64, repo string, runID int64, attempt int) (*gh.WorkflowRun, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	cl, err := c.inst(installationID)
	if err != nil {
		return nil, err
	}
	var run *gh.WorkflowRun
	err = withRateLimitRetry(ctx, func() error {
		var e error
		excl := true
		run, _, e = cl.Actions.GetWorkflowRunAttempt(ctx, owner, name, runID, attempt, &gh.WorkflowRunAttemptOptions{ExcludePullRequests: &excl})
		return e
	})
	c.record("get_run", err)
	if err != nil {
		return nil, fmt.Errorf("github: get run %s#%d#%d: %w", repo, runID, attempt, err)
	}
	return run, nil
}

// Deliveries lists App webhook deliveries newer than since (newest first).
func (c *Client) Deliveries(ctx context.Context, since time.Time) ([]Delivery, error) {
	var out []Delivery
	opts := &gh.ListCursorOptions{PerPage: 100}
	for {
		ds, resp, err := c.apps.Apps.ListHookDeliveries(ctx, opts)
		c.record("list_deliveries", err)
		if err != nil {
			return nil, fmt.Errorf("github: list deliveries: %w", err)
		}
		for _, d := range ds {
			at := d.GetDeliveredAt().Time
			if at.Before(since) {
				return out, nil
			}
			out = append(out, Delivery{
				ID: d.GetID(), GUID: d.GetGUID(), Event: d.GetEvent(), Action: d.GetAction(),
				StatusCode: d.GetStatusCode(), DeliveredAt: at, Redelivery: d.GetRedelivery(),
			})
		}
		if resp.Cursor == "" || len(ds) == 0 {
			return out, nil
		}
		opts.Cursor = resp.Cursor
	}
}

func (c *Client) Redeliver(ctx context.Context, deliveryID int64) error {
	_, _, err := c.apps.Apps.RedeliverHookDelivery(ctx, deliveryID)
	// go-github reports 202 Accepted as an AcceptedError; that's success here.
	var acc *gh.AcceptedError
	if errors.As(err, &acc) {
		err = nil
	}
	c.record("redeliver", err)
	return err
}

// withRateLimitRetry retries once when GitHub says we're rate limited and
// the reset is close enough to wait for.
func withRateLimitRetry(ctx context.Context, fn func() error) error {
	err := fn()
	var wait time.Duration
	var rl *gh.RateLimitError
	var ab *gh.AbuseRateLimitError
	switch {
	case errors.As(err, &rl):
		wait = time.Until(rl.Rate.Reset.Time)
	case errors.As(err, &ab):
		wait = ab.GetRetryAfter()
		if wait == 0 {
			wait = time.Minute
		}
	default:
		return err
	}
	if wait <= 0 {
		wait = time.Second
	}
	if wait > 2*time.Minute { // let the caller's retry/backoff handle long waits
		return err
	}
	select {
	case <-time.After(wait):
	case <-ctx.Done():
		return ctx.Err()
	}
	return fn()
}

func splitRepo(full string) (string, string, error) {
	o, r, ok := strings.Cut(full, "/")
	if !ok || o == "" || r == "" {
		return "", "", fmt.Errorf("github: bad repo %q", full)
	}
	return o, r, nil
}

// RunFromAPI adapts a workflow run (webhook or REST) into a tree.Run.
func RunFromAPI(r *gh.WorkflowRun) tree.Run {
	var refs []string
	for _, w := range r.ReferencedWorkflows {
		p := w.GetPath()
		if ref := w.GetRef(); ref != "" {
			p += "@" + ref
		} else if sha := w.GetSHA(); sha != "" {
			p += "@" + sha
		}
		refs = append(refs, p)
	}
	actor := r.GetTriggeringActor().GetLogin()
	if actor == "" {
		actor = r.GetActor().GetLogin()
	}
	return tree.Run{
		Repo:                r.GetRepository().GetFullName(),
		ID:                  r.GetID(),
		Attempt:             max(r.GetRunAttempt(), 1),
		Workflow:            r.GetName(),
		HeadBranch:          r.GetHeadBranch(),
		HeadSHA:             r.GetHeadSHA(),
		Event:               r.GetEvent(),
		Conclusion:          r.GetConclusion(),
		Actor:               actor,
		HTMLURL:             r.GetHTMLURL(),
		StartedAt:           firstTime(r.RunStartedAt, r.CreatedAt),
		UpdatedAt:           r.GetUpdatedAt().Time,
		ReferencedWorkflows: refs,
	}
}

func JobFromAPI(j *gh.WorkflowJob) tree.Job {
	out := tree.Job{
		ID:          j.GetID(),
		Name:        j.GetName(),
		Conclusion:  j.GetConclusion(),
		CreatedAt:   j.GetCreatedAt().Time,
		StartedAt:   j.GetStartedAt().Time,
		CompletedAt: j.GetCompletedAt().Time,
		Labels:      j.Labels,
		RunnerName:  j.GetRunnerName(),
		HTMLURL:     j.GetHTMLURL(),
	}
	for _, s := range j.Steps {
		out.Steps = append(out.Steps, tree.Step{
			Number:      int(s.GetNumber()),
			Name:        s.GetName(),
			Conclusion:  s.GetConclusion(),
			StartedAt:   s.GetStartedAt().Time,
			CompletedAt: s.GetCompletedAt().Time,
		})
	}
	return out
}

func firstTime(ts ...*gh.Timestamp) time.Time {
	for _, t := range ts {
		if t != nil && !t.IsZero() {
			return t.Time
		}
	}
	return time.Time{}
}

// InstallationForRepo looks up the App installation that covers a repo
// (used by the backfill tool; webhooks carry the installation ID).
func (c *Client) InstallationForRepo(ctx context.Context, repo string) (int64, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return 0, err
	}
	inst, _, err := c.apps.Apps.GetRepositoryInstallation(ctx, owner, name)
	c.record("get_installation", err)
	if err != nil {
		return 0, fmt.Errorf("github: installation for %s: %w", repo, err)
	}
	return inst.GetID(), nil
}

// CompletedRuns lists completed runs for a branch or tag name, newest first.
func (c *Client) CompletedRuns(ctx context.Context, installationID int64, repo, branch string, limit int) ([]*gh.WorkflowRun, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	cl, err := c.inst(installationID)
	if err != nil {
		return nil, err
	}
	var out []*gh.WorkflowRun
	opts := &gh.ListWorkflowRunsOptions{Branch: branch, Status: "completed", ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		runs, resp, err := cl.Actions.ListRepositoryWorkflowRuns(ctx, owner, name, opts)
		c.record("list_runs", err)
		if err != nil {
			return nil, fmt.Errorf("github: list runs %s@%s: %w", repo, branch, err)
		}
		for _, r := range runs.WorkflowRuns {
			out = append(out, r)
			if limit > 0 && len(out) >= limit {
				return out, nil
			}
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

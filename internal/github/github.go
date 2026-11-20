// Package github wraps the few GitHub App APIs the service needs: fetching a
// run (re-poll) and listing/redelivering failed webhook deliveries.
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
)

// API is what the rest of the service depends on (faked in tests).
type API interface {
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

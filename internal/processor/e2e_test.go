package processor_test

// End-to-end: real webhook handler, GitHub App client, processor and OTLP
// exporter, wired against a fake GitHub API and a fake OTLP receiver.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Himanshu-216/gha-tracer/internal/emit"
	"github.com/Himanshu-216/gha-tracer/internal/github"
	"github.com/Himanshu-216/gha-tracer/internal/live"
	"github.com/Himanshu-216/gha-tracer/internal/metrics"
	"github.com/Himanshu-216/gha-tracer/internal/processor"
	"github.com/Himanshu-216/gha-tracer/internal/release"
	"github.com/Himanshu-216/gha-tracer/internal/webhook"
)

const secret = "s3cret"

type fakeGitHub struct {
	srv        *httptest.Server
	tokenCalls atomic.Int32
	jobsCalls  atomic.Int32
	runStatus  atomic.Value // string, for the attempt endpoint
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	jobs := readJSON(t, "jobs_9001_1.json")
	all := jobs["jobs"].([]any)
	runPayload := readJSON(t, "workflow_run_completed.json")["workflow_run"].(map[string]any)

	f := &fakeGitHub{}
	f.runStatus.Store("completed")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "no jwt", 401)
			return
		}
		f.tokenCalls.Add(1)
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"token":"ghs_test","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	})
	// Two pages to exercise pagination.
	mux.HandleFunc("GET /repos/acme/payments/actions/runs/9001/attempts/1/jobs", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token ghs_test" {
			http.Error(w, "bad token "+r.Header.Get("Authorization"), 401)
			return
		}
		f.jobsCalls.Add(1)
		page := all[:2]
		if r.URL.Query().Get("page") == "2" {
			page = all[2:]
		} else {
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=2&per_page=100>; rel="next"`, f.srv.URL, r.URL.Path))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(all), "jobs": page})
	})
	mux.HandleFunc("GET /repos/acme/payments/actions/runs/9001/attempts/1", func(w http.ResponseWriter, r *http.Request) {
		run := map[string]any{}
		for k, v := range runPayload {
			run[k] = v
		}
		run["status"] = f.runStatus.Load().(string)
		_ = json.NewEncoder(w).Encode(run)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

type fakeOTLP struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []emit.Request
	fail atomic.Int32 // number of upcoming requests to fail with 503
}

func newFakeOTLP(t *testing.T) *fakeOTLP {
	f := &fakeOTLP{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.fail.Load() > 0 {
			f.fail.Add(-1)
			http.Error(w, "unavailable", 503)
			return
		}
		var req emit.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, req)
		f.mu.Unlock()
		w.WriteHeader(200)
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOTLP) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

type harness struct {
	gh    *fakeGitHub
	otlp  *fakeOTLP
	store *live.Store
	m     *metrics.Metrics
	proc  *processor.Processor
	hook  http.Handler
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	h := &harness{gh: newFakeGitHub(t), otlp: newFakeOTLP(t), store: live.NewStore(100)}
	h.m = metrics.New(h.store, time.Hour)
	api, err := github.New(1, pemKey, h.gh.srv.URL, func(ep, res string) { h.m.GitHubAPICalls.WithLabelValues(ep, res).Inc() })
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	exp := emit.NewExporter(h.otlp.srv.URL+"/v1/traces", nil)
	exp.SetBackoff(5 * time.Millisecond)
	h.proc = processor.New(processor.Options{TraceEvents: []string{"push"}, Workers: 2, QueueSize: 10, RetryBase: 10 * time.Millisecond},
		release.MustDefault(), api, exp, h.store, h.m, log)
	h.hook = webhook.NewHandler([]byte(secret), h.proc.Enqueue, func(typ, res string) {
		h.m.WebhookEvents.WithLabelValues(typ, res).Inc()
	}, log)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); h.proc.Wait() })
	h.proc.Start(ctx)
	return h
}

func (h *harness) post(t *testing.T, event, delivery string, body []byte, sign bool) int {
	t.Helper()
	req := httptest.NewRequest("POST", "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	if sign {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	rec := httptest.NewRecorder()
	h.hook.ServeHTTP(rec, req)
	return rec.Code
}

func TestEndToEndReleaseRun(t *testing.T) {
	h := newHarness(t)

	if code := h.post(t, "workflow_run", "d1", fixture(t, "workflow_run_requested.json"), false); code != 401 {
		t.Fatalf("unsigned request: got %d, want 401", code)
	}
	if code := h.post(t, "workflow_run", "d1", fixture(t, "workflow_run_requested.json"), true); code != 202 {
		t.Fatalf("requested: got %d", code)
	}
	eventually(t, func() bool { return len(h.store.Active()) == 1 })
	row := h.store.Active()[0]
	if row.ReleaseLine != "v1.20" || row.ChangeType != "release" || row.Version != "v1.20.0" {
		t.Fatalf("live row = %+v", row)
	}

	h.post(t, "workflow_job", "d2", fixture(t, "workflow_job_in_progress.json"), true)
	eventually(t, func() bool {
		a := h.store.Active()
		return len(a) == 1 && len(a[0].RunningJobs) == 1 && a[0].RunningJobs[0] == "deploy / apply"
	})

	// First export attempt hits a 503 twice; the exporter's own retry recovers.
	h.otlp.fail.Store(2)
	if code := h.post(t, "workflow_run", "d3", fixture(t, "workflow_run_completed.json"), true); code != 202 {
		t.Fatalf("completed: got %d", code)
	}
	eventually(t, func() bool { return h.otlp.count() == 1 })

	// Duplicate delivery is acknowledged but not reprocessed.
	if code := h.post(t, "workflow_run", "d3", fixture(t, "workflow_run_completed.json"), true); code != 200 {
		t.Fatalf("duplicate: got %d, want 200", code)
	}
	// A redelivery with a new GUID (e.g. after restart) is still skipped in-process.
	h.post(t, "workflow_run", "d4", fixture(t, "workflow_run_completed.json"), true)
	time.Sleep(100 * time.Millisecond)
	if n := h.otlp.count(); n != 1 {
		t.Fatalf("exports = %d, want 1", n)
	}
	if n := h.gh.jobsCalls.Load(); n != 2 { // 2 pages, one build
		t.Fatalf("jobs API calls = %d, want 2", n)
	}
	if h.gh.tokenCalls.Load() != 1 {
		t.Fatalf("token mints = %d, want 1 (cached)", h.gh.tokenCalls.Load())
	}

	if len(h.store.Active()) != 0 || len(h.store.Recent()) != 1 {
		t.Fatalf("active=%d recent=%d", len(h.store.Active()), len(h.store.Recent()))
	}
	if r := h.store.Recent()[0]; r.Conclusion != "failure" || r.JobsTotal != 4 || r.JobsFailed != 1 || r.DurationSeconds != 1800 {
		t.Fatalf("recent row = %+v", r)
	}

	// Trace content.
	req := h.otlp.reqs[0]
	rs := req.ResourceSpans[0]
	res := attrMap(rs.Resource.Attributes)
	if res["service.name"] != "acme/payments" || res["gha.release_line"] != "v1.20" || res["gha.change_type"] != "release" {
		t.Fatalf("resource attrs = %v", res)
	}
	spans := rs.ScopeSpans[0].Spans
	if len(spans) != 1+1+4+4 { // run + deploy call + 4 jobs + 4 steps
		t.Fatalf("spans = %d", len(spans))
	}
	byName := map[string]emit.Span{}
	for _, s := range spans {
		byName[s.Name] = s
	}
	root, call, apply := byName["Release"], byName["deploy"], byName["deploy / apply"]
	if root.ParentSpanID != "" || root.Status.Code != 2 {
		t.Fatalf("root = %+v", root)
	}
	if call.ParentSpanID != root.SpanID || apply.ParentSpanID != call.SpanID || call.Status.Code != 2 {
		t.Fatalf("hierarchy broken: call=%+v apply=%+v", call, apply)
	}
	if byName["terraform apply"].ParentSpanID != apply.SpanID {
		t.Fatal("step not under its job")
	}
	if attrMap(root.Attributes)["gha.referenced_workflows"] == "" {
		t.Fatal("missing referenced workflows")
	}

	// Metrics.
	if v := testutil.ToFloat64(h.m.RunFailures.WithLabelValues("acme/payments", "Release", "release")); v != 1 {
		t.Fatalf("gha_run_failures_total = %v", v)
	}
	if v := testutil.ToFloat64(h.m.WebhookEvents.WithLabelValues("workflow_run", "bad_signature")); v != 1 {
		t.Fatalf("bad_signature = %v", v)
	}
	if v := testutil.ToFloat64(h.m.WebhookEvents.WithLabelValues("workflow_run", "duplicate")); v != 1 {
		t.Fatalf("duplicate = %v", v)
	}
	if n := testutil.CollectAndCount(h.m.JobQueue); n == 0 {
		t.Fatal("no queue-time samples")
	}
}

func TestUntrackedRefIgnored(t *testing.T) {
	h := newHarness(t)
	body := bytes.Replace(fixture(t, "workflow_run_completed.json"), []byte(`"head_branch": "v1.20.0"`), []byte(`"head_branch": "main"`), 1)
	h.post(t, "workflow_run", "x1", body, true)
	time.Sleep(100 * time.Millisecond)
	if h.otlp.count() != 0 || h.gh.jobsCalls.Load() != 0 {
		t.Fatal("main branch run should not be traced")
	}
}

func TestHotfixMergeClassified(t *testing.T) {
	h := newHarness(t)
	body := bytes.Replace(fixture(t, "workflow_run_requested.json"), []byte(`"head_branch": "v1.20.0"`), []byte(`"head_branch": "release/v1.20.0"`), 1)
	h.post(t, "workflow_run", "h1", body, true)
	eventually(t, func() bool { return len(h.store.Active()) == 1 })
	if r := h.store.Active()[0]; r.ChangeType != "hotfix-merge" || r.ReleaseLine != "v1.20" || r.Version != "" {
		t.Fatalf("row = %+v", r)
	}
}

func TestExportFailureRetriedByProcessor(t *testing.T) {
	h := newHarness(t)
	h.otlp.fail.Store(5) // exporter gives up after 4 tries; processor retries the event
	h.post(t, "workflow_run", "r1", fixture(t, "workflow_run_completed.json"), true)
	eventually(t, func() bool { return h.otlp.count() == 1 })
	if v := testutil.ToFloat64(h.m.EmitErrors); v != 1 {
		t.Fatalf("emit errors = %v, want 1", v)
	}
}

func TestRepollRecoversMissedCompletion(t *testing.T) {
	h := newHarness(t)
	h.gh.runStatus.Store("in_progress")
	h.post(t, "workflow_run", "p1", fixture(t, "workflow_run_requested.json"), true)
	eventually(t, func() bool { return len(h.store.Active()) == 1 })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.proc.Repoll(ctx, 20*time.Millisecond, 0)

	// Still running: nothing exported, run stays active.
	time.Sleep(100 * time.Millisecond)
	if h.otlp.count() != 0 || len(h.store.Active()) != 1 {
		t.Fatal("in-progress run must not be exported")
	}
	// The completed webhook was lost; the API now says completed.
	h.gh.runStatus.Store("completed")
	eventually(t, func() bool { return h.otlp.count() == 1 && len(h.store.Active()) == 0 })
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readJSON(t *testing.T, name string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal(fixture(t, name), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func attrMap(kvs []emit.KeyValue) map[string]string {
	m := map[string]string{}
	for _, kv := range kvs {
		switch {
		case kv.Value.StringValue != nil:
			m[kv.Key] = *kv.Value.StringValue
		case kv.Value.IntValue != nil:
			m[kv.Key] = *kv.Value.IntValue
		}
	}
	return m
}

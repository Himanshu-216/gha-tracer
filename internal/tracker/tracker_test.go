package tracker_test

// End-to-end: real webhook handler, tracker, run store and GitHub App client,
// wired against a fake GitHub API; assertions are on the /metrics output.

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/Himanshu-216/gha-tracer/internal/github"
	"github.com/Himanshu-216/gha-tracer/internal/metrics"
	"github.com/Himanshu-216/gha-tracer/internal/runs"
	"github.com/Himanshu-216/gha-tracer/internal/tags"
	"github.com/Himanshu-216/gha-tracer/internal/tracker"
	"github.com/Himanshu-216/gha-tracer/internal/webhook"
)

const secret = "s3cret"

const series = `{repo="acme/payments",run_id="9001",tag="v1.20.0",workflow="Release"}`

type harness struct {
	tr        *tracker.Tracker
	hook      http.Handler
	metrics   http.Handler
	runStatus atomic.Value // what the fake API reports for the run
	runCalls  atomic.Int32
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{}
	h.runStatus.Store("in_progress")

	completed := readJSON(t, "workflow_run_completed.json")["workflow_run"].(map[string]any)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"token":"ghs_test","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	})
	mux.HandleFunc("GET /repos/acme/payments/actions/runs/9001/attempts/1", func(w http.ResponseWriter, r *http.Request) {
		h.runCalls.Add(1)
		run := map[string]any{}
		for k, v := range completed {
			run[k] = v
		}
		if st := h.runStatus.Load().(string); st != "completed" {
			run["status"], run["conclusion"] = st, nil
		}
		_ = json.NewEncoder(w).Encode(run)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	store := runs.NewStore(time.Hour)
	m := metrics.New(store)
	api, err := github.New(1, pemKey, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h.tr = tracker.New(tags.MustDefault(), []string{"push"}, store, api, log)
	h.hook = webhook.NewHandler([]byte(secret), h.tr.Handle, func(typ, res string) {
		m.WebhookEvents.WithLabelValues(typ, res).Inc()
	}, log)
	h.metrics = m.Handler()
	return h
}

func (h *harness) post(t *testing.T, delivery string, body []byte) int {
	t.Helper()
	req := httptest.NewRequest("POST", "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "workflow_run")
	req.Header.Set("X-GitHub-Delivery", delivery)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	h.hook.ServeHTTP(rec, req)
	return rec.Code
}

func (h *harness) scrape(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.metrics.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func TestTagRunLifecycle(t *testing.T) {
	h := newHarness(t)

	if code := h.post(t, "d1", fixture(t, "workflow_run_requested.json")); code != 202 {
		t.Fatalf("requested: got %d", code)
	}
	out := h.scrape(t)
	mustContain(t, out, "gha_tag_run_status"+series+" 0")
	mustContain(t, out, "gha_tag_run_started_seconds"+series+" 1.7935272e+09") // 2026-11-01T10:00:00Z

	if code := h.post(t, "d2", fixture(t, "workflow_run_completed.json")); code != 202 {
		t.Fatalf("completed: got %d", code)
	}
	if code := h.post(t, "d2", fixture(t, "workflow_run_completed.json")); code != 200 {
		t.Fatalf("duplicate delivery: got %d, want 200", code)
	}
	out = h.scrape(t)
	mustContain(t, out, "gha_tag_run_status"+series+" 2") // failure
	mustContain(t, out, "gha_tag_run_duration_seconds"+series+" 1800")
	mustContain(t, out, `gha_webhook_events_total{result="duplicate",type="workflow_run"} 1`)

	// A late "requested" redelivery must not flip it back to running.
	h.post(t, "d3", fixture(t, "workflow_run_requested.json"))
	mustContain(t, h.scrape(t), "gha_tag_run_status"+series+" 2")
}

func TestNonTagRunsIgnored(t *testing.T) {
	h := newHarness(t)
	for i, b := range [][]byte{
		replace(fixture(t, "workflow_run_completed.json"), `"head_branch": "v1.20.0"`, `"head_branch": "main"`),
		replace(fixture(t, "workflow_run_completed.json"), `"head_branch": "v1.20.0"`, `"head_branch": "release/v1.20.0"`),
		replace(fixture(t, "workflow_run_completed.json"), `"event": "push"`, `"event": "workflow_dispatch"`),
	} {
		h.post(t, fmt.Sprint("x", i), b)
	}
	if out := h.scrape(t); strings.Contains(out, "gha_tag_run_status{") {
		t.Fatalf("non-tag run exported:\n%s", out)
	}
}

func TestRepollRecoversMissedCompletion(t *testing.T) {
	h := newHarness(t)
	h.post(t, "p1", fixture(t, "workflow_run_requested.json"))

	h.tr.RepollOnce(context.Background(), 0) // API says still running
	mustContain(t, h.scrape(t), "gha_tag_run_status"+series+" 0")

	h.runStatus.Store("completed") // the completed webhook was lost
	h.tr.RepollOnce(context.Background(), 0)
	mustContain(t, h.scrape(t), "gha_tag_run_status"+series+" 2")

	h.tr.RepollOnce(context.Background(), 0) // finished runs are not polled again
	if n := h.runCalls.Load(); n != 2 {
		t.Fatalf("run API calls = %d, want 2", n)
	}
}

func mustContain(t *testing.T, out, want string) {
	t.Helper()
	if !strings.Contains(out, want) {
		t.Fatalf("metrics missing %q\n%s", want, out)
	}
}

func replace(b []byte, old, new string) []byte {
	out := bytes.Replace(b, []byte(old), []byte(new), 1)
	if bytes.Equal(out, b) {
		panic("fixture does not contain " + old)
	}
	return out
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

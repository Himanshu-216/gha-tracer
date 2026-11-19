// Package metrics defines the service's Prometheus metrics. Labels are kept
// low-cardinality: no run IDs, SHAs or versions.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Himanshu-216/gha-tracer/internal/live"
)

// Buckets from 30s to ~4h — CI runs, not HTTP requests.
var durationBuckets = []float64{30, 60, 120, 300, 600, 900, 1200, 1800, 2700, 3600, 5400, 7200, 10800, 14400}
var queueBuckets = []float64{1, 5, 10, 30, 60, 120, 300, 600, 1200, 1800, 3600}

type Metrics struct {
	Reg *prometheus.Registry

	RunDuration    *prometheus.HistogramVec
	JobDuration    *prometheus.HistogramVec
	JobQueue       *prometheus.HistogramVec
	RunFailures    *prometheus.CounterVec
	RunsCompleted  *prometheus.CounterVec
	WebhookEvents  *prometheus.CounterVec
	GitHubAPICalls *prometheus.CounterVec
	EmitErrors     prometheus.Counter
	ProcessErrors  *prometheus.CounterVec
	Redeliveries   *prometheus.CounterVec
	LastEvent      prometheus.Gauge
}

func New(store *live.Store, staleAfter time.Duration) *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	m := &Metrics{
		Reg: reg,
		RunDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gha_run_duration_seconds", Help: "Wall time of completed workflow runs.", Buckets: durationBuckets,
		}, []string{"repo", "workflow", "change_type", "conclusion"}),
		JobDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gha_job_duration_seconds", Help: "Wall time of completed jobs.", Buckets: durationBuckets,
		}, []string{"repo", "workflow", "job", "conclusion"}),
		JobQueue: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gha_job_queue_seconds", Help: "Time jobs waited for a runner.", Buckets: queueBuckets,
		}, []string{"repo", "runner_label"}),
		RunFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gha_run_failures_total", Help: "Runs that concluded failure, timed_out or startup_failure.",
		}, []string{"repo", "workflow", "change_type"}),
		RunsCompleted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gha_runs_completed_total", Help: "Completed runs by conclusion.",
		}, []string{"repo", "workflow", "change_type", "conclusion"}),
		WebhookEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gha_webhook_events_total", Help: "Webhook requests by event type and result.",
		}, []string{"type", "result"}),
		GitHubAPICalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gha_github_api_calls_total", Help: "GitHub API calls by endpoint and result.",
		}, []string{"endpoint", "result"}),
		EmitErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gha_emit_errors_total", Help: "Failed OTLP exports (after retries).",
		}),
		ProcessErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gha_process_errors_total", Help: "Event processing failures by stage.",
		}, []string{"stage"}),
		Redeliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gha_redeliveries_total", Help: "Failed webhook deliveries we asked GitHub to redeliver.",
		}, []string{"result"}),
		LastEvent: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gha_last_webhook_timestamp_seconds", Help: "Unix time of the last accepted webhook.",
		}),
	}
	reg.MustRegister(m.RunDuration, m.JobDuration, m.JobQueue, m.RunFailures, m.RunsCompleted,
		m.WebhookEvents, m.GitHubAPICalls, m.EmitErrors, m.ProcessErrors, m.Redeliveries, m.LastEvent,
		&liveCollector{store: store, staleAfter: staleAfter})
	return m
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Reg, promhttp.HandlerOpts{})
}

// liveCollector exposes gauges computed from the live store at scrape time,
// so there's no inc/dec bookkeeping to get wrong.
type liveCollector struct {
	store      *live.Store
	staleAfter time.Duration
}

var (
	activeDesc = prometheus.NewDesc("gha_runs_active", "Tracked runs currently in progress.", []string{"repo", "change_type"}, nil)
	staleDesc  = prometheus.NewDesc("gha_run_stale_active", "Tracked runs in progress longer than STALE_AFTER.", []string{"repo", "change_type"}, nil)
)

func (c *liveCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- activeDesc
	ch <- staleDesc
}

func (c *liveCollector) Collect(ch chan<- prometheus.Metric) {
	active, stale := c.store.Counts(c.staleAfter)
	for k, v := range active {
		ch <- prometheus.MustNewConstMetric(activeDesc, prometheus.GaugeValue, float64(v), k.Repo, k.ChangeType)
		ch <- prometheus.MustNewConstMetric(staleDesc, prometheus.GaugeValue, float64(stale[k]), k.Repo, k.ChangeType)
	}
}

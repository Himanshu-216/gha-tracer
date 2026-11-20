// Package metrics holds the service's Prometheus registry: the per-run tag
// gauges (from runs.Store) plus three small counters used only for alerting
// on gha-tracer itself.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	Reg *prometheus.Registry

	WebhookEvents  *prometheus.CounterVec
	GitHubAPICalls *prometheus.CounterVec
	Redeliveries   *prometheus.CounterVec
}

func New(runs prometheus.Collector) *Metrics {
	m := &Metrics{
		Reg: prometheus.NewRegistry(),
		WebhookEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gha_webhook_events_total", Help: "Webhook requests by event type and result.",
		}, []string{"type", "result"}),
		GitHubAPICalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gha_github_api_calls_total", Help: "GitHub API calls by endpoint and result.",
		}, []string{"endpoint", "result"}),
		Redeliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gha_redeliveries_total", Help: "Failed webhook deliveries we asked GitHub to redeliver.",
		}, []string{"result"}),
	}
	m.Reg.MustRegister(runs, m.WebhookEvents, m.GitHubAPICalls, m.Redeliveries)
	return m
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Reg, promhttp.HandlerOpts{})
}

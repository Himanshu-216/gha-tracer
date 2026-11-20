// Command gha-tracer receives GitHub Actions workflow_run webhooks and exposes
// the status of every tag-triggered run (v1.20.0, v1.20.1, ...) as Prometheus
// gauges for a single Grafana dashboard.
//
// Listeners:
//
//	LISTEN_ADDR   (public, behind the ALB)  POST /webhook, GET /healthz
//	INTERNAL_ADDR (VPC only)                GET /metrics, /healthz
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Himanshu-216/gha-tracer/internal/config"
	"github.com/Himanshu-216/gha-tracer/internal/github"
	"github.com/Himanshu-216/gha-tracer/internal/metrics"
	"github.com/Himanshu-216/gha-tracer/internal/redeliver"
	"github.com/Himanshu-216/gha-tracer/internal/runs"
	"github.com/Himanshu-216/gha-tracer/internal/tags"
	"github.com/Himanshu-216/gha-tracer/internal/tracker"
	"github.com/Himanshu-216/gha-tracer/internal/webhook"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	matcher, err := tags.New(cfg.TagPattern)
	if err != nil {
		return err
	}

	store := runs.NewStore(cfg.KeepCompleted)
	m := metrics.New(store)

	api, err := github.New(cfg.AppID, cfg.PrivateKey, cfg.GitHubAPIURL, func(ep, res string) {
		m.GitHubAPICalls.WithLabelValues(ep, res).Inc()
	})
	if err != nil {
		return err
	}

	tr := tracker.New(matcher, cfg.RunEvents, store, api, log)
	hook := webhook.NewHandler(cfg.WebhookSecret, tr.Handle, func(typ, res string) {
		m.WebhookEvents.WithLabelValues(typ, res).Inc()
	}, log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go tr.Repoll(ctx, cfg.RepollInterval, cfg.RepollAfter)
	go redeliver.New(api, cfg.RedeliverInterval, cfg.RedeliverLookback, func(res string) {
		m.Redeliveries.WithLabelValues(res).Inc()
	}, log).Run(ctx)

	healthz := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	pub := http.NewServeMux()
	pub.Handle("/webhook", hook)
	pub.Handle("GET /healthz", healthz)

	internal := http.NewServeMux()
	internal.Handle("GET /metrics", m.Handler())
	internal.Handle("GET /healthz", healthz)

	servers := []*http.Server{
		{Addr: cfg.ListenAddr, Handler: pub, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second},
		{Addr: cfg.InternalAddr, Handler: internal, ReadHeaderTimeout: 10 * time.Second},
	}
	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			log.Info("listening", "addr", s.Addr)
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errc:
		return err
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutCtx)
	}
	return nil
}

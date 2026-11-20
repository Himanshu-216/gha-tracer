// Package webhook receives GitHub App webhooks: verifies the HMAC signature,
// drops duplicate deliveries, decodes workflow_run events and hands them to a
// sink.
package webhook

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	gh "github.com/google/go-github/v88/github"
)

type Event struct {
	DeliveryID string
	Type       string // "workflow_run"
	Payload    any    // *gh.WorkflowRunEvent
}

// Sink accepts an event; false means the queue is full.
type Sink func(Event) bool

// OnEvent is called for every request with the event type and the outcome
// (accepted, duplicate, ignored, bad_signature, bad_payload, queue_full).
type OnEvent func(eventType, result string)

type Handler struct {
	secret  []byte
	sink    Sink
	onEvent OnEvent
	seen    *TTLSet
	log     *slog.Logger
}

func NewHandler(secret []byte, sink Sink, onEvent OnEvent, log *slog.Logger) *Handler {
	if onEvent == nil {
		onEvent = func(string, string) {}
	}
	return &Handler{secret: secret, sink: sink, onEvent: onEvent, seen: NewTTLSet(time.Hour), log: log}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 25<<20) // GitHub caps payloads at 25 MB
	typ := gh.WebHookType(r)
	id := gh.DeliveryID(r)

	payload, err := gh.ValidatePayload(r, h.secret)
	if err != nil {
		h.onEvent(typ, "bad_signature")
		h.log.Warn("webhook rejected", "delivery", id, "err", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	switch typ {
	case "ping":
		h.onEvent(typ, "ignored")
		w.WriteHeader(http.StatusOK)
		return
	case "workflow_run":
	default: // workflow_job and anything else the App may still be subscribed to
		h.onEvent(typ, "ignored")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if id != "" && h.seen.Has(id) {
		h.onEvent(typ, "duplicate")
		w.WriteHeader(http.StatusOK)
		return
	}

	ev, err := gh.ParseWebHook(typ, payload)
	if err != nil {
		h.onEvent(typ, "bad_payload")
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}

	if !h.sink(Event{DeliveryID: id, Type: typ, Payload: ev}) {
		// Non-2xx marks the delivery failed at GitHub; the redelivery sweeper
		// will replay it later.
		h.onEvent(typ, "queue_full")
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	if id != "" {
		h.seen.Add(id)
	}
	h.onEvent(typ, "accepted")
	w.WriteHeader(http.StatusAccepted)
}

// TTLSet is a small concurrency-safe set whose entries expire.
type TTLSet struct {
	ttl  time.Duration
	mu   sync.Mutex
	m    map[string]time.Time
	last time.Time
	now  func() time.Time
}

func NewTTLSet(ttl time.Duration) *TTLSet {
	return &TTLSet{ttl: ttl, m: map[string]time.Time{}, now: time.Now}
}

func (s *TTLSet) Has(k string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.m[k]
	return ok && s.now().Sub(t) < s.ttl
}

func (s *TTLSet) Add(k string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.m[k] = now
	if now.Sub(s.last) > s.ttl/4 { // amortised pruning
		for k, t := range s.m {
			if now.Sub(t) >= s.ttl {
				delete(s.m, k)
			}
		}
		s.last = now
	}
}

func (s *TTLSet) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// Package emit converts a tree.Tree into OTLP spans and sends them over
// OTLP/HTTP (JSON encoding), which Tempo, the OTel Collector and hosted
// backends all accept. Hand-rolled JSON keeps the binary small and lets us set
// our own trace/span IDs and historical timestamps.
package emit

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/Himanshu-216/gha-tracer/internal/tree"
)

type Exporter struct {
	endpoint string
	headers  map[string]string
	client   *http.Client
	retries  int
	backoff  time.Duration
}

func NewExporter(endpoint string, headers map[string]string) *Exporter {
	return &Exporter{
		endpoint: endpoint,
		headers:  headers,
		client:   &http.Client{Timeout: 15 * time.Second},
		retries:  3,
		backoff:  time.Second,
	}
}

// SetBackoff changes the first retry delay (doubles per retry). For tests.
func (e *Exporter) SetBackoff(d time.Duration) { e.backoff = d }

// Export sends one tree as a single OTLP request, retrying transient failures.
func (e *Exporter) Export(ctx context.Context, t tree.Tree) error {
	body, err := json.Marshal(Encode(t))
	if err != nil {
		return fmt.Errorf("emit: encode: %w", err)
	}
	var lastErr error
	for i := 0; i <= e.retries; i++ {
		if i > 0 {
			select {
			case <-time.After(e.backoff << (i - 1)):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		retry, err := e.post(ctx, body)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retry {
			break
		}
	}
	return lastErr
}

func (e *Exporter) post(ctx context.Context, body []byte) (retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return true, fmt.Errorf("emit: post: %w", err)
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode/100 == 2 {
		return false, nil
	}
	retry = resp.StatusCode == 429 || resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504
	return retry, fmt.Errorf("emit: %s: %s", resp.Status, bytes.TrimSpace(msg))
}

// ---- OTLP JSON model (subset) ----

type Request struct {
	ResourceSpans []ResourceSpans `json:"resourceSpans"`
}

type ResourceSpans struct {
	Resource   Resource     `json:"resource"`
	ScopeSpans []ScopeSpans `json:"scopeSpans"`
}

type Resource struct {
	Attributes []KeyValue `json:"attributes"`
}

type ScopeSpans struct {
	Scope Scope  `json:"scope"`
	Spans []Span `json:"spans"`
}

type Scope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type Span struct {
	TraceID           string     `json:"traceId"`
	SpanID            string     `json:"spanId"`
	ParentSpanID      string     `json:"parentSpanId,omitempty"`
	Name              string     `json:"name"`
	Kind              int        `json:"kind"`
	StartTimeUnixNano string     `json:"startTimeUnixNano"`
	EndTimeUnixNano   string     `json:"endTimeUnixNano"`
	Attributes        []KeyValue `json:"attributes,omitempty"`
	Links             []Link     `json:"links,omitempty"`
	Status            Status     `json:"status"`
}

type Link struct {
	TraceID    string     `json:"traceId"`
	SpanID     string     `json:"spanId"`
	Attributes []KeyValue `json:"attributes,omitempty"`
}

type Status struct {
	Code    int    `json:"code,omitempty"` // 0 unset, 1 ok, 2 error
	Message string `json:"message,omitempty"`
}

type KeyValue struct {
	Key   string   `json:"key"`
	Value AnyValue `json:"value"`
}

type AnyValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"` // int64 as string per OTLP JSON
	DoubleValue *float64 `json:"doubleValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
}

const (
	spanKindInternal = 1
	spanKindServer   = 2
	statusError      = 2
)

// resourceKeys are copied from the run node onto the resource so every span
// (jobs, steps) can be filtered by release in TraceQL via resource.<key>.
var resourceKeys = []string{
	"gha.release_line", "gha.version", "gha.change_type", "gha.head_sha",
	"vcs.repository.name", "gha.workflow", "gha.run_id", "gha.run_attempt",
}

// Encode builds the OTLP request for a tree. Exported for tests and backfill.
func Encode(t tree.Tree) Request {
	traceID := hex.EncodeToString(t.TraceID[:])
	root := t.Root

	res := []KeyValue{kv("service.name", root.Attrs["vcs.repository.name"])}
	for _, k := range resourceKeys {
		if v, ok := root.Attrs[k]; ok && !empty(v) {
			res = append(res, kv(k, v))
		}
	}

	var spans []Span
	t.Walk(func(n *tree.Node) {
		s := Span{
			TraceID:           traceID,
			SpanID:            hex.EncodeToString(n.SpanID[:]),
			Name:              n.Name,
			Kind:              spanKindInternal,
			StartTimeUnixNano: nanos(n.Start),
			EndTimeUnixNano:   nanos(n.End),
			Attributes:        attrs(n),
		}
		if n == root {
			s.Kind = spanKindServer
			for _, l := range t.Links {
				s.Links = append(s.Links, Link{
					TraceID:    hex.EncodeToString(l.TraceID[:]),
					SpanID:     hex.EncodeToString(l.SpanID[:]),
					Attributes: []KeyValue{kv("gha.link", "previous_attempt")},
				})
			}
		} else {
			s.ParentSpanID = hex.EncodeToString(n.ParentID[:])
		}
		if tree.IsError(n.Conclusion) {
			s.Status = Status{Code: statusError, Message: n.Conclusion}
		}
		spans = append(spans, s)
	})

	return Request{ResourceSpans: []ResourceSpans{{
		Resource:   Resource{Attributes: res},
		ScopeSpans: []ScopeSpans{{Scope: Scope{Name: "gha-tracer"}, Spans: spans}},
	}}}
}

func attrs(n *tree.Node) []KeyValue {
	keys := make([]string, 0, len(n.Attrs))
	for k, v := range n.Attrs {
		if !empty(v) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := []KeyValue{kv("gha.node_kind", string(n.Kind))}
	for _, k := range keys {
		out = append(out, kv(k, n.Attrs[k]))
	}
	return out
}

func kv(k string, v any) KeyValue {
	var av AnyValue
	switch x := v.(type) {
	case string:
		av.StringValue = &x
	case int:
		s := strconv.Itoa(x)
		av.IntValue = &s
	case int64:
		s := strconv.FormatInt(x, 10)
		av.IntValue = &s
	case float64:
		av.DoubleValue = &x
	case bool:
		av.BoolValue = &x
	default:
		s := fmt.Sprint(x)
		av.StringValue = &s
	}
	return KeyValue{Key: k, Value: av}
}

func empty(v any) bool {
	s, ok := v.(string)
	return v == nil || (ok && s == "")
}

func nanos(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return strconv.FormatInt(t.UnixNano(), 10)
}

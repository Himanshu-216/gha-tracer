package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"
)

func do(h *Handler, event, delivery string, body []byte, secret string) int {
	req := httptest.NewRequest("POST", "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestHandler(t *testing.T) {
	var got []Event
	full := false
	sink := func(e Event) bool {
		if full {
			return false
		}
		got = append(got, e)
		return true
	}
	h := NewHandler([]byte("k"), sink, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	run := []byte(`{"action":"requested","workflow_run":{"id":1}}`)

	cases := []struct {
		name, event, id, secret string
		body                    []byte
		full                    bool
		want                    int
	}{
		{"bad signature", "workflow_run", "1", "wrong", run, false, 401},
		{"ping", "ping", "2", "k", []byte(`{}`), false, 200},
		{"other event", "push", "3", "k", []byte(`{}`), false, 204},
		{"workflow_job ignored", "workflow_job", "3b", "k", []byte(`{}`), false, 204},
		{"queue full", "workflow_run", "4", "k", run, true, 503},
		{"retry after full is accepted", "workflow_run", "4", "k", run, false, 202},
		{"duplicate", "workflow_run", "4", "k", run, false, 200},
		{"bad json", "workflow_run", "5", "k", []byte(`{`), false, 400},
	}
	for _, c := range cases {
		full = c.full
		if code := do(h, c.event, c.id, c.body, c.secret); code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, code, c.want)
		}
	}
	if len(got) != 1 || got[0].DeliveryID != "4" || got[0].Type != "workflow_run" {
		t.Fatalf("sink got %+v", got)
	}
}

func TestTTLSet(t *testing.T) {
	now := time.Unix(0, 0)
	s := NewTTLSet(time.Hour)
	s.now = func() time.Time { return now }
	s.Add("a")
	if !s.Has("a") || s.Has("b") {
		t.Fatal("membership")
	}
	now = now.Add(2 * time.Hour)
	if s.Has("a") {
		t.Fatal("entry should expire")
	}
	s.Add("b") // triggers prune
	if s.Len() != 1 {
		t.Fatalf("len = %d after prune", s.Len())
	}
}

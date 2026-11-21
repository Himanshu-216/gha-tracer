package config

import (
	"strings"
	"testing"
)

func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("GITHUB_APP_ID", "123")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", "-----BEGIN RSA PRIVATE KEY-----\nx\n-----END RSA PRIVATE KEY-----")
	t.Setenv("GITHUB_WEBHOOK_SECRET", strings.Repeat("a", MinWebhookSecretLen))
	t.Setenv("GITHUB_API_URL", "")
}

func TestLoadDefaults(t *testing.T) {
	setRequired(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":8080" || c.InternalAddr != ":9090" || c.AppID != 123 {
		t.Fatalf("unexpected config: %+v", c)
	}
}

func TestWebhookSecret(t *testing.T) {
	for _, tc := range []struct {
		secret string
		ok     bool
	}{
		{"", false},
		{"short", false},
		{strings.Repeat("x", MinWebhookSecretLen-1), false},
		{strings.Repeat("x", MinWebhookSecretLen), true},
	} {
		setRequired(t)
		t.Setenv("GITHUB_WEBHOOK_SECRET", tc.secret)
		_, err := Load()
		if (err == nil) != tc.ok {
			t.Errorf("secret len %d: err=%v, want ok=%v", len(tc.secret), err, tc.ok)
		}
	}
}

func TestAPIURL(t *testing.T) {
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"", true},
		{"https://api.github.com", true},
		{"https://ghes.example.com/api/v3", true},
		{"http://127.0.0.1:8081", true},
		{"http://localhost:8081", true},
		{"http://[::1]:8081", true},
		{"http://ghes.example.com/api/v3", false},
		{"ftp://ghes.example.com", false},
		{"https://", false},
		{"://bad", false},
	} {
		if err := checkAPIURL(tc.url); (err == nil) != tc.ok {
			t.Errorf("%q: err=%v, want ok=%v", tc.url, err, tc.ok)
		}
	}
}

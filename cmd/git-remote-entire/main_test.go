package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseProtocolVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		env      string
		want     int
		wantWarn string
	}{
		{"unset", "", 2, ""},
		{"version_0", "version=0", 0, ""},
		{"version_1", "version=1", 1, ""},
		{"version_2", "version=2", 2, ""},
		{"unknown_version_warns", "version=3", 2, "ignoring unrecognised protocol.version"},
		{"malformed_value_warns", "version=abc", 2, "ignoring unrecognised protocol.version"},
		{"empty_value_warns", "version=", 2, "ignoring unrecognised protocol.version"},
		{"no_version_key", "foo=bar", 2, ""},
		{"version_after_other_key", "foo=bar:version=1", 1, ""},
		{"version_before_other_key", "version=2:foo=bar", 2, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			got := parseProtocolVersion(tc.env, &buf)
			if got != tc.want {
				t.Errorf("parseProtocolVersion(%q) = %d, want %d", tc.env, got, tc.want)
			}
			switch {
			case tc.wantWarn == "" && buf.Len() != 0:
				t.Errorf("expected no warning, got %q", buf.String())
			case tc.wantWarn != "" && !strings.Contains(buf.String(), tc.wantWarn):
				t.Errorf("expected warning containing %q, got %q", tc.wantWarn, buf.String())
			}
		})
	}
}

func TestGitActionFromRequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		method string
		path   string
		query  string
		want   string
	}{
		{"upload-pack RPC", http.MethodPost, "/et/p/r/git-upload-pack", "", "pull"},
		{"receive-pack RPC", http.MethodPost, "/et/p/r/git-receive-pack", "", "push"},
		{"info/refs pull", http.MethodGet, "/et/p/r/info/refs", "service=git-upload-pack", "pull"},
		{"info/refs push", http.MethodGet, "/et/p/r/info/refs", "service=git-receive-pack", "push"},
		{"info/refs no service", http.MethodGet, "/et/p/r/info/refs", "", ""},
		{"unrelated GET", http.MethodGet, "/et/p/r/objects/info/packs", "", ""},
		{"unrelated POST", http.MethodPost, "/et/p/r/whatever", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(context.Background(), tc.method, "https://host"+tc.path+"?"+tc.query, nil)
			if got := gitActionFromRequest(req); got != tc.want {
				t.Fatalf("gitActionFromRequest(%s %s?%s) = %q, want %q", tc.method, tc.path, tc.query, got, tc.want)
			}
		})
	}
}

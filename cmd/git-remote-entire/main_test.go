package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/entireio/cli/internal/entireclient/contexts"
)

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

func TestMakeBindHook(t *testing.T) {
	t.Parallel()

	t.Run("nil when no context name", func(t *testing.T) {
		t.Parallel()
		if makeBindHook(t.TempDir(), "host.example", "") != nil {
			t.Fatal("expected nil hook for empty context name")
		}
	})

	t.Run("binds cluster after first call", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// BindCluster only writes a binding for a context that exists.
		if err := contexts.Modify(dir, func(f *contexts.File) (bool, error) {
			f.Upsert(&contexts.Context{
				Name:            "ctx",
				CoreURL:         "https://core.example",
				Handle:          "h",
				KeychainService: "svc",
			})
			return true, nil
		}); err != nil {
			t.Fatalf("seed context: %v", err)
		}

		hook := makeBindHook(dir, "cluster.example", "ctx")
		if hook == nil {
			t.Fatal("expected non-nil hook")
		}
		hook()
		hook() // second call is a no-op (sync.Once); must not error or change state

		f, err := contexts.Load(dir)
		if err != nil {
			t.Fatalf("load contexts: %v", err)
		}
		if got := f.ClusterContexts["cluster.example"]; got != "ctx" {
			t.Fatalf("binding for cluster.example = %q, want %q", got, "ctx")
		}
	})

	t.Run("no binding when context is missing", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// No context named "ghost" exists, so BindCluster fails and the hook
		// logs without persisting anything.
		makeBindHook(dir, "cluster.example", "ghost")()

		f, err := contexts.Load(dir)
		if err != nil {
			t.Fatalf("load contexts: %v", err)
		}
		if _, ok := f.ClusterContexts["cluster.example"]; ok {
			t.Fatal("expected no binding to be written for a missing context")
		}
	})
}

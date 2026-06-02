package cli

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

// TestParseGitHubURL is ported from entiredb's cmd/entire-repo/cli
// mirror_test.go, since parseGitHubURL was carried over verbatim.
func TestParseGitHubURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		url       string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{name: "HTTPS", url: "https://github.com/entirehq/entiredb", wantOwner: "entirehq", wantRepo: "entiredb"},
		{name: "HTTPS with .git", url: "https://github.com/entirehq/entiredb.git", wantOwner: "entirehq", wantRepo: "entiredb"},
		{name: "SSH", url: "git@github.com:entirehq/entiredb", wantOwner: "entirehq", wantRepo: "entiredb"},
		{name: "SSH with .git", url: "git@github.com:entirehq/entiredb.git", wantOwner: "entirehq", wantRepo: "entiredb"},
		{name: "HTTP", url: "http://github.com/owner/repo", wantOwner: "owner", wantRepo: "repo"},
		{name: "bare with github.com prefix", url: "github.com/octocat/hello-world", wantOwner: "octocat", wantRepo: "hello-world"},
		{name: "bare github.com prefix with .git", url: "github.com/octocat/hello-world.git", wantOwner: "octocat", wantRepo: "hello-world"},
		{name: "bare owner/repo", url: "octocat/hello-world", wantOwner: "octocat", wantRepo: "hello-world"},
		{name: "bare lowercased", url: "OctoCat/Hello-World", wantOwner: "octocat", wantRepo: "hello-world"},
		{name: "repo with dot", url: "github.com/octocat/hello.world", wantOwner: "octocat", wantRepo: "hello.world"},
		{name: "repo with underscore", url: "octocat/hello_world", wantOwner: "octocat", wantRepo: "hello_world"},
		{name: "GitLab", url: "https://gitlab.com/owner/repo", wantErr: true},
		{name: "missing repo", url: "https://github.com/owner", wantErr: true},
		{name: "not a URL", url: "not-a-url", wantErr: true},
		{name: "entire URL", url: "entire://host/git/owner/repo", wantErr: true},
		// Parameter-smuggling shapes the tightened owner/repo charset rejects:
		// these would otherwise mutate the audience / probe URL built from
		// owner/repo.
		{name: "repo with query smuggle", url: "octocat/repo?bypass=1", wantErr: true},
		{name: "repo with fragment", url: "octocat/repo#anchor", wantErr: true},
		{name: "owner with at-sign", url: "a@b/repo", wantErr: true},
		{name: "repo with encoded slash", url: "octocat/repo%2fevil", wantErr: true},
		{name: "owner with dot-dot", url: "../repo", wantErr: true},
		{name: "owner with underscore (not a GitHub login)", url: "oct_cat/repo", wantErr: true},
		// Dot-only repo names pass the gitHubRepoPat charset (which allows
		// dots) but would embed a literal "." or ".." in the audience and
		// probe URL — reject at the boundary.
		{name: "dot-only repo", url: "github.com/owner/..", wantErr: true},
		{name: "single-dot repo", url: "github.com/owner/.", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			owner, repo, err := parseGitHubURL(tt.url)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseGitHubURL(%q) expected error, got %q/%q", tt.url, owner, repo)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGitHubURL(%q) unexpected error: %v", tt.url, err)
			}
			if owner != tt.wantOwner || repo != tt.wantRepo {
				t.Errorf("parseGitHubURL(%q) = %q/%q, want %q/%q", tt.url, owner, repo, tt.wantOwner, tt.wantRepo)
			}
		})
	}
}

func TestMirrorRow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mirror coreapi.Mirror
		want   []string
	}{
		{
			name:   "private mirror synthesises clone URL",
			mirror: coreapi.Mirror{Owner: "entirehq", Repo: "entire.io", ClusterHost: "aws-us-east-2.entire.io", IsPrivate: coreapi.NewOptBool(true)},
			want:   []string{"entirehq/entire.io", "entire://aws-us-east-2.entire.io/gh/entirehq/entire.io", "yes"},
		},
		{
			name:   "public mirror, unset IsPrivate defaults to no",
			mirror: coreapi.Mirror{Owner: "octocat", Repo: "hello", ClusterHost: "eu-west-1.entire.io"},
			want:   []string{"octocat/hello", "entire://eu-west-1.entire.io/gh/octocat/hello", "no"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := mirrorRow(tt.mirror)
			if len(got) != len(tt.want) {
				t.Fatalf("mirrorRow len = %d, want %d (%v)", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("mirrorRow[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestClusterArg(t *testing.T) {
	t.Parallel()
	if got := clusterArg([]string{"github.com/o/r", "eu-west-1.entire.io"}); got != "eu-west-1.entire.io" {
		t.Errorf("explicit cluster = %q, want eu-west-1.entire.io", got)
	}
	if got := clusterArg([]string{"github.com/o/r"}); got != defaultClusterHost {
		t.Errorf("omitted cluster = %q, want default %q", got, defaultClusterHost)
	}
}

func TestValidateClusterHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		host    string
		wantErr bool
	}{
		{name: "default cluster", host: defaultClusterHost},
		{name: "other region", host: "eu-west-1.entire.io"},
		{name: "single label", host: "localhost"},
		{name: "host with port", host: "localhost:8080"},
		{name: "ipv4", host: "10.0.0.1"},
		{name: "ipv4 with port", host: "10.0.0.1:8080"},
		// IPv6 takes a different path through validateClusterHost: the
		// host must be bracketed for url.Parse to round-trip, and
		// u.Hostname() strips the brackets before net.ParseIP sees it.
		{name: "ipv6 with port", host: "[::1]:8080"},
		// The token-leak primitive: userinfo demotes the real cluster so the
		// request (and basic-auth token) targets evil.com.
		{name: "userinfo smuggle", host: "aws-us-east-2.entire.io@evil.com", wantErr: true},
		{name: "path smuggle", host: "aws-us-east-2.entire.io/../evil", wantErr: true},
		{name: "query smuggle", host: "aws-us-east-2.entire.io?x=1", wantErr: true},
		{name: "fragment smuggle", host: "aws-us-east-2.entire.io#x", wantErr: true},
		{name: "scheme prefix", host: "https://evil.com", wantErr: true},
		{name: "empty", host: "", wantErr: true},
		{name: "whitespace", host: "   ", wantErr: true},
		{name: "leading hyphen label", host: "-bad.entire.io", wantErr: true},
		{name: "space in host", host: "evil .com", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateClusterHost(tt.host)
			if tt.wantErr && err == nil {
				t.Errorf("validateClusterHost(%q) = nil, want error", tt.host)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("validateClusterHost(%q) = %v, want nil", tt.host, err)
			}
		})
	}
}

// TestMirrorAdvertisesHead_ReusesConnection is the regression test for the
// body drain in mirrorAdvertisesHead. The probe runs every 2s for up to 30m,
// and probeClient keeps a small idle pool specifically to reuse the TLS
// session across ticks — but Go only returns a connection to that pool when
// the response body is read to EOF before Close. If the drain is removed (or
// re-capped shorter than the body), the body is left partially read and the
// transport closes the connection instead, so every probe pays a fresh
// handshake.
//
// We serve a non-200 with a non-empty body: mirrorAdvertisesHead returns at
// the status check without reading the body itself, so the deferred drain is
// the *only* thing that consumes it. Counting StateNew transitions then
// distinguishes "drained → one reused connection" from "not drained → a new
// connection per call".
func TestMirrorAdvertisesHead_ReusesConnection(t *testing.T) {
	t.Parallel()

	// Larger than any plausible "small cap" someone might reintroduce, so a
	// capped drain would stop before EOF and fail this test too.
	body := strings.Repeat("x", 64<<10)

	var newConns atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 503 = mirror reachable but not ready; the function returns at the
		// status check below http.StatusOK without touching the body.
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(body)) //nolint:errcheck // test server write; failure surfaces as a client error
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			newConns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	// Isolated client (not the package-global probeClient) so the idle pool
	// is private to this test and the assertion stays deterministic under
	// t.Parallel(). Keep-alives and idle pooling are on by default.
	client := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{MaxIdleConns: 2, MaxIdleConnsPerHost: 2, IdleConnTimeout: 90 * time.Second},
	}

	const probes = 5
	for range probes {
		ready, status := mirrorAdvertisesHead(context.Background(), client, srv.URL, "tok")
		require.False(t, ready)
		require.Equal(t, http.StatusServiceUnavailable, status)
	}

	require.Equal(t, int64(1), newConns.Load(),
		"expected the drained body to let all %d probes share one connection; got %d new connections (body not drained to EOF?)",
		probes, newConns.Load())
}

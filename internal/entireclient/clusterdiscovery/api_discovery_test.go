package clusterdiscovery

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/discovery"
)

// schemeRewriteTransport rewrites the scheme to http (DiscoverAPI hard-codes
// https://) while leaving the host untouched, so a cross-origin redirect
// reaches its real target rather than being pinned back to the first server.
type schemeRewriteTransport struct{ base http.RoundTripper }

func (s schemeRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	return s.base.RoundTrip(req)
}

// apiDiscoveryBody includes jwks_uris (the server's real field, plural per
// entire.io#2281) to prove the CLI ignores fields it doesn't model.
const apiDiscoveryBody = `{
  "issuer": "https://us.auth.partial.to",
  "trusted_issuers": ["https://us.auth.partial.to", "https://eu.auth.partial.to"],
  "audience": "https://partial.to",
  "jwks_uris": {"https://us.auth.partial.to": "https://us.auth.partial.to/.well-known/jwks.json"}
}`

func TestDiscoverAPI(t *testing.T) {
	t.Parallel()

	t.Run("parses the document on 200", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, APIPath, r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(apiDiscoveryBody)) //nolint:errcheck // test handler
		}))
		defer srv.Close()

		doc, err := DiscoverAPI(t.Context(), "partial.to", hostPinningClient(t, srv), t.Logf)
		require.NoError(t, err)
		assert.Equal(t, "https://us.auth.partial.to", doc.Issuer)
		assert.Equal(t, []string{"https://us.auth.partial.to", "https://eu.auth.partial.to"}, doc.TrustedIssuers)
		assert.Equal(t, "https://partial.to", doc.Audience)
	})

	// 404 (deployment predating the well-known), 503 (unconfigured), transport
	// failure, malformed body, and an incomplete document all fold into
	// ErrDiscoveryUnavailable so the caller has a single fallback signal.
	t.Run("404 → ErrDiscoveryUnavailable", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "not found", http.StatusNotFound)
		}))
		defer srv.Close()

		_, err := DiscoverAPI(t.Context(), "partial.to", hostPinningClient(t, srv), t.Logf)
		assert.ErrorIs(t, err, ErrDiscoveryUnavailable)
	})

	t.Run("503 → ErrDiscoveryUnavailable", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "not configured", http.StatusServiceUnavailable)
		}))
		defer srv.Close()

		_, err := DiscoverAPI(t.Context(), "partial.to", hostPinningClient(t, srv), t.Logf)
		assert.ErrorIs(t, err, ErrDiscoveryUnavailable)
	})

	t.Run("transport error → ErrDiscoveryUnavailable", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		client := hostPinningClient(t, srv)
		srv.Close()

		_, err := DiscoverAPI(t.Context(), "partial.to", client, t.Logf)
		assert.ErrorIs(t, err, ErrDiscoveryUnavailable)
	})

	t.Run("malformed JSON → ErrDiscoveryUnavailable", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{not json`)) //nolint:errcheck // test handler
		}))
		defer srv.Close()

		_, err := DiscoverAPI(t.Context(), "partial.to", hostPinningClient(t, srv), t.Logf)
		assert.ErrorIs(t, err, ErrDiscoveryUnavailable)
	})

	// A trust-root fetch must not follow a 3xx to another origin. The redirect
	// target serves a perfectly valid document, so this test only passes if the
	// redirect is genuinely refused (not merely erroring on a loop): following
	// it would succeed and return the target's doc.
	t.Run("refuses cross-origin redirect → ErrDiscoveryUnavailable", func(t *testing.T) {
		t.Parallel()
		target := httptest.NewServer(apiHandler(t, "https://us.auth.partial.to"))
		defer target.Close()
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+APIPath, http.StatusFound)
		}))
		defer redirector.Close()

		// schemeRewriteClient rewrites the hard-coded https:// to http:// but
		// leaves the host alone, so the redirect actually reaches `target`
		// rather than being pinned back to `redirector`.
		client := &http.Client{Transport: schemeRewriteTransport{base: http.DefaultTransport}}
		host := strings.TrimPrefix(redirector.URL, "http://")

		doc, err := DiscoverAPI(t.Context(), host, client, t.Logf)
		assert.Nil(t, doc, "must not return the redirect target's document")
		assert.ErrorIs(t, err, ErrDiscoveryUnavailable)
	})

	t.Run("missing audience → ErrDiscoveryUnavailable", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"trusted_issuers":["https://us.auth.partial.to"]}`)) //nolint:errcheck // test handler
		}))
		defer srv.Close()

		_, err := DiscoverAPI(t.Context(), "partial.to", hostPinningClient(t, srv), t.Logf)
		assert.ErrorIs(t, err, ErrDiscoveryUnavailable)
	})
}

// apiTestAudience is the advertised audience every apiHandler serves; the data
// host origin, matching real entire.io config.
const apiTestAudience = "https://partial.to"

func apiHandler(t *testing.T, trustedIssuers ...string) http.HandlerFunc {
	t.Helper()
	doc := APIResponse{
		Issuer:         trustedIssuers[0],
		TrustedIssuers: trustedIssuers,
		Audience:       apiTestAudience,
	}
	body, err := json.Marshal(doc)
	require.NoError(t, err)
	return func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, APIPath, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body) //nolint:errcheck // test handler
	}
}

func TestResolveContextForAPI(t *testing.T) {
	t.Parallel()

	t.Run("active context wins when eligible, returns the doc", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(apiHandler(t, "https://us.auth.partial.to", "https://eu.auth.partial.to"))
		defer srv.Close()

		configDir := t.TempDir()
		require.NoError(t, contexts.Save(configDir, &contexts.File{
			CurrentContext: "me@us-partial",
			Contexts: []*contexts.Context{
				{Name: "me@prod", CoreURL: "https://us.auth.entire.io", Handle: "me", KeychainService: "kc:prod"},
				{Name: "me@us-partial", CoreURL: "https://us.auth.partial.to", Handle: "me", KeychainService: "kc:partial"},
			},
		}))

		c, doc, err := ResolveContextForAPI(t.Context(), configDir, t.TempDir(), "partial.to", hostPinningClient(t, srv), t.Logf)
		require.NoError(t, err)
		assert.Equal(t, "me@us-partial", c.Name)
		assert.Equal(t, "https://partial.to", doc.Audience)
	})

	// The cross-core case the slice exists to fix: the active context is a prod
	// login, but the only context eligible for the partial.to API is the
	// staging one — pick it without the operator setting ENTIRE_AUTH_BASE_URL.
	t.Run("sole eligible context used despite unrelated active", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(apiHandler(t, "https://us.auth.partial.to", "https://eu.auth.partial.to"))
		defer srv.Close()

		configDir := t.TempDir()
		require.NoError(t, contexts.Save(configDir, &contexts.File{
			CurrentContext: "me@prod",
			Contexts: []*contexts.Context{
				{Name: "me@prod", CoreURL: "https://us.auth.entire.io", Handle: "me", KeychainService: "kc:prod"},
				{Name: "me@staging", CoreURL: "https://eu.auth.partial.to", Handle: "me", KeychainService: "kc:staging"},
			},
		}))

		c, _, err := ResolveContextForAPI(t.Context(), configDir, t.TempDir(), "partial.to", hostPinningClient(t, srv), t.Logf)
		require.NoError(t, err)
		assert.Equal(t, "me@staging", c.Name)
	})

	t.Run("no eligible context → login hint naming the API host", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(apiHandler(t, "https://us.auth.partial.to"))
		defer srv.Close()

		configDir := t.TempDir()
		require.NoError(t, contexts.Save(configDir, &contexts.File{
			CurrentContext: "me@prod",
			Contexts:       []*contexts.Context{{Name: "me@prod", CoreURL: "https://us.auth.entire.io", Handle: "me", KeychainService: "kc:prod"}},
		}))

		_, _, err := ResolveContextForAPI(t.Context(), configDir, t.TempDir(), "partial.to", hostPinningClient(t, srv), t.Logf)
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrDiscoveryUnavailable, "a reachable-but-unmatched API is a real login error, not a fallback case")
		assert.Contains(t, err.Error(), "no auth context for API host partial.to")
	})

	t.Run("ambiguous eligible contexts → explicit-choice error", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(apiHandler(t, "https://us.auth.partial.to"))
		defer srv.Close()

		configDir := t.TempDir()
		require.NoError(t, contexts.Save(configDir, &contexts.File{
			CurrentContext: "me@prod",
			Contexts: []*contexts.Context{
				{Name: "me@prod", CoreURL: "https://us.auth.entire.io", Handle: "me", KeychainService: "kc:prod"},
				{Name: "alice@partial", CoreURL: "https://us.auth.partial.to", Handle: "alice", KeychainService: "kc:a"},
				{Name: "bob@partial", CoreURL: "https://us.auth.partial.to", Handle: "bob", KeychainService: "kc:b"},
			},
		}))

		_, _, err := ResolveContextForAPI(t.Context(), configDir, t.TempDir(), "partial.to", hostPinningClient(t, srv), t.Logf)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "multiple login contexts")
		assert.Contains(t, err.Error(), "API host partial.to")
	})

	t.Run("unadvertised → ErrDiscoveryUnavailable for fallback", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "not found", http.StatusNotFound)
		}))
		defer srv.Close()

		_, _, err := ResolveContextForAPI(t.Context(), t.TempDir(), t.TempDir(), "partial.to", hostPinningClient(t, srv), t.Logf)
		assert.ErrorIs(t, err, ErrDiscoveryUnavailable)
	})
}

// countingAPIHandler serves a fixed trust-root document and counts how many
// times /.well-known/entire-api.json is hit, so tests can assert cache hits vs
// live fetches.
func countingAPIHandler(t *testing.T, calls *int32, trustedIssuers ...string) http.HandlerFunc {
	t.Helper()
	doc := APIResponse{Issuer: trustedIssuers[0], TrustedIssuers: trustedIssuers, Audience: apiTestAudience}
	body, err := json.Marshal(doc)
	require.NoError(t, err)
	return func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		assert.Equal(t, APIPath, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body) //nolint:errcheck // test handler
	}
}

func partialContexts(t *testing.T, configDir string) {
	t.Helper()
	require.NoError(t, contexts.Save(configDir, &contexts.File{
		CurrentContext: "me@us-partial",
		Contexts: []*contexts.Context{
			{Name: "me@us-partial", CoreURL: "https://us.auth.partial.to", Handle: "me", KeychainService: "kc:partial"},
		},
	}))
}

// TestResolveContextForAPI_CachedAcrossCalls: the first call fetches
// /.well-known/entire-api.json and caches it; the second is served from
// api_discovery.json with no network hit — symmetric with the cluster cache.
func TestResolveContextForAPI_CachedAcrossCalls(t *testing.T) {
	t.Parallel()
	var calls int32
	srv := httptest.NewServer(countingAPIHandler(t, &calls, "https://us.auth.partial.to"))
	defer srv.Close()

	configDir := t.TempDir()
	cacheDir := t.TempDir()
	partialContexts(t, configDir)

	_, doc, err := ResolveContextForAPI(t.Context(), configDir, cacheDir, "partial.to", hostPinningClient(t, srv), t.Logf)
	require.NoError(t, err)
	assert.Equal(t, "https://partial.to", doc.Audience)
	require.Equal(t, int32(1), atomic.LoadInt32(&calls), "first call fetches /.well-known")

	_, doc2, err := ResolveContextForAPI(t.Context(), configDir, cacheDir, "partial.to", hostPinningClient(t, srv), t.Logf)
	require.NoError(t, err)
	assert.Equal(t, "https://partial.to", doc2.Audience)
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "second call is served from the api-discovery cache")

	// The trust roots are persisted; the audience round-trips through the cache.
	cache, err := discovery.LoadAPIDiscovery(cacheDir)
	require.NoError(t, err)
	entry, fresh, ok := cache.Get("partial.to")
	require.True(t, ok)
	assert.True(t, fresh)
	assert.Equal(t, "https://partial.to", entry.Audience)
}

// TestResolveContextForAPI_StaleFallbackOnFetchFailure: a present-but-stale
// cache entry is used when the live re-fetch fails, so a brief outage doesn't
// break the command — and crucially does NOT degrade to ErrDiscoveryUnavailable
// (which would drop the caller to static resolution).
func TestResolveContextForAPI_StaleFallbackOnFetchFailure(t *testing.T) {
	t.Parallel()
	// Server always 503s, so every live fetch fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not configured", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	configDir := t.TempDir()
	cacheDir := t.TempDir()
	partialContexts(t, configDir)

	// Seed a stale entry (fetched longer ago than the TTL).
	require.NoError(t, discovery.ModifyAPIDiscovery(cacheDir, func(c discovery.APIDiscoveryCache) error {
		c["partial.to"] = &discovery.APIDiscoveryEntry{
			Issuer:         "https://us.auth.partial.to",
			TrustedIssuers: []string{"https://us.auth.partial.to"},
			Audience:       "https://partial.to",
			FetchedAt:      time.Now().Add(-discovery.APIDiscoveryTTL - time.Hour),
		}
		return nil
	}))

	c, doc, err := ResolveContextForAPI(t.Context(), configDir, cacheDir, "partial.to", hostPinningClient(t, srv), t.Logf)
	require.NoError(t, err, "stale cache entry should rescue a failed re-fetch")
	require.NotErrorIs(t, err, ErrDiscoveryUnavailable)
	assert.Equal(t, "me@us-partial", c.Name)
	assert.Equal(t, "https://partial.to", doc.Audience)
}

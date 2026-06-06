package clusterdiscovery

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/discovery"
)

// APIPath is the well-known path a data/web API (entire.io) serves to
// advertise its trust roots, mirroring entire.io's api/src/app.ts route.
// Unlike the cluster blob (core_urls only) it also carries the audience
// the CLI must exchange its core token for, since a resource API
// validates a fixed `aud` claim.
const APIPath = "/.well-known/entire-api.json"

// APIResponse is the parsed shape of /.well-known/entire-api.json. New
// fields may be added by the server; unknown ones are ignored.
type APIResponse struct {
	// Issuer is the API's home core (its preferred login server).
	Issuer string `json:"issuer"`
	// TrustedIssuers is every core whose JWTs the API accepts. Used the
	// same way cluster discovery uses core_urls: to pick the local
	// context whose CoreURL the API will honour.
	TrustedIssuers []string `json:"trusted_issuers"`
	// Audience is the `aud` the exchanged token must carry. The CLI
	// passes this verbatim as the RFC 8693 audience so the issued token
	// matches what the API validates — today the data host origin, but
	// advertised (not assumed) so the server can change it without a CLI
	// release.
	Audience string `json:"audience"`
	// The server also advertises its JWKS URI(s) for verifying inbound
	// tokens, but that's a server-side concern — the CLI never fetches
	// JWKS — so we don't model the field here. Unknown JSON fields are
	// ignored on decode, so the server's shape can evolve freely.
}

// ErrDiscoveryUnavailable wraps every "the API didn't give us a usable
// trust-root document" outcome: it doesn't serve /.well-known/entire-api.json
// (404 — old deployment), is unreachable, answers 503 (unconfigured), or
// returns a malformed/empty body. Callers match on it to fall back to
// static token resolution so behaviour is never worse than before
// discovery existed. Selection failures (no eligible / ambiguous
// context) are NOT wrapped — those are real "log in / pick one" errors
// the user must see.
var ErrDiscoveryUnavailable = errors.New("api discovery unavailable")

// DiscoverAPI fetches and parses an API host's /.well-known/entire-api.json.
// On success it returns a body with a non-empty TrustedIssuers and
// Audience. Every failure mode (transport, non-200, decode, empty
// required fields) is folded under ErrDiscoveryUnavailable so the caller
// has a single sentinel to fall back on.
//
// debugf is optional; nil suppresses debug output.
func DiscoverAPI(ctx context.Context, apiHost string, c *http.Client, debugf DebugFunc) (*APIResponse, error) {
	if debugf == nil {
		debugf = func(string, ...any) {}
	}
	var body APIResponse
	if err := fetchWellKnownJSON(ctx, apiHost, APIPath, c, &body, debugf); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDiscoveryUnavailable, err)
	}
	if len(body.TrustedIssuers) == 0 || body.Audience == "" {
		debugf("api discovery: incomplete document from https://%s%s (trusted_issuers=%d, audience=%q)",
			apiHost, APIPath, len(body.TrustedIssuers), body.Audience)
		return nil, fmt.Errorf("%w: incomplete /.well-known/entire-api.json from %s", ErrDiscoveryUnavailable, apiHost)
	}
	return &body, nil
}

// resolveAPIDoc returns apiHost's trust-root document, from api_discovery.json
// when fresh, otherwise via a live /.well-known/entire-api.json fetch (which is
// then cached). A stale-but-present cache entry is used as a fallback when the
// live fetch fails, so a brief outage doesn't break a command whose trust roots
// we already knew. Mirrors resolveClusterCores. Every cold failure stays folded
// under ErrDiscoveryUnavailable (from DiscoverAPI) for the caller's fallback.
func resolveAPIDoc(ctx context.Context, cacheDir, apiHost string, httpClient *http.Client, debugf DebugFunc) (*APIResponse, error) {
	cache, err := discovery.LoadAPIDiscovery(cacheDir)
	if err != nil {
		// A cache read problem must not block resolution — discover live.
		debugf("api-discovery cache load failed: %v; discovering live", err)
		cache = nil
	}

	var stale *APIResponse
	if cache != nil {
		if entry, fresh, ok := cache.Get(apiHost); ok {
			doc := &APIResponse{Issuer: entry.Issuer, TrustedIssuers: entry.TrustedIssuers, Audience: entry.Audience}
			if fresh {
				debugf("api host %s trust roots from cache", apiHost)
				return doc, nil
			}
			stale = doc
			debugf("api host %s trust-roots cache expired; re-fetching %s", apiHost, APIPath)
		}
	}

	doc, err := DiscoverAPI(ctx, apiHost, httpClient, debugf)
	if err != nil {
		if stale != nil {
			debugf("api discovery for %s failed (%v); falling back to stale cached trust roots", apiHost, err)
			return stale, nil
		}
		return nil, err
	}

	if mErr := discovery.ModifyAPIDiscovery(cacheDir, func(c discovery.APIDiscoveryCache) error {
		c.Set(apiHost, discovery.APIDiscoveryEntry{Issuer: doc.Issuer, TrustedIssuers: doc.TrustedIssuers, Audience: doc.Audience})
		return nil
	}); mErr != nil {
		// Non-fatal: we resolved the doc, the next command just re-fetches.
		debugf("api-discovery cache write for %s failed: %v", apiHost, mErr)
	}
	return doc, nil
}

// ResolveContextForAPI picks the local login context to authenticate data-API
// calls against apiHost, and returns the discovery document alongside it so
// the caller can exchange for the advertised audience.
//
// It mirrors ResolveContextForCluster's account selection — active context
// wins when its CoreURL is among the API's trusted issuers, else the sole
// eligible context, else an explicit-choice / login error — but sources the
// trusted issuers from /.well-known/entire-api.json instead of
// entire-cluster.json. The trust-root document is cached (api_discovery.json,
// long TTL) and re-fetched on expiry, with stale fallback — the same
// cache-then-/.well-known path the cluster resolver uses (resolveClusterCores).
// Account selection is recomputed every call from the live contexts, never
// persisted.
//
// When the API doesn't advertise discovery (404 / unreachable / 503 /
// malformed) and no cache entry exists, the returned error wraps
// ErrDiscoveryUnavailable so the caller falls back to static resolution. A
// successful fetch whose context selection fails returns that selection error
// unwrapped — the user must act on it.
//
// debugf is optional; nil suppresses debug output.
func ResolveContextForAPI(ctx context.Context, configDir, cacheDir, apiHost string, httpClient *http.Client, debugf DebugFunc) (*contexts.Context, *APIResponse, error) {
	if debugf == nil {
		debugf = func(string, ...any) {}
	}
	doc, err := resolveAPIDoc(ctx, cacheDir, apiHost, httpClient, debugf)
	if err != nil {
		return nil, nil, err
	}
	f, err := contexts.Load(configDir)
	if err != nil {
		return nil, nil, fmt.Errorf("load contexts: %w", err)
	}
	selected, err := selectContext(f, "API host "+apiHost, doc.TrustedIssuers, debugf)
	if err != nil {
		return nil, nil, err
	}
	return selected, doc, nil
}

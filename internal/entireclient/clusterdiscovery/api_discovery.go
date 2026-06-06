package clusterdiscovery

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/discovery"
)

// APIPath is the well-known path a data/web API (entire.io) serves to advertise
// its trust roots, mirroring entire.io's api/src/app.ts route. The document also
// carries an `audience`, but the CLI doesn't consume it: the data API requires
// `aud` == its own base URI (https://entire.io / https://partial.to), which the
// token manager already derives from the resource origin it's dialing, so the
// only field the CLI needs is the trusted-issuer list — exactly like a git
// cluster's core_urls.
const APIPath = "/.well-known/entire-api.json"

// APIResponse is the parsed shape of /.well-known/entire-api.json. The CLI reads
// only trusted_issuers (to pick the login context); issuer/audience/jwks_uris
// are server-side concerns and ignored on decode.
type APIResponse struct {
	// TrustedIssuers is every core whose JWTs the API accepts. Used the same way
	// cluster discovery uses core_urls: to pick the local context whose CoreURL
	// the API will honour.
	TrustedIssuers []string `json:"trusted_issuers"`
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

// DiscoverAPI fetches and parses an API host's /.well-known/entire-api.json,
// returning its trusted issuers. Every failure mode (transport, non-200,
// decode, empty trusted_issuers) is folded under ErrDiscoveryUnavailable so the
// caller has a single sentinel to fall back on.
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
	if len(body.TrustedIssuers) == 0 {
		debugf("api discovery: no trusted_issuers in response from https://%s%s", apiHost, APIPath)
		return nil, fmt.Errorf("%w: incomplete /.well-known/entire-api.json from %s", ErrDiscoveryUnavailable, apiHost)
	}
	return &body, nil
}

// resolveAPICores returns apiHost's trusted issuer URLs, from
// api_discovery.json when fresh, otherwise via a live
// /.well-known/entire-api.json fetch (which is then cached). A stale-but-present
// cache entry is used as a fallback when the live fetch fails, so a brief outage
// doesn't break a command whose trust roots we already knew. Mirrors
// resolveClusterCores exactly — the data-API trusted issuers ARE core URLs, so
// they share the cores cache (different file). Cold failures stay folded under
// ErrDiscoveryUnavailable (from DiscoverAPI) for the caller's static fallback.
func resolveAPICores(ctx context.Context, cacheDir, apiHost string, httpClient *http.Client, debugf DebugFunc) ([]string, error) {
	cache, err := discovery.LoadAPICores(cacheDir)
	if err != nil {
		// A cache read problem must not block resolution — discover live.
		debugf("api-discovery cache load failed: %v; discovering live", err)
		cache = nil
	}

	var stale []string
	if cache != nil {
		if urls, fresh, ok := cache.Get(apiHost); ok {
			if fresh {
				debugf("api host %s trusted issuers from cache: %v", apiHost, urls)
				return urls, nil
			}
			stale = urls
			debugf("api host %s trusted-issuers cache expired; re-fetching %s", apiHost, APIPath)
		}
	}

	body, err := DiscoverAPI(ctx, apiHost, httpClient, debugf)
	if err != nil {
		if stale != nil {
			debugf("api discovery for %s failed (%v); falling back to stale cached trusted issuers %v", apiHost, err, stale)
			return stale, nil
		}
		return nil, err
	}

	if mErr := discovery.ModifyAPICores(cacheDir, func(c discovery.ClusterCoresCache) error {
		c.Set(apiHost, body.TrustedIssuers)
		return nil
	}); mErr != nil {
		// Non-fatal: we resolved the issuers, the next command just re-fetches.
		debugf("api-discovery cache write for %s failed: %v", apiHost, mErr)
	}
	return body.TrustedIssuers, nil
}

// ResolveContextForAPI picks the local login context to authenticate data-API
// calls against apiHost.
//
// It mirrors ResolveContextForCluster: active context wins when its CoreURL is
// among the API's trusted issuers, else the sole eligible context, else an
// explicit-choice / login error — sourcing the trusted issuers from
// /.well-known/entire-api.json (cached in api_discovery.json, long TTL,
// re-fetched on expiry with stale fallback) instead of entire-cluster.json.
// Account selection is recomputed every call from the live contexts, never
// persisted. The caller exchanges the chosen context's token for the data host
// origin (which is the audience the API requires); no audience is read here.
//
// When the API doesn't advertise discovery (404 / unreachable / 503 /
// malformed) and no cache entry exists, the returned error wraps
// ErrDiscoveryUnavailable so the caller falls back to static resolution. A
// successful fetch whose context selection fails returns that selection error
// unwrapped — the user must act on it.
//
// debugf is optional; nil suppresses debug output.
func ResolveContextForAPI(ctx context.Context, configDir, cacheDir, apiHost string, httpClient *http.Client, debugf DebugFunc) (*contexts.Context, error) {
	if debugf == nil {
		debugf = func(string, ...any) {}
	}
	trustedIssuers, err := resolveAPICores(ctx, cacheDir, apiHost, httpClient, debugf)
	if err != nil {
		return nil, err
	}
	f, err := contexts.Load(configDir)
	if err != nil {
		return nil, fmt.Errorf("load contexts: %w", err)
	}
	return selectContext(f, "API host "+apiHost, trustedIssuers, debugf)
}

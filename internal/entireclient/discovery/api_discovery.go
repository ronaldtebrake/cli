package discovery

import (
	"path/filepath"
	"time"
)

const (
	apiDiscoveryFileName = "api_discovery.json"

	// APIDiscoveryTTL bounds how long a cached data-API trust-root document
	// (issuer / trusted_issuers / audience) is treated as fresh. These roots are
	// near-static infra — the same rationale as ClusterCoresTTL — so a long TTL
	// is fine. On expiry we re-fetch /.well-known/entire-api.json and only fall
	// back to the stale entry if that fetch fails.
	APIDiscoveryTTL = 24 * time.Hour
)

// APIDiscoveryCache maps a data-API host to its cached trust-root document,
// memoizing /.well-known/entire-api.json so routine activity/search/trail/
// dispatch/recap commands don't re-fetch it every invocation. It stores only
// the objective host→trust-roots fact (issuer, trusted issuers, audience) —
// never which local account to authenticate as. The account is chosen fresh on
// every command from the user's contexts, so a multi-account user is never
// silently pinned to one identity.
//
// Cache file: api_discovery.json in the cache dir (alongside cluster_cores.json).
// Safe to delete by hand to force re-discovery. Mirrors ClusterCoresCache; the
// extra Audience field is what distinguishes a resource API from a git cluster.
type APIDiscoveryCache map[string]*APIDiscoveryEntry

// APIDiscoveryEntry is one host's cached trust-root document plus when it was
// fetched. Freshness is fetched_at + APIDiscoveryTTL, computed at read time so a
// TTL change re-interprets existing entries without a migration.
type APIDiscoveryEntry struct {
	Issuer         string    `json:"issuer"`
	TrustedIssuers []string  `json:"trusted_issuers"`
	Audience       string    `json:"audience"`
	FetchedAt      time.Time `json:"fetched_at"`
}

// LoadAPIDiscovery reads the api-host→trust-roots cache. A missing or corrupt
// file yields an empty cache. Unlocked read; use ModifyAPIDiscovery for a
// read-modify-write sequence.
func LoadAPIDiscovery(cacheDir string) (APIDiscoveryCache, error) {
	return readAPIDiscoveryNoLock(filepath.Join(cacheDir, apiDiscoveryFileName))
}

// ModifyAPIDiscovery atomically applies fn to the api-host→trust-roots cache
// under a single exclusive flock.
func ModifyAPIDiscovery(cacheDir string, fn func(APIDiscoveryCache) error) error {
	return modifyCacheFile(cacheDir, apiDiscoveryFileName, readAPIDiscoveryNoLock, writeAPIDiscoveryNoLock, fn)
}

func readAPIDiscoveryNoLock(path string) (APIDiscoveryCache, error) {
	cache := make(APIDiscoveryCache)
	err := loadCacheFile(path, &cache, func() APIDiscoveryCache { return make(APIDiscoveryCache) })
	return cache, err
}

func writeAPIDiscoveryNoLock(path string, cache APIDiscoveryCache) error {
	return writeCacheFile(path, cache)
}

// Get returns a host's cached entry, whether it's still fresh, and whether it
// exists at all. A present-but-stale entry returns (entry, false, true) so
// callers can attempt a re-fetch yet fall back to it if that fetch fails. An
// entry missing the required fields (no trusted issuers, or no audience) is
// treated as absent — a half-written document must not be trusted.
func (c APIDiscoveryCache) Get(host string) (entry *APIDiscoveryEntry, fresh, ok bool) {
	e := c[host]
	if e == nil || len(e.TrustedIssuers) == 0 || e.Audience == "" {
		return nil, false, false
	}
	return e, time.Now().Before(e.FetchedAt.Add(APIDiscoveryTTL)), true
}

// Set records a host's trust-root document, stamping the fetch time to now. The
// TrustedIssuers slice is copied so later mutation by the caller can't corrupt
// the cache; the caller-supplied FetchedAt is ignored.
func (c APIDiscoveryCache) Set(host string, entry APIDiscoveryEntry) {
	cp := entry
	cp.TrustedIssuers = append([]string(nil), entry.TrustedIssuers...)
	cp.FetchedAt = time.Now()
	c[host] = &cp
}

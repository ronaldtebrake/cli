package clusterdiscovery

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/entireio/cli/internal/entireclient/contexts"
)

// ResolveContextForCluster picks the auth context for clusterHost.
// Resolution order:
//
//  1. Explicit `cluster_contexts[clusterHost]` binding in contexts.json
//     pointing at an existing context — used as-is. Bindings are
//     created only by deliberate action; this helper never writes one.
//
//  2. Discovery via /.well-known/entire-cluster.json on the cluster
//     itself, matched against existing local contexts by the
//     advertised core_urls. The first advertised URL with a local
//     context wins.
//
//     A discovery match is NOT persisted here. The caller binds the
//     clusterHost→context mapping into contexts.json only after the
//     first scoped-token exchange against the cluster succeeds (see
//     makeBindHook in cmd/git-remote-entire). Binding post-success
//     rather than on the bare /.well-known match means a host that
//     advertises a core we hold a context for, but then can't actually
//     authenticate us, never leaves a stale binding behind. Once bound,
//     subsequent invocations short-circuit at step 1 and skip discovery.
//
//  3. No local context matches any advertised URL — return a
//     fatal-ready error with the login hint listing the cluster's
//     advertised issuers.
//
// We deliberately do NOT fall back to current_context for an unknown
// cluster host. current_context can point at a different environment
// than clusterHost (e.g. a staging context against a prod cluster); the
// cluster then rejects the exchanged token with "unknown cluster_host"
// because its own registry doesn't know that core. The cluster's
// /.well-known is the authoritative answer to "which env am I in", so we
// ask it rather than guessing from the active context.
//
// debugf is optional; nil suppresses debug output.
func ResolveContextForCluster(ctx context.Context, configDir, clusterHost string, httpClient *http.Client, debugf DebugFunc) (*contexts.Context, error) {
	if debugf == nil {
		debugf = func(string, ...any) {}
	}
	f, err := contexts.Load(configDir)
	if err != nil {
		return nil, fmt.Errorf("load contexts: %w", err)
	}
	if name, ok := f.ClusterContexts[clusterHost]; ok && name != "" {
		if c := f.Find(name); c != nil {
			debugf("contexts.json binding %s -> %s", clusterHost, c.Name)
			return c, nil
		}
		debugf("stale binding %s -> %q (context no longer exists); falling through to discovery", clusterHost, name)
	}
	body, err := Discover(ctx, clusterHost, httpClient, debugf)
	if err != nil {
		return nil, formatDiscoveryError(clusterHost, err)
	}
	current := f.Find(f.CurrentContext)
	for _, coreURL := range body.CoreURLs {
		matches := f.ContextsForIssuer(coreURL)
		if len(matches) == 0 {
			continue
		}
		// Prefer the active context when it's one of the eligible matches —
		// otherwise a core with several accounts (alice@core, bob@core) would
		// resolve to whichever was saved first, silently authenticating as the
		// wrong user. Fall back to the first match when the current context
		// isn't eligible for this cluster. This tie-break only applies until
		// the caller binds the cluster (after the first successful exchange);
		// from then on step 1 returns the bound context directly. `entire auth
		// use` therefore affects only not-yet-bound clusters — matching the
		// `auth use`/`auth unbind` contract.
		c := matches[0]
		if current != nil {
			for _, m := range matches {
				if m.Name == current.Name {
					c = current
					break
				}
			}
		}
		debugf("resolved %s -> %s via discovery match on %s (caller binds on first successful exchange)", clusterHost, c.Name, coreURL)
		return c, nil
	}
	return nil, errors.New(RenderLoginHint(clusterHost, body.CoreURLs))
}

// formatDiscoveryError turns a Discover error into the message
// operators have always seen at this layer. Kept here (not on the
// sentinels themselves) so the package's errors stay machine-readable
// while the caller-facing strings remain centralised.
func formatDiscoveryError(clusterHost string, err error) error {
	switch {
	case errors.Is(err, ErrUnreachable):
		return fmt.Errorf("%s doesn't look like a cluster, or it is unreachable: %w", clusterHost, err)
	case errors.Is(err, ErrNoIssuers):
		return fmt.Errorf("cluster %s does not advertise any trusted login servers (HTTP 503 from %s); contact the cluster administrator",
			clusterHost, Path)
	case errors.Is(err, ErrNoCoreURLs):
		return fmt.Errorf("cluster %s advertises no trusted core URLs (empty list at %s); contact the cluster administrator",
			clusterHost, Path)
	default:
		return fmt.Errorf("cluster discovery for %s: %w", clusterHost, err)
	}
}

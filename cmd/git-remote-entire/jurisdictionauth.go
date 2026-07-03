package main

// Jurisdiction access tokens (docs/auth.md, ADR 20260612) are how git-remote-entire
// authenticates git smart-HTTP: a token minted with scope=openid and
// aud = the cluster's advertised jurisdiction_audience. The data plane
// authorizes it live against regional SpiceDB, so one token covers every
// repo the account can reach.
//
// The token is persisted in the OS keychain (like the login JWT) so fresh
// git-remote-entire processes reuse it instead of paying the exchange per
// git command — with the server-side 8h JurisdictionAccessTokenTTL that removes the
// exchange from the hot path entirely.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/httputil"
	"github.com/entireio/cli/internal/entireclient/repocreds"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
	"github.com/entireio/cli/internal/remotehelper/debuglog"
)

// jurisdictionKeyringService is the keychain service name for jurisdiction
// tokens, keyed by audience so tokens for different jurisdictions
// (and prod vs staging) can't be confused. The account is the context handle.
func jurisdictionKeyringService(audience string) string {
	return "entire-jurisdiction:" + strings.TrimRight(audience, "/")
}

// jurisdictionTokenSource satisfies the same Token(ctx, audienceSuffix, action)
// seam as repocreds.Cache but ignores the repo/action: one jurisdiction token
// covers every repo the account can reach. Tokens come from, in order: the
// in-process memo, the OS keychain (persisted by an earlier invocation),
// or a fresh /oauth/token exchange (then persisted).
type jurisdictionTokenSource struct {
	// homeCoreURL is the login context's core — the exchange target for the
	// common case where the cluster's jurisdiction is the login's home.
	homeCoreURL string
	// audience is the cluster's advertised jurisdiction audience.
	audience string
	// jurisdictionCoreURL is the cluster's advertised core that mints for
	// audience — the cross-jurisdiction exchange endpoint. It rides the
	// same TLS-authenticated /.well-known trust root as the audience.
	jurisdictionCoreURL string
	// handle is the context's account handle — the keychain account key.
	handle string
	login  func(context.Context) (string, error)
	client *http.Client

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

func newJurisdictionTokenSource(homeCoreURL, audience, jurisdictionCoreURL, handle string, login func(context.Context) (string, error), client *http.Client) *jurisdictionTokenSource {
	return &jurisdictionTokenSource{
		// Both core URLs feed httputil.PostOAuthToken, which appends
		// "/oauth/token" to the base verbatim — trim so a trailing slash
		// (context core_url comes from the login JWT's iss claim) can't
		// produce a double-slash endpoint.
		homeCoreURL:         strings.TrimRight(homeCoreURL, "/"),
		audience:            audience,
		jurisdictionCoreURL: strings.TrimRight(jurisdictionCoreURL, "/"),
		handle:              handle,
		login:               login,
		client:              client,
	}
}

// Token returns the jurisdiction token, minting only when neither the memo nor
// the keychain has a fresh one.
func (s *jurisdictionTokenSource) Token(ctx context.Context, _, _ string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Before(s.expiresAt) {
		return s.token, nil
	}

	service := jurisdictionKeyringService(s.audience)
	if encoded, err := tokenstore.Get(service, s.handle); err == nil {
		// The empty-token guard rejects a corrupted keychain entry that
		// decodes to a valid timestamp but no token — otherwise it would
		// produce bare "Bearer " headers until the entry expired.
		//
		// Freshness margins differ by design: cross-process reuse stops at
		// the 5m TokenExpirationBuffer (a fresh process should not start on
		// a nearly-dead token), while this process keeps its token until
		// SafetyMargin before actual expiry — individual git requests are
		// short.
		if token, expiresAt := tokenstore.DecodeTokenWithExpiration(encoded); token != "" && !tokenstore.IsTokenExpiredOrExpiring(expiresAt) {
			debuglog.Printf("jurisdiction token from keychain (aud=%s, expires %s)", s.audience, expiresAt.Format(time.RFC3339))
			s.token = token
			s.expiresAt = expiresAt.Add(-repocreds.SafetyMargin)
			return token, nil
		}
	}

	token, ttl, err := s.mint(ctx)
	if err != nil {
		return "", err
	}
	if ttl <= 0 {
		// A non-positive expires_in would memoize/persist an already-dead
		// token; serve this one request and re-mint next time.
		return token, nil
	}

	s.token = token
	margin := min(repocreds.SafetyMargin, ttl/2)
	s.expiresAt = time.Now().Add(ttl - margin)
	if err := tokenstore.Set(service, s.handle, tokenstore.EncodeTokenWithExpiration(token, int64(ttl/time.Second))); err != nil {
		// Non-fatal: the token still serves this process; the next
		// invocation just re-mints.
		debuglog.Printf("jurisdiction token keychain write failed: %v", err)
	}
	return token, nil
}

// Invalidate drops the memoized token and the persisted keychain entry, so
// the next acquisition (this process or the next) mints fresh. Wired to the
// transport's 401 observer: when the data plane rejects the credential
// itself (signing-key rotation, expiry skew), replaying it until its
// recorded expiry — up to the full 8h TTL — would keep every git command
// failing. The in-flight command still fails; the next one self-heals.
func (s *jurisdictionTokenSource) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = ""
	s.expiresAt = time.Time{}
	if err := tokenstore.Delete(jurisdictionKeyringService(s.audience), s.handle); err != nil && !errors.Is(err, tokenstore.ErrNotFound) {
		debuglog.Printf("jurisdiction token keychain delete failed: %v", err)
	}
	debuglog.Printf("jurisdiction token invalidated after 401 (aud=%s); next invocation re-mints", s.audience)
}

// mint exchanges the login JWT for a jurisdiction token at the core owning the
// target jurisdiction. For a same-jurisdiction login that is the login's own
// core; for a cross-jurisdiction repo the sibling core accepts our login JWT
// via the foreign-session path and mints the jurisdiction token in the same
// single POST.
func (s *jurisdictionTokenSource) mint(ctx context.Context) (string, time.Duration, error) {
	loginJWT, err := s.login(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("refresh login token: %w", err)
	}

	coreURL, err := s.exchangeCore(loginJWT)
	if err != nil {
		return "", 0, err
	}

	form := httputil.TokenExchangeForm(loginJWT, s.audience, auth.JurisdictionIdentityScope)

	token, expiresIn, err := httputil.PostOAuthToken(ctx, s.client, coreURL, form)
	if err != nil {
		return "", 0, fmt.Errorf("jurisdiction token exchange (aud=%s at %s): %w", s.audience, coreURL, err)
	}
	if strings.TrimSpace(token) == "" {
		return "", 0, errors.New("jurisdiction token exchange returned an empty access token")
	}
	ttl := time.Duration(expiresIn) * time.Second
	debuglog.Printf("jurisdiction token minted: aud=%s at %s ttl=%s", s.audience, coreURL, ttl)
	return token, ttl, nil
}

// exchangeCore picks the core to exchange at: the login's own core when the
// audience is in the login's home jurisdiction, else the cluster's
// advertised jurisdiction core. The home core is the issuer the user logged
// in at, used verbatim (it may legitimately be loopback http in local dev);
// the advertised core comes from a foreign cluster's /.well-known, so it
// must be https before the login JWT is POSTed to it.
func (s *jurisdictionTokenSource) exchangeCore(loginJWT string) (string, error) {
	label, err := jurisdictionLabel(s.audience)
	if err != nil {
		return "", err
	}
	home, err := auth.HomeJurisdictionFromLoginJWT(loginJWT)
	if err != nil {
		return "", fmt.Errorf("read home jurisdiction: %w", err)
	}
	if home == "" {
		return "", errors.New("login token has no home_jurisdiction claim")
	}
	// jurisdictionLabel lowercases the audience host; fold the claim too so
	// the comparison doesn't hinge on core config region keys being lowercase.
	if strings.ToLower(home) == label {
		return s.homeCoreURL, nil
	}

	if s.jurisdictionCoreURL == "" {
		return "", fmt.Errorf("cross-jurisdiction token exchange for %s: cluster advertises no jurisdiction_core_url", s.audience)
	}
	if !strings.HasPrefix(s.jurisdictionCoreURL, "https://") {
		return "", fmt.Errorf("advertised jurisdiction core %q must be https", s.jurisdictionCoreURL)
	}
	return s.jurisdictionCoreURL, nil
}

// jurisdictionLabel extracts the jurisdiction label from an audience like
// https://au.entire.io ("au"), for comparison against the login JWT's
// home_jurisdiction claim.
func jurisdictionLabel(audience string) (string, error) {
	u, err := url.Parse(audience)
	if err != nil {
		return "", fmt.Errorf("parse jurisdiction audience %q: %w", audience, err)
	}
	host := strings.ToLower(u.Hostname())
	label, rest, ok := strings.Cut(host, ".")
	if !ok || label == "" || rest == "" {
		return "", fmt.Errorf("jurisdiction audience %q has no <jurisdiction>.<domain> host", audience)
	}
	return label, nil
}

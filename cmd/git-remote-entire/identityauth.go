package main

// Jurisdiction identity tokens (ADR 20260612) are how git-remote-entire
// authenticates git smart-HTTP: a token minted with scope=openid and
// aud = the cluster's advertised jurisdiction_audience. The data plane
// authorizes it live against regional SpiceDB, so one token covers every
// repo the account can reach.
//
// The token is persisted in the OS keychain (like the login JWT) so fresh
// git-remote-entire processes reuse it instead of paying the exchange per
// git command — with the server-side 2h IdentityTokenTTL that removes the
// exchange from the hot path entirely.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/internal/entireclient/httputil"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
	"github.com/entireio/cli/internal/remotehelper/debuglog"
)

// identityKeyringService is the keychain service name for a jurisdiction's
// identity tokens, keyed by audience so tokens for different jurisdictions
// (and prod vs staging) can't be confused. The account is the context handle.
func identityKeyringService(audience string) string {
	return "entire-identity:" + strings.TrimRight(audience, "/")
}

// identityTokenSource satisfies the same Token(ctx, audienceSuffix, action)
// seam as repocreds.Cache but ignores the repo/action: one identity token
// covers every repo the account can reach. Tokens come from, in order: the
// in-process memo, the OS keychain (persisted by an earlier invocation),
// or a fresh /oauth/token exchange (then persisted).
type identityTokenSource struct {
	// homeCoreURL is the login context's core — the exchange target for the
	// common case where the cluster's jurisdiction is the login's home.
	homeCoreURL string
	// audience is the cluster's advertised jurisdiction audience.
	audience string
	// trustedCores is the cluster's advertised core set; a derived
	// cross-juris exchange core must be in it before the login JWT is
	// POSTed there.
	trustedCores []string
	// handle is the context's account handle — the keychain account key.
	handle string
	login  func(context.Context) (string, error)
	client *http.Client

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

func newIdentityTokenSource(homeCoreURL, audience string, trustedCores []string, handle string, login func(context.Context) (string, error), client *http.Client) *identityTokenSource {
	if override := strings.TrimSpace(os.Getenv("ENTIRE_IDENTITY_AUDIENCE")); override != "" {
		audience = override
	}
	return &identityTokenSource{
		homeCoreURL:  homeCoreURL,
		audience:     audience,
		trustedCores: trustedCores,
		handle:       handle,
		login:        login,
		client:       client,
	}
}

// Token returns the identity token, minting only when neither the memo nor
// the keychain has a fresh one.
func (s *identityTokenSource) Token(ctx context.Context, _, _ string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Before(s.expiresAt) {
		return s.token, nil
	}

	service := identityKeyringService(s.audience)
	if encoded, err := tokenstore.Get(service, s.handle); err == nil {
		token, expiresAt := tokenstore.DecodeTokenWithExpiration(encoded)
		if !tokenstore.IsTokenExpiredOrExpiring(expiresAt) {
			debuglog.Printf("identity token from keychain (aud=%s, expires %s)", s.audience, expiresAt.Format(time.RFC3339))
			s.token = token
			s.expiresAt = expiresAt.Add(-time.Minute)
			return token, nil
		}
	}

	token, ttl, err := s.mint(ctx)
	if err != nil {
		return "", err
	}

	s.token = token
	margin := min(time.Minute, ttl/2)
	s.expiresAt = time.Now().Add(ttl - margin)
	if err := tokenstore.Set(service, s.handle, tokenstore.EncodeTokenWithExpiration(token, int64(ttl/time.Second))); err != nil {
		// Non-fatal: the token still serves this process; the next
		// invocation just re-mints.
		debuglog.Printf("identity token keychain write failed: %v", err)
	}
	return token, nil
}

// mint exchanges the login JWT for an identity token at the core owning the
// target jurisdiction. For a same-jurisdiction login that is the login's own
// core; for a cross-jurisdiction repo the sibling core accepts our login JWT
// via the foreign-session path and mints the identity token in the same
// single POST.
func (s *identityTokenSource) mint(ctx context.Context) (string, time.Duration, error) {
	loginJWT, err := s.login(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("refresh login token: %w", err)
	}

	coreURL, err := s.exchangeCore(loginJWT)
	if err != nil {
		return "", 0, err
	}

	form := url.Values{}
	form.Set("grant_type", httputil.GrantTypeTokenExchange)
	form.Set("subject_token", loginJWT)
	form.Set("subject_token_type", "urn:ietf:params:oauth:token-type:access_token")
	form.Set("requested_token_type", "urn:ietf:params:oauth:token-type:access_token")
	form.Set("audience", s.audience)
	form.Set("scope", "openid")
	form.Set("client_id", "entire-cli") // same client as repocreds' oauthClientID

	token, expiresIn, err := httputil.PostOAuthToken(ctx, s.client, coreURL, form)
	if err != nil {
		return "", 0, fmt.Errorf("identity token exchange (aud=%s at %s): %w", s.audience, coreURL, err)
	}
	if strings.TrimSpace(token) == "" {
		return "", 0, errors.New("identity token exchange returned an empty access token")
	}
	ttl := time.Duration(expiresIn) * time.Second
	debuglog.Printf("identity token minted: aud=%s at %s ttl=%s", s.audience, coreURL, ttl)
	return token, ttl, nil
}

// exchangeCore picks the core to exchange at: the login's own core when the
// audience is in the login's home jurisdiction, else the audience
// jurisdiction's sibling core (https://<label>.auth.<family>), which must be
// in the cluster's advertised core set before the login JWT is sent to it.
// ENTIRE_IDENTITY_CORE overrides.
func (s *identityTokenSource) exchangeCore(loginJWT string) (string, error) {
	if override := strings.TrimSpace(os.Getenv("ENTIRE_IDENTITY_CORE")); override != "" {
		return strings.TrimRight(override, "/"), nil
	}

	label, family, err := splitJurisdictionHost(s.audience)
	if err != nil {
		return "", err
	}
	home, err := homeJurisdictionFromLoginJWT(loginJWT)
	if err != nil {
		return "", err
	}
	if home == label {
		return s.homeCoreURL, nil
	}

	sibling := "https://" + label + ".auth." + family
	for _, trusted := range s.trustedCores {
		if strings.TrimRight(trusted, "/") == sibling {
			return sibling, nil
		}
	}
	return "", fmt.Errorf("cross-jurisdiction identity exchange: derived core %s is not in the cluster's advertised core set %v; set ENTIRE_IDENTITY_CORE", sibling, s.trustedCores)
}

// splitJurisdictionHost splits a jurisdiction audience like
// https://au.entire.io into its jurisdiction label ("au") and domain family
// ("entire.io").
func splitJurisdictionHost(audience string) (label, family string, err error) {
	u, err := url.Parse(audience)
	if err != nil {
		return "", "", fmt.Errorf("parse jurisdiction audience %q: %w", audience, err)
	}
	host := strings.ToLower(u.Hostname())
	label, family, ok := strings.Cut(host, ".")
	if !ok || label == "" || family == "" {
		return "", "", fmt.Errorf("jurisdiction audience %q has no <jurisdiction>.<domain> host", audience)
	}
	return label, family, nil
}

// homeJurisdictionFromLoginJWT reads the home_jurisdiction claim without
// verifying the signature (we only route with it; the server re-verifies).
// Copied from auth.homeJurisdictionFromLoginJWT, which is unexported.
func homeJurisdictionFromLoginJWT(loginJWT string) (string, error) {
	parts := strings.Split(loginJWT, ".")
	if len(parts) < 2 {
		return "", errors.New("login token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode login token payload: %w", err)
	}
	var claims struct {
		HomeJurisdiction string `json:"home_jurisdiction"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("parse login token payload: %w", err)
	}
	if claims.HomeJurisdiction == "" {
		return "", errors.New("login token has no home_jurisdiction claim")
	}
	return claims.HomeJurisdiction, nil
}

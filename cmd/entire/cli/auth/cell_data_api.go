package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
	"github.com/entireio/cli/internal/entireclient/httputil"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

const (
	cellDataAPITimeout = 30 * time.Second

	defaultJurisdictionAudienceTemplate = "https://{jurisdiction}.entire.io"
	defaultCoreBaseURLTemplate          = "https://{jurisdiction}.auth.entire.io"

	jurisdictionIdentityScope = "openid"
)

// resolveContextForCellAPI is the discovery seam for cell routing, swapped in
// tests. Mirrors resolveContextForAPI.
var resolveContextForCellAPI resolveContextFunc = clusterdiscovery.ResolveContextForAPI

// SetResolveContextForCellAPIForTest overrides the cell-API discovery seam.
func SetResolveContextForCellAPIForTest(t interface{ Helper() }, fn resolveContextFunc) func() {
	t.Helper()
	prev := resolveContextForCellAPI
	resolveContextForCellAPI = fn
	return func() { resolveContextForCellAPI = prev }
}

// cellExchangeTransportForTest, when non-nil, is the HTTP transport used for
// jurisdiction token exchange and cluster listing. Production leaves it nil.
var cellExchangeTransportForTest http.RoundTripper

// NewEntireAPICellClient returns an authenticated client aimed at the caller's
// home-jurisdiction entire-api cell with a jurisdictional identity token
// (scope=openid, aud=jurisdiction host). This is the COR-666 CLI-side routing
// path: repo-scoped entire-api reads do not accept the narrowed api-access
// bearer minted for https://entire.io — they require a cell identity token.
//
// When ENTIRE_API_BASE_URL already targets a cell (not the entire.io BFF), the
// configured origin is kept and only the token exchange is performed.
func NewEntireAPICellClient(ctx context.Context, insecureHTTP bool) (*api.Client, error) {
	dataURL := api.BaseURL()
	if insecureHTTP {
		EnableInsecureHTTP()
	} else if err := api.RequireSecureURL(dataURL); err != nil {
		return nil, fmt.Errorf("base URL check: %w", err)
	}

	dataOrigin := api.OriginOnly(dataURL)
	host, ok := hostOf(dataOrigin)
	if !ok {
		return nil, fmt.Errorf("data API URL %q has no host to discover against", dataURL)
	}

	dctx, cancel := context.WithTimeout(ctx, dataAPIDiscoveryTimeout)
	defer cancel()
	var httpClient *http.Client
	if cellExchangeTransportForTest != nil {
		httpClient = &http.Client{Timeout: cellDataAPITimeout, Transport: cellExchangeTransportForTest}
	} else if shouldUsePlainHTTPDiscovery(dataOrigin) {
		httpClient = dataAPIDiscoveryClient(dataOrigin)
		httpClient.Timeout = cellDataAPITimeout
	} else {
		httpClient = &http.Client{Timeout: cellDataAPITimeout}
	}

	selected, err := resolveContextForCellAPI(dctx, userdirs.Config(), userdirs.Cache(), host, httpClient, nil)
	if errors.Is(err, clusterdiscovery.ErrDiscoveryUnavailable) {
		return nil, fmt.Errorf("%s does not advertise its trusted login servers (/.well-known/entire-api.json missing or unreachable); cannot authenticate: %w", host, err)
	}
	if err != nil {
		return nil, err
	}

	allowInsecure := insecureHTTPEnabled() || isLoopbackHTTP(selected.CoreURL) || isLoopbackHTTP(dataOrigin)
	loginProvider, err := NewRefreshingLoginProvider(selected, cellExchangeTransportForTest, allowInsecure)
	if err != nil {
		return nil, err
	}

	loginJWT, err := loginProvider(ctx)
	if err != nil {
		if errors.Is(err, ErrNotLoggedIn) {
			return nil, fmt.Errorf("not logged in (run 'entire login' first): %w", err)
		}
		return nil, fmt.Errorf("refresh login token: %w", err)
	}

	jurisdiction, err := homeJurisdictionFromLoginJWT(loginJWT)
	if err != nil {
		return nil, err
	}
	if jurisdiction == "" {
		return nil, errors.New("login token has no home_jurisdiction claim; cannot route to entire-api cell")
	}

	cellBaseURL := strings.TrimRight(dataOrigin, "/")
	if isEntireIOBFF(dataOrigin) {
		cellBaseURL, err = resolveCellAPIBaseURL(ctx, jurisdictionCoreURL(jurisdiction, selected.CoreURL), loginJWT, jurisdiction, httpClient)
		if err != nil {
			return nil, err
		}
	}

	audience := jurisdictionAudience(jurisdiction)
	token, err := exchangeJurisdictionToken(ctx, jurisdictionCoreURL(jurisdiction, selected.CoreURL), loginJWT, audience, httpClient)
	if err != nil {
		return nil, fmt.Errorf("exchange jurisdictional identity token: %w", err)
	}

	return api.NewClientWithBaseURL(token, cellBaseURL), nil
}

func isEntireIOBFF(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ".api.") {
		return false
	}
	return host == "entire.io" || strings.HasSuffix(host, ".entire.io")
}

func jurisdictionAudience(jurisdiction string) string {
	tmpl := strings.TrimSpace(os.Getenv("ENTIRE_API_AUDIENCE_TEMPLATE"))
	if tmpl == "" {
		tmpl = defaultJurisdictionAudienceTemplate
	}
	return strings.ReplaceAll(strings.TrimRight(tmpl, "/"), "{jurisdiction}", jurisdiction)
}

func jurisdictionCoreURL(homeJurisdiction, fallbackCore string) string {
	tmpl := strings.TrimSpace(os.Getenv("ENTIRE_CORE_BASE_URL_TEMPLATE"))
	if tmpl == "" {
		tmpl = defaultCoreBaseURLTemplate
	}
	if strings.Contains(tmpl, "{jurisdiction}") {
		return strings.ReplaceAll(strings.TrimRight(tmpl, "/"), "{jurisdiction}", homeJurisdiction)
	}
	return strings.TrimRight(fallbackCore, "/")
}

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
	return claims.HomeJurisdiction, nil
}

type clusterListingRow struct {
	Jurisdiction string `json:"jurisdiction"`
	IsDefault    bool   `json:"isDefault"`
	APIURL       string `json:"apiUrl"`
}

func resolveCellAPIBaseURL(ctx context.Context, coreURL, loginJWT, jurisdiction string, httpClient *http.Client) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(coreURL, "/")+"/api/v1/clusters", nil)
	if err != nil {
		return "", fmt.Errorf("build clusters request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+loginJWT)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("list clusters: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("list clusters: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var listing struct {
		Clusters []clusterListingRow `json:"clusters"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		return "", fmt.Errorf("decode clusters response: %w", err)
	}

	var matches []clusterListingRow
	for _, row := range listing.Clusters {
		if row.Jurisdiction == jurisdiction && strings.TrimSpace(row.APIURL) != "" {
			matches = append(matches, row)
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no entire-api cell configured for home jurisdiction %q", jurisdiction)
	}
	chosen := matches[0]
	for _, row := range matches {
		if row.IsDefault {
			chosen = row
			break
		}
	}
	return strings.TrimRight(chosen.APIURL, "/"), nil
}

func exchangeJurisdictionToken(ctx context.Context, coreURL, loginJWT, audience string, httpClient *http.Client) (string, error) {
	if coreURL == "" {
		return "", errors.New("no entire-core URL configured for jurisdiction token exchange")
	}
	form := url.Values{}
	form.Set("grant_type", httputil.GrantTypeTokenExchange)
	form.Set("subject_token_type", "urn:ietf:params:oauth:token-type:access_token")
	form.Set("subject_token", loginJWT)
	form.Set("requested_token_type", "urn:ietf:params:oauth:token-type:access_token")
	form.Set("audience", audience)
	form.Set("scope", jurisdictionIdentityScope)
	form.Set("client_id", oauthClientID)

	token, _, err := httputil.PostOAuthToken(ctx, httpClient, coreURL, form)
	if err != nil {
		return "", err
	}
	return token, nil
}

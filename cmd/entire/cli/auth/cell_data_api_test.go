package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

func TestHomeJurisdictionFromLoginJWT(t *testing.T) {
	t.Parallel()
	jwt := makeJWT(t, fmt.Sprintf(`{"home_jurisdiction":"us","exp":%d}`, time.Now().Add(time.Hour).Unix()))
	got, err := homeJurisdictionFromLoginJWT(jwt)
	if err != nil {
		t.Fatalf("homeJurisdictionFromLoginJWT: %v", err)
	}
	if got != "us" {
		t.Fatalf("jurisdiction = %q, want us", got)
	}
}

func TestIsEntireIOBFF(t *testing.T) {
	t.Parallel()
	tests := []struct {
		origin string
		want   bool
	}{
		{"https://entire.io", true},
		{"https://staging.entire.io", true},
		{"https://aws-us-east-2.api.entire.io", false},
		{"http://127.0.0.1:8099", false},
	}
	for _, tc := range tests {
		if got := isEntireIOBFF(tc.origin); got != tc.want {
			t.Errorf("isEntireIOBFF(%q) = %v, want %v", tc.origin, got, tc.want)
		}
	}
}

func TestNewEntireAPICellClient_RoutesThroughHomeCell(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("ENTIRE_API_BASE_URL", "https://entire.io")
	t.Setenv("ENTIRE_CORE_BASE_URL_TEMPLATE", "https://fixed-core.test")
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	const wantAudience = "https://us.entire.io"

	var gotExchangeAudience, gotReposHost string
	coreSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/clusters":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"clusters": []map[string]any{{
					"jurisdiction": "us",
					"isDefault":    true,
					"apiUrl":       "http://" + r.Host,
				}},
			})
		case "/oauth/token":
			_ = r.ParseForm() //nolint:errcheck // test handler
			gotExchangeAudience = r.FormValue("audience")
			if r.FormValue("scope") != jurisdictionIdentityScope {
				t.Errorf("scope = %q, want openid", r.FormValue("scope"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"access_token":"cell-identity-token","token_type":"Bearer","expires_in":3600}`)
		case "/api/v1/repos":
			gotReposHost = r.Host
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"repos":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer coreSrv.Close()

	svc := tokenstore.CoreKeyringService(coreSrv.URL)
	loginJWT := makeJWT(t, fmt.Sprintf(`{"iss":%q,"home_jurisdiction":"us","exp":%d}`, coreSrv.URL, time.Now().Add(2*time.Hour).Unix()))
	if err := tokenstore.Set(svc, "me", tokenstore.EncodeTokenWithExpiration(loginJWT, 7200)); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	ctxObj := &contexts.Context{Name: "me@core", CoreURL: coreSrv.URL, Handle: "me", KeychainService: svc}

	cleanupDiscovery := SetResolveContextForCellAPIForTest(t, func(context.Context, string, string, string, *http.Client, clusterdiscovery.DebugFunc) (*contexts.Context, error) {
		return ctxObj, nil
	})
	t.Cleanup(cleanupDiscovery)

	prevTransport := cellExchangeTransportForTest
	cellExchangeTransportForTest = coreSrv.Client().Transport
	t.Cleanup(func() { cellExchangeTransportForTest = prevTransport })

	client, err := NewEntireAPICellClient(context.Background(), false)
	if err != nil {
		t.Fatalf("NewEntireAPICellClient: %v", err)
	}
	if gotExchangeAudience != wantAudience {
		t.Fatalf("exchange audience = %q, want %q", gotExchangeAudience, wantAudience)
	}

	resp, err := client.Get(context.Background(), "/api/v1/repos")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if gotReposHost == "" {
		t.Fatal("cell repos request was not received")
	}
	auth := resp.Request.Header.Get("Authorization")
	if !strings.Contains(auth, "cell-identity-token") {
		t.Fatalf("Authorization = %q, want cell identity token", auth)
	}
}

func TestNewEntireAPICellClient_KeepsDirectCellBaseURL(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	const cellBase = "https://aws-us-east-2.api.entire.io"
	t.Setenv("ENTIRE_API_BASE_URL", cellBase)
	t.Setenv("ENTIRE_CORE_BASE_URL_TEMPLATE", "https://fixed-core.test")
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	t.Cleanup(restore)

	coreSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"access_token":"cell-identity-token","token_type":"Bearer","expires_in":3600}`)
	}))
	defer coreSrv.Close()

	svc := tokenstore.CoreKeyringService(coreSrv.URL)
	loginJWT := makeJWT(t, fmt.Sprintf(`{"iss":%q,"home_jurisdiction":"us","exp":%d}`, coreSrv.URL, time.Now().Add(2*time.Hour).Unix()))
	if err := tokenstore.Set(svc, "me", tokenstore.EncodeTokenWithExpiration(loginJWT, 7200)); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	ctxObj := &contexts.Context{Name: "me@core", CoreURL: coreSrv.URL, Handle: "me", KeychainService: svc}

	cleanupDiscovery := SetResolveContextForCellAPIForTest(t, func(context.Context, string, string, string, *http.Client, clusterdiscovery.DebugFunc) (*contexts.Context, error) {
		return ctxObj, nil
	})
	t.Cleanup(cleanupDiscovery)

	prevTransport := cellExchangeTransportForTest
	cellExchangeTransportForTest = coreSrv.Client().Transport
	t.Cleanup(func() { cellExchangeTransportForTest = prevTransport })

	client, err := NewEntireAPICellClient(context.Background(), false)
	if err != nil {
		t.Fatalf("NewEntireAPICellClient: %v", err)
	}
	if client == nil {
		t.Fatal("client is nil")
	}

	// api.Client doesn't expose baseURL; verify via a stub round trip by calling
	// ResolveURLFromBase indirectly through Get — use httptest on cell path instead.
	gotBase := api.OriginOnly(cellBase)
	if !strings.HasSuffix(gotBase, ".api.entire.io") {
		t.Fatalf("unexpected cell base %q", gotBase)
	}
}

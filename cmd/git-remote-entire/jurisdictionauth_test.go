package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

// fakeLoginJWT builds an unsigned JWT whose payload carries the given
// home_jurisdiction claim — enough for the client-side routing decisions,
// which never verify the signature.
func fakeLoginJWT(t *testing.T, homeJurisdiction string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"home_jurisdiction": homeJurisdiction})
	if err != nil {
		t.Fatal(err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func staticLogin(jwt string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return jwt, nil }
}

func TestExchangeCore(t *testing.T) {
	t.Parallel()
	const auCore = "https://au.auth.example.io"

	t.Run("same jurisdiction uses home core", func(t *testing.T) {
		t.Parallel()
		s := newJurisdictionTokenSource(auCore, "https://au.example.io", auCore, "h", nil, nil)
		core, err := s.exchangeCore(fakeLoginJWT(t, "au"))
		if err != nil {
			t.Fatal(err)
		}
		if core != auCore {
			t.Fatalf("core = %q, want home core", core)
		}
	})

	t.Run("home core trailing slash is trimmed", func(t *testing.T) {
		t.Parallel()
		s := newJurisdictionTokenSource(auCore+"/", "https://au.example.io", "", "h", nil, nil)
		core, err := s.exchangeCore(fakeLoginJWT(t, "au"))
		if err != nil {
			t.Fatal(err)
		}
		if core != auCore {
			t.Fatalf("core = %q, want trimmed home core", core)
		}
	})

	t.Run("home jurisdiction claim casing is folded", func(t *testing.T) {
		t.Parallel()
		s := newJurisdictionTokenSource(auCore, "https://au.example.io", "", "h", nil, nil)
		core, err := s.exchangeCore(fakeLoginJWT(t, "AU"))
		if err != nil {
			t.Fatal(err)
		}
		if core != auCore {
			t.Fatalf("core = %q, want home core despite mixed-case claim", core)
		}
	})

	t.Run("cross jurisdiction uses advertised jurisdiction core", func(t *testing.T) {
		t.Parallel()
		s := newJurisdictionTokenSource("https://eu.auth.example.io", "https://au.example.io", auCore+"/", "h", nil, nil)
		core, err := s.exchangeCore(fakeLoginJWT(t, "eu"))
		if err != nil {
			t.Fatal(err)
		}
		if core != auCore {
			t.Fatalf("core = %q, want advertised au core (trimmed)", core)
		}
	})

	t.Run("cross jurisdiction without advertised core is refused", func(t *testing.T) {
		t.Parallel()
		s := newJurisdictionTokenSource("https://eu.auth.example.io", "https://au.example.io", "", "h", nil, nil)
		if _, err := s.exchangeCore(fakeLoginJWT(t, "eu")); err == nil {
			t.Fatal("expected error when no jurisdiction core is advertised")
		}
	})

	t.Run("non-https advertised core is refused", func(t *testing.T) {
		t.Parallel()
		s := newJurisdictionTokenSource("https://eu.auth.example.io", "https://au.example.io", "http://au.auth.example.io", "h", nil, nil)
		if _, err := s.exchangeCore(fakeLoginJWT(t, "eu")); err == nil {
			t.Fatal("expected error for plaintext advertised core")
		}
	})
}

func TestJurisdictionToken_MintsPersistsAndReuses(t *testing.T) {
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	defer restore()

	mints := 0
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mints++
		if got := r.FormValue("scope"); got != "openid" {
			t.Errorf("scope = %q, want openid", got)
		}
		if got := r.FormValue("audience"); got != "https://eu.example.io" {
			t.Errorf("audience = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"juri-jwt","token_type":"Bearer","expires_in":7200}`)) //nolint:errcheck // test
	}))
	defer core.Close()

	// Same-jurisdiction routing: home_jurisdiction "eu" matches the audience
	// label, so the exchange goes to homeCoreURL — the fake core.
	login := staticLogin(fakeLoginJWT(t, "eu"))
	s := newJurisdictionTokenSource(core.URL, "https://eu.example.io", "", "toothbrush", login, core.Client())

	token, err := s.Token(context.Background(), "/et/x/y", "pull")
	if err != nil {
		t.Fatal(err)
	}
	if token != "juri-jwt" {
		t.Fatalf("token = %q", token)
	}
	if mints != 1 {
		t.Fatalf("mints = %d, want 1", mints)
	}

	// Same source: memoized, no second exchange.
	if _, err := s.Token(context.Background(), "/et/x/y", "push"); err != nil {
		t.Fatal(err)
	}
	if mints != 1 {
		t.Fatalf("mints after memo hit = %d, want 1", mints)
	}

	// Fresh source (new process): served from the persisted keychain entry.
	s2 := newJurisdictionTokenSource(core.URL, "https://eu.example.io", "", "toothbrush", login, core.Client())
	token2, err := s2.Token(context.Background(), "/et/x/y", "pull")
	if err != nil {
		t.Fatal(err)
	}
	if token2 != "juri-jwt" {
		t.Fatalf("token2 = %q", token2)
	}
	if mints != 1 {
		t.Fatalf("mints after keychain hit = %d, want 1", mints)
	}

	// A different handle must not share the token.
	s3 := newJurisdictionTokenSource(core.URL, "https://eu.example.io", "", "other-account", login, core.Client())
	if _, err := s3.Token(context.Background(), "/et/x/y", "pull"); err != nil {
		t.Fatal(err)
	}
	if mints != 2 {
		t.Fatalf("mints for different handle = %d, want 2", mints)
	}
}

func TestJurisdictionToken_InvalidateDropsMemoAndKeychain(t *testing.T) {
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	defer restore()

	mints := 0
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mints++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"juri-jwt","token_type":"Bearer","expires_in":7200}`)) //nolint:errcheck // test
	}))
	defer core.Close()

	login := staticLogin(fakeLoginJWT(t, "eu"))
	s := newJurisdictionTokenSource(core.URL, "https://eu.example.io", "", "toothbrush", login, core.Client())
	if _, err := s.Token(context.Background(), "/et/x/y", "pull"); err != nil {
		t.Fatal(err)
	}
	if mints != 1 {
		t.Fatalf("mints = %d, want 1", mints)
	}

	s.Invalidate()

	// Memo gone: this source re-mints.
	if _, err := s.Token(context.Background(), "/et/x/y", "pull"); err != nil {
		t.Fatal(err)
	}
	if mints != 2 {
		t.Fatalf("mints after invalidate = %d, want 2", mints)
	}

	// Keychain entry gone too: a fresh source (next process) also re-mints
	// rather than reading a stale persisted copy. (The re-mint above stored
	// a new entry, so invalidate again to observe the delete.)
	s.Invalidate()
	s2 := newJurisdictionTokenSource(core.URL, "https://eu.example.io", "", "toothbrush", login, core.Client())
	if _, err := s2.Token(context.Background(), "/et/x/y", "pull"); err != nil {
		t.Fatal(err)
	}
	if mints != 3 {
		t.Fatalf("mints for fresh source after invalidate = %d, want 3", mints)
	}
}

func TestJurisdictionToken_EmptyPersistedTokenRemints(t *testing.T) {
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	defer restore()

	// A corrupted entry: valid future timestamp, empty token. Must not be
	// served as a bare "Bearer " header.
	service := jurisdictionKeyringService("https://eu.example.io")
	corrupted := tokenstore.TokenExpirationSeparator +
		strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	if err := tokenstore.Set(service, "toothbrush", corrupted); err != nil {
		t.Fatal(err)
	}

	mints := 0
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mints++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh-jwt","token_type":"Bearer","expires_in":7200}`)) //nolint:errcheck // test
	}))
	defer core.Close()

	s := newJurisdictionTokenSource(core.URL, "https://eu.example.io", "", "toothbrush", staticLogin(fakeLoginJWT(t, "eu")), core.Client())
	token, err := s.Token(context.Background(), "/et/x/y", "pull")
	if err != nil {
		t.Fatal(err)
	}
	if token != "fresh-jwt" || mints != 1 {
		t.Fatalf("token = %q mints = %d, want fresh mint over corrupted entry", token, mints)
	}
}

func TestJurisdictionToken_ExpiredPersistedTokenRemints(t *testing.T) {
	restore := tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json"))
	defer restore()

	// Seed a token 1 minute from expiry — inside the 5m refresh buffer.
	service := jurisdictionKeyringService("https://eu.example.io")
	expiring := "stale-jwt" + tokenstore.TokenExpirationSeparator +
		strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)
	if err := tokenstore.Set(service, "toothbrush", expiring); err != nil {
		t.Fatal(err)
	}

	mints := 0
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mints++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh-jwt","token_type":"Bearer","expires_in":7200}`)) //nolint:errcheck // test
	}))
	defer core.Close()

	s := newJurisdictionTokenSource(core.URL, "https://eu.example.io", "", "toothbrush", staticLogin(fakeLoginJWT(t, "eu")), core.Client())
	token, err := s.Token(context.Background(), "/et/x/y", "pull")
	if err != nil {
		t.Fatal(err)
	}
	if token != "fresh-jwt" || mints != 1 {
		t.Fatalf("token = %q mints = %d, want fresh mint", token, mints)
	}
}

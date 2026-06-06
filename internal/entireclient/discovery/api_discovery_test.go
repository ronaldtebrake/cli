package discovery

import (
	"testing"
	"time"
)

const testCoreURL = "https://core.example"

func TestAPIDiscovery_RoundTripFresh(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := ModifyAPIDiscovery(dir, func(c APIDiscoveryCache) error {
		c.Set("partial.to", APIDiscoveryEntry{
			Issuer:         "https://us.auth.partial.to",
			TrustedIssuers: []string{"https://us.auth.partial.to", "https://eu.auth.partial.to"},
			Audience:       "https://partial.to",
		})
		return nil
	}); err != nil {
		t.Fatalf("ModifyAPIDiscovery: %v", err)
	}

	cache, err := LoadAPIDiscovery(dir)
	if err != nil {
		t.Fatalf("LoadAPIDiscovery: %v", err)
	}
	entry, fresh, ok := cache.Get("partial.to")
	if !ok {
		t.Fatal("expected entry to exist")
	}
	if !fresh {
		t.Fatal("expected freshly-set entry to be fresh")
	}
	if entry.Audience != "https://partial.to" {
		t.Fatalf("audience = %q, want https://partial.to", entry.Audience)
	}
	if len(entry.TrustedIssuers) != 2 {
		t.Fatalf("unexpected trusted issuers: %v", entry.TrustedIssuers)
	}
}

func TestAPIDiscovery_Miss(t *testing.T) {
	t.Parallel()
	cache, err := LoadAPIDiscovery(t.TempDir())
	if err != nil {
		t.Fatalf("LoadAPIDiscovery: %v", err)
	}
	if _, _, ok := cache.Get("unknown.example"); ok {
		t.Fatal("expected miss for unknown host")
	}
}

func TestAPIDiscovery_IncompleteEntryTreatedAsAbsent(t *testing.T) {
	t.Parallel()
	cache := make(APIDiscoveryCache)
	// Trusted issuers but no audience — a half-written document must not be
	// trusted, since the CLI exchanges for that audience.
	cache["x.example"] = &APIDiscoveryEntry{
		TrustedIssuers: []string{testCoreURL},
		FetchedAt:      time.Now(),
	}
	if _, _, ok := cache.Get("x.example"); ok {
		t.Fatal("entry without an audience should report ok=false")
	}
}

func TestAPIDiscovery_StaleEntryStillReturned(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := ModifyAPIDiscovery(dir, func(c APIDiscoveryCache) error {
		c["old.example"] = &APIDiscoveryEntry{
			Issuer:         testCoreURL,
			TrustedIssuers: []string{testCoreURL},
			Audience:       "https://old.example",
			FetchedAt:      time.Now().Add(-APIDiscoveryTTL - time.Hour),
		}
		return nil
	}); err != nil {
		t.Fatalf("ModifyAPIDiscovery: %v", err)
	}

	cache, err := LoadAPIDiscovery(dir)
	if err != nil {
		t.Fatalf("LoadAPIDiscovery: %v", err)
	}
	entry, fresh, ok := cache.Get("old.example")
	if !ok {
		t.Fatal("stale entry should still report ok=true so callers can fall back to it")
	}
	if fresh {
		t.Fatal("entry older than the TTL should report fresh=false")
	}
	if entry.Audience != "https://old.example" {
		t.Fatalf("unexpected stale audience: %q", entry.Audience)
	}
}

func TestAPIDiscovery_SetCopiesSlice(t *testing.T) {
	t.Parallel()
	cache := make(APIDiscoveryCache)
	issuers := []string{testCoreURL}
	cache.Set("c.example", APIDiscoveryEntry{
		TrustedIssuers: issuers,
		Audience:       "https://c.example",
	})
	issuers[0] = "https://evil.example" // mutate caller's slice after Set

	got, _, ok := cache.Get("c.example")
	if !ok {
		t.Fatal("expected entry")
	}
	if got.TrustedIssuers[0] != testCoreURL {
		t.Fatalf("Set did not copy the slice; cache corrupted to %v", got.TrustedIssuers)
	}
}

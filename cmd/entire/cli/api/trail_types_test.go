package api

import "testing"

func TestTrailResourceToMetadataUsesID(t *testing.T) {
	t.Parallel()

	metadata := (&TrailResource{ID: "trail-db-id", Branch: "feature/x", Phase: "has_code"}).ToMetadata()
	if got := metadata.TrailID.String(); got != "trail-db-id" {
		t.Fatalf("metadata TrailID = %q, want stable API id", got)
	}
	if metadata.Phase != "has_code" {
		t.Fatalf("metadata Phase = %q, want has_code", metadata.Phase)
	}
}

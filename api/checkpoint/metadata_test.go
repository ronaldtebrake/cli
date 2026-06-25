package checkpoint

import (
	"encoding/json"
	"testing"
)

func TestProvenanceRoundTrips(t *testing.T) {
	t.Parallel()
	p := &Provenance{
		Source:         "claude-code",
		TranscriptPath: "/home/u/.claude/projects/x/abc.jsonl",
		SessionID:      "abc",
		TurnUUID:       "uuid-1",
		ParentUUID:     "uuid-0",
		LineStart:      3,
		LineEnd:        9,
		ContentHash:    "sha256:deadbeef",
		ImportVersion:  1,
	}
	b, err := json.Marshal(Metadata{Kind: "imported", Provenance: p})
	if err != nil {
		t.Fatal(err)
	}
	var got Metadata
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Provenance == nil || got.Provenance.TurnUUID != "uuid-1" || got.Kind != "imported" {
		t.Fatalf("round-trip lost provenance/kind: %+v", got)
	}
}

func TestImportedFlagsOnSummaryAndInfo(t *testing.T) {
	t.Parallel()
	if !(CheckpointSummary{Imported: true}).Imported {
		t.Fatal("CheckpointSummary.Imported not settable")
	}
	if !(CheckpointInfo{Imported: true}).Imported {
		t.Fatal("CheckpointInfo.Imported not settable")
	}
}

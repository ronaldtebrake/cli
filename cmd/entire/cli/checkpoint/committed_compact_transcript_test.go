package checkpoint

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/redact"

	// Registers the Claude Code agent so compactAgentName resolves the
	// "claude-code" slug instead of falling back to the raw agent type.
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
)

// claudeStyleTranscript returns a Claude Code-format JSONL transcript with two
// user/assistant exchanges (4 lines total).
func claudeStyleTranscript() []byte {
	lines := []string{
		`{"type":"user","uuid":"u1","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"hello one"}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-01-01T00:00:01Z","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"reply one"}],"usage":{"input_tokens":5,"output_tokens":7}}}`,
		`{"type":"user","uuid":"u2","timestamp":"2026-01-01T00:00:02Z","message":{"role":"user","content":"hello two"}}`,
		`{"type":"assistant","uuid":"a2","timestamp":"2026-01-01T00:00:03Z","message":{"id":"msg_2","role":"assistant","content":[{"type":"text","text":"reply two"}],"usage":{"input_tokens":6,"output_tokens":8}}}`,
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// readBranchFile reads a file from the committed checkpoints branch tree.
// Returns ("", false) when the file does not exist.
func readBranchFile(t *testing.T, store *GitStore, path string) (string, bool) {
	t.Helper()
	tree, err := store.getSessionsBranchTree()
	if err != nil {
		t.Fatalf("getSessionsBranchTree() error = %v", err)
	}
	file, err := tree.File(path)
	if err != nil {
		return "", false
	}
	content, err := file.Contents()
	if err != nil {
		t.Fatalf("Contents(%s) error = %v", path, err)
	}
	return content, true
}

// compactTranscriptLine is the subset of the compact transcript line format
// asserted in these tests.
type compactTranscriptLine struct {
	V       int             `json:"v"`
	Agent   string          `json:"agent"`
	Type    string          `json:"type"`
	Content json.RawMessage `json:"content"`
}

func parseCompactLines(t *testing.T, content string) []compactTranscriptLine {
	t.Helper()
	var lines []compactTranscriptLine
	for _, raw := range strings.Split(strings.TrimSpace(content), "\n") {
		var line compactTranscriptLine
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("compact transcript line is not valid JSON: %v\nline: %s", err, raw)
		}
		lines = append(lines, line)
	}
	return lines
}

func TestWriteCommitted_WritesCompactTranscript(t *testing.T) {
	t.Parallel()
	repo, _ := setupTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("a1b2c3d4e5f6")

	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(claudeStyleTranscript()),
		Prompts:      []string{"hello one"},
		Agent:        agent.AgentTypeClaudeCode,
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	sessionPath := cpID.Path() + "/0/"

	// full.jsonl is still written for CLI read paths.
	if _, ok := readBranchFile(t, store, sessionPath+paths.TranscriptFileName); !ok {
		t.Error("full.jsonl missing from checkpoint tree")
	}

	// transcript.jsonl holds the compact format.
	compactContent, ok := readBranchFile(t, store, sessionPath+paths.CompactTranscriptFileName)
	if !ok {
		t.Fatal("transcript.jsonl missing from checkpoint tree")
	}
	lines := parseCompactLines(t, compactContent)
	if len(lines) != 4 {
		t.Fatalf("compact transcript line count = %d, want 4\ncontent: %s", len(lines), compactContent)
	}
	for i, line := range lines {
		if line.V != 1 {
			t.Errorf("line %d: v = %d, want 1", i, line.V)
		}
		if line.Agent != "claude-code" {
			t.Errorf("line %d: agent = %q, want %q", i, line.Agent, "claude-code")
		}
	}
	if lines[0].Type != "user" || lines[1].Type != "assistant" {
		t.Errorf("unexpected line types: %q, %q", lines[0].Type, lines[1].Type)
	}
	if !strings.Contains(compactContent, "reply two") {
		t.Error("compact transcript missing assistant content")
	}

	// Root metadata.json points at the compact transcript.
	summary := readSummaryFromBranch(t, repo, cpID)
	if len(summary.Sessions) != 1 {
		t.Fatalf("session count = %d, want 1", len(summary.Sessions))
	}
	wantTranscript := "/" + sessionPath + paths.CompactTranscriptFileName
	if summary.Sessions[0].Transcript != wantTranscript {
		t.Errorf("sessions[0].transcript = %q, want %q", summary.Sessions[0].Transcript, wantTranscript)
	}
	wantHash := "/" + sessionPath + paths.ContentHashFileName
	if summary.Sessions[0].ContentHash != wantHash {
		t.Errorf("sessions[0].content_hash = %q, want %q", summary.Sessions[0].ContentHash, wantHash)
	}
}

func TestWriteCommitted_CompactTranscriptScopedToCheckpointStart(t *testing.T) {
	t.Parallel()
	repo, _ := setupTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("b2c3d4e5f6a1")

	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID:              cpID,
		SessionID:                 "session-001",
		Strategy:                  "manual-commit",
		Transcript:                redact.AlreadyRedacted(claudeStyleTranscript()),
		Agent:                     agent.AgentTypeClaudeCode,
		CheckpointTranscriptStart: 2,
		AuthorName:                "Test",
		AuthorEmail:               "test@test.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	compactContent, ok := readBranchFile(t, store, cpID.Path()+"/0/"+paths.CompactTranscriptFileName)
	if !ok {
		t.Fatal("transcript.jsonl missing from checkpoint tree")
	}
	if strings.Contains(compactContent, "hello one") || strings.Contains(compactContent, "reply one") {
		t.Errorf("compact transcript contains content before checkpoint start:\n%s", compactContent)
	}
	if !strings.Contains(compactContent, "hello two") || !strings.Contains(compactContent, "reply two") {
		t.Errorf("compact transcript missing checkpoint-scoped content:\n%s", compactContent)
	}
}

func TestWriteCommitted_NonCompactableTranscriptPointsAtFull(t *testing.T) {
	t.Parallel()
	repo, _ := setupTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("c3d4e5f6a1b2")

	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("not json at all\nstill not json\n")),
		Agent:        agent.AgentTypeClaudeCode,
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	sessionPath := cpID.Path() + "/0/"
	if _, ok := readBranchFile(t, store, sessionPath+paths.CompactTranscriptFileName); ok {
		t.Error("transcript.jsonl written for non-compactable transcript")
	}

	summary := readSummaryFromBranch(t, repo, cpID)
	wantTranscript := "/" + sessionPath + paths.TranscriptFileName
	if summary.Sessions[0].Transcript != wantTranscript {
		t.Errorf("sessions[0].transcript = %q, want %q", summary.Sessions[0].Transcript, wantTranscript)
	}
}

func TestUpdateCommitted_RegeneratesCompactTranscript(t *testing.T) {
	t.Parallel()
	repo, _ := setupTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("d4e5f6a1b2c3")

	initial := claudeStyleTranscript()
	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(initial),
		Agent:        agent.AgentTypeClaudeCode,
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() error = %v", err)
	}

	extended := append([]byte{}, initial...)
	extended = append(extended,
		[]byte(`{"type":"user","uuid":"u3","timestamp":"2026-01-01T00:00:04Z","message":{"role":"user","content":"hello three"}}`+"\n")...)
	err = store.UpdateCommitted(context.Background(), UpdateCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Transcript:   redact.AlreadyRedacted(extended),
		Agent:        agent.AgentTypeClaudeCode,
	})
	if err != nil {
		t.Fatalf("UpdateCommitted() error = %v", err)
	}

	compactContent, ok := readBranchFile(t, store, cpID.Path()+"/0/"+paths.CompactTranscriptFileName)
	if !ok {
		t.Fatal("transcript.jsonl missing after UpdateCommitted")
	}
	if !strings.Contains(compactContent, "hello three") {
		t.Errorf("compact transcript not regenerated with new content:\n%s", compactContent)
	}
}

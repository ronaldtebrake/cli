package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestSessionAdopt_CopiesExternalSessionIntoCurrentWorktree(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	sessionID := "test-adopt-session-001"
	transcriptPath := claudeAdoptTranscriptPath(t, sourceRepo, sessionID)
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"user","message":{"role":"user","content":"update target file"},"uuid":"u1"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	lastInteraction := time.Now().Add(-1 * time.Minute)
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:             sessionID,
		AgentType:             agent.AgentTypeClaudeCode,
		StartedAt:             time.Now().Add(-5 * time.Minute),
		LastInteractionTime:   &lastInteraction,
		Phase:                 session.PhaseActive,
		BaseCommit:            testutil.GetHeadHash(t, sourceRepo),
		AttributionBaseCommit: testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:          sourceRepo,
		TranscriptPath:        transcriptPath,
		LastPrompt:            "update target file",
		FilesTouched:          []string{"source-only.txt"},
		TurnCheckpointIDs:     []string{"abc123def456"},
		AttachedManually:      true,
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)

	var out bytes.Buffer
	err := runAdopt(context.Background(), &out, sessionID, adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err != nil {
		t.Fatalf("runAdopt failed: %v", err)
	}

	targetStore, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := targetStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if adopted == nil {
		t.Fatal("expected adopted session state in target repo")
	}
	if adopted.WorktreePath != targetRepo {
		t.Fatalf("WorktreePath = %q, want %q", adopted.WorktreePath, targetRepo)
	}
	if adopted.BaseCommit != testutil.GetHeadHash(t, targetRepo) {
		t.Fatalf("BaseCommit = %q, want target HEAD", adopted.BaseCommit)
	}
	if adopted.TranscriptPath != transcriptPath {
		t.Fatalf("TranscriptPath = %q, want %q", adopted.TranscriptPath, transcriptPath)
	}
	if adopted.AttachedManually {
		t.Fatal("adopted active sessions should not be marked manually attached")
	}
	if len(adopted.FilesTouched) != 1 || adopted.FilesTouched[0] != "feature.txt" {
		t.Fatalf("FilesTouched = %v, want [feature.txt]", adopted.FilesTouched)
	}
	if len(adopted.TurnCheckpointIDs) != 0 {
		t.Fatalf("TurnCheckpointIDs = %v, want empty target-local checkpoint bookkeeping", adopted.TurnCheckpointIDs)
	}
	if !bytes.Contains(out.Bytes(), []byte("Adopted session")) {
		t.Fatalf("output = %q, want adoption confirmation", out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("Review tracked files before committing")) {
		t.Fatalf("output = %q, want tracked-file attribution warning", out.String())
	}
}

func TestSessionAdopt_RejectsUnexpectedSourceTranscriptPath(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	sessionID := "test-adopt-reject-transcript"
	transcriptPath := filepath.Join(t.TempDir(), sessionID+".jsonl")
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	lastInteraction := time.Now().Add(-1 * time.Minute)
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:             sessionID,
		AgentType:             agent.AgentTypeClaudeCode,
		StartedAt:             time.Now().Add(-5 * time.Minute),
		LastInteractionTime:   &lastInteraction,
		Phase:                 session.PhaseActive,
		BaseCommit:            testutil.GetHeadHash(t, sourceRepo),
		AttributionBaseCommit: testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:          sourceRepo,
		TranscriptPath:        transcriptPath,
		LastPrompt:            "update target file",
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)

	var out bytes.Buffer
	err := runAdopt(context.Background(), &out, sessionID, adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err == nil {
		t.Fatal("runAdopt succeeded, want transcript-path refusal")
	}
	if !strings.Contains(err.Error(), "unexpected transcript path") {
		t.Fatalf("runAdopt error = %v, want unexpected transcript path", err)
	}

	targetStore, storeErr := session.NewStateStore(context.Background())
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	adopted, loadErr := targetStore.Load(context.Background(), sessionID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if adopted != nil {
		t.Fatalf("target state was written despite transcript-path refusal: %#v", adopted)
	}
}

func TestSessionAdopt_EnablesPrepareCommitMsgTrailer(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	sessionID := "test-adopt-trailer-001"
	targetRelPath := "src/feature.go"
	targetAbsPath := filepath.Join(targetRepo, targetRelPath)

	transcriptPath := claudeAdoptTranscriptPath(t, sourceRepo, sessionID)
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o750); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"human","message":{"content":"write feature.go"}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"` + targetAbsPath + `","content":"package src\n"}}]}}
`
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-3 * time.Minute)
	if err := os.Chtimes(transcriptPath, stale, stale); err != nil {
		t.Fatal(err)
	}

	lastInteraction := time.Now().Add(-1 * time.Minute)
	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:             sessionID,
		AgentType:             agent.AgentTypeClaudeCode,
		StartedAt:             time.Now().Add(-5 * time.Minute),
		LastInteractionTime:   &lastInteraction,
		Phase:                 session.PhaseActive,
		BaseCommit:            testutil.GetHeadHash(t, sourceRepo),
		AttributionBaseCommit: testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:          sourceRepo,
		TranscriptPath:        transcriptPath,
		LastPrompt:            "write feature.go",
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, targetRelPath, "package src\n")
	testutil.GitAdd(t, targetRepo, targetRelPath)
	t.Chdir(targetRepo)

	var out bytes.Buffer
	err := runAdopt(context.Background(), &out, sessionID, adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err != nil {
		t.Fatalf("runAdopt failed: %v", err)
	}

	commitMsgFile := filepath.Join(targetRepo, "COMMIT_EDITMSG")
	if err := os.WriteFile(commitMsgFile, []byte("add feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := strategy.NewManualCommitStrategy().PrepareCommitMsg(context.Background(), commitMsgFile, ""); err != nil {
		t.Fatalf("PrepareCommitMsg failed: %v", err)
	}

	content, err := os.ReadFile(commitMsgFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "Entire-Checkpoint:") {
		t.Fatalf("commit message = %q, want Entire-Checkpoint trailer", string(content))
	}
}

func TestSessionAdopt_IdleSourceSurvivesPrepareCommitMsgTrailer(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	sessionID := "test-adopt-idle-source"
	targetRelPath := "src/idle.go"
	targetAbsPath := filepath.Join(targetRepo, targetRelPath)
	transcriptPath := claudeAdoptTranscriptPath(t, sourceRepo, sessionID)
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o750); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"human","message":{"content":"write idle.go"}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"` + targetAbsPath + `","content":"package src\n"}}]}}
`
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}

	lastInteraction := time.Now().Add(-1 * time.Minute)
	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:             sessionID,
		AgentType:             agent.AgentTypeClaudeCode,
		StartedAt:             time.Now().Add(-5 * time.Minute),
		LastInteractionTime:   &lastInteraction,
		Phase:                 session.PhaseIdle,
		BaseCommit:            testutil.GetHeadHash(t, sourceRepo),
		AttributionBaseCommit: testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:          sourceRepo,
		TranscriptPath:        transcriptPath,
		LastPrompt:            "write idle.go",
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, targetRelPath, "package src\n")
	testutil.GitAdd(t, targetRepo, targetRelPath)
	t.Chdir(targetRepo)

	var out bytes.Buffer
	err := runAdopt(context.Background(), &out, sessionID, adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err != nil {
		t.Fatalf("runAdopt failed: %v", err)
	}

	targetStore, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := targetStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if adopted == nil {
		t.Fatal("expected adopted session state")
	}
	if adopted.Phase != session.PhaseActive {
		t.Fatalf("Phase = %q, want active so commit hooks do not sweep adopted state", adopted.Phase)
	}
	if adopted.EndedAt != nil {
		t.Fatalf("EndedAt = %v, want nil", adopted.EndedAt)
	}

	commitMsgFile := filepath.Join(targetRepo, "COMMIT_EDITMSG")
	if err := os.WriteFile(commitMsgFile, []byte("add idle feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := strategy.NewManualCommitStrategy().PrepareCommitMsg(context.Background(), commitMsgFile, ""); err != nil {
		t.Fatalf("PrepareCommitMsg failed: %v", err)
	}
	content, err := os.ReadFile(commitMsgFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "Entire-Checkpoint:") {
		t.Fatalf("commit message = %q, want Entire-Checkpoint trailer", string(content))
	}
}

func TestSessionAdopt_RejectsEndedAtSourceSession(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	sessionID := "test-adopt-ended-at"
	endedAt := time.Now().Add(-30 * time.Second)
	lastInteraction := time.Now().Add(-1 * time.Minute)
	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:             sessionID,
		AgentType:             agent.AgentTypeClaudeCode,
		StartedAt:             time.Now().Add(-5 * time.Minute),
		LastInteractionTime:   &lastInteraction,
		EndedAt:               &endedAt,
		Phase:                 session.PhaseIdle,
		BaseCommit:            testutil.GetHeadHash(t, sourceRepo),
		AttributionBaseCommit: testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:          sourceRepo,
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)

	var out bytes.Buffer
	err := runAdopt(context.Background(), &out, sessionID, adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err == nil {
		t.Fatal("runAdopt succeeded, want ended-session refusal")
	}
	if !strings.Contains(err.Error(), "ended or fully condensed") {
		t.Fatalf("runAdopt error = %v, want ended-session refusal", err)
	}

	_, err = selectAdoptSourceSession(context.Background(), sourceStore, sourceRepo, "")
	if err == nil {
		t.Fatal("selectAdoptSourceSession succeeded, want no recent active sessions")
	}
	if !strings.Contains(err.Error(), "no recent active sessions") {
		t.Fatalf("selectAdoptSourceSession error = %v, want no recent active sessions", err)
	}
}

func TestSessionAdopt_ResetsSourceCheckpointWindow(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	sessionID := "test-adopt-reset-window"
	targetRelPath := "src/feature.go"
	targetAbsPath := filepath.Join(targetRepo, targetRelPath)

	transcriptPath := claudeAdoptTranscriptPath(t, sourceRepo, sessionID)
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o750); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"human","message":{"content":"first source prompt"},"uuid":"source-user"}
{"type":"assistant","message":{"content":"source response"},"uuid":"source-assistant"}
{"type":"human","message":{"content":"write target feature"},"uuid":"target-user"}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"` + targetAbsPath + `","content":"package src\n"}}]},"uuid":"target-assistant"}
`
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}

	lastInteraction := time.Now().Add(-1 * time.Minute)
	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:                   sessionID,
		AgentType:                   agent.AgentTypeClaudeCode,
		StartedAt:                   time.Now().Add(-5 * time.Minute),
		LastInteractionTime:         &lastInteraction,
		Phase:                       session.PhaseActive,
		BaseCommit:                  testutil.GetHeadHash(t, sourceRepo),
		AttributionBaseCommit:       testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:                sourceRepo,
		TranscriptPath:              transcriptPath,
		LastPrompt:                  "write target feature",
		StepCount:                   4,
		SessionDurationMs:           120_000,
		SessionTurnCount:            7,
		ContextTokens:               42_000,
		ContextWindowSize:           200_000,
		CheckpointTranscriptStart:   2,
		CheckpointTranscriptSize:    1234,
		CondensedTranscriptLines:    2,
		TranscriptLinesAtStart:      2,
		TranscriptIdentifierAtStart: "source-assistant",
		TurnCheckpointIDs:           []string{"abc123def456"},
		LastCheckpointID:            id.MustCheckpointID("abc123def456"),
		LastCheckpointCommitHash:    "source-commit",
		PromptWindowBase:            3,
		PromptWindowResetPending:    true,
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, targetRelPath, "package src\n")
	testutil.GitAdd(t, targetRepo, targetRelPath)
	t.Chdir(targetRepo)

	var out bytes.Buffer
	err := runAdopt(context.Background(), &out, sessionID, adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err != nil {
		t.Fatalf("runAdopt failed: %v", err)
	}

	targetStore, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := targetStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if adopted == nil {
		t.Fatal("expected adopted session state in target repo")
	}
	if adopted.StepCount != 0 {
		t.Fatalf("StepCount = %d, want 0 for first target checkpoint", adopted.StepCount)
	}
	if adopted.CheckpointTranscriptStart != 0 {
		t.Fatalf("CheckpointTranscriptStart = %d, want 0", adopted.CheckpointTranscriptStart)
	}
	if adopted.CheckpointTranscriptSize != 0 {
		t.Fatalf("CheckpointTranscriptSize = %d, want 0", adopted.CheckpointTranscriptSize)
	}
	if adopted.TranscriptIdentifierAtStart != "" {
		t.Fatalf("TranscriptIdentifierAtStart = %q, want empty", adopted.TranscriptIdentifierAtStart)
	}
	if adopted.SessionDurationMs != 120_000 {
		t.Fatalf("SessionDurationMs = %d, want preserved source duration", adopted.SessionDurationMs)
	}
	if adopted.SessionTurnCount != 7 {
		t.Fatalf("SessionTurnCount = %d, want preserved source turn count", adopted.SessionTurnCount)
	}
	if adopted.ContextTokens != 42_000 {
		t.Fatalf("ContextTokens = %d, want preserved source context tokens", adopted.ContextTokens)
	}
	if adopted.ContextWindowSize != 200_000 {
		t.Fatalf("ContextWindowSize = %d, want preserved source context window size", adopted.ContextWindowSize)
	}
	if adopted.PromptWindowBase != adopted.SessionTurnCount {
		t.Fatalf("PromptWindowBase = %d, want current SessionTurnCount %d", adopted.PromptWindowBase, adopted.SessionTurnCount)
	}
	if adopted.PromptWindowResetPending {
		t.Fatal("PromptWindowResetPending = true, want false for adopted target window")
	}
	if len(adopted.TurnCheckpointIDs) != 0 {
		t.Fatalf("TurnCheckpointIDs = %v, want empty", adopted.TurnCheckpointIDs)
	}
	if !adopted.LastCheckpointID.IsEmpty() {
		t.Fatalf("LastCheckpointID = %s, want empty", adopted.LastCheckpointID.String())
	}
	if adopted.LastCheckpointCommitHash != "" {
		t.Fatalf("LastCheckpointCommitHash = %q, want empty", adopted.LastCheckpointCommitHash)
	}

	commitMsgFile := filepath.Join(targetRepo, "COMMIT_EDITMSG")
	if err := os.WriteFile(commitMsgFile, []byte("add target feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := strategy.NewManualCommitStrategy().PrepareCommitMsg(context.Background(), commitMsgFile, ""); err != nil {
		t.Fatalf("PrepareCommitMsg failed: %v", err)
	}
	content, err := os.ReadFile(commitMsgFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "Entire-Checkpoint:") {
		t.Fatalf("commit message = %q, want Entire-Checkpoint trailer", string(content))
	}
}

func TestSessionAdopt_ClearsLegacyTranscriptOffsets(t *testing.T) {
	targetRepo := setupAdoptRepo(t)
	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)

	adopted, _, err := buildAdoptedSessionState(context.Background(), &session.State{
		SessionID:                 "test-adopt-legacy-offsets",
		AgentType:                 agent.AgentTypeClaudeCode,
		StartedAt:                 time.Now().Add(-5 * time.Minute),
		Phase:                     session.PhaseActive,
		BaseCommit:                "source-head",
		WorktreePath:              "/source/repo",
		CheckpointTranscriptStart: 9,
		CondensedTranscriptLines:  9,
		TranscriptLinesAtStart:    9,
	})
	if err != nil {
		t.Fatalf("buildAdoptedSessionState failed: %v", err)
	}
	if adopted.CheckpointTranscriptStart != 0 {
		t.Fatalf("CheckpointTranscriptStart = %d, want 0", adopted.CheckpointTranscriptStart)
	}

	encoded, err := json.Marshal(adopted)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("condensed_transcript_lines")) {
		t.Fatalf("adopted state JSON contains condensed_transcript_lines: %s", encoded)
	}
	if bytes.Contains(encoded, []byte("transcript_lines_at_start")) {
		t.Fatalf("adopted state JSON contains transcript_lines_at_start: %s", encoded)
	}
}

func TestSessionAdopt_CloneSourceStateDoesNotShareMutableFields(t *testing.T) {
	lastInteraction := time.Now().Add(-1 * time.Minute)
	endedAt := time.Now()
	source := &session.State{
		SessionID:             "test-adopt-deep-copy",
		StartedAt:             time.Now().Add(-5 * time.Minute),
		EndedAt:               &endedAt,
		LastInteractionTime:   &lastInteraction,
		ReviewSkills:          []string{"/review"},
		TurnCheckpointIDs:     []string{"source-checkpoint"},
		UntrackedFilesAtStart: []string{"untracked.txt"},
		FilesTouched:          []string{"source.txt"},
		TokenUsage: &agent.TokenUsage{
			InputTokens: 1,
			SubagentTokens: &agent.TokenUsage{
				OutputTokens: 2,
			},
		},
		SkillEvents: []agent.SkillEvent{
			{
				ID: "skill-event",
				TranscriptAnchor: &agent.SkillEventTranscriptAnchor{
					EntryIDs: []string{"entry-1"},
				},
				Native: map[string]string{"tool": "skill"},
			},
		},
		PromptAttributions: []session.PromptAttribution{
			{
				UserAddedPerFile:   map[string]int{"source.txt": 1},
				UserRemovedPerFile: map[string]int{"source.txt": 2},
			},
		},
		PendingPromptAttribution: &session.PromptAttribution{
			UserAddedPerFile:   map[string]int{"pending.txt": 3},
			UserRemovedPerFile: map[string]int{"pending.txt": 4},
		},
	}

	adopted := cloneAdoptSourceState(source)
	*adopted.EndedAt = endedAt.Add(1 * time.Hour)
	*adopted.LastInteractionTime = lastInteraction.Add(1 * time.Hour)
	adopted.ReviewSkills[0] = "/changed"
	adopted.TurnCheckpointIDs[0] = "changed-checkpoint"
	adopted.UntrackedFilesAtStart[0] = "changed-untracked.txt"
	adopted.FilesTouched[0] = "changed-source.txt"
	adopted.TokenUsage.SubagentTokens.OutputTokens = 99
	adopted.SkillEvents[0].TranscriptAnchor.EntryIDs[0] = "changed-entry"
	adopted.SkillEvents[0].Native["tool"] = "changed-skill"
	adopted.PromptAttributions[0].UserAddedPerFile["source.txt"] = 99
	adopted.PromptAttributions[0].UserRemovedPerFile["source.txt"] = 99
	adopted.PendingPromptAttribution.UserAddedPerFile["pending.txt"] = 99
	adopted.PendingPromptAttribution.UserRemovedPerFile["pending.txt"] = 99

	if !source.EndedAt.Equal(endedAt) {
		t.Fatalf("source EndedAt was mutated: %v", source.EndedAt)
	}
	if !source.LastInteractionTime.Equal(lastInteraction) {
		t.Fatalf("source LastInteractionTime was mutated: %v", source.LastInteractionTime)
	}
	if source.ReviewSkills[0] != "/review" {
		t.Fatalf("source ReviewSkills = %v, want unchanged", source.ReviewSkills)
	}
	if source.TurnCheckpointIDs[0] != "source-checkpoint" {
		t.Fatalf("source TurnCheckpointIDs = %v, want unchanged", source.TurnCheckpointIDs)
	}
	if source.UntrackedFilesAtStart[0] != "untracked.txt" {
		t.Fatalf("source UntrackedFilesAtStart = %v, want unchanged", source.UntrackedFilesAtStart)
	}
	if source.FilesTouched[0] != "source.txt" {
		t.Fatalf("source FilesTouched = %v, want unchanged", source.FilesTouched)
	}
	if source.TokenUsage.SubagentTokens.OutputTokens != 2 {
		t.Fatalf("source TokenUsage.SubagentTokens.OutputTokens = %d, want unchanged", source.TokenUsage.SubagentTokens.OutputTokens)
	}
	if source.SkillEvents[0].TranscriptAnchor.EntryIDs[0] != "entry-1" {
		t.Fatalf("source SkillEvents entry IDs = %v, want unchanged", source.SkillEvents[0].TranscriptAnchor.EntryIDs)
	}
	if source.SkillEvents[0].Native["tool"] != "skill" {
		t.Fatalf("source SkillEvents native = %v, want unchanged", source.SkillEvents[0].Native)
	}
	if source.PromptAttributions[0].UserAddedPerFile["source.txt"] != 1 {
		t.Fatalf("source PromptAttributions user added = %v, want unchanged", source.PromptAttributions[0].UserAddedPerFile)
	}
	if source.PromptAttributions[0].UserRemovedPerFile["source.txt"] != 2 {
		t.Fatalf("source PromptAttributions user removed = %v, want unchanged", source.PromptAttributions[0].UserRemovedPerFile)
	}
	if source.PendingPromptAttribution.UserAddedPerFile["pending.txt"] != 3 {
		t.Fatalf("source PendingPromptAttribution user added = %v, want unchanged", source.PendingPromptAttribution.UserAddedPerFile)
	}
	if source.PendingPromptAttribution.UserRemovedPerFile["pending.txt"] != 4 {
		t.Fatalf("source PendingPromptAttribution user removed = %v, want unchanged", source.PendingPromptAttribution.UserRemovedPerFile)
	}
}

func TestSessionAdopt_FromSubdirectoryReadsSourceStore(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	sourceSubdir := filepath.Join(sourceRepo, "nested", "dir")
	if err := os.MkdirAll(sourceSubdir, 0o750); err != nil {
		t.Fatal(err)
	}

	sessionID := "test-adopt-from-subdir"
	lastInteraction := time.Now().Add(-1 * time.Minute)
	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:           sessionID,
		AgentType:           agent.AgentTypeClaudeCode,
		StartedAt:           time.Now().Add(-5 * time.Minute),
		LastInteractionTime: &lastInteraction,
		Phase:               session.PhaseActive,
		BaseCommit:          testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:        sourceRepo,
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)

	var out bytes.Buffer
	err := runAdopt(context.Background(), &out, sessionID, adoptOptions{
		FromWorktree: sourceSubdir,
		Force:        true,
	})
	if err != nil {
		t.Fatalf("runAdopt failed from source subdir: %v", err)
	}
}

func TestSessionAdopt_FiltersSharedSourceStoreByFromWorktree(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	siblingWorktree := filepath.Join(t.TempDir(), "sibling-worktree")
	runAdoptGit(t, sourceRepo, "worktree", "add", siblingWorktree, "-b", "sibling-worktree")
	resolvedSiblingWorktree, err := filepath.EvalSymlinks(siblingWorktree)
	if err != nil {
		t.Fatal(err)
	}
	siblingWorktree = resolvedSiblingWorktree
	t.Cleanup(func() {
		runAdoptGit(t, sourceRepo, "worktree", "remove", siblingWorktree, "--force")
	})
	targetRepo := setupAdoptRepo(t)

	sourceWorktreeID, err := paths.GetWorktreeID(sourceRepo)
	if err != nil {
		t.Fatal(err)
	}
	siblingWorktreeID, err := paths.GetWorktreeID(siblingWorktree)
	if err != nil {
		t.Fatal(err)
	}

	lastInteraction := time.Now().Add(-1 * time.Minute)
	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:           "source-worktree-session",
		AgentType:           agent.AgentTypeClaudeCode,
		StartedAt:           time.Now().Add(-5 * time.Minute),
		LastInteractionTime: &lastInteraction,
		Phase:               session.PhaseActive,
		BaseCommit:          testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:        sourceRepo,
		WorktreeID:          sourceWorktreeID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:           "sibling-worktree-session",
		AgentType:           agent.AgentTypeClaudeCode,
		StartedAt:           time.Now().Add(-5 * time.Minute),
		LastInteractionTime: &lastInteraction,
		Phase:               session.PhaseActive,
		BaseCommit:          testutil.GetHeadHash(t, siblingWorktree),
		WorktreePath:        siblingWorktree,
		WorktreeID:          siblingWorktreeID,
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)

	var out bytes.Buffer
	err = runAdopt(context.Background(), &out, "", adoptOptions{
		FromWorktree: sourceRepo,
	})
	if err != nil {
		t.Fatalf("runAdopt failed: %v", err)
	}

	targetStore, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := targetStore.Load(context.Background(), "source-worktree-session")
	if err != nil {
		t.Fatal(err)
	}
	if adopted == nil {
		t.Fatal("expected source worktree session to be adopted")
	}
	if wrong, err := targetStore.Load(context.Background(), "sibling-worktree-session"); err != nil {
		t.Fatal(err)
	} else if wrong != nil {
		t.Fatalf("adopted sibling worktree session unexpectedly: %#v", wrong)
	}
}

func TestStateStoreForWorktreeIgnoresGitStderrOnSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell script fake git")
	}

	fakeBin := t.TempDir()
	fakeGit := filepath.Join(fakeBin, "git")
	script := `#!/bin/sh
printf 'advice: noisy git warning\n' >&2
printf '%s\n%s\n' "$FAKE_WORKTREE_ROOT" "$FAKE_GIT_COMMON_DIR"
`
	if err := os.WriteFile(fakeGit, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	sourceRoot := filepath.Join(t.TempDir(), "source")
	commonDir := filepath.Join(t.TempDir(), "common.git")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_WORKTREE_ROOT", sourceRoot)
	t.Setenv("FAKE_GIT_COMMON_DIR", commonDir)

	_, gotSourceRoot, gotCommonDir, err := stateStoreForWorktree(context.Background(), ".")
	if err != nil {
		t.Fatalf("stateStoreForWorktree failed: %v", err)
	}
	if gotSourceRoot != sourceRoot {
		t.Fatalf("sourceRoot = %q, want %q", gotSourceRoot, sourceRoot)
	}
	if gotCommonDir != filepath.Clean(commonDir) {
		t.Fatalf("commonDir = %q, want %q", gotCommonDir, filepath.Clean(commonDir))
	}
}

func TestSessionAdopt_SameStoreReloadsSourceStateUnderLock(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetWorktree := filepath.Join(t.TempDir(), "target-worktree")
	runAdoptGit(t, sourceRepo, "worktree", "add", targetWorktree, "-b", "target-worktree")
	resolvedTargetWorktree, err := filepath.EvalSymlinks(targetWorktree)
	if err != nil {
		t.Fatal(err)
	}
	targetWorktree = resolvedTargetWorktree
	t.Cleanup(func() {
		runAdoptGit(t, sourceRepo, "worktree", "remove", targetWorktree, "--force")
	})

	sourceWorktreeID, err := paths.GetWorktreeID(sourceRepo)
	if err != nil {
		t.Fatal(err)
	}
	targetWorktreeID, err := paths.GetWorktreeID(targetWorktree)
	if err != nil {
		t.Fatal(err)
	}

	sessionID := "test-adopt-same-store-reload"
	lastInteraction := time.Now().Add(-1 * time.Minute)
	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:           sessionID,
		AgentType:           agent.AgentTypeClaudeCode,
		StartedAt:           time.Now().Add(-5 * time.Minute),
		LastInteractionTime: &lastInteraction,
		Phase:               session.PhaseActive,
		BaseCommit:          testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:        sourceRepo,
		WorktreeID:          sourceWorktreeID,
		LastPrompt:          "stale prompt",
		SessionTurnCount:    1,
	}); err != nil {
		t.Fatal(err)
	}
	staleSelected, err := sourceStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:           sessionID,
		AgentType:           agent.AgentTypeClaudeCode,
		StartedAt:           time.Now().Add(-5 * time.Minute),
		LastInteractionTime: &lastInteraction,
		Phase:               session.PhaseActive,
		BaseCommit:          testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:        sourceRepo,
		WorktreeID:          sourceWorktreeID,
		LastPrompt:          "fresh hook prompt",
		SessionTurnCount:    9,
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetWorktree, "feature.txt", "agent change\n")
	testutil.GitAdd(t, targetWorktree, "feature.txt")
	t.Chdir(targetWorktree)

	adopted, _, err := adoptFromSameSessionStore(context.Background(), sourceRepo, staleSelected, adoptOptions{
		Force: true,
	})
	if err != nil {
		t.Fatalf("adoptFromSameSessionStore failed: %v", err)
	}
	if adopted.LastPrompt != "fresh hook prompt" {
		t.Fatalf("adopted LastPrompt = %q, want fresh hook prompt", adopted.LastPrompt)
	}
	if adopted.SessionTurnCount != 9 {
		t.Fatalf("adopted SessionTurnCount = %d, want fresh source value", adopted.SessionTurnCount)
	}

	loaded, err := sourceStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.WorktreePath != targetWorktree {
		t.Fatalf("WorktreePath = %q, want %q", loaded.WorktreePath, targetWorktree)
	}
	if loaded.WorktreeID != targetWorktreeID {
		t.Fatalf("WorktreeID = %q, want %q", loaded.WorktreeID, targetWorktreeID)
	}
	if loaded.LastPrompt != "fresh hook prompt" {
		t.Fatalf("loaded LastPrompt = %q, want fresh hook prompt", loaded.LastPrompt)
	}
	if loaded.SessionTurnCount != 9 {
		t.Fatalf("loaded SessionTurnCount = %d, want fresh source value", loaded.SessionTurnCount)
	}
}

func TestSessionAdopt_MovesSameStoreSessionIntoCurrentWorktree(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetWorktree := filepath.Join(t.TempDir(), "target-worktree")
	runAdoptGit(t, sourceRepo, "worktree", "add", targetWorktree, "-b", "target-worktree")
	resolvedTargetWorktree, err := filepath.EvalSymlinks(targetWorktree)
	if err != nil {
		t.Fatal(err)
	}
	targetWorktree = resolvedTargetWorktree
	t.Cleanup(func() {
		runAdoptGit(t, sourceRepo, "worktree", "remove", targetWorktree, "--force")
	})

	sourceWorktreeID, err := paths.GetWorktreeID(sourceRepo)
	if err != nil {
		t.Fatal(err)
	}
	targetWorktreeID, err := paths.GetWorktreeID(targetWorktree)
	if err != nil {
		t.Fatal(err)
	}

	sessionID := "test-adopt-same-store"
	lastInteraction := time.Now().Add(-1 * time.Minute)
	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:                 sessionID,
		AgentType:                 agent.AgentTypeClaudeCode,
		StartedAt:                 time.Now().Add(-5 * time.Minute),
		LastInteractionTime:       &lastInteraction,
		Phase:                     session.PhaseActive,
		BaseCommit:                testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:              sourceRepo,
		WorktreeID:                sourceWorktreeID,
		StepCount:                 4,
		CheckpointTranscriptStart: 2,
		LastCheckpointID:          id.MustCheckpointID("abc123def456"),
		LastCheckpointCommitHash:  "source-commit",
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetWorktree, "feature.txt", "agent change\n")
	testutil.GitAdd(t, targetWorktree, "feature.txt")
	t.Chdir(targetWorktree)

	var out bytes.Buffer
	err = runAdopt(context.Background(), &out, sessionID, adoptOptions{
		FromWorktree: sourceRepo,
	})
	if err == nil {
		t.Fatal("runAdopt succeeded without --force, want existing same-store state refusal")
	}
	if !strings.Contains(err.Error(), "already tracked in this repo") {
		t.Fatalf("runAdopt error = %v, want existing-state refusal", err)
	}

	loaded, err := sourceStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.WorktreePath != sourceRepo {
		t.Fatalf("WorktreePath changed without --force: %q", loaded.WorktreePath)
	}

	err = runAdopt(context.Background(), &out, sessionID, adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err != nil {
		t.Fatalf("runAdopt failed: %v", err)
	}

	loaded, err = sourceStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.WorktreePath != targetWorktree {
		t.Fatalf("WorktreePath = %q, want %q", loaded.WorktreePath, targetWorktree)
	}
	if loaded.WorktreeID != targetWorktreeID {
		t.Fatalf("WorktreeID = %q, want %q", loaded.WorktreeID, targetWorktreeID)
	}
	if loaded.BaseCommit != testutil.GetHeadHash(t, targetWorktree) {
		t.Fatalf("BaseCommit = %q, want target HEAD", loaded.BaseCommit)
	}
	if loaded.StepCount != 0 {
		t.Fatalf("StepCount = %d, want reset target-local checkpoint state", loaded.StepCount)
	}
	if loaded.CheckpointTranscriptStart != 0 {
		t.Fatalf("CheckpointTranscriptStart = %d, want reset target-local transcript window", loaded.CheckpointTranscriptStart)
	}
	if !loaded.LastCheckpointID.IsEmpty() {
		t.Fatalf("LastCheckpointID = %s, want empty target-local checkpoint ID", loaded.LastCheckpointID.String())
	}
	if loaded.LastCheckpointCommitHash != "" {
		t.Fatalf("LastCheckpointCommitHash = %q, want empty target-local commit hash", loaded.LastCheckpointCommitHash)
	}

	commitMsgFile := filepath.Join(targetWorktree, "COMMIT_EDITMSG")
	if err := os.WriteFile(commitMsgFile, []byte("add same-store feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := strategy.NewManualCommitStrategy().PrepareCommitMsg(context.Background(), commitMsgFile, ""); err != nil {
		t.Fatalf("PrepareCommitMsg failed: %v", err)
	}
	content, err := os.ReadFile(commitMsgFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "Entire-Checkpoint:") {
		t.Fatalf("commit message = %q, want Entire-Checkpoint trailer", string(content))
	}
}

func setupAdoptRepo(t *testing.T) string {
	t.Helper()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "init.txt", "init\n")
	testutil.GitAdd(t, repoDir, "init.txt")
	testutil.GitCommit(t, repoDir, "init")
	enableEntire(t, repoDir)
	realRepoDir, err := filepath.EvalSymlinks(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	return realRepoDir
}

func claudeAdoptTranscriptPath(t *testing.T, sourceRepo, sessionID string) string {
	t.Helper()

	transcriptDir := filepath.Join(sourceRepo, ".claude", "projects", "adopt-test")
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", transcriptDir)
	return filepath.Join(transcriptDir, sessionID+".jsonl")
}

func runAdoptGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
}

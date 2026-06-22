package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestSessionAdopt_CopiesExternalSessionIntoCurrentWorktree(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	sessionID := "test-adopt-session-001"
	transcriptPath := filepath.Join(sourceRepo, ".claude", sessionID+".jsonl")
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

func TestSessionAdopt_EnablesPrepareCommitMsgTrailer(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	sessionID := "test-adopt-trailer-001"
	targetRelPath := "src/feature.go"
	targetAbsPath := filepath.Join(targetRepo, targetRelPath)

	transcriptPath := filepath.Join(sourceRepo, ".claude", sessionID+".jsonl")
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

func TestSessionAdopt_ResetsSourceCheckpointWindow(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	sessionID := "test-adopt-reset-window"
	targetRelPath := "src/feature.go"
	targetAbsPath := filepath.Join(targetRepo, targetRelPath)

	transcriptPath := filepath.Join(sourceRepo, ".claude", sessionID+".jsonl")
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
		CheckpointTranscriptStart:   2,
		CheckpointTranscriptSize:    1234,
		CondensedTranscriptLines:    2,
		TranscriptLinesAtStart:      2,
		TranscriptIdentifierAtStart: "source-assistant",
		TurnCheckpointIDs:           []string{"abc123def456"},
		LastCheckpointID:            id.MustCheckpointID("abc123def456"),
		LastCheckpointCommitHash:    "source-commit",
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

func TestSessionAdopt_RejectsSameGitCommonDir(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetWorktree := filepath.Join(t.TempDir(), "target-worktree")
	runAdoptGit(t, sourceRepo, "worktree", "add", targetWorktree, "-b", "target-worktree")
	t.Cleanup(func() {
		runAdoptGit(t, sourceRepo, "worktree", "remove", targetWorktree, "--force")
	})

	sessionID := "test-adopt-same-common-dir"
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
		StepCount:                 4,
		CheckpointTranscriptStart: 2,
		LastCheckpointID:          id.MustCheckpointID("abc123def456"),
		LastCheckpointCommitHash:  "source-commit",
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetWorktree, "feature.txt", "agent change\n")
	t.Chdir(targetWorktree)

	var out bytes.Buffer
	err := runAdopt(context.Background(), &out, sessionID, adoptOptions{
		FromWorktree: sourceRepo,
		Force:        true,
	})
	if err == nil {
		t.Fatal("runAdopt succeeded, want same-common-dir refusal")
	}
	if !strings.Contains(err.Error(), "same git common dir") {
		t.Fatalf("runAdopt error = %v, want same git common dir refusal", err)
	}

	loaded, err := sourceStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil {
		t.Fatal("expected source session state to remain")
	}
	if loaded.StepCount != 4 {
		t.Fatalf("StepCount = %d, want source state preserved at 4", loaded.StepCount)
	}
	if loaded.CheckpointTranscriptStart != 2 {
		t.Fatalf("CheckpointTranscriptStart = %d, want source state preserved at 2", loaded.CheckpointTranscriptStart)
	}
	if loaded.LastCheckpointID.String() != "abc123def456" {
		t.Fatalf("LastCheckpointID = %s, want source checkpoint preserved", loaded.LastCheckpointID.String())
	}
	if loaded.LastCheckpointCommitHash != "source-commit" {
		t.Fatalf("LastCheckpointCommitHash = %q, want source commit preserved", loaded.LastCheckpointCommitHash)
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

func runAdoptGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
}

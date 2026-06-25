package importclaude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"

	cp "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func initRepoWithCommit(t *testing.T) (*git.Repository, string) {
	t.Helper()
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, repoDir, "f.txt", "x")
	if _, err := wt.Add("f.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	}); err != nil {
		t.Fatal(err)
	}
	return repo, repoDir
}

func writeFixtureSession(t *testing.T, dir, name string) {
	t.Helper()
	content := strings.Join([]string{
		`{"type":"user","uuid":"u1","timestamp":"2026-06-20T00:00:00Z","message":{"role":"user","content":"first"}}`,
		`{"type":"assistant","uuid":"a1","message":{"id":"m1","model":"claude-x","content":[{"type":"text","text":"ok"}],"usage":{"output_tokens":5}}}`,
		`{"type":"user","uuid":"u2","timestamp":"2026-06-20T00:01:00Z","message":{"role":"user","content":"second"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRun_ImportsAndIsIdempotent(t *testing.T) {
	t.Parallel()
	repo, repoDir := initRepoWithCommit(t)
	claudeDir := t.TempDir()
	writeFixtureSession(t, claudeDir, "sess1.jsonl")

	opts := Options{RepoRoot: repoDir, OverridePath: claudeDir, Now: time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)}

	res, err := Run(context.Background(), repo, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnsImported != 2 {
		t.Fatalf("want 2 imported, got %+v", res)
	}

	res2, err := Run(context.Background(), repo, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res2.TurnsImported != 0 || res2.TurnsSkipped != 2 {
		t.Fatalf("re-run not idempotent: %+v", res2)
	}

	stores, err := cp.Open(context.Background(), repo, cp.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	infos, err := stores.Persistent.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("expected 2 imported checkpoints on v1, got %+v", infos)
	}
	for _, in := range infos {
		if !in.Imported {
			t.Fatalf("checkpoint %s missing Imported flag: %+v", in.CheckpointID, in)
		}
	}
}

func TestRun_DryRunWritesNothing(t *testing.T) {
	t.Parallel()
	repo, repoDir := initRepoWithCommit(t)
	claudeDir := t.TempDir()
	writeFixtureSession(t, claudeDir, "sess1.jsonl")

	res, err := Run(context.Background(), repo, Options{
		RepoRoot: repoDir, OverridePath: claudeDir, DryRun: true,
		Now: time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnsImported != 2 {
		t.Fatalf("dry-run should count 2 turns, got %+v", res)
	}

	stores, err := cp.Open(context.Background(), repo, cp.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	infos, err := stores.Persistent.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 0 {
		t.Fatalf("dry-run must not write, got %+v", infos)
	}
}

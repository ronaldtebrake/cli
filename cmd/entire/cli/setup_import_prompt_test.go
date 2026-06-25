package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestPromptImportClaudeContext_SkipsWhenNoTranscripts(t *testing.T) {
	// Not parallel: t.Chdir + t.Setenv.
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	t.Chdir(repoDir)
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", t.TempDir()) // empty dir
	t.Setenv("ENTIRE_TEST_TTY", "0")                        // non-interactive

	var out bytes.Buffer
	if err := promptImportClaudeContext(context.Background(), &out, EnableOptions{}); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("expected silent no-op, got %q", out.String())
	}
}

func TestPromptImportClaudeContext_NonInteractiveDoesNotImport(t *testing.T) {
	// Not parallel: t.Chdir + t.Setenv.
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "f.txt", "x")
	testutil.GitAdd(t, repoDir, "f.txt")
	testutil.GitCommit(t, repoDir, "init")
	t.Chdir(repoDir)

	cdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cdir, "s.jsonl"),
		[]byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":"hi"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", cdir)
	t.Setenv("ENTIRE_TEST_TTY", "0") // non-interactive => must not prompt or import

	var out bytes.Buffer
	if err := promptImportClaudeContext(context.Background(), &out, EnableOptions{}); err != nil {
		t.Fatal(err)
	}

	repo, err := strategy.OpenRepository(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	stores, err := checkpoint.OpenImports(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	infos, err := stores.Persistent.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 0 {
		t.Fatalf("non-interactive import should not have written: %+v", infos)
	}
}

package strategy

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// setupRepoWithCheckpointRefs creates a work repo with two per-checkpoint refs
// pointing at HEAD, plus a fresh bare remote. Returns (workDir, bareDir, refs).
func setupRepoWithCheckpointRefs(t *testing.T) (string, string, []plumbing.ReferenceName) {
	t.Helper()
	ctx := context.Background()

	workDir := t.TempDir()
	testutil.InitRepo(t, workDir)
	testutil.WriteFile(t, workDir, "README.md", "# test")
	testutil.GitAdd(t, workDir, "README.md")
	testutil.GitCommit(t, workDir, "init")

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)

	refs := []plumbing.ReferenceName{
		checkpoint.RefName(id.MustCheckpointID("a1b2c3d4e5f6")),
		checkpoint.RefName(id.MustCheckpointID("b2c3d4e5f6a1")),
	}
	for _, ref := range refs {
		require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(ref, head.Hash())))
	}

	bareDir := t.TempDir()
	initCmd := exec.CommandContext(ctx, "git", "init", "--bare")
	initCmd.Dir = bareDir
	initCmd.Env = testutil.GitIsolatedEnv()
	out, err := initCmd.CombinedOutput()
	require.NoError(t, err, "git init --bare failed: %s", out)

	return workDir, bareDir, refs
}

func TestPartitionLocalRefs(t *testing.T) {
	t.Parallel()
	workDir, _, refs := setupRepoWithCheckpointRefs(t)
	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)

	stale := checkpoint.RefName(id.MustCheckpointID("ffffffffffff"))
	existing, missing := partitionLocalRefs(repo, append([]plumbing.ReferenceName{stale}, refs...))

	assert.ElementsMatch(t, refs, existing, "local refs are pushable")
	assert.Equal(t, []plumbing.ReferenceName{stale}, missing, "absent ref is stale")
}

func TestBatchPushRefs(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)

	require.NoError(t, batchPushRefs(context.Background(), bareDir, refs))

	// All refs now exist on the bare remote.
	lsCmd := exec.CommandContext(context.Background(), "git", "ls-remote", bareDir)
	lsCmd.Env = testutil.GitIsolatedEnv()
	out, err := lsCmd.CombinedOutput()
	require.NoError(t, err, "ls-remote failed: %s", out)
	remoteRefs := string(out)
	for _, ref := range refs {
		assert.Contains(t, remoteRefs, ref.String(), "ref should be present on the remote after batch push")
	}
}

func TestBatchPushRefs_Empty(t *testing.T) {
	t.Parallel()
	// No refs → no git invocation, no error.
	require.NoError(t, batchPushRefs(context.Background(), "unused-target", nil))
}

// TestBatchPushRefs_AllowsFastForward: advancing a checkpoint ref to a descendant
// commit (the normal case) pushes fine without force.
func TestBatchPushRefs_AllowsFastForward(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	ctx := context.Background()

	require.NoError(t, batchPushRefs(ctx, bareDir, refs))

	// Advance refs[0] to a child commit (fast-forward).
	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	testutil.WriteFile(t, workDir, "two.txt", "second")
	testutil.GitAdd(t, workDir, "two.txt")
	testutil.GitCommit(t, workDir, "second")
	head2, err := repo.Head()
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refs[0], head2.Hash())))

	require.NoError(t, batchPushRefs(ctx, bareDir, refs[:1]), "fast-forward update should push without force")
	assert.Equal(t, head2.Hash().String(), remoteRefHash(t, bareDir, refs[0]),
		"remote ref should advance to the descendant commit")
}

// TestBatchPushRefs_RejectsNonFastForward: a divergent (non-descendant) update is
// rejected, and the remote ref is left untouched — the safety property that
// distinguishes this from a force push (we have no server-side ref protection).
func TestBatchPushRefs_RejectsNonFastForward(t *testing.T) {
	workDir, bareDir, refs := setupRepoWithCheckpointRefs(t)
	t.Chdir(workDir)
	ctx := context.Background()

	require.NoError(t, batchPushRefs(ctx, bareDir, refs))
	original := remoteRefHash(t, bareDir, refs[0])

	// Point refs[0] at an orphan commit (no parent) — not a descendant of what was
	// pushed, so the update is non-fast-forward.
	runGit := func(args ...string) string {
		c := exec.CommandContext(ctx, "git", args...)
		c.Dir = workDir
		c.Env = testutil.GitIsolatedEnv()
		out, gitErr := c.CombinedOutput()
		require.NoError(t, gitErr, "git %v failed: %s", args, out)
		return strings.TrimSpace(string(out))
	}
	tree := runGit("rev-parse", "HEAD^{tree}")
	orphan := runGit("commit-tree", tree, "-m", "divergent")
	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refs[0], plumbing.NewHash(orphan))))

	err = batchPushRefs(ctx, bareDir, refs[:1])
	require.Error(t, err, "a non-fast-forward update must be rejected, not force-pushed")
	assert.Equal(t, original, remoteRefHash(t, bareDir, refs[0]),
		"remote ref must be unchanged after a rejected non-fast-forward push")
}

// remoteRefHash returns the object hash a ref points at on the bare remote.
func remoteRefHash(t *testing.T, bareDir string, ref plumbing.ReferenceName) string {
	t.Helper()
	lsCmd := exec.CommandContext(context.Background(), "git", "ls-remote", bareDir, ref.String())
	lsCmd.Env = testutil.GitIsolatedEnv()
	out, err := lsCmd.CombinedOutput()
	require.NoError(t, err, "ls-remote failed: %s", out)
	fields := strings.Fields(strings.TrimSpace(string(out)))
	require.NotEmpty(t, fields, "ref %s not found on remote", ref)
	return fields[0]
}

package strategy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

// fakeOPFForRewrite tags any occurrence of "PERSONABC" as private_person.
// Deterministic + offline; real OPF inference is not needed to exercise
// the rewrite plumbing.
type fakeOPFForRewrite struct{}

func (f *fakeOPFForRewrite) Redact(_ context.Context, text string, _ []string) ([]redact.Span, error) {
	return findSentinelSpans(text), nil
}

func (f *fakeOPFForRewrite) RedactBatch(_ context.Context, inputs []string, _ []string) ([][]redact.Span, error) {
	out := make([][]redact.Span, len(inputs))
	for i, in := range inputs {
		out[i] = findSentinelSpans(in)
	}
	return out, nil
}

func findSentinelSpans(s string) []redact.Span {
	const sentinel = "PERSONABC"
	var spans []redact.Span
	for idx := 0; ; {
		hit := strings.Index(s[idx:], sentinel)
		if hit < 0 {
			break
		}
		start := idx + hit
		end := start + len(sentinel)
		spans = append(spans, redact.Span{Start: start, End: end, Label: "private_person"})
		idx = end
	}
	return spans
}

// fakeRuntimeAlwaysFails trips the OPF circuit breaker on first call.
// Used to test the fail-closed assertion that breaker-trip during
// rewrite aborts before CAS.
type fakeRuntimeAlwaysFails struct{}

func (f *fakeRuntimeAlwaysFails) Redact(_ context.Context, _ string, _ []string) ([]redact.Span, error) {
	return nil, errors.New("simulated OPF runtime failure")
}
func (f *fakeRuntimeAlwaysFails) RedactBatch(_ context.Context, _ []string, _ []string) ([][]redact.Span, error) {
	return nil, errors.New("simulated OPF runtime failure")
}

// testOPFRuntime is the structural interface the redact package's
// ConfigurePrivacyFilterWithRuntime accepts. Mirrors redact.opfRuntime
// (unexported, can't be named directly from this package).
type testOPFRuntime interface {
	Redact(ctx context.Context, text string, categories []string) ([]redact.Span, error)
	RedactBatch(ctx context.Context, inputs []string, categories []string) ([][]redact.Span, error)
}

// configureFakeOPF resets state and wires the given runtime as the
// process-global OPF.
func configureFakeOPF(t *testing.T, rt testOPFRuntime) {
	t.Helper()
	redact.ResetOPFConfigForTest()
	t.Cleanup(redact.ResetOPFConfigForTest)
	redact.ConfigurePrivacyFilterWithRuntime(redact.OPFConfig{
		Enabled:    true,
		Categories: map[string]bool{"private_person": true},
		Command:    "/tmp/test-opf",
	}, rt)
}

// setupV1Repo creates a repo + one v1 checkpoint with "PERSONABC" in
// both the transcript and prompt. Returns the repo and the v1 tip.
func setupV1Repo(t *testing.T) (*git.Repository, plumbing.Hash) {
	t.Helper()
	tempDir := t.TempDir()
	testutil.InitRepo(t, tempDir)
	repo, err := git.PlainOpen(tempDir)
	require.NoError(t, err)

	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, "README.md"), []byte("# Test"), 0o644))
	_, err = wt.Add("README.md")
	require.NoError(t, err)
	_, err = wt.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	require.NoError(t, err)

	store := checkpoint.NewGitStore(repo)
	cpID := id.MustCheckpointID("a1b2c3d4e5f6")
	require.NoError(t, store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "test-session",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"role":"user","content":"Hello, PERSONABC asked"}` + "\n")),
		Prompts:      []string{"Look up PERSONABC"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	return repo, ref.Hash()
}

// makeOrphanCommit writes a single v1 commit (no parents = orphan).
// Used by edge-case tests that need many cheap commits or commits
// with unrelated histories.
func makeOrphanCommit(t *testing.T, repo *git.Repository, treeHash plumbing.Hash, parents []plumbing.Hash, message string) plumbing.Hash {
	t.Helper()
	sig := &object.Signature{Name: "Test", Email: "test@test.com"}
	c := &object.Commit{Author: *sig, Committer: *sig, Message: message, TreeHash: treeHash, ParentHashes: parents}
	obj := repo.Storer.NewEncodedObject()
	require.NoError(t, c.Encode(obj))
	hash, err := repo.Storer.SetEncodedObject(obj)
	require.NoError(t, err)
	return hash
}

// emptyTreeHash writes (or resolves) git's well-known empty tree.
func emptyTreeHash(t *testing.T, repo *git.Repository) plumbing.Hash {
	t.Helper()
	obj := repo.Storer.NewEncodedObject()
	require.NoError(t, (&object.Tree{}).Encode(obj))
	hash, err := repo.Storer.SetEncodedObject(obj)
	require.NoError(t, err)
	return hash
}

// buildOrphanChain builds n linear orphan commits on v1 with the
// empty tree. Returns the tip. Useful for testing bootstrap/limit paths
// where the only thing that matters is commit count.
func buildOrphanChain(t *testing.T, repo *git.Repository, n int) plumbing.Hash {
	t.Helper()
	tree := emptyTreeHash(t, repo)
	var parent, tip plumbing.Hash
	for i := range n {
		var parents []plumbing.Hash
		if !parent.IsZero() {
			parents = []plumbing.Hash{parent}
		}
		tip = makeOrphanCommit(t, repo, tree, parents, fmt.Sprintf("commit %d", i))
		parent = tip
	}
	require.NoError(t, repo.Storer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), tip)))
	return tip
}

// Happy path: a single unpushed unapplied commit gets rewritten, tagged
// applied, and its sentinel-bearing blobs no longer contain the sentinel.
func TestRewriteUnpushedV1WithOPF_HappyPath_RewritesAndTagsApplied(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	repo, originalTip := setupV1Repo(t)

	newTip, err := RewriteUnpushedV1WithOPF(context.Background(), repo, "origin")
	require.NoError(t, err)
	if newTip == originalTip {
		t.Fatalf("rewrite returned same tip %s; expected new tip", newTip.String()[:7])
	}

	newCommit, err := repo.CommitObject(newTip)
	require.NoError(t, err)
	if !trailers.HasOPFApplied(newCommit.Message) {
		t.Errorf("new commit missing Entire-OPF-Applied trailer:\n%s", newCommit.Message)
	}

	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	require.Equal(t, newTip, ref.Hash(), "local v1 ref should point to new tip")

	tree, err := newCommit.Tree()
	require.NoError(t, err)
	require.NoError(t, tree.Files().ForEach(func(f *object.File) error {
		if !strings.HasSuffix(f.Name, ".jsonl") && !strings.HasSuffix(f.Name, ".txt") {
			return nil
		}
		content, err := f.Contents()
		if err != nil {
			return err
		}
		if strings.Contains(content, "PERSONABC") {
			t.Errorf("rewritten %s still contains sentinel 'PERSONABC'", f.Name)
		}
		return nil
	}))
}

// Idempotent re-run: a commit already tagged Entire-OPF-Applied is
// re-parented without re-redacting the tree and without duplicating
// the trailer.
func TestRewriteUnpushedV1WithOPF_SecondRun_IdempotentNoDuplicateTrailer(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	repo, _ := setupV1Repo(t)

	firstTip, err := RewriteUnpushedV1WithOPF(context.Background(), repo, "origin")
	require.NoError(t, err)
	firstCommit, err := repo.CommitObject(firstTip)
	require.NoError(t, err)
	require.True(t, trailers.HasOPFApplied(firstCommit.Message))
	firstTreeHash := firstCommit.TreeHash

	secondTip, err := RewriteUnpushedV1WithOPF(context.Background(), repo, "origin")
	require.NoError(t, err)

	secondCommit, err := repo.CommitObject(secondTip)
	require.NoError(t, err)

	wantTrailer := trailers.OPFAppliedTrailerKey + ": " + trailers.OPFAppliedTrailerValue
	if count := strings.Count(secondCommit.Message, wantTrailer); count != 1 {
		t.Errorf("trailer count = %d, want exactly 1\n%s", count, secondCommit.Message)
	}
	require.Equal(t, firstTreeHash, secondCommit.TreeHash, "applied commit tree should be preserved")
}

// No v1 branch → no-op, no error.
func TestRewriteUnpushedV1WithOPF_NoV1Branch_ReturnsZeroHashNoError(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	tempDir := t.TempDir()
	testutil.InitRepo(t, tempDir)
	repo, err := git.PlainOpen(tempDir)
	require.NoError(t, err)

	tip, err := RewriteUnpushedV1WithOPF(context.Background(), repo, "origin")
	require.NoError(t, err)
	require.True(t, tip.IsZero(), "expected zero hash for missing v1 ref")
}

// Diverged remote: local has commits unreachable from remote. Refusal
// prevents silent rebase of work the remote already rejected.
func TestRewriteUnpushedV1WithOPF_DivergedRemote_ReturnsV1DivergedError(t *testing.T) {
	configureFakeOPF(t, &fakeOPFForRewrite{})
	tempDir := t.TempDir()
	testutil.InitRepo(t, tempDir)
	repo, err := git.PlainOpen(tempDir)
	require.NoError(t, err)

	tree := emptyTreeHash(t, repo)
	localTip := makeOrphanCommit(t, repo, tree, nil, "local only")
	remoteTip := makeOrphanCommit(t, repo, tree, nil, "remote only")
	require.NoError(t, repo.Storer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), localTip)))
	require.NoError(t, repo.Storer.SetReference(
		plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName), remoteTip)))

	_, err = RewriteUnpushedV1WithOPF(context.Background(), repo, "origin")
	var diverged *V1DivergedError
	require.ErrorAs(t, err, &diverged)
	require.Equal(t, localTip, diverged.Local)
	require.Equal(t, remoteTip, diverged.Remote)
}

// Bootstrap cap: a single table-driven test covers both the over-limit
// rejection and the unlimited-override pass paths since they share
// 90% of setup.
func TestRewriteUnpushedV1WithOPF_BootstrapCap(t *testing.T) {
	cases := []struct {
		name      string
		envLimit  string
		commits   int
		wantErr   bool
		wantCount int
		wantLimit int
	}{
		{name: "over_limit_rejected", envLimit: "2", commits: 3, wantErr: true, wantCount: 3, wantLimit: 2},
		{name: "unlimited_allows_any_size", envLimit: "unlimited", commits: 3, wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configureFakeOPF(t, &fakeOPFForRewrite{})
			t.Setenv("ENTIRE_OPF_BOOTSTRAP_LIMIT", tc.envLimit)

			tempDir := t.TempDir()
			testutil.InitRepo(t, tempDir)
			repo, err := git.PlainOpen(tempDir)
			require.NoError(t, err)
			tip := buildOrphanChain(t, repo, tc.commits)

			newTip, err := RewriteUnpushedV1WithOPF(context.Background(), repo, "origin")
			if !tc.wantErr {
				require.NoError(t, err)
				require.False(t, newTip.IsZero(), "expected new tip on success")
				return
			}
			var tooLarge *BootstrapTooLargeError
			require.ErrorAs(t, err, &tooLarge)
			require.Equal(t, tc.wantCount, tooLarge.Count)
			require.Equal(t, tc.wantLimit, tooLarge.Limit)
			_ = tip // tip is the local v1 tip; on error we don't move the ref but we also don't assert here
		})
	}
}

// Fail-closed regression: when the OPF runtime fails and the breaker
// trips, the rewrite must NOT CAS the ref. Otherwise the new commits
// would carry Entire-OPF-Applied: true while their content is 7-layer
// only, and future pushes would skip them — silently shipping unredacted
// content to the remote.
func TestRewriteUnpushedV1WithOPF_BreakerTrippedMidRewrite_AbortsBeforeCAS(t *testing.T) {
	configureFakeOPF(t, &fakeRuntimeAlwaysFails{})
	repo, originalTip := setupV1Repo(t)

	_, err := RewriteUnpushedV1WithOPF(context.Background(), repo, "origin")
	var runtimeFail *OPFRuntimeFailedError
	require.ErrorAs(t, err, &runtimeFail)
	require.Contains(t, runtimeFail.OPFCommand, "test-opf", "OPFCommand should reflect configured command")

	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	require.Equal(t, originalTip, ref.Hash(), "local v1 ref must not move on OPF failure")
}

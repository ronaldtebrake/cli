package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
)

// newTestRepo creates an isolated repo with a single "init" commit and returns
// its directory, an open handle, and the commit hash.
func newTestRepo(t *testing.T) (string, *git.Repository, plumbing.Hash) {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	return dir, repo, commitFile(t, repo, dir, "f.txt", "init", "init")
}

// commitFile commits content to path; successive calls build a linear chain.
func commitFile(t *testing.T, repo *git.Repository, dir, path, content, msg string) plumbing.Hash {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add(path)
	require.NoError(t, err)
	h, err := wt.Commit(msg, &git.CommitOptions{Author: &object.Signature{Name: "Test", Email: "test@test.com"}})
	require.NoError(t, err)
	return h
}

func setRef(t *testing.T, repo *git.Repository, name plumbing.ReferenceName, hash plumbing.Hash) {
	t.Helper()
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(name, hash)))
}

func v1BranchRef() plumbing.ReferenceName {
	return plumbing.NewBranchReferenceName(paths.MetadataBranchName)
}
func customRef() plumbing.ReferenceName { return plumbing.ReferenceName(paths.MetadataRefName) }
func originV1Ref() plumbing.ReferenceName {
	return plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName)
}

func customRefHash(t *testing.T, repo *git.Repository) (plumbing.Hash, bool) {
	t.Helper()
	ref, err := repo.Reference(customRef(), true)
	if err != nil {
		return plumbing.ZeroHash, false
	}
	return ref.Hash(), true
}

// writeV1Checkpoint writes a committed checkpoint to the v1 branch.
func writeV1Checkpoint(t *testing.T, repo *git.Repository, cpID id.CheckpointID) {
	t.Helper()
	require.NoError(t, NewGitStore(repo).WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("transcript\n")),
		Prompts:      []string{"prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))
}

// enableV11 chdirs into dir and opts into checkpoints v1.1.
func enableV11(t *testing.T, dir string) {
	t.Helper()
	t.Chdir(dir)
	writeSettings(t, dir, `"1.1"`)
}

// writeSettings writes .entire/settings.json (empty version omits the option).
func writeSettings(t *testing.T, dir, version string) {
	t.Helper()
	body := `{"enabled": true}`
	if version != "" {
		body = `{"enabled": true, "strategy_options": {"checkpoints_version": ` + version + `}}`
	}
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".entire", paths.SettingsFileName), []byte(body), 0o644))
}

// blockCustomRefWrite occupies refs/entire with a file so refs/entire/* writes fail.
func blockCustomRefWrite(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "refs", "entire"), []byte("blocked"), 0o644))
}

func TestGitStore_CommittedReadRef(t *testing.T) {
	t.Parallel()
	assert.Equal(t, v1BranchRef(), NewGitStore(nil).CommittedReadRef())
	assert.Equal(t, customRef(), NewGitStoreWithRef(nil, customRef()).CommittedReadRef())
}

func TestSyncV1CustomRefForRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string, repo *git.Repository, init plumbing.Hash) (want plumbing.Hash, exists bool)
	}{
		{"seeds from local v1 when missing", func(t *testing.T, _ string, repo *git.Repository, init plumbing.Hash) (plumbing.Hash, bool) {
			setRef(t, repo, v1BranchRef(), init)
			return init, true
		}},
		{"seeds from origin when local v1 missing", func(t *testing.T, _ string, repo *git.Repository, init plumbing.Hash) (plumbing.Hash, bool) {
			setRef(t, repo, originV1Ref(), init)
			return init, true
		}},
		{"no-op when equal", func(t *testing.T, _ string, repo *git.Repository, init plumbing.Hash) (plumbing.Hash, bool) {
			setRef(t, repo, v1BranchRef(), init)
			setRef(t, repo, customRef(), init)
			return init, true
		}},
		{"advances when ancestor", func(t *testing.T, dir string, repo *git.Repository, init plumbing.Hash) (plumbing.Hash, bool) {
			setRef(t, repo, customRef(), init)
			newHash := commitFile(t, repo, dir, "f2.txt", "more", "second")
			setRef(t, repo, v1BranchRef(), newHash)
			return newHash, true
		}},
		{"leaves non-ancestor ref", func(t *testing.T, dir string, repo *git.Repository, init plumbing.Hash) (plumbing.Hash, bool) {
			ahead := commitFile(t, repo, dir, "f2.txt", "more", "second")
			setRef(t, repo, v1BranchRef(), init) // parent
			setRef(t, repo, customRef(), ahead)  // child, not an ancestor of v1
			return ahead, true
		}},
		{"no v1 tip", func(_ *testing.T, _ string, _ *git.Repository, _ plumbing.Hash) (plumbing.Hash, bool) {
			return plumbing.ZeroHash, false
		}},
		{"write failure leaves ref unset", func(t *testing.T, dir string, repo *git.Repository, init plumbing.Hash) (plumbing.Hash, bool) {
			setRef(t, repo, v1BranchRef(), init)
			blockCustomRefWrite(t, dir)
			return plumbing.ZeroHash, false
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir, repo, init := newTestRepo(t)
			want, exists := tt.setup(t, dir, repo, init)

			syncV1CustomRefForRead(context.Background(), repo)

			got, ok := customRefHash(t, repo)
			require.Equal(t, exists, ok)
			if exists {
				assert.Equal(t, want, got)
			}
		})
	}
}

// Not parallel: uses t.Chdir() so settings.Load resolves the test repo.
func TestNewCommittedReadStore_SelectsRefByVersion(t *testing.T) {
	dir, repo, h := newTestRepo(t)
	setRef(t, repo, v1BranchRef(), h)
	t.Chdir(dir)

	writeSettings(t, dir, "") // v1 only
	assert.Equal(t, v1BranchRef(), NewCommittedReadStore(context.Background(), repo).CommittedReadRef())

	writeSettings(t, dir, `"1.1"`)
	assert.Equal(t, customRef(), NewCommittedReadStore(context.Background(), repo).CommittedReadRef())
}

// v1.1 reads always go through the custom ref (no v1 fallback): a checkpoint is
// found when the ref can be synced to v1, and not found when it can't.
// Not parallel: subtests use t.Chdir().
func TestNewCommittedReadStore_V11Reads(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(t *testing.T, dir string, repo *git.Repository)
		wantFound bool
	}{
		{"reads v1 data via custom ref", func(_ *testing.T, _ string, _ *git.Repository) {}, true},
		{"reads remote-only metadata", func(t *testing.T, _ string, repo *git.Repository) {
			ref, err := repo.Reference(v1BranchRef(), true)
			require.NoError(t, err)
			setRef(t, repo, originV1Ref(), ref.Hash())
			require.NoError(t, repo.Storer.RemoveReference(v1BranchRef()))
		}, true},
		{"sync write fails", func(t *testing.T, dir string, _ *git.Repository) {
			blockCustomRefWrite(t, dir)
		}, false},
		{"custom ref diverges", func(t *testing.T, dir string, repo *git.Repository) {
			setRef(t, repo, customRef(), commitFile(t, repo, dir, "other.txt", "diverged", "diverged"))
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, repo, _ := newTestRepo(t)
			enableV11(t, dir)
			cpID := id.MustCheckpointID("a1b2c3d4e5f6")
			writeV1Checkpoint(t, repo, cpID)
			tt.mutate(t, dir, repo)

			SyncCommittedReadRef(context.Background(), repo)
			store := NewCommittedReadStore(context.Background(), repo)
			require.Equal(t, customRef(), store.CommittedReadRef(), "must read the custom ref, not fall back to v1")

			summary, err := store.ReadCommitted(context.Background(), cpID)
			require.NoError(t, err)
			if tt.wantFound {
				require.NotNil(t, summary)
				assert.Equal(t, cpID, summary.CheckpointID)
			} else {
				assert.Nil(t, summary, "must not fall back to v1")
			}
		})
	}
}

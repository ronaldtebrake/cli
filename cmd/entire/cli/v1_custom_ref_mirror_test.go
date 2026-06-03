package cli

import (
	"errors"
	"os"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

type setReferenceErrorStorer struct {
	storage.Storer

	err error
}

func (s setReferenceErrorStorer) SetReference(*plumbing.Reference) error {
	return s.err
}

// v1CustomRefs returns the v1.1 mirror topology these tests exercise.
func v1CustomRefs() checkpoint.CommittedRefs {
	return checkpoint.ResolveCommittedRefsFromSettings(&settings.EntireSettings{
		StrategyOptions: map[string]any{"checkpoints_version": "1.1"},
	})
}

func setupCustomRefRepo(t *testing.T) *git.Repository {
	t.Helper()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	t.Chdir(tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	require.NoError(t, err)
	return repo
}

func pointV1MetadataBranchAtHead(t *testing.T, repo *git.Repository) plumbing.Hash {
	t.Helper()
	head, err := repo.Head()
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), head.Hash())))
	return head.Hash()
}

func readCustomRefHash(t *testing.T, repo *git.Repository) (plumbing.Hash, bool) {
	t.Helper()
	ref, err := repo.Reference(plumbing.ReferenceName(paths.MetadataRefName), true)
	if err != nil {
		return plumbing.ZeroHash, false
	}
	return ref.Hash(), true
}

// Not parallel: uses t.Chdir().
func TestMirrorToV1CustomRef_CreatesRef(t *testing.T) {
	repo := setupCustomRefRepo(t)
	v1Hash := pointV1MetadataBranchAtHead(t, repo)

	require.NoError(t, mirrorToV1CustomRef(v1CustomRefs(), repo))

	got, ok := readCustomRefHash(t, repo)
	require.True(t, ok, "expected %s to exist", paths.MetadataRefName)
	assert.Equal(t, v1Hash, got)
}

// Not parallel: uses t.Chdir().
func TestMirrorToV1CustomRef_AdvancesExistingRef(t *testing.T) {
	repo := setupCustomRefRepo(t)
	oldHash := pointV1MetadataBranchAtHead(t, repo)
	require.NoError(t, repo.Storer.SetReference(
		plumbing.NewHashReference(plumbing.ReferenceName(paths.MetadataRefName), oldHash)))

	cwd, err := os.Getwd()
	require.NoError(t, err)
	testutil.WriteFile(t, cwd, "f2.txt", "more")
	testutil.GitAdd(t, cwd, "f2.txt")
	testutil.GitCommit(t, cwd, "second")
	newHash := pointV1MetadataBranchAtHead(t, repo)
	require.NotEqual(t, oldHash, newHash)

	require.NoError(t, mirrorToV1CustomRef(v1CustomRefs(), repo))

	got, ok := readCustomRefHash(t, repo)
	require.True(t, ok)
	assert.Equal(t, newHash, got)
}

// Not parallel: uses t.Chdir().
func TestMirrorToV1CustomRef_ReplacesLocallyAheadRef(t *testing.T) {
	repo := setupCustomRefRepo(t)
	v1Hash := pointV1MetadataBranchAtHead(t, repo)

	cwd, err := os.Getwd()
	require.NoError(t, err)
	testutil.WriteFile(t, cwd, "f2.txt", "more")
	testutil.GitAdd(t, cwd, "f2.txt")
	testutil.GitCommit(t, cwd, "second")
	head, err := repo.Head()
	require.NoError(t, err)
	require.NotEqual(t, v1Hash, head.Hash())
	require.NoError(t, repo.Storer.SetReference(
		plumbing.NewHashReference(plumbing.ReferenceName(paths.MetadataRefName), head.Hash())))

	require.NoError(t, mirrorToV1CustomRef(v1CustomRefs(), repo))

	got, ok := readCustomRefHash(t, repo)
	require.True(t, ok)
	assert.Equal(t, v1Hash, got)
}

// Not parallel: uses t.Chdir().
func TestMirrorToV1CustomRef_V1MissingErrors(t *testing.T) {
	repo := setupCustomRefRepo(t) // no v1 metadata branch created

	err := mirrorToV1CustomRef(v1CustomRefs(), repo)
	require.Error(t, err)
	assert.Contains(t, err.Error(), paths.MetadataBranchName)

	_, ok := readCustomRefHash(t, repo)
	assert.False(t, ok, "v1 custom ref must not be created when v1 metadata branch is absent")
}

// Not parallel: uses t.Chdir().
func TestMirrorToV1CustomRef_SetReferenceErrorNamesTarget(t *testing.T) {
	repo := setupCustomRefRepo(t)
	v1Hash := pointV1MetadataBranchAtHead(t, repo)
	storerErr := errors.New("set failed")
	repo.Storer = setReferenceErrorStorer{Storer: repo.Storer, err: storerErr}

	err := mirrorToV1CustomRef(v1CustomRefs(), repo)
	require.ErrorIs(t, err, storerErr)
	assert.Contains(t, err.Error(), paths.MetadataRefName)
	assert.Contains(t, err.Error(), v1Hash.String())
}

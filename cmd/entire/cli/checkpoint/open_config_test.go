package checkpoint

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

const fakeMirrorBackendType = "faketest-mirror"

var fakeMirrorBackendOnce sync.Once

// registerFakeMirrorBackend registers a non-git backend so mirror-selection
// paths can be exercised without the real fsstore. Registration is process-wide
// and idempotent (Register panics on duplicates).
func registerFakeMirrorBackend(t *testing.T) {
	t.Helper()
	fakeMirrorBackendOnce.Do(func() {
		Register(fakeMirrorBackendType, func(_ context.Context, _ OpenEnv, _ json.RawMessage) (PersistentStore, error) {
			return &fakePrimary{}, nil
		})
	})
}

func writeRawSettings(t *testing.T, dir, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".entire", paths.SettingsFileName), []byte(body), 0o644))
}

// Not parallel: uses t.Chdir so settings resolve to the test repo.
func TestOpen_DefaultIsGitPrimaryNoMirrors(t *testing.T) {
	dir, repo, _ := newTestRepo(t)
	t.Chdir(dir)

	stores, err := Open(context.Background(), repo, OpenOptions{})
	require.NoError(t, err)
	_, isGit := stores.Persistent.(*GitStore)
	assert.True(t, isGit, "default persistent store should be the raw git store, not a fan-out wrapper")
}

func TestOpen_RejectsNonGitPrimary(t *testing.T) {
	dir, repo, _ := newTestRepo(t)
	t.Chdir(dir)
	writeRawSettings(t, dir, `{"enabled": true, "checkpoints": {"primary": {"type": "fs"}}}`)

	_, err := Open(context.Background(), repo, OpenOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not supported")
	assert.Contains(t, err.Error(), "primary")
}

func TestOpen_RejectsGitMirror(t *testing.T) {
	dir, repo, _ := newTestRepo(t)
	t.Chdir(dir)
	writeRawSettings(t, dir, `{"enabled": true, "checkpoints": {"primary": {"type": "git"}, "mirrors": [{"type": "git"}]}}`)

	_, err := Open(context.Background(), repo, OpenOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate the primary ref")
}

func TestOpen_BuildsConfiguredMirror(t *testing.T) {
	registerFakeMirrorBackend(t)
	dir, repo, _ := newTestRepo(t)
	t.Chdir(dir)
	writeRawSettings(t, dir, `{"enabled": true, "checkpoints": {"primary": {"type": "git"}, "mirrors": [{"type": "`+fakeMirrorBackendType+`"}]}}`)

	stores, err := Open(context.Background(), repo, OpenOptions{})
	require.NoError(t, err)

	// With a mirror configured the persistent store is the fan-out wrapper, not
	// the raw git store, and it still exposes AuthorReader (git primary has it).
	_, isGit := stores.Persistent.(*GitStore)
	assert.False(t, isGit, "configured mirror should wrap the primary in a fan-out store")
	_, isAuthor := stores.Persistent.(AuthorReader)
	assert.True(t, isAuthor, "fan-out store should preserve the git primary's AuthorReader")
}

func TestOpen_InvalidCheckpointsBlockErrors(t *testing.T) {
	dir, repo, _ := newTestRepo(t)
	t.Chdir(dir)
	// Present checkpoints block, but a mirror with no type is invalid.
	writeRawSettings(t, dir, `{"enabled": true, "checkpoints": {"primary": {"type": "git"}, "mirrors": [{"config": {}}]}}`)

	_, err := Open(context.Background(), repo, OpenOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checkpoints")
}

func TestOpen_ToleratesUnrelatedMalformedSettings(t *testing.T) {
	dir, repo, _ := newTestRepo(t)
	t.Chdir(dir)
	// summary_generation is the wrong shape and the JSON has no checkpoints
	// block: checkpoint construction must stay fail-soft and default to git.
	writeRawSettings(t, dir, `{"enabled": true, "summary_generation": "not-an-object"}`)

	stores, err := Open(context.Background(), repo, OpenOptions{})
	require.NoError(t, err)
	_, isGit := stores.Persistent.(*GitStore)
	assert.True(t, isGit)
}

func TestOpen_ToleratesWholeFileSyntaxError(t *testing.T) {
	dir, repo, _ := newTestRepo(t)
	t.Chdir(dir)
	writeRawSettings(t, dir, `{"enabled": true,,}`) // invalid JSON

	stores, err := Open(context.Background(), repo, OpenOptions{})
	require.NoError(t, err)
	_, isGit := stores.Persistent.(*GitStore)
	assert.True(t, isGit)
}

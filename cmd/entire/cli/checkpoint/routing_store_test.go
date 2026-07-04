package checkpoint

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"
)

const routingSampleULID = "01KVBJCWYA4YW6J5M9GP655HZN"

// writeRoutingCheckpoint writes a minimal one-session checkpoint to store.
func writeRoutingCheckpoint(t *testing.T, store PersistentStore, cid id.CheckpointID, sessionID string) {
	t.Helper()
	require.NoError(t, store.Write(context.Background(), Session{
		CheckpointID: cid,
		SessionID:    sessionID,
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("transcript for " + sessionID)),
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}))
}

func TestKindRoutingStore_Read(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	hexID := id.MustCheckpointID("a1b2c3d4e5f6")
	ulidID := id.MustCheckpointID(routingSampleULID)

	t.Run("git-branch primary: hex on branch, ULID still from refs", func(t *testing.T) {
		t.Parallel()
		_, repo, _ := newTestRepo(t)
		branch := NewGitStore(repo, DefaultV1Refs())
		refs := newGitRefsStore(repo)
		writeRoutingCheckpoint(t, branch, hexID, "hex-on-branch")
		writeRoutingCheckpoint(t, refs, ulidID, "ulid-in-refs")

		router := newKindRoutingStore(branch, branch, refs, BackendTypeGitBranch)

		got, err := router.Read(ctx, hexID)
		require.NoError(t, err)
		require.NotNil(t, got, "hex checkpoint should resolve from the branch")

		got, err = router.Read(ctx, ulidID)
		require.NoError(t, err)
		require.NotNil(t, got, "ULID checkpoint should resolve from refs even under a git-branch primary")
	})

	t.Run("git-refs primary: ULID from refs, pre-migration hex from branch fallback", func(t *testing.T) {
		t.Parallel()
		_, repo, _ := newTestRepo(t)
		branch := NewGitStore(repo, DefaultV1Refs())
		refs := newGitRefsStore(repo)
		writeRoutingCheckpoint(t, branch, hexID, "hex-on-branch")
		writeRoutingCheckpoint(t, refs, ulidID, "ulid-in-refs")

		router := newKindRoutingStore(refs, branch, refs, BackendTypeGitRefs)

		got, err := router.Read(ctx, ulidID)
		require.NoError(t, err)
		require.NotNil(t, got)

		got, err = router.Read(ctx, hexID)
		require.NoError(t, err)
		require.NotNil(t, got, "hex checkpoint on the branch should resolve via fallback under a git-refs primary")
	})

	t.Run("git-refs primary: migrated hex in refs resolves from refs first", func(t *testing.T) {
		t.Parallel()
		_, repo, _ := newTestRepo(t)
		branch := NewGitStore(repo, DefaultV1Refs())
		refs := newGitRefsStore(repo)
		migratedHex := id.MustCheckpointID("ffffffffeeee")
		writeRoutingCheckpoint(t, refs, migratedHex, "hex-migrated-to-refs")

		router := newKindRoutingStore(refs, branch, refs, BackendTypeGitRefs)

		got, err := router.Read(ctx, migratedHex)
		require.NoError(t, err)
		require.NotNil(t, got, "a hex checkpoint migrated into refs should resolve under a git-refs primary")
	})

	t.Run("a ULID is never read from the branch", func(t *testing.T) {
		t.Parallel()
		_, repo, _ := newTestRepo(t)
		branch := NewGitStore(repo, DefaultV1Refs())
		refs := newGitRefsStore(repo)
		// Deliberately put a ULID-named checkpoint on the branch (the wrong place)
		// and nothing in refs; routing must not find it, proving branch is never
		// consulted for a ULID.
		writeRoutingCheckpoint(t, branch, ulidID, "stray-ulid-on-branch")

		router := newKindRoutingStore(branch, branch, refs, BackendTypeGitBranch)

		got, err := router.Read(ctx, ulidID)
		require.NoError(t, err)
		assert.Nil(t, got, "a ULID must be read only from refs; a stray ULID on the branch must not resolve")
	})
}

func TestKindRoutingStore_SessionReadRoutes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, _ := newTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	refs := newGitRefsStore(repo)

	ulidID := id.MustCheckpointID(routingSampleULID)
	writeRoutingCheckpoint(t, refs, ulidID, "ulid-in-refs")

	router := newKindRoutingStore(branch, branch, refs, BackendTypeGitBranch)

	meta, err := router.ReadSessionMetadata(ctx, ulidID, 0)
	require.NoError(t, err)
	require.NotNil(t, meta, "session metadata for a ULID checkpoint should route to refs")
	assert.Equal(t, "ulid-in-refs", meta.SessionID)
}

func TestKindRoutingStore_ListUnionsBothBackends(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, _ := newTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	refs := newGitRefsStore(repo)

	hexID := id.MustCheckpointID("a1b2c3d4e5f6")
	ulidID := id.MustCheckpointID(routingSampleULID)
	writeRoutingCheckpoint(t, branch, hexID, "hex")
	writeRoutingCheckpoint(t, refs, ulidID, "ulid")

	router := newKindRoutingStore(branch, branch, refs, BackendTypeGitBranch)

	infos, err := router.List(ctx)
	require.NoError(t, err)
	seen := make(map[string]bool, len(infos))
	for _, info := range infos {
		seen[info.CheckpointID.String()] = true
	}
	assert.True(t, seen[hexID.String()], "list should include the hex checkpoint from the branch")
	assert.True(t, seen[ulidID.String()], "list should include the ULID checkpoint from refs")
}

func TestKindRoutingStore_GetCheckpointAuthorRoutes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, _ := newTestRepo(t)
	branch := NewGitStore(repo, DefaultV1Refs())
	refs := newGitRefsStore(repo)

	ulidID := id.MustCheckpointID(routingSampleULID)
	writeRoutingCheckpoint(t, refs, ulidID, "ulid-in-refs")

	router := newKindRoutingStore(branch, branch, refs, BackendTypeGitBranch)
	author, ok := router.(AuthorReader)
	require.True(t, ok, "routing store over git backends should expose AuthorReader")

	got, err := author.GetCheckpointAuthor(ctx, ulidID)
	require.NoError(t, err)
	assert.Equal(t, "Test", got.Name, "author of a ULID checkpoint should route to refs")
}

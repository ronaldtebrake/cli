package checkpoint

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// Not parallel: uses t.Setenv to drive the checkpoints-config env override.
func TestGenerateCheckpointID(t *testing.T) {
	ctx := context.Background()

	t.Run("git-refs primary mints a ULID", func(t *testing.T) {
		t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", "git-refs")
		cid, err := GenerateCheckpointID(ctx)
		require.NoError(t, err)
		assert.Equal(t, id.KindULID, cid.Kind(), "git-refs primary should mint a ULID")
	})

	t.Run("default primary mints legacy hex", func(t *testing.T) {
		t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", "") // unset → default git-branch
		cid, err := GenerateCheckpointID(ctx)
		require.NoError(t, err)
		assert.Equal(t, id.KindLegacy, cid.Kind(), "default primary should mint a 12-hex id")
	})

	t.Run("git-branch primary mints legacy hex", func(t *testing.T) {
		t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", "git-branch")
		cid, err := GenerateCheckpointID(ctx)
		require.NoError(t, err)
		assert.Equal(t, id.KindLegacy, cid.Kind())
	})
}

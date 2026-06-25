package checkpoint

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistry_GitBackendRegistered(t *testing.T) {
	t.Parallel()

	_, err := build(context.Background(), OpenEnv{}, BackendTypeGit, nil)
	// The git factory rejects a nil repo, which proves it is registered and
	// reached (an unknown type would fail earlier with a different message).
	require.Error(t, err)
	assert.Contains(t, err.Error(), "git checkpoint backend requires a repository")
}

func TestRegistry_UnknownType(t *testing.T) {
	t.Parallel()

	_, err := build(context.Background(), OpenEnv{}, "definitely-not-a-backend", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown checkpoint backend type "definitely-not-a-backend"`)
	// The error lists registered types so misconfiguration is debuggable.
	assert.Contains(t, err.Error(), BackendTypeGit)
}

func TestRegistry_GitFactoryIgnoresConfig(t *testing.T) {
	t.Parallel()

	// A non-nil cfg block must not change the nil-repo rejection: the git
	// backend takes its topology from env.Refs, not from settings cfg.
	_, err := build(context.Background(), OpenEnv{}, BackendTypeGit, json.RawMessage(`{"anything":true}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "git checkpoint backend requires a repository")
}

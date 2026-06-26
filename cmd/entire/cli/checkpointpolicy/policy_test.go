package checkpointpolicy_test

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/stretchr/testify/require"
)

func TestDefaultPolicy(t *testing.T) {
	t.Parallel()
	got := checkpointpolicy.DefaultPolicy()
	require.Equal(t, checkpoint.CheckpointVersionBranchV1, got.CheckpointVersion)
	require.Equal(t, checkpoint.CheckpointVersionBranchV1, got.CheckpointMinVersion)
}

func TestCheckpointVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		policy       checkpointpolicy.Policy
		wantVersion  string
		wantFallback bool
	}{
		{
			name:        "default",
			policy:      checkpointpolicy.DefaultPolicy(),
			wantVersion: checkpoint.CheckpointVersionBranchV1,
		},
		{
			name:        "missing version",
			policy:      checkpointpolicy.Policy{CheckpointMinVersion: checkpoint.CheckpointVersionBranchV1},
			wantVersion: checkpoint.CheckpointVersionBranchV1,
		},
		{
			name:         "unsupported configured version",
			policy:       checkpointpolicy.Policy{CheckpointVersion: "refs-v1", CheckpointMinVersion: checkpoint.CheckpointVersionBranchV1},
			wantVersion:  checkpoint.CheckpointVersionBranchV1,
			wantFallback: true,
		},
		{
			name:         "invalid configured version",
			policy:       checkpointpolicy.Policy{CheckpointVersion: "invalid", CheckpointMinVersion: checkpoint.CheckpointVersionBranchV1},
			wantVersion:  checkpoint.CheckpointVersionBranchV1,
			wantFallback: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotVersion, gotFallback := checkpointpolicy.CheckpointVersion(tt.policy)
			require.Equal(t, tt.wantVersion, gotVersion)
			require.Equal(t, tt.wantFallback, gotFallback)
		})
	}
}

func TestValidatePolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		policy  checkpointpolicy.Policy
		wantErr string
	}{
		{name: "default", policy: checkpointpolicy.DefaultPolicy()},
		{name: "unknown current", policy: checkpointpolicy.Policy{CheckpointVersion: "future-v1", CheckpointMinVersion: "branch-v1"}, wantErr: `checkpoint_version "future-v1" is not supported by this Entire CLI`},
		{name: "unsupported current", policy: checkpointpolicy.Policy{CheckpointVersion: "branch-v2342", CheckpointMinVersion: "branch-v1"}, wantErr: `checkpoint_version "branch-v2342" is not supported by this Entire CLI`},
		{name: "unsupported minimum", policy: checkpointpolicy.Policy{CheckpointVersion: "branch-v1", CheckpointMinVersion: "refs-v1"}, wantErr: `checkpoint_min_version "refs-v1" is not supported by this Entire CLI`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := checkpointpolicy.ValidatePolicy(tt.policy)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

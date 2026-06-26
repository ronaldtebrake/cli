package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// ErrInvalidCheckpointsConfig is returned when a present "checkpoints" settings
// block is malformed (e.g. a backend with no type).
var ErrInvalidCheckpointsConfig = errors.New("invalid checkpoints config")

// CheckpointsConfig selects checkpoint storage backends: one primary (source of
// truth, serves all reads and writes) and zero or more mirrors (independent
// backends that receive best-effort write fan-out). When absent, the checkpoint
// layer defaults to the built-in git backend with no mirrors.
type CheckpointsConfig struct {
	Primary BackendConfig   `json:"primary"`
	Mirrors []BackendConfig `json:"mirrors,omitempty"`
}

// BackendConfig is a discriminated backend selector: Type names the registered
// backend and Config carries the backend-specific options block (opaque here,
// decoded by the backend factory).
type BackendConfig struct {
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config,omitempty"`
}

// checkpointsEnvelope extracts only the "checkpoints" key, leaving every other
// settings field untouched so unrelated malformed/unknown settings cannot break
// checkpoint backend resolution.
type checkpointsEnvelope struct {
	Checkpoints json.RawMessage `json:"checkpoints"`
}

// LoadCheckpointsConfig reads the checkpoint backend selection from settings
// without the strict whole-settings validation that Load performs. It is
// deliberately fail-soft: a missing settings file, a whole-file JSON syntax
// error, or unrelated invalid fields all resolve to "no checkpoints config"
// (nil), so checkpoint construction falls back to the default git backend. It
// errors only when a "checkpoints" block is present but itself invalid.
//
// Precedence mirrors Load: a "checkpoints" block in settings.local.json
// replaces the one in settings.json wholesale (this is a selection config, not
// a deep-merged document). Clone preferences carry no checkpoint config and are
// not consulted.
func LoadCheckpointsConfig(ctx context.Context) (*CheckpointsConfig, error) {
	base, local := checkpointsSettingsPaths(ctx)

	cfg, err := loadCheckpointsFromFile(base)
	if err != nil {
		return nil, err
	}
	if local != "" {
		localCfg, err := loadCheckpointsFromFile(local)
		if err != nil {
			return nil, err
		}
		if localCfg != nil {
			cfg = localCfg
		}
	}

	if cfg != nil {
		if err := cfg.validate(); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

func loadCheckpointsFromFile(filePath string) (*CheckpointsConfig, error) {
	data, err := os.ReadFile(filePath) //nolint:gosec // path is from AbsPath or a worktree-root join
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil //nolint:nilnil // absent file => no checkpoints config, not an error
		}
		return nil, fmt.Errorf("reading settings file %s: %w", filePath, err)
	}

	var env checkpointsEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		// A whole-file parse failure is an unrelated breakage that the strict
		// Load path surfaces for normal commands. Stay fail-soft here so
		// checkpoint construction defaults to git rather than failing.
		return nil, nil //nolint:nilnil,nilerr // intentionally fail-soft on unrelated malformed settings
	}
	if len(env.Checkpoints) == 0 {
		return nil, nil //nolint:nilnil // no checkpoints block present
	}

	var cfg CheckpointsConfig
	dec := json.NewDecoder(bytes.NewReader(env.Checkpoints))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("invalid checkpoints config in %s: %w", filePath, err)
	}
	return &cfg, nil
}

func (c *CheckpointsConfig) validate() error {
	if c.Primary.Type == "" {
		return fmt.Errorf("%w: checkpoints.primary.type is required", ErrInvalidCheckpointsConfig)
	}
	for i, m := range c.Mirrors {
		if m.Type == "" {
			return fmt.Errorf("%w: checkpoints.mirrors[%d].type is required", ErrInvalidCheckpointsConfig, i)
		}
	}
	return nil
}

// checkpointsSettingsPaths resolves the base and local settings file paths the
// same way Load does (minus clone preferences, which carry no checkpoint config).
func checkpointsSettingsPaths(ctx context.Context) (base, local string) {
	if worktreeRoot, ok := worktreeRootFromContext(ctx); ok {
		return filepath.Join(worktreeRoot, EntireSettingsFile), filepath.Join(worktreeRoot, EntireSettingsLocalFile)
	}
	base, err := paths.AbsPath(ctx, EntireSettingsFile)
	if err != nil {
		base = EntireSettingsFile
	}
	local, err = paths.AbsPath(ctx, EntireSettingsLocalFile)
	if err != nil {
		local = EntireSettingsLocalFile
	}
	return base, local
}

package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// managedScaffoldStatus is the outcome of writing an Entire-managed scaffold file
// (a search skill, an agent-help skill, ...). Statuses are shared so each
// feature's reporter can switch on them.
type managedScaffoldStatus string

const (
	managedScaffoldUnsupported     managedScaffoldStatus = "unsupported"
	managedScaffoldCreated         managedScaffoldStatus = "created"
	managedScaffoldUpdated         managedScaffoldStatus = "updated"
	managedScaffoldUnchanged       managedScaffoldStatus = "unchanged"
	managedScaffoldSkippedConflict managedScaffoldStatus = "skipped_conflict"
)

type managedScaffoldResult struct {
	Status  managedScaffoldStatus
	RelPath string
}

// writeManagedScaffold writes content to targetPath idempotently: it creates the
// file when absent, leaves it untouched (Unchanged) when identical, rewrites it
// (Updated) only when Entire already manages it, and refuses to clobber an
// unmanaged file (SkippedConflict). isManaged reports whether existing bytes
// carry this feature's management marker.
func writeManagedScaffold(targetPath, relPath string, content []byte, isManaged func([]byte) bool) (managedScaffoldResult, error) {
	existingData, err := os.ReadFile(targetPath) //nolint:gosec // target path is derived from repo root + fixed relative path
	if err == nil {
		if !isManaged(existingData) {
			return managedScaffoldResult{Status: managedScaffoldSkippedConflict, RelPath: relPath}, nil
		}
		if bytes.Equal(existingData, content) {
			return managedScaffoldResult{Status: managedScaffoldUnchanged, RelPath: relPath}, nil
		}
		if err := os.WriteFile(targetPath, content, 0o600); err != nil {
			return managedScaffoldResult{}, fmt.Errorf("update managed scaffold: %w", err)
		}
		return managedScaffoldResult{Status: managedScaffoldUpdated, RelPath: relPath}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return managedScaffoldResult{}, fmt.Errorf("read managed scaffold: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(targetPath), 0o750); err != nil {
		return managedScaffoldResult{}, fmt.Errorf("create managed scaffold directory: %w", err)
	}
	if err := os.WriteFile(targetPath, content, 0o600); err != nil {
		return managedScaffoldResult{}, fmt.Errorf("write managed scaffold: %w", err)
	}
	return managedScaffoldResult{Status: managedScaffoldCreated, RelPath: relPath}, nil
}

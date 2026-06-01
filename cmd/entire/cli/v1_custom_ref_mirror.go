package cli

import (
	"errors"
	"fmt"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// mirrorToV1CustomRef sets refs/entire/checkpoints/v1.1 to the v1 metadata
// branch tip, returning errors so callers can surface them. v1 is the source
// of truth; the v1.1 ref is a strict local mirror, so this force-overwrites
// rather than safely advancing. The hook-side equivalent
// (strategy.mirrorMetadataToV1CustomRef) logs errors instead of returning them.
func mirrorToV1CustomRef(repo *git.Repository) error {
	v1Ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return fmt.Errorf("v1 metadata branch %s missing after v1 write", paths.MetadataBranchName)
		}
		return fmt.Errorf("read v1 metadata branch %s: %w", paths.MetadataBranchName, err)
	}
	customRef := plumbing.NewHashReference(plumbing.ReferenceName(paths.MetadataRefName), v1Ref.Hash())
	if err := repo.Storer.SetReference(customRef); err != nil {
		return fmt.Errorf("set ref %s to %s: %w", paths.MetadataRefName, v1Ref.Hash(), err)
	}
	return nil
}

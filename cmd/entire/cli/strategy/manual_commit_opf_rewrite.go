// Pre-push OPF rewrite for entire/checkpoints/v1.
//
// This is the ONLY production code path that runs the OPF-augmented
// redaction entry points. Post-commit condensation stays on 7-layer
// for predictable latency; OPF runs here, once per push, after the
// user opted in via settings.
package strategy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// V1DivergedError: local entire/checkpoints/v1 has commits that aren't
// ancestors of the remote tip (force-push or another machine pushed).
// Rewriting under divergence would silently rebase rejected work, so
// we refuse.
type V1DivergedError struct {
	Local, Remote, MergeBase plumbing.Hash
}

func (e *V1DivergedError) Error() string {
	return fmt.Sprintf("entire/checkpoints/v1 has diverged from remote (local=%s remote=%s merge_base=%s); "+
		"fetch the remote and either reset entire/checkpoints/v1 to <remote>/entire/checkpoints/v1 "+
		"or run `entire doctor --recover-v1` before pushing",
		e.Local.String()[:7], e.Remote.String()[:7], e.MergeBase.String()[:7])
}

// BootstrapTooLargeError: first push to a remote with no v1 yet, but
// more unpushed commits than the safety cap. OPF inference is ~30s per
// commit, so unbounded bootstraps could take hours.
type BootstrapTooLargeError struct {
	Count, Limit int
}

func (e *BootstrapTooLargeError) Error() string {
	return fmt.Sprintf("OPF bootstrap would rewrite %d entire/checkpoints/v1 commits "+
		"(limit %d). Set ENTIRE_OPF_BOOTSTRAP_LIMIT=<N> or =unlimited to override, "+
		"or push without OPF (ENTIRE_OPF=no git push) to bring the remote into sync first",
		e.Count, e.Limit)
}

// V1RefMovedError: another worktree advanced the local ref during our
// rewrite (CAS conflict). Orphan rewritten objects sit in .git/objects
// until git gc --prune; no manual cleanup needed.
type V1RefMovedError struct {
	Expected, Actual plumbing.Hash
}

func (e *V1RefMovedError) Error() string {
	return fmt.Sprintf("entire/checkpoints/v1 moved during OPF rewrite "+
		"(expected %s, found %s); concurrent push detected — re-run `git push` after fetching",
		e.Expected.String()[:7], e.Actual.String()[:7])
}

// OPFRuntimeFailedError: the OPF circuit breaker tripped mid-rewrite.
// Some blobs were silently downgraded to 7-layer; tagging those commits
// as Entire-OPF-Applied would be a privacy regression (future pushes
// would skip them while their content is 7-layer-only). Abort before
// CAS so the user fixes their OPF install and retries.
type OPFRuntimeFailedError struct {
	OPFCommand string
}

func (e *OPFRuntimeFailedError) Error() string {
	return fmt.Sprintf("OPF runtime failed during pre-push rewrite (command=%q); "+
		"aborting push so 7-layer content isn't tagged as 8-layer-applied. "+
		"Run `%s --help` to verify your OPF install, then retry. Or set "+
		"ENTIRE_OPF=no on the push to skip OPF for this push only.",
		e.OPFCommand, e.OPFCommand)
}

const (
	// bootstrapDefaultLimit caps first-push history rewrites. Picked
	// to bound worst-case wall-clock at ~50min @ 30s/commit.
	bootstrapDefaultLimit = 100
	bootstrapEnvVar       = "ENTIRE_OPF_BOOTSTRAP_LIMIT"
)

func resolveBootstrapLimit() int {
	v := strings.TrimSpace(os.Getenv(bootstrapEnvVar))
	switch {
	case v == "":
		return bootstrapDefaultLimit
	case strings.EqualFold(v, "unlimited"):
		return math.MaxInt32
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return bootstrapDefaultLimit
}

// RewriteUnpushedV1WithOPF re-redacts unpushed entire/checkpoints/v1
// commits with OPF, builds new commits carrying Entire-OPF-Applied:
// true, and CAS-updates the local ref. Idempotent: already-applied
// commits are re-parented without re-running OPF.
//
// Caller checks redact.OPFEnabled() and skips this when OPF is off.
// Returns one of {V1DivergedError, BootstrapTooLargeError,
// V1RefMovedError, OPFRuntimeFailedError} for privacy-critical
// failures — the pre-push hook propagates these so git push aborts.
func RewriteUnpushedV1WithOPF(ctx context.Context, repo *git.Repository, remoteName string) (plumbing.Hash, error) {
	localTip, err := readV1Tip(repo, plumbing.NewBranchReferenceName(paths.MetadataBranchName))
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("read local v1: %w", err)
	}
	if localTip.IsZero() {
		return plumbing.ZeroHash, nil // no checkpoints yet
	}
	remoteTip, err := readV1Tip(repo, plumbing.NewRemoteReferenceName(remoteName, paths.MetadataBranchName))
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("read remote v1: %w", err)
	}

	if !remoteTip.IsZero() {
		mergeBase, mbErr := computeMergeBase(repo, localTip, remoteTip)
		if mbErr != nil {
			return plumbing.ZeroHash, fmt.Errorf("compute merge-base: %w", mbErr)
		}
		if mergeBase != remoteTip {
			return plumbing.ZeroHash, &V1DivergedError{Local: localTip, Remote: remoteTip, MergeBase: mergeBase}
		}
	}

	unpushed, err := listUnpushedV1Commits(repo, localTip, remoteTip)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("list unpushed v1 commits: %w", err)
	}
	if len(unpushed) == 0 {
		return localTip, nil
	}
	if remoteTip.IsZero() {
		if limit := resolveBootstrapLimit(); len(unpushed) > limit {
			return plumbing.ZeroHash, &BootstrapTooLargeError{Count: len(unpushed), Limit: limit}
		}
	}

	parent := remoteTip
	for _, c := range unpushed {
		newHash, err := rebuildV1Commit(ctx, repo, c, parent)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("rebuild commit %s: %w", c.Hash.String()[:7], err)
		}
		parent = newHash
	}

	// Fail-closed: if OPF tripped its breaker mid-rewrite, some blobs
	// got 7-layer fallback. Don't CAS — the orphan commits get GC'd.
	if redact.OPFBreakerTripped() {
		return plumbing.ZeroHash, &OPFRuntimeFailedError{OPFCommand: redact.OPFCommand()}
	}
	if err := atomicSetV1Ref(repo, localTip, parent); err != nil {
		return plumbing.ZeroHash, err
	}
	return parent, nil
}

func readV1Tip(repo *git.Repository, refName plumbing.ReferenceName) (plumbing.Hash, error) {
	ref, err := repo.Reference(refName, true)
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return plumbing.ZeroHash, nil
		}
		return plumbing.ZeroHash, fmt.Errorf("resolve ref %s: %w", refName, err)
	}
	return ref.Hash(), nil
}

// computeMergeBase returns the merge-base commit hash. Multi-base
// (criss-cross) and unrelated-histories both return ZeroHash —
// caller treats those as diverged.
func computeMergeBase(repo *git.Repository, local, remote plumbing.Hash) (plumbing.Hash, error) {
	lc, err := repo.CommitObject(local)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("load local commit: %w", err)
	}
	rc, err := repo.CommitObject(remote)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("load remote commit: %w", err)
	}
	bases, err := lc.MergeBase(rc)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("merge-base: %w", err)
	}
	if len(bases) != 1 {
		return plumbing.ZeroHash, nil
	}
	return bases[0].Hash, nil
}

// listUnpushedV1Commits returns commits reachable from localTip but not
// remoteTip, in graph order (oldest-first). Graph order matters more
// than timestamp order — commits made in rapid succession can share
// Author.When; the parent chain is the unambiguous truth.
func listUnpushedV1Commits(repo *git.Repository, localTip, remoteTip plumbing.Hash) ([]*object.Commit, error) {
	remoteReachable := map[plumbing.Hash]struct{}{}
	if !remoteTip.IsZero() {
		iter, err := repo.Log(&git.LogOptions{From: remoteTip})
		if err != nil {
			return nil, fmt.Errorf("log remote tip: %w", err)
		}
		if walkErr := iter.ForEach(func(c *object.Commit) error {
			remoteReachable[c.Hash] = struct{}{}
			return nil
		}); walkErr != nil {
			return nil, fmt.Errorf("walk remote ancestry: %w", walkErr)
		}
	}

	var unpushed []*object.Commit
	iter, err := repo.Log(&git.LogOptions{From: localTip})
	if err != nil {
		return nil, fmt.Errorf("log local tip: %w", err)
	}
	if walkErr := iter.ForEach(func(c *object.Commit) error {
		if _, ok := remoteReachable[c.Hash]; ok {
			return errStop
		}
		unpushed = append(unpushed, c)
		return nil
	}); walkErr != nil && !errors.Is(walkErr, errStop) {
		return nil, fmt.Errorf("walk local v1 history: %w", walkErr)
	}
	// reverse for oldest-first
	for i, j := 0, len(unpushed)-1; i < j; i, j = i+1, j-1 {
		unpushed[i], unpushed[j] = unpushed[j], unpushed[i]
	}
	return unpushed, nil
}

// rebuildV1Commit re-parents the commit onto parent. Already-applied
// commits keep their tree (idempotent); unapplied commits get an
// OPF-redacted tree + Entire-OPF-Applied: true trailer.
func rebuildV1Commit(ctx context.Context, repo *git.Repository, oldCommit *object.Commit, parent plumbing.Hash) (plumbing.Hash, error) {
	newTree := oldCommit.TreeHash
	if !trailers.HasOPFApplied(oldCommit.Message) {
		tree, err := repo.TreeObject(oldCommit.TreeHash)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("load tree: %w", err)
		}
		newTree, err = rebuildTreeWithOPF(ctx, repo, tree, "")
		if err != nil {
			return plumbing.ZeroHash, err
		}
	}

	parents := []plumbing.Hash{}
	if !parent.IsZero() {
		parents = append(parents, parent)
	}
	c := &object.Commit{
		Author:       oldCommit.Author,
		Committer:    oldCommit.Committer,
		Message:      trailers.AppendOPFAppliedTrailer(oldCommit.Message),
		TreeHash:     newTree,
		ParentHashes: parents,
	}
	obj := repo.Storer.NewEncodedObject()
	if err := c.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("encode commit: %w", err)
	}
	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store commit: %w", err)
	}
	return hash, nil
}

// rebuildTreeWithOPF walks a tree and produces a new tree with
// OPF-redacted file blobs. content_hash.txt files are recomputed in a
// second pass against the new full.jsonl in the same directory.
//
// Path-specific behavior:
//   - *.jsonl, *.txt → redacted via checkpoint.RedactBlobBytes (OPF on)
//   - content_hash.txt → SHA256 of the sibling full.jsonl's new bytes
//   - other files (metadata.json, *.json) → copied verbatim (no user text)
func rebuildTreeWithOPF(ctx context.Context, repo *git.Repository, tree *object.Tree, pathPrefix string) (plumbing.Hash, error) {
	entries := make([]object.TreeEntry, 0, len(tree.Entries))
	// deferredHashes records indexes of content_hash.txt entries we
	// need to recompute after the full.jsonl in the same dir is built.
	type deferred struct {
		idx       int
		entryName string
		entryMode filemode.FileMode
	}
	var deferredHashes []deferred
	var newFullJSONLHash plumbing.Hash

	for _, e := range tree.Entries {
		switch e.Mode { //nolint:exhaustive // non-tree/blob modes fall through to copy
		case filemode.Dir:
			subTree, err := repo.TreeObject(e.Hash)
			if err != nil {
				return plumbing.ZeroHash, fmt.Errorf("load subtree %s/%s: %w", pathPrefix, e.Name, err)
			}
			subPath := e.Name
			if pathPrefix != "" {
				subPath = pathPrefix + "/" + e.Name
			}
			newSub, err := rebuildTreeWithOPF(ctx, repo, subTree, subPath)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			entries = append(entries, object.TreeEntry{Name: e.Name, Mode: e.Mode, Hash: newSub})

		case filemode.Regular, filemode.Executable:
			switch {
			case e.Name == paths.ContentHashFileName:
				deferredHashes = append(deferredHashes, deferred{idx: len(entries), entryName: e.Name, entryMode: e.Mode})
				entries = append(entries, e) // placeholder; fixed in second pass
			case strings.HasSuffix(e.Name, ".jsonl"), strings.HasSuffix(e.Name, ".txt"):
				content, err := readBlob(repo, e.Hash)
				if err != nil {
					return plumbing.ZeroHash, fmt.Errorf("read blob %s/%s: %w", pathPrefix, e.Name, err)
				}
				newBytes := checkpoint.RedactBlobBytes(ctx, content, e.Name, true)
				newHash, err := checkpoint.CreateBlobFromContent(repo, newBytes)
				if err != nil {
					return plumbing.ZeroHash, fmt.Errorf("write redacted blob %s/%s: %w", pathPrefix, e.Name, err)
				}
				entries = append(entries, object.TreeEntry{Name: e.Name, Mode: e.Mode, Hash: newHash})
				if e.Name == paths.TranscriptFileName {
					newFullJSONLHash = newHash
				}
			default:
				entries = append(entries, e)
			}
		default:
			entries = append(entries, e)
		}
	}

	for _, d := range deferredHashes {
		if newFullJSONLHash.IsZero() {
			continue // no transcript in this dir; keep original hash
		}
		jsonlBytes, err := readBlob(repo, newFullJSONLHash)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("read new transcript for content_hash: %w", err)
		}
		sum := sha256.Sum256(jsonlBytes)
		hashBlob, err := checkpoint.CreateBlobFromContent(repo, []byte(fmt.Sprintf("sha256:%x", sum)))
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("write content_hash: %w", err)
		}
		entries[d.idx] = object.TreeEntry{Name: d.entryName, Mode: d.entryMode, Hash: hashBlob}
	}

	newTree := &object.Tree{Entries: entries}
	obj := repo.Storer.NewEncodedObject()
	if err := newTree.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("encode tree: %w", err)
	}
	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store tree: %w", err)
	}
	return hash, nil
}

func readBlob(repo *git.Repository, hash plumbing.Hash) ([]byte, error) {
	blob, err := repo.BlobObject(hash)
	if err != nil {
		return nil, fmt.Errorf("blob: %w", err)
	}
	r, err := blob.Reader()
	if err != nil {
		return nil, fmt.Errorf("blob reader: %w", err)
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("blob read: %w", err)
	}
	return data, nil
}

// atomicSetV1Ref CAS-updates the local v1 ref. On conflict returns
// V1RefMovedError so the hook aborts the push.
func atomicSetV1Ref(repo *git.Repository, expectedOld, newHash plumbing.Hash) error {
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	if err := repo.Storer.CheckAndSetReference(
		plumbing.NewHashReference(refName, newHash),
		plumbing.NewHashReference(refName, expectedOld),
	); err != nil {
		actual := plumbing.ZeroHash
		if cur, refErr := repo.Reference(refName, true); refErr == nil {
			actual = cur.Hash()
		}
		return &V1RefMovedError{Expected: expectedOld, Actual: actual}
	}
	return nil
}

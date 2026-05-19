package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// CommittedReader provides read access to committed checkpoint data.
// Both GitStore (v1) and V2GitStore (v2) implement this interface.
type CommittedReader interface {
	ReadCommitted(ctx context.Context, checkpointID id.CheckpointID) (*CheckpointSummary, error)
	ReadSessionContent(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*SessionContent, error)
}

// CommittedListReader provides read and list access to committed checkpoint data.
// GitStore, V2GitStore, and DualCheckpointReader implement this interface.
type CommittedListReader interface {
	CommittedReader
	ListCommitted(ctx context.Context) ([]CommittedInfo, error)
	ReadSessionMetadata(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*CommittedMetadata, error)
	ReadSessionMetadataAndPrompts(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*SessionContent, error)
}

// CommittedStore provides read/list access plus committed checkpoint metadata updates.
// NewCommittedReader returns this abstraction so callers do not need to know which
// backing checkpoint format was selected.
type CommittedStore interface {
	CommittedListReader
	UpdateSummary(ctx context.Context, checkpointID id.CheckpointID, summary *Summary) error
	GetCheckpointAuthor(ctx context.Context, checkpointID id.CheckpointID) (Author, error)
}

type committedReadMode int

const (
	committedReadV1 committedReadMode = iota
	committedReadDual
	committedReadV2
)

// CommittedReaderOptions configures NewCommittedReader.
type CommittedReaderOptions struct {
	BlobFetcher BlobFetchFunc
}

//nolint:ireturn // Factory selects among v1, v2, and dual store implementations.
func NewCommittedReader(ctx context.Context, repo *git.Repository, opts CommittedReaderOptions) (CommittedStore, error) {
	if repo == nil {
		return nil, errors.New("git repository is required")
	}

	v1Store := NewGitStore(repo)
	if opts.BlobFetcher != nil {
		v1Store.SetBlobFetcher(opts.BlobFetcher)
	}

	mode := resolveCommittedReadMode(
		settings.IsCheckpointsV2Enabled(ctx),
		settings.CheckpointsVersion(ctx),
		hasLocalV2MainRef(repo),
	)

	var v2Store *V2GitStore
	if mode != committedReadV1 {
		v2Store = NewV2GitStore(repo)
		if opts.BlobFetcher != nil {
			v2Store.SetBlobFetcher(opts.BlobFetcher)
		}
	}

	switch mode {
	case committedReadV2:
		return v2Store, nil
	case committedReadDual:
		return &DualCheckpointReader{v2: v2Store, v1: v1Store}, nil
	case committedReadV1:
		return v1Store, nil
	default:
		return nil, fmt.Errorf("unknown committed checkpoint read mode: %d", mode)
	}
}

func resolveCommittedReadMode(checkpointsV2Enabled bool, checkpointsVersion int, localV2MainRef bool) committedReadMode {
	if checkpointsVersion == 2 {
		return committedReadV2
	}
	if checkpointsV2Enabled || localV2MainRef {
		return committedReadDual
	}
	return committedReadV1
}

func hasLocalV2MainRef(repo *git.Repository) bool {
	if repo == nil {
		return false
	}
	_, err := repo.Reference(plumbing.ReferenceName(paths.V2MainRefName), true)
	return err == nil
}

type DualCheckpointReader struct {
	v2 *V2GitStore
	v1 *GitStore
}

func (r *DualCheckpointReader) ReadCommitted(ctx context.Context, checkpointID id.CheckpointID) (*CheckpointSummary, error) {
	summary, err := r.v2.ReadCommitted(ctx, checkpointID)
	if err == nil && summary != nil {
		return summary, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr //nolint:wrapcheck // Propagating context cancellation
	}
	if err != nil {
		logV2Fallback(ctx, "v2 ReadCommitted failed, falling back to v1", checkpointID, err)
	}
	return r.v1.ReadCommitted(ctx, checkpointID)
}

func (r *DualCheckpointReader) ReadSessionContent(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*SessionContent, error) {
	content, err := r.v2.ReadSessionContent(ctx, checkpointID, sessionIndex)
	if err == nil {
		return content, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr //nolint:wrapcheck // Propagating context cancellation
	}
	if errors.Is(err, ErrCheckpointNotFound) {
		v2CheckpointExists, existsErr := r.v2CheckpointExists(ctx, checkpointID)
		if existsErr != nil {
			return nil, existsErr
		}
		if v2CheckpointExists {
			return r.readSingleV1SessionContent(ctx, checkpointID, sessionIndex, err)
		}
		return r.readV1SessionContentByIndex(ctx, checkpointID, sessionIndex, err)
	}
	sessionID, sessionIDErr := r.v2SessionID(ctx, checkpointID, sessionIndex)
	if sessionIDErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr //nolint:wrapcheck // Propagating context cancellation
		}
		originalErr := fallbackReadError(err, "read v2 session metadata for v1 fallback", sessionIDErr)
		return r.readSingleV1SessionContent(ctx, checkpointID, sessionIndex, originalErr)
	}
	return r.readV1SessionContentByID(ctx, checkpointID, sessionID, err)
}

func (r *DualCheckpointReader) ReadSessionMetadata(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*CommittedMetadata, error) {
	metadata, err := r.v2.ReadSessionMetadata(ctx, checkpointID, sessionIndex)
	if err == nil {
		return metadata, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr //nolint:wrapcheck // Propagating context cancellation
	}
	if errors.Is(err, ErrCheckpointNotFound) {
		v2CheckpointExists, existsErr := r.v2CheckpointExists(ctx, checkpointID)
		if existsErr != nil {
			return nil, existsErr
		}
		if v2CheckpointExists {
			return r.readSingleV1SessionMetadata(ctx, checkpointID, sessionIndex, err)
		}
		return r.readV1SessionMetadataByIndex(ctx, checkpointID, sessionIndex, err)
	}
	return r.readSingleV1SessionMetadata(ctx, checkpointID, sessionIndex, err)
}

func (r *DualCheckpointReader) ReadSessionMetadataAndPrompts(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*SessionContent, error) {
	content, err := r.v2.ReadSessionMetadataAndPrompts(ctx, checkpointID, sessionIndex)
	if err == nil {
		return content, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr //nolint:wrapcheck // Propagating context cancellation
	}
	if errors.Is(err, ErrCheckpointNotFound) {
		v2CheckpointExists, existsErr := r.v2CheckpointExists(ctx, checkpointID)
		if existsErr != nil {
			return nil, existsErr
		}
		if v2CheckpointExists {
			return nil, err
		}
	}
	content, fallbackErr := r.v1.ReadSessionMetadataAndPrompts(ctx, checkpointID, sessionIndex)
	if fallbackErr == nil {
		return content, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr //nolint:wrapcheck // Propagating context cancellation
	}
	return nil, fallbackReadError(err, "read v1 session metadata and prompts", fallbackErr)
}

func (r *DualCheckpointReader) ReadSessionCompactTranscript(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) ([]byte, error) {
	return r.v2.ReadSessionCompactTranscript(ctx, checkpointID, sessionIndex)
}

func (r *DualCheckpointReader) ListCommitted(ctx context.Context) ([]CommittedInfo, error) {
	v2Committed, v2Err := r.v2.ListCommitted(ctx)
	v1Committed, v1Err := r.v1.ListCommitted(ctx)

	if v2Err != nil {
		logging.Debug(ctx, "v2 ListCommitted failed, using v1 only",
			slog.String("error", v2Err.Error()),
		)
		if v1Err != nil {
			return nil, fmt.Errorf("listing checkpoints: %w", v1Err)
		}
		return v1Committed, nil
	}

	if v1Err != nil {
		logging.Debug(ctx, "v1 ListCommitted failed, returning v2 only",
			slog.String("error", v1Err.Error()),
		)
		return v2Committed, nil
	}

	seen := make(map[id.CheckpointID]struct{}, len(v2Committed))
	for _, c := range v2Committed {
		seen[c.CheckpointID] = struct{}{}
	}
	committed := make([]CommittedInfo, 0, len(v2Committed)+len(v1Committed))
	committed = append(committed, v2Committed...)
	for _, c := range v1Committed {
		if _, ok := seen[c.CheckpointID]; !ok {
			committed = append(committed, c)
		}
	}
	return committed, nil
}

func (r *DualCheckpointReader) UpdateSummary(ctx context.Context, checkpointID id.CheckpointID, summary *Summary) error {
	err := r.v2.UpdateSummary(ctx, checkpointID, summary)
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr //nolint:wrapcheck // Propagating context cancellation
	}
	if !errors.Is(err, ErrCheckpointNotFound) {
		return fmt.Errorf("update v2 checkpoint summary: %w", err)
	}
	fallbackErr := r.v1.UpdateSummary(ctx, checkpointID, summary)
	if fallbackErr == nil {
		return nil
	}
	return fallbackReadError(err, "update v1 checkpoint summary", fallbackErr)
}

func (r *DualCheckpointReader) GetCheckpointAuthor(ctx context.Context, checkpointID id.CheckpointID) (Author, error) {
	author, err := r.v2.GetCheckpointAuthor(ctx, checkpointID)
	if err == nil && (author.Name != "" || author.Email != "") {
		return author, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Author{}, ctxErr //nolint:wrapcheck // Propagating context cancellation
	}
	v1Author, v1Err := r.v1.GetCheckpointAuthor(ctx, checkpointID)
	if v1Err == nil {
		return v1Author, nil
	}
	return Author{}, fallbackReadError(err, "read v1 checkpoint author", v1Err)
}

func (r *DualCheckpointReader) v2CheckpointExists(ctx context.Context, checkpointID id.CheckpointID) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err //nolint:wrapcheck // Propagating context cancellation
	}
	checkpointExists := false
	_, rootTreeHash, err := r.v2.GetRefState(plumbing.ReferenceName(paths.V2MainRefName))
	if err == nil {
		rootTree, err := r.v2.repo.TreeObject(rootTreeHash)
		if err == nil {
			_, err = rootTree.Tree(checkpointID.Path())
			checkpointExists = err == nil
		}
	}
	return checkpointExists, nil
}

func (r *DualCheckpointReader) v2SessionID(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (string, error) {
	metadata, err := r.v2.ReadSessionMetadata(ctx, checkpointID, sessionIndex)
	if err != nil {
		return "", err
	}
	if metadata == nil || metadata.SessionID == "" {
		return "", ErrNoTranscript
	}
	return metadata.SessionID, nil
}

func (r *DualCheckpointReader) readV1SessionContentByID(ctx context.Context, checkpointID id.CheckpointID, sessionID string, originalErr error) (*SessionContent, error) {
	content, fallbackErr := r.v1.ReadSessionContentByID(ctx, checkpointID, sessionID)
	if fallbackErr == nil {
		return content, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr //nolint:wrapcheck // Propagating context cancellation
	}
	return nil, fallbackReadError(originalErr, "read v1 fallback session content", fallbackErr)
}

func (r *DualCheckpointReader) readSingleV1SessionContent(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int, originalErr error) (*SessionContent, error) {
	if err := r.requireSingleV1Session(ctx, checkpointID, sessionIndex, originalErr); err != nil {
		return nil, err
	}
	return r.readV1SessionContentByIndex(ctx, checkpointID, sessionIndex, originalErr)
}

func (r *DualCheckpointReader) readV1SessionContentByIndex(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int, originalErr error) (*SessionContent, error) {
	content, err := r.v1.ReadSessionContent(ctx, checkpointID, sessionIndex)
	if err == nil {
		return content, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr //nolint:wrapcheck // Propagating context cancellation
	}
	return nil, fallbackReadError(originalErr, "read v1 session content", err)
}

func (r *DualCheckpointReader) readSingleV1SessionMetadata(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int, originalErr error) (*CommittedMetadata, error) {
	if err := r.requireSingleV1Session(ctx, checkpointID, sessionIndex, originalErr); err != nil {
		return nil, err
	}
	return r.readV1SessionMetadataByIndex(ctx, checkpointID, sessionIndex, originalErr)
}

func (r *DualCheckpointReader) readV1SessionMetadataByIndex(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int, originalErr error) (*CommittedMetadata, error) {
	metadata, err := r.v1.ReadSessionMetadata(ctx, checkpointID, sessionIndex)
	if err == nil {
		return metadata, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr //nolint:wrapcheck // Propagating context cancellation
	}
	return nil, fallbackReadError(originalErr, "read v1 session metadata", err)
}

func (r *DualCheckpointReader) requireSingleV1Session(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int, originalErr error) error {
	summary, err := r.v1.ReadCommitted(ctx, checkpointID)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr //nolint:wrapcheck // Propagating context cancellation
		}
		return fallbackReadError(originalErr, "read v1 checkpoint summary", err)
	}
	if summary == nil || len(summary.Sessions) != 1 || sessionIndex != 0 {
		return originalErr
	}
	return nil
}

func fallbackReadError(primaryErr error, fallbackOperation string, fallbackErr error) error {
	if fallbackErr == nil {
		return primaryErr
	}
	return errors.Join(primaryErr, fmt.Errorf("%s: %w", fallbackOperation, fallbackErr))
}

func logV2Fallback(ctx context.Context, message string, checkpointID id.CheckpointID, err error) {
	if errors.Is(err, ErrCheckpointNotFound) || errors.Is(err, ErrNoTranscript) {
		return
	}
	logging.Debug(ctx, message,
		slog.String("checkpoint_id", checkpointID.String()),
		slog.String("error", err.Error()),
	)
}

// ReadCommittedCheckpoint reads a committed checkpoint summary and normalizes
// a nil store response into ErrCheckpointNotFound.
func ReadCommittedCheckpoint(ctx context.Context, reader CommittedReader, checkpointID id.CheckpointID) (*CheckpointSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // Propagating context cancellation
	}

	summary, err := reader.ReadCommitted(ctx, checkpointID)
	if err != nil {
		return nil, fmt.Errorf("read committed checkpoint: %w", err)
	}
	if summary == nil {
		return nil, ErrCheckpointNotFound
	}
	return summary, nil
}

// ReadLatestSessionContent reads the latest session from an already-resolved
// committed reader and summary.
func ReadLatestSessionContent(ctx context.Context, reader CommittedReader, checkpointID id.CheckpointID, summary *CheckpointSummary) (*SessionContent, error) {
	if summary == nil || len(summary.Sessions) == 0 {
		return nil, ErrCheckpointNotFound
	}
	latestIndex := len(summary.Sessions) - 1
	content, err := reader.ReadSessionContent(ctx, checkpointID, latestIndex)
	if err != nil {
		return nil, fmt.Errorf("read session %d content: %w", latestIndex, err)
	}
	return content, nil
}

func ReadRawSessionLogForCheckpoint(ctx context.Context, reader CommittedReader, checkpointID id.CheckpointID) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err //nolint:wrapcheck // Propagating context cancellation
	}

	summary, err := ReadCommittedCheckpoint(ctx, reader, checkpointID)
	if err != nil {
		return nil, "", err
	}

	content, err := ReadLatestSessionContent(ctx, reader, checkpointID, summary)
	if err != nil {
		return nil, "", err
	}
	return content.Transcript, content.Metadata.SessionID, nil
}

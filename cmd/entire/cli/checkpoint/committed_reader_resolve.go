package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
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
}

type CommittedReadMode int

const (
	CommittedReadV1 CommittedReadMode = iota
	CommittedReadDual
	CommittedReadV2
)

func CommittedReadModeForOptions(checkpointsV2Enabled bool, checkpointsVersion int) CommittedReadMode {
	if checkpointsVersion == 2 {
		return CommittedReadV2
	}
	if checkpointsV2Enabled {
		return CommittedReadDual
	}
	return CommittedReadV1
}

func NewCommittedReader(v1Store *GitStore, v2Store *V2GitStore, mode CommittedReadMode) (CommittedListReader, error) { //nolint:ireturn // Factory selects among v1, v2, and dual reader implementations.
	switch mode {
	case CommittedReadV2:
		if v2Store == nil {
			return nil, errors.New("v2 committed checkpoint reader unavailable")
		}
		return v2Store, nil
	case CommittedReadDual:
		if v2Store == nil {
			if v1Store == nil {
				return nil, errors.New("committed checkpoint reader unavailable")
			}
			return v1Store, nil
		}
		if v1Store == nil {
			return v2Store, nil
		}
		return &DualCheckpointReader{v2: v2Store, v1: v1Store}, nil
	case CommittedReadV1:
		if v1Store == nil {
			return nil, errors.New("v1 committed checkpoint reader unavailable")
		}
		return v1Store, nil
	default:
		return nil, fmt.Errorf("unknown committed checkpoint read mode: %d", mode)
	}
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
	if errors.Is(err, ErrNoTranscript) {
		fallbackContent, fallbackErr := r.readFallbackSessionContent(ctx, checkpointID, sessionIndex)
		if fallbackErr == nil {
			return fallbackContent, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr //nolint:wrapcheck // Propagating context cancellation
		}
		return nil, err
	}
	return r.readV1SessionContentByIndex(ctx, checkpointID, sessionIndex, err)
}

func (r *DualCheckpointReader) ReadSessionMetadata(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*CommittedMetadata, error) {
	metadata, err := r.v2.ReadSessionMetadata(ctx, checkpointID, sessionIndex)
	if err == nil {
		return metadata, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr //nolint:wrapcheck // Propagating context cancellation
	}
	return r.readV1SessionMetadataByIndex(ctx, checkpointID, sessionIndex, err)
}

// ReadSessionMetadataAndPrompts is intentionally v2-only because callers pair
// this metadata with the v2 compact transcript. Returning v1 raw content here
// would bypass the checkpoint transcript offset handling in ReadSessionContent.
func (r *DualCheckpointReader) ReadSessionMetadataAndPrompts(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*SessionContent, error) {
	return r.v2.ReadSessionMetadataAndPrompts(ctx, checkpointID, sessionIndex)
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

func (r *DualCheckpointReader) readFallbackSessionContent(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*SessionContent, error) {
	metadata, err := r.v2.ReadSessionMetadata(ctx, checkpointID, sessionIndex)
	if err == nil && metadata != nil && metadata.SessionID != "" {
		return r.v1.ReadSessionContentByID(ctx, checkpointID, metadata.SessionID)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr //nolint:wrapcheck // Propagating context cancellation
	}
	if err != nil {
		return nil, err
	}
	return nil, ErrNoTranscript
}

func (r *DualCheckpointReader) readV1SessionContentByIndex(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int, originalErr error) (*SessionContent, error) {
	content, err := r.v1.ReadSessionContent(ctx, checkpointID, sessionIndex)
	if err == nil {
		return content, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr //nolint:wrapcheck // Propagating context cancellation
	}
	return nil, originalErr
}

func (r *DualCheckpointReader) readV1SessionMetadataByIndex(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int, originalErr error) (*CommittedMetadata, error) {
	metadata, err := r.v1.ReadSessionMetadata(ctx, checkpointID, sessionIndex)
	if err == nil {
		return metadata, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr //nolint:wrapcheck // Propagating context cancellation
	}
	return nil, originalErr
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

// ResolveCommittedReaderForCheckpoint resolves a committed checkpoint summary
// through the mode-level reader selected from the supplied stores.
//
// Fallback behavior:
//   - Uses v1 when preferCheckpointsV2 is false
//   - Uses DualCheckpointReader when preferCheckpointsV2 is true and v1/v2
//     stores are available
//   - Falls back to v1 through DualCheckpointReader when v2 artifacts are
//     missing in dual-read mode
func ResolveCommittedReaderForCheckpoint(
	ctx context.Context,
	checkpointID id.CheckpointID,
	v1Store *GitStore,
	v2Store *V2GitStore,
	preferCheckpointsV2 bool,
) (CommittedReader, *CheckpointSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err //nolint:wrapcheck // Propagating context cancellation
	}

	mode := CommittedReadV1
	if preferCheckpointsV2 {
		mode = CommittedReadDual
	}
	reader, err := NewCommittedReader(v1Store, v2Store, mode)
	if err != nil {
		return nil, nil, err
	}

	summary, err := ReadCommittedCheckpoint(ctx, reader, checkpointID)
	if err != nil {
		return nil, nil, err
	}

	return reader, summary, nil
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

// ResolveRawSessionLogForCheckpoint resolves the raw transcript log bytes for a
// checkpoint with v2-first, v1-fallback behavior.
//
// Fallback behavior:
//   - Uses ResolveCommittedReaderForCheckpoint
//   - When v2 /main is chosen, raw transcript reads fall back to the matching
//     v1 session ID if v2 /full/* has no raw transcript
func ResolveRawSessionLogForCheckpoint(
	ctx context.Context,
	checkpointID id.CheckpointID,
	v1Store *GitStore,
	v2Store *V2GitStore,
	preferCheckpointsV2 bool,
) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err //nolint:wrapcheck // Propagating context cancellation
	}

	reader, _, err := ResolveCommittedReaderForCheckpoint(ctx, checkpointID, v1Store, v2Store, preferCheckpointsV2)
	if err != nil {
		return nil, "", err
	}

	return ReadRawSessionLogForCheckpoint(ctx, reader, checkpointID)
}

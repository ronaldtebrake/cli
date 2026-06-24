package checkpoint

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// CheckpointReader provides read access to checkpoint-level persistent data.
//
//nolint:revive // CheckpointReader stutter is accepted — the name marks the checkpoint (vs session) read tier.
type CheckpointReader interface {
	Read(ctx context.Context, checkpointID id.CheckpointID) (*CheckpointSummary, error)
	List(ctx context.Context) ([]CheckpointInfo, error)
}

// SessionReader provides read access to session-level data within a checkpoint.
type SessionReader interface {
	ReadSessionContent(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*SessionContent, error)
	ReadSessionMetadata(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*Metadata, error)
	ReadSessionPrompts(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (string, error)
	ReadSessionMetadataAndPrompts(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*SessionContent, error)
}

// PersistentStore provides the production persistent checkpoint storage surface:
// checkpoint-level reads, session-level reads, and the unified Write. Writes go
// through Writer.Write(ctx, WriteRequest); the concrete per-operation methods
// (writeSession/backfillTranscript/...) live on the git implementation as the
// methods Write dispatches to.
type PersistentStore interface {
	CheckpointReader
	SessionReader
	Writer
}

// AuthorReader provides optional checkpoint author lookup.
type AuthorReader interface {
	GetCheckpointAuthor(ctx context.Context, checkpointID id.CheckpointID) (Author, error)
}

// ReadCheckpoint reads a checkpoint summary and normalizes a nil store response
// into ErrCheckpointNotFound.
func ReadCheckpoint(ctx context.Context, reader CheckpointReader, checkpointID id.CheckpointID) (*CheckpointSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // Propagating context cancellation
	}

	summary, err := reader.Read(ctx, checkpointID)
	if err != nil {
		return nil, fmt.Errorf("read persistent checkpoint: %w", err)
	}
	if summary == nil {
		return nil, ErrCheckpointNotFound
	}
	return summary, nil
}

// ReadLatestSessionContent reads the latest session from an already-resolved
// session reader and summary.
func ReadLatestSessionContent(ctx context.Context, reader SessionReader, checkpointID id.CheckpointID, summary *CheckpointSummary) (*SessionContent, error) {
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

// ReadRawSessionLogForCheckpoint reads a checkpoint's latest-session transcript;
// it needs both reader tiers (resolve the checkpoint, then its latest session).
func ReadRawSessionLogForCheckpoint(ctx context.Context, reader interface {
	CheckpointReader
	SessionReader
}, checkpointID id.CheckpointID) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err //nolint:wrapcheck // Propagating context cancellation
	}

	summary, err := ReadCheckpoint(ctx, reader, checkpointID)
	if err != nil {
		return nil, "", err
	}

	content, err := ReadLatestSessionContent(ctx, reader, checkpointID, summary)
	if err != nil {
		return nil, "", err
	}
	return content.Transcript, content.Metadata.SessionID, nil
}

package checkpoint

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// WriteRequest is a single committed-store write command. The set is closed to
// other packages: only types in package checkpoint can implement it, sealed via
// the unexported isWriteRequest marker. A store dispatches on the concrete type;
// a mirror/fan-out store forwards the same value to each backend's Write.
//
// This replaces the four separate writer methods (writeSession /
// backfillTranscript / backfillSummary / backfillAttribution) with one
// Store.Write(ctx, req) entry point, so adding a write operation is a new
// request type plus one dispatch case — the Store interface stays unchanged
// and existing backends keep compiling.
type WriteRequest interface {
	isWriteRequest()
}

// Session creates or replaces a session document within a checkpoint,
// materializing the checkpoint on its first session. This is condensation's
// write. (Maps to the former writeSession.)
type Session WriteOptions

// SessionTranscript replaces a session's transcript, prompts, and skill
// events at stop time without clobbering sibling fields. (Maps to the former
// backfillTranscript.)
type SessionTranscript UpdateOptions

// SessionSummary rewrites only the summary of the checkpoint's latest
// session. (Maps to the former backfillSummary.)
type SessionSummary struct {
	CheckpointID id.CheckpointID
	Summary      *Summary
}

// CheckpointAttribution rewrites the checkpoint root's combined attribution.
// (Maps to the former backfillAttribution.)
//
//nolint:revive // CheckpointAttribution stutter is accepted — the name makes the checkpoint (vs session) tier explicit alongside the Session* requests.
type CheckpointAttribution struct {
	CheckpointID id.CheckpointID
	Attribution  *Attribution
}

func (Session) isWriteRequest()               {}
func (SessionTranscript) isWriteRequest()     {}
func (SessionSummary) isWriteRequest()        {}
func (CheckpointAttribution) isWriteRequest() {}

// Writer is the committed-store write surface: a single Write that accepts any
// WriteRequest. It is the natural type for mirror fan-out.
type Writer interface {
	Write(ctx context.Context, req WriteRequest) error
}

// Write dispatches a committed write request to the matching git operation.
// Unknown request types are a programmer error, surfaced rather than ignored.
func (s *GitStore) Write(ctx context.Context, req WriteRequest) error {
	switch r := req.(type) {
	case Session:
		return s.writeSession(ctx, WriteOptions(r))
	case SessionTranscript:
		return s.backfillTranscript(ctx, UpdateOptions(r))
	case SessionSummary:
		return s.backfillSummary(ctx, r.CheckpointID, r.Summary)
	case CheckpointAttribution:
		return s.backfillAttribution(ctx, r.CheckpointID, r.Attribution)
	default:
		return fmt.Errorf("checkpoint: unsupported write request %T", req)
	}
}

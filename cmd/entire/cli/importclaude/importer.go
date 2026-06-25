package importclaude

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v6"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	cp "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/redact"
)

const importVersion = 1

// Options configures an import run.
type Options struct {
	RepoRoot      string
	OverridePath  string
	SessionFilter []string
	Now           time.Time
	DryRun        bool
}

// Result summarizes an import run.
type Result struct {
	SessionsScanned int
	TurnsImported   int
	TurnsSkipped    int
}

// Run imports the repo's Claude Code transcripts (within the lookback window)
// as read-only, local-only checkpoints on entire/imports/v1. It is idempotent:
// turns whose deterministic ID already exists are skipped.
func Run(ctx context.Context, repo *git.Repository, opts Options) (Result, error) {
	var res Result
	files, err := DiscoverSessions(opts.RepoRoot, opts.OverridePath, opts.Now, opts.SessionFilter)
	if err != nil {
		return res, err
	}

	stores, err := cp.Open(ctx, repo, cp.OpenOptions{Refs: ptrRefs(cp.ImportsRefs())})
	if err != nil {
		return res, fmt.Errorf("open imports store: %w", err)
	}
	existing := make(map[string]bool)
	if infos, listErr := stores.Persistent.List(ctx); listErr == nil {
		for _, in := range infos {
			existing[in.CheckpointID.String()] = true
		}
	}

	for _, path := range files {
		res.SessionsScanned++
		sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		full, readErr := os.ReadFile(path) //nolint:gosec // G304: path is a discovered .jsonl in the user's own Claude transcript dir
		if readErr != nil {
			return res, fmt.Errorf("read %s: %w", path, readErr)
		}
		subagentsDir := filepath.Join(filepath.Dir(path), sessionID, "subagents")
		turns, splitErr := SplitTurns(full, subagentsDir)
		if splitErr != nil {
			return res, splitErr
		}
		for _, turn := range turns {
			cid := DeriveCheckpointID(sessionID, turn.UUID)
			if existing[cid.String()] {
				res.TurnsSkipped++
				continue
			}
			if opts.DryRun {
				res.TurnsImported++ // counts what would import
				continue
			}
			if err := writeTurn(ctx, stores, cid, sessionID, path, full, turn); err != nil {
				return res, err
			}
			existing[cid.String()] = true
			res.TurnsImported++
		}
	}
	return res, nil
}

func writeTurn(ctx context.Context, stores *cp.Stores, cid id.CheckpointID, sessionID, path string, full []byte, turn Turn) error {
	red, err := redact.JSONLBytes(full)
	if err != nil {
		return fmt.Errorf("redact transcript: %w", err)
	}
	prov := &cp.Provenance{
		Source: "claude-code", TranscriptPath: path, SessionID: sessionID,
		TurnUUID: turn.UUID, ParentUUID: turn.ParentUUID,
		LineStart: turn.LineStart, LineEnd: turn.LineEnd,
		ContentHash: turn.ContentHash, ImportVersion: importVersion,
	}
	if err := stores.Persistent.Write(ctx, cp.Session(cp.WriteOptions{
		CheckpointID:              cid,
		SessionID:                 sessionID,
		CreatedAt:                 turn.CreatedAt,
		Strategy:                  "import",
		Kind:                      string(session.KindImported),
		Agent:                     agent.AgentTypeClaudeCode,
		Model:                     turn.Model,
		Transcript:                red,
		Prompts:                   []string{turn.Prompt},
		CheckpointsCount:          1,
		CheckpointTranscriptStart: turn.LineStart,
		TokenUsage:                turn.Tokens,
		Provenance:                prov,
	})); err != nil {
		return fmt.Errorf("write imported checkpoint %s: %w", cid, err)
	}
	return nil
}

func ptrRefs(r cp.PersistentRefs) *cp.PersistentRefs { return &r }

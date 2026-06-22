package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/spf13/cobra"
)

type adoptOptions struct {
	FromWorktree string
	Force        bool
}

const adoptRecentWindow = 12 * time.Hour

func newAdoptCmd() *cobra.Command {
	var opts adoptOptions

	cmd := &cobra.Command{
		Use:   "adopt [session-id]",
		Short: "Adopt an active session from another worktree",
		Long: `Adopt an active session from another worktree into the current repository.

This is useful when an agent starts in one repository or worktree, then moves
and makes changes in another. Adoption copies the live session state into the
current repo and seeds it with the current repo's uncommitted file changes so
the next commit can be linked normally.`,
		Example: `  entire session adopt 019ed5fe-ec49-7a72-89fd-f38e323f5448 --from ../cli
  entire session adopt --from /path/to/source/worktree`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID := ""
			if len(args) > 0 {
				sessionID = args[0]
			}
			return runAdopt(cmd.Context(), cmd.OutOrStdout(), sessionID, opts)
		},
	}

	cmd.Flags().StringVar(&opts.FromWorktree, "from", "", "source worktree that already tracks the session")
	cmd.Flags().BoolVar(&opts.Force, "force", false, "replace an existing local state file for the same session")
	cmd.Flags().BoolVar(&opts.Force, "yes", false, "replace an existing local state file for the same session")

	return cmd
}

func runAdopt(ctx context.Context, w io.Writer, sessionID string, opts adoptOptions) error {
	if strings.TrimSpace(opts.FromWorktree) == "" {
		return errors.New("source worktree is required; pass --from <path>")
	}

	sourceStore, sourceWorktree, sourceCommonDir, err := stateStoreForWorktree(ctx, opts.FromWorktree)
	if err != nil {
		return err
	}

	targetStore, _, targetCommonDir, err := stateStoreForWorktree(ctx, ".")
	if err != nil {
		return fmt.Errorf("open current session store: %w", err)
	}
	if sourceCommonDir == targetCommonDir {
		return errors.New("source and target share the same git common dir; session adopt only moves sessions across independent git session stores")
	}

	sourceState, err := selectAdoptSourceSession(ctx, sourceStore, sourceWorktree, sessionID)
	if err != nil {
		return err
	}

	adopted, filesTouched, err := buildAdoptedSessionState(ctx, sourceState)
	if err != nil {
		return err
	}

	existing, err := targetStore.Load(ctx, adopted.SessionID)
	if err != nil {
		return fmt.Errorf("load current session state: %w", err)
	}
	if existing != nil && !opts.Force {
		return fmt.Errorf("session %s is already tracked in this repo; rerun with --force to replace it", adopted.SessionID)
	}
	if err := targetStore.Save(ctx, adopted); err != nil {
		return fmt.Errorf("save adopted session state: %w", err)
	}

	fmt.Fprintf(w, "Adopted session %s from %s\n", shortSessionID(adopted.SessionID), sourceWorktree)
	if len(filesTouched) == 0 {
		fmt.Fprintln(w, "No current file changes were detected, so the next commit may not link until hooks record changes.")
		return nil
	}
	fmt.Fprintf(w, "Tracking %d file(s): %s\n", len(filesTouched), strings.Join(filesTouched, ", "))
	fmt.Fprintln(w, "Review tracked files before committing; adoption attributes current changes in this repo to the adopted session.")
	return nil
}

func stateStoreForWorktree(ctx context.Context, worktreePath string) (*session.StateStore, string, string, error) {
	absWorktree, err := filepath.Abs(worktreePath)
	if err != nil {
		return nil, "", "", fmt.Errorf("resolve source worktree: %w", err)
	}

	cmd := exec.CommandContext(ctx, "git", "-C", absWorktree, "rev-parse", "--show-toplevel", "--git-common-dir")
	output, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(output))
		if msg != "" {
			return nil, "", "", fmt.Errorf("resolve source git directory: %s: %w", msg, err)
		}
		return nil, "", "", fmt.Errorf("resolve source git directory: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) < 2 {
		return nil, "", "", fmt.Errorf("resolve source git directory: unexpected git output %q", strings.TrimSpace(string(output)))
	}
	sourceRoot := strings.TrimSpace(lines[0])
	commonDir := strings.TrimSpace(lines[1])
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(absWorktree, commonDir)
	}
	commonDir = filepath.Clean(commonDir)
	if resolved, err := filepath.EvalSymlinks(commonDir); err == nil {
		commonDir = resolved
	}

	return session.NewStateStoreWithDir(filepath.Join(commonDir, session.SessionStateDirName)), sourceRoot, commonDir, nil
}

func selectAdoptSourceSession(ctx context.Context, store *session.StateStore, sourceWorktree, sessionID string) (*session.State, error) {
	if sessionID != "" {
		sourceState, err := store.Load(ctx, sessionID)
		if err != nil {
			return nil, fmt.Errorf("load source session state: %w", err)
		}
		if sourceState == nil {
			return nil, fmt.Errorf("session %s was not found in %s", sessionID, sourceWorktree)
		}
		if sourceState.Phase == session.PhaseEnded || sourceState.FullyCondensed {
			return nil, fmt.Errorf("session %s is ended or fully condensed and cannot be adopted", sessionID)
		}
		return sourceState, nil
	}

	states, err := store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list source sessions: %w", err)
	}
	candidates := make([]*session.State, 0, len(states))
	for _, state := range states {
		if isRecentAdoptCandidate(state) {
			candidates = append(candidates, state)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return sessionLastSeen(candidates[i]).After(sessionLastSeen(candidates[j]))
	})

	switch len(candidates) {
	case 0:
		return nil, fmt.Errorf("no recent active sessions found in %s", sourceWorktree)
	case 1:
		return candidates[0], nil
	default:
		ids := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			ids = append(ids, candidate.SessionID)
		}
		return nil, fmt.Errorf("multiple recent active sessions found in %s; pass one of: %s",
			sourceWorktree, strings.Join(ids, ", "))
	}
}

func isRecentAdoptCandidate(state *session.State) bool {
	if state == nil || state.Phase == session.PhaseEnded || state.FullyCondensed {
		return false
	}
	lastSeen := sessionLastSeen(state)
	if lastSeen.IsZero() {
		return false
	}
	return time.Since(lastSeen) <= adoptRecentWindow
}

func sessionLastSeen(state *session.State) time.Time {
	if state.LastInteractionTime != nil {
		return *state.LastInteractionTime
	}
	return state.StartedAt
}

func buildAdoptedSessionState(ctx context.Context, source *session.State) (*session.State, []string, error) {
	repo, err := openRepository(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("open current repository: %w", err)
	}
	defer repo.Close()

	head, err := repo.Head()
	if err != nil {
		return nil, nil, fmt.Errorf("resolve current HEAD: %w", err)
	}

	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve current worktree root: %w", err)
	}
	worktreeID, err := paths.GetWorktreeID(worktreeRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve current worktree ID: %w", err)
	}

	branch, branchErr := GetCurrentBranch(ctx)
	if branchErr != nil {
		branch = ""
	}
	filesTouched, err := currentFilesTouched(ctx)
	if err != nil {
		return nil, nil, err
	}

	now := time.Now()
	adopted := *source

	// Keep the source live transcript path. In cross-repo adoption the transcript
	// belongs to the continuing agent session, not the target repository; clearing
	// or recomputing it from the target repo would drop live transcript capture.
	adopted.CLIVersion = versioninfo.Version
	adopted.TranscriptPath = source.TranscriptPath
	adopted.BaseCommit = head.Hash().String()
	adopted.AttributionBaseCommit = head.Hash().String()
	adopted.WorktreePath = worktreeRoot
	adopted.WorktreeID = worktreeID
	adopted.Branch = branch
	adopted.LastInteractionTime = &now
	adopted.FilesTouched = filesTouched

	// Reset target-local checkpoint bookkeeping. Source checkpoint IDs can point
	// at metadata in another repository or checkpoint branch; carrying them into
	// this repo would let amend and turn-finalization paths operate on unrelated
	// checkpoints.
	adopted.StepCount = 0
	adopted.CheckpointTranscriptStart = 0
	adopted.CheckpointTranscriptSize = 0
	adopted.TranscriptIdentifierAtStart = ""
	adopted.TurnCheckpointIDs = nil
	adopted.LastCheckpointID = id.EmptyCheckpointID
	adopted.LastCheckpointCommitHash = ""

	adopted.FullyCondensed = false
	adopted.DivergenceNoticeShown = false
	adopted.UntrackedFilesAtStart = nil
	adopted.PromptAttributions = nil
	adopted.PendingPromptAttribution = nil
	adopted.PromptWindowBase = 0
	adopted.PromptWindowResetPending = false
	adopted.AttachedManually = false

	return &adopted, filesTouched, nil
}

func currentFilesTouched(ctx context.Context) ([]string, error) {
	changes, err := DetectFileChanges(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("detect current file changes: %w", err)
	}
	files := mergeUnique(nil, changes.Modified)
	files = mergeUnique(files, changes.New)
	files = mergeUnique(files, changes.Deleted)
	sort.Strings(files)
	return files, nil
}

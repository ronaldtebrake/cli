package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/spf13/cobra"
)

type attributionAuthorship string

const (
	attributionAI          attributionAuthorship = "ai"
	attributionHuman       attributionAuthorship = "human"
	attributionMixed       attributionAuthorship = "mixed"
	attributionUncommitted attributionAuthorship = "uncommitted"
)

type attributionLineRange struct {
	Start int
	End   int
}

type rawBlameLine struct {
	LineNumber int
	CommitSHA  string
	Author     string
	AuthorTime *time.Time
	Content    string
}

type attributionLine struct {
	LineNumber      int                    `json:"line_number"`
	Authorship      attributionAuthorship  `json:"authorship"`
	Tag             string                 `json:"tag"`
	CommitSHA       string                 `json:"commit_sha,omitempty"`
	ShortCommitSHA  string                 `json:"short_commit_sha,omitempty"`
	Author          string                 `json:"author,omitempty"`
	AuthorTime      *time.Time             `json:"author_time,omitempty"`
	CheckpointID    string                 `json:"checkpoint_id,omitempty"`
	CheckpointIDs   []string               `json:"checkpoint_ids,omitempty"`
	SessionID       string                 `json:"session_id,omitempty"`
	Agent           string                 `json:"agent,omitempty"`
	Model           string                 `json:"model,omitempty"`
	Prompt          string                 `json:"prompt,omitempty"`
	Intent          string                 `json:"intent,omitempty"`
	MetadataMissing bool                   `json:"metadata_missing,omitempty"`
	Content         string                 `json:"content"`
	Candidates      []attributionCandidate `json:"candidates,omitempty"`
}

type attributionCandidate struct {
	CheckpointID    string   `json:"checkpoint_id"`
	SessionID       string   `json:"session_id,omitempty"`
	Agent           string   `json:"agent,omitempty"`
	Model           string   `json:"model,omitempty"`
	Prompt          string   `json:"prompt,omitempty"`
	Intent          string   `json:"intent,omitempty"`
	FilesTouched    []string `json:"files_touched,omitempty"`
	MetadataMissing bool     `json:"metadata_missing,omitempty"`
	Mixed           bool     `json:"mixed,omitempty"`
}

type attributionCheckpointContext struct {
	CheckpointID    string   `json:"checkpoint_id"`
	SessionIDs      []string `json:"session_ids,omitempty"`
	SessionID       string   `json:"session_id,omitempty"`
	Agent           string   `json:"agent,omitempty"`
	Model           string   `json:"model,omitempty"`
	Prompt          string   `json:"prompt,omitempty"`
	Intent          string   `json:"intent,omitempty"`
	FilesTouched    []string `json:"files_touched,omitempty"`
	MetadataMissing bool     `json:"metadata_missing,omitempty"`
	Mixed           bool     `json:"mixed,omitempty"`
}

type fileAttributionResult struct {
	File        string                                  `json:"file"`
	Lines       []attributionLine                       `json:"lines"`
	Checkpoints map[string]attributionCheckpointContext `json:"checkpoints,omitempty"`
	Summary     attributionSummary                      `json:"summary"`
}

type attributionSummary struct {
	TotalLines       int `json:"total_lines"`
	AILines          int `json:"ai_lines"`
	HumanLines       int `json:"human_lines"`
	MixedLines       int `json:"mixed_lines"`
	UncommittedLines int `json:"uncommitted_lines"`
	AIPercentage     int `json:"ai_percentage"`
	HumanPercentage  int `json:"human_percentage"`
	MixedPercentage  int `json:"mixed_percentage"`
}

type attributionResolver struct {
	ctx         context.Context
	repo        *git.Repository
	store       *checkpoint.GitStore
	fetchOnMiss bool

	commitCache     map[string]*object.Commit
	checkpointCache map[string]attributionCheckpointContext
}

func newBlameCmd() *cobra.Command {
	var lineFlag string
	var jsonFlag bool

	cmd := &cobra.Command{
		Use:   "blame <file>",
		Short: "Show which lines came from Entire checkpoints",
		Long:  "Show git-blame-style line attribution enriched with Entire checkpoint metadata.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAttributionBlame(cmd.Context(), cmd.OutOrStdout(), args[0], lineFlag, jsonFlag)
		},
	}

	cmd.Flags().StringVar(&lineFlag, "line", "", "Only show a line or range, for example 12 or 12-20")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output attribution as JSON")
	return cmd
}

func newWhyCmd() *cobra.Command {
	var jsonFlag bool

	cmd := &cobra.Command{
		Use:   "why <file[:line]>",
		Short: "Show why a line exists",
		Long:  "Explain the commit, checkpoint, prompt, and session behind a file or line.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAttributionWhy(cmd.Context(), cmd.OutOrStdout(), args[0], jsonFlag)
		},
	}

	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output explanation as JSON")
	return cmd
}

func runAttributionBlame(ctx context.Context, w io.Writer, file, lineFlag string, jsonOutput bool) error {
	var lineRange *attributionLineRange
	if lineFlag != "" {
		parsed, err := parseAttributionLineRange(lineFlag)
		if err != nil {
			return err
		}
		lineRange = parsed
	}

	result, err := resolveFileAttribution(ctx, file, false)
	if err != nil {
		return err
	}
	if lineRange != nil {
		result.Lines = filterAttributionLines(result.Lines, *lineRange)
		result.Summary = summarizeAttributionLines(result.Lines)
		result.Checkpoints = checkpointContextsForLines(result.Lines, result.Checkpoints)
	}

	if jsonOutput {
		return writeJSON(w, result)
	}
	renderAttributionBlame(w, result, lineFlag)
	return nil
}

func runAttributionWhy(ctx context.Context, w io.Writer, target string, jsonOutput bool) error {
	file, line, hasLine, err := parseAttributionWhyTarget(target)
	if err != nil {
		return err
	}

	result, err := resolveFileAttribution(ctx, file, false)
	if err != nil {
		return err
	}

	if !hasLine {
		if jsonOutput {
			return writeJSON(w, result)
		}
		renderAttributionFileWhy(w, result)
		return nil
	}

	var selected *attributionLine
	for i := range result.Lines {
		if result.Lines[i].LineNumber == line {
			selected = &result.Lines[i]
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("line %d is outside %s", line, result.File)
	}
	if selected.MetadataMissing && selected.CheckpointID != "" {
		if err := enrichAttributionLineWithFetch(ctx, result.File, selected, result.Checkpoints); err != nil {
			// Remote metadata enrichment is best-effort; the trailer-level
			// explanation is still useful and should remain available.
			selected.MetadataMissing = true
		}
	}

	if jsonOutput {
		payload := struct {
			File        string                                  `json:"file"`
			Line        attributionLine                         `json:"line"`
			Checkpoints map[string]attributionCheckpointContext `json:"checkpoints,omitempty"`
		}{
			File:        result.File,
			Line:        *selected,
			Checkpoints: checkpointContextsForLines([]attributionLine{*selected}, result.Checkpoints),
		}
		return writeJSON(w, payload)
	}
	renderAttributionLineWhy(w, result.File, *selected)
	return nil
}

func resolveFileAttribution(ctx context.Context, file string, fetchOnMiss bool) (*fileAttributionResult, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, errors.New("not a git repository")
	}
	relFile, err := normalizeAttributionPath(repoRoot, file)
	if err != nil {
		return nil, err
	}

	rawLines, err := runGitBlame(ctx, repoRoot, relFile)
	if err != nil {
		return nil, err
	}

	resolver, err := newAttributionResolver(ctx, fetchOnMiss)
	if err != nil {
		return nil, err
	}
	defer resolver.Close()

	result := &fileAttributionResult{
		File:        relFile,
		Checkpoints: make(map[string]attributionCheckpointContext),
	}
	for _, raw := range rawLines {
		line := resolver.resolveLine(raw, relFile)
		result.Lines = append(result.Lines, line)
		for _, candidate := range line.Candidates {
			if candidate.MetadataMissing {
				result.Checkpoints[candidate.CheckpointID] = attributionCheckpointContext{
					CheckpointID:    candidate.CheckpointID,
					MetadataMissing: true,
				}
				continue
			}
			if ctx, ok := resolver.checkpointCache[candidate.CheckpointID]; ok {
				result.Checkpoints[candidate.CheckpointID] = ctx
			}
		}
	}
	result.Summary = summarizeAttributionLines(result.Lines)
	return result, nil
}

func newAttributionResolver(ctx context.Context, fetchOnMiss bool) (*attributionResolver, error) {
	repo, err := openRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}

	store := checkpoint.NewGitStore(repo)
	store.SetBlobFetcher(FetchBlobsByHash)

	return &attributionResolver{
		ctx:             ctx,
		repo:            repo,
		store:           store,
		fetchOnMiss:     fetchOnMiss,
		commitCache:     make(map[string]*object.Commit),
		checkpointCache: make(map[string]attributionCheckpointContext),
	}, nil
}

func (r *attributionResolver) Close() {
	if r != nil && r.repo != nil {
		_ = r.repo.Close()
	}
}

func (r *attributionResolver) resolveLine(raw rawBlameLine, file string) attributionLine {
	line := attributionLine{
		LineNumber: raw.LineNumber,
		CommitSHA:  raw.CommitSHA,
		Author:     raw.Author,
		AuthorTime: raw.AuthorTime,
		Content:    raw.Content,
	}
	if raw.CommitSHA != "" && !isZeroCommit(raw.CommitSHA) {
		line.ShortCommitSHA = shortSHA(raw.CommitSHA)
	}

	if isZeroCommit(raw.CommitSHA) {
		line.Authorship = attributionUncommitted
		line.Tag = attributionTag(line.Authorship)
		return line
	}

	commit, err := r.commit(raw.CommitSHA)
	if err != nil {
		line.Authorship = attributionHuman
		line.Tag = attributionTag(line.Authorship)
		return line
	}

	cpIDs := trailers.ParseAllCheckpoints(commit.Message)
	if len(cpIDs) == 0 {
		line.Authorship = attributionHuman
		line.Tag = attributionTag(line.Authorship)
		return line
	}

	line.Authorship = attributionAI
	var candidates []attributionCandidate
	for _, cpID := range cpIDs {
		ctx := r.checkpointContext(cpID, file)
		candidate := attributionCandidate{
			CheckpointID:    ctx.CheckpointID,
			SessionID:       ctx.SessionID,
			Agent:           ctx.Agent,
			Model:           ctx.Model,
			Prompt:          ctx.Prompt,
			Intent:          ctx.Intent,
			FilesTouched:    ctx.FilesTouched,
			MetadataMissing: ctx.MetadataMissing,
			Mixed:           ctx.Mixed,
		}
		candidates = append(candidates, candidate)
		line.CheckpointIDs = append(line.CheckpointIDs, ctx.CheckpointID)
	}

	preferred := preferredAttributionCandidate(candidates, file)
	if preferred != nil {
		line.CheckpointID = preferred.CheckpointID
		line.SessionID = preferred.SessionID
		line.Agent = preferred.Agent
		line.Model = preferred.Model
		line.Prompt = preferred.Prompt
		line.Intent = preferred.Intent
		line.MetadataMissing = preferred.MetadataMissing
		if preferred.Mixed {
			line.Authorship = attributionMixed
		}
	}
	if len(candidates) > 0 {
		line.Candidates = candidates
	}

	line.Tag = attributionTag(line.Authorship)
	return line
}

func (r *attributionResolver) commit(sha string) (*object.Commit, error) {
	if commit, ok := r.commitCache[sha]; ok {
		return commit, nil
	}
	commit, err := r.repo.CommitObject(plumbing.NewHash(sha))
	if err != nil {
		return nil, err //nolint:wrapcheck // caller treats as missing attribution
	}
	r.commitCache[sha] = commit
	return commit, nil
}

func (r *attributionResolver) checkpointContext(cpID id.CheckpointID, file string) attributionCheckpointContext {
	key := cpID.String()
	if ctx, ok := r.checkpointCache[key]; ok {
		return ctx
	}

	ctx := r.readCheckpointContext(cpID, file)
	r.checkpointCache[key] = ctx
	return ctx
}

func (r *attributionResolver) readCheckpointContext(cpID id.CheckpointID, file string) attributionCheckpointContext {
	ctx := attributionCheckpointContext{CheckpointID: cpID.String()}
	summary, err := checkpoint.ReadCommittedCheckpoint(r.ctx, r.store, cpID)
	if err != nil && r.fetchOnMiss {
		if fetched, fetchErr := r.fetchCheckpointContext(cpID, file); fetchErr == nil {
			return fetched
		}
	}
	if err != nil {
		ctx.MetadataMissing = true
		return ctx
	}

	ctx.FilesTouched = normalizePathSlice(summary.FilesTouched)
	if attributionIsMixed(summary.CombinedAttribution) {
		ctx.Mixed = true
	}

	selected := checkpointSessionForFile{}
	var fallback checkpointSessionForFile
	for i := range summary.Sessions {
		sessionCtx, readErr := r.readSessionForCheckpoint(cpID, i)
		if readErr != nil {
			continue
		}
		if sessionCtx.SessionID != "" {
			ctx.SessionIDs = appendUniqueString(ctx.SessionIDs, sessionCtx.SessionID)
		}
		if attributionIsMixed(sessionCtx.Attribution) {
			ctx.Mixed = true
		}
		if fallback.SessionID == "" {
			fallback = sessionCtx
		}
		if selected.SessionID == "" && pathsContainFile(sessionCtx.FilesTouched, file) {
			selected = sessionCtx
		}
	}

	if selected.SessionID == "" {
		selected = fallback
	}
	ctx.SessionID = selected.SessionID
	ctx.Agent = selected.Agent
	ctx.Model = selected.Model
	ctx.Prompt = selected.Prompt
	ctx.Intent = selected.Intent
	if len(selected.FilesTouched) > 0 {
		ctx.FilesTouched = selected.FilesTouched
	}
	if len(ctx.FilesTouched) == 0 {
		ctx.FilesTouched = normalizePathSlice(summary.FilesTouched)
	}
	return ctx
}

func enrichAttributionLineWithFetch(ctx context.Context, file string, line *attributionLine, checkpoints map[string]attributionCheckpointContext) error {
	if line == nil || len(line.Candidates) == 0 {
		return nil
	}
	resolver, err := newAttributionResolver(ctx, true)
	if err != nil {
		return err
	}
	defer resolver.Close()

	candidates := make([]attributionCandidate, 0, len(line.Candidates))
	authorship := attributionAI
	for _, candidate := range line.Candidates {
		cpID, idErr := id.NewCheckpointID(candidate.CheckpointID)
		if idErr != nil {
			candidates = append(candidates, candidate)
			continue
		}
		cpCtx := resolver.checkpointContext(cpID, file)
		checkpoints[cpCtx.CheckpointID] = cpCtx
		updated := attributionCandidate{
			CheckpointID:    cpCtx.CheckpointID,
			SessionID:       cpCtx.SessionID,
			Agent:           cpCtx.Agent,
			Model:           cpCtx.Model,
			Prompt:          cpCtx.Prompt,
			Intent:          cpCtx.Intent,
			FilesTouched:    cpCtx.FilesTouched,
			MetadataMissing: cpCtx.MetadataMissing,
			Mixed:           cpCtx.Mixed,
		}
		if cpCtx.Mixed {
			authorship = attributionMixed
		}
		candidates = append(candidates, updated)
	}
	preferred := preferredAttributionCandidate(candidates, file)
	if preferred != nil {
		line.CheckpointID = preferred.CheckpointID
		line.SessionID = preferred.SessionID
		line.Agent = preferred.Agent
		line.Model = preferred.Model
		line.Prompt = preferred.Prompt
		line.Intent = preferred.Intent
		line.MetadataMissing = preferred.MetadataMissing
	}
	line.Candidates = candidates
	line.Authorship = authorship
	line.Tag = attributionTag(authorship)
	return nil
}

func (r *attributionResolver) fetchCheckpointContext(cpID id.CheckpointID, file string) (attributionCheckpointContext, error) {
	lookup, err := newExplainCheckpointLookup(r.ctx)
	if err != nil {
		return attributionCheckpointContext{}, err
	}
	defer lookup.Close()

	matches, fresh := matchCheckpointPrefixWithRemoteFallback(r.ctx, io.Discard, lookup, cpID.String())
	if fresh != lookup {
		defer fresh.Close()
	}
	if len(matches) != 1 {
		return attributionCheckpointContext{}, checkpoint.ErrCheckpointNotFound
	}

	oldStore := r.store
	oldFetchOnMiss := r.fetchOnMiss
	r.store = fresh.store
	r.fetchOnMiss = false
	ctx := r.readCheckpointContext(cpID, file)
	r.store = oldStore
	r.fetchOnMiss = oldFetchOnMiss
	return ctx, nil
}

type checkpointSessionForFile struct {
	SessionID    string
	Agent        string
	Model        string
	Prompt       string
	Intent       string
	FilesTouched []string
	Attribution  *checkpoint.InitialAttribution
}

func (r *attributionResolver) readSessionForCheckpoint(cpID id.CheckpointID, index int) (checkpointSessionForFile, error) {
	content, err := r.store.ReadSessionMetadataAndPrompts(r.ctx, cpID, index)
	if err != nil {
		return checkpointSessionForFile{}, err //nolint:wrapcheck // caller skips partial metadata
	}
	meta := content.Metadata
	intent := ""
	if meta.Summary != nil {
		intent = strings.TrimSpace(meta.Summary.Intent)
	}
	prompt := strings.TrimSpace(content.Prompts)
	if prompt == "" {
		prompt = strings.TrimSpace(meta.ReviewPrompt)
	}
	if prompt == "" {
		prompt = intent
	}
	return checkpointSessionForFile{
		SessionID:    meta.SessionID,
		Agent:        string(meta.Agent),
		Model:        meta.Model,
		Prompt:       prompt,
		Intent:       intent,
		FilesTouched: normalizePathSlice(meta.FilesTouched),
		Attribution:  meta.InitialAttribution,
	}, nil
}

func runGitBlame(ctx context.Context, repoRoot, file string) ([]rawBlameLine, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "blame", "--line-porcelain", "--", file)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, fmt.Errorf("git blame --line-porcelain %s: %w (stderr: %s)", file, err, msg)
		}
		return nil, fmt.Errorf("git blame --line-porcelain %s: %w", file, err)
	}
	return parseBlamePorcelain(string(out))
}

var blameHeaderRe = regexp.MustCompile(`^([0-9a-f]{40})\s+\d+\s+(\d+)(?:\s+\d+)?$`)

func parseBlamePorcelain(output string) ([]rawBlameLine, error) {
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var current *rawBlameLine
	var lines []rawBlameLine
	for scanner.Scan() {
		line := scanner.Text()
		if match := blameHeaderRe.FindStringSubmatch(line); match != nil {
			lineNumber, err := strconv.Atoi(match[2])
			if err != nil {
				return nil, fmt.Errorf("parse blame line number %q: %w", match[2], err)
			}
			current = &rawBlameLine{CommitSHA: match[1], LineNumber: lineNumber}
			continue
		}
		if current == nil {
			continue
		}
		switch {
		case strings.HasPrefix(line, "author "):
			current.Author = strings.TrimPrefix(line, "author ")
		case strings.HasPrefix(line, "author-time "):
			seconds, err := strconv.ParseInt(strings.TrimPrefix(line, "author-time "), 10, 64)
			if err == nil {
				t := time.Unix(seconds, 0)
				current.AuthorTime = &t
			}
		case strings.HasPrefix(line, "\t"):
			current.Content = strings.TrimPrefix(line, "\t")
			lines = append(lines, *current)
			current = nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan git blame output: %w", err)
	}
	return lines, nil
}

func parseAttributionLineRange(input string) (*attributionLineRange, error) {
	parts := strings.Split(input, "-")
	if len(parts) > 2 || parts[0] == "" {
		return nil, fmt.Errorf("invalid line range %q: use N or N-M", input)
	}
	start, err := strconv.Atoi(parts[0])
	if err != nil || start < 1 {
		return nil, fmt.Errorf("invalid line range %q: start must be a positive integer", input)
	}
	end := start
	if len(parts) == 2 {
		if parts[1] == "" {
			return nil, fmt.Errorf("invalid line range %q: end must be a positive integer", input)
		}
		end, err = strconv.Atoi(parts[1])
		if err != nil || end < 1 {
			return nil, fmt.Errorf("invalid line range %q: end must be a positive integer", input)
		}
	}
	if end < start {
		return nil, fmt.Errorf("invalid line range %q: end must be >= start", input)
	}
	return &attributionLineRange{Start: start, End: end}, nil
}

func parseAttributionWhyTarget(input string) (file string, line int, hasLine bool, err error) {
	colon := strings.LastIndex(input, ":")
	if colon == -1 || colon == len(input)-1 {
		return input, 0, false, nil
	}
	if volume := filepath.VolumeName(input); volume != "" && colon < len(volume) {
		return input, 0, false, nil
	}
	linePart := input[colon+1:]
	parsed, parseErr := strconv.Atoi(linePart)
	if parseErr != nil || parsed < 1 {
		return "", 0, false, fmt.Errorf("invalid line target %q: use file:line", input)
	}
	return input[:colon], parsed, true, nil
}

func normalizeAttributionPath(repoRoot, file string) (string, error) {
	path := file
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("resolve path %s: %w", file, err)
		}
		path = abs
	}
	canonicalRepoRoot := repoRoot
	if resolved, err := filepath.EvalSymlinks(repoRoot); err == nil {
		canonicalRepoRoot = resolved
	}
	canonicalPath := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		canonicalPath = resolved
	}
	rel, err := filepath.Rel(canonicalRepoRoot, canonicalPath)
	if err != nil {
		return "", fmt.Errorf("resolve path %s relative to repository: %w", file, err)
	}
	if rel == "." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." {
		return "", fmt.Errorf("%s is outside the repository", file)
	}
	return filepath.ToSlash(rel), nil
}

func filterAttributionLines(lines []attributionLine, lineRange attributionLineRange) []attributionLine {
	filtered := make([]attributionLine, 0, len(lines))
	for _, line := range lines {
		if line.LineNumber >= lineRange.Start && line.LineNumber <= lineRange.End {
			filtered = append(filtered, line)
		}
	}
	return filtered
}

func checkpointContextsForLines(lines []attributionLine, contexts map[string]attributionCheckpointContext) map[string]attributionCheckpointContext {
	if len(lines) == 0 || len(contexts) == 0 {
		return nil
	}
	pruned := make(map[string]attributionCheckpointContext)
	for _, line := range lines {
		for _, candidate := range line.Candidates {
			if ctx, ok := contexts[candidate.CheckpointID]; ok {
				pruned[candidate.CheckpointID] = ctx
			}
		}
		if line.CheckpointID != "" {
			if ctx, ok := contexts[line.CheckpointID]; ok {
				pruned[line.CheckpointID] = ctx
			}
		}
	}
	if len(pruned) == 0 {
		return nil
	}
	return pruned
}

func summarizeAttributionLines(lines []attributionLine) attributionSummary {
	var summary attributionSummary
	summary.TotalLines = len(lines)
	for _, line := range lines {
		switch line.Authorship {
		case attributionAI:
			summary.AILines++
		case attributionHuman:
			summary.HumanLines++
		case attributionMixed:
			summary.MixedLines++
		case attributionUncommitted:
			summary.UncommittedLines++
		}
	}
	if summary.TotalLines > 0 {
		summary.AIPercentage = summary.AILines * 100 / summary.TotalLines
		summary.HumanPercentage = summary.HumanLines * 100 / summary.TotalLines
		summary.MixedPercentage = summary.MixedLines * 100 / summary.TotalLines
	}
	return summary
}

func renderAttributionBlame(w io.Writer, result *fileAttributionResult, lineFlag string) {
	sty := newStatusStyles(w)
	fmt.Fprintf(w, "\n  %s\n\n", sty.render(sty.bold, result.File))

	if len(result.Lines) == 0 {
		fmt.Fprintln(w, sty.render(sty.dim, "  No lines to display."))
		return
	}

	lineWidth := len(strconv.Itoa(maxAttributionLineNumber(result.Lines)))
	const checkpointColumnWidth = 21
	fmt.Fprintf(w, "  %*s  Tag   %-12s  %-18s  %-16s  %-21s  Content\n",
		lineWidth, "Line", "Agent", "Model", "Author", "Checkpoint/Session")
	fmt.Fprintf(w, "  %s\n", sty.render(sty.dim, strings.Repeat("─", lineWidth+92)))

	for _, line := range result.Lines {
		fmt.Fprintf(w, "  %s  %s  %-12s  %-18s  %-16s  %-21s  %s\n",
			sty.render(sty.dim, fmt.Sprintf("%*d", lineWidth, line.LineNumber)),
			renderAttributionTag(sty, line.Authorship),
			stringutil.TruncateRunes(line.Agent, 12, ""),
			stringutil.TruncateRunes(line.Model, 18, ""),
			stringutil.TruncateRunes(shortAuthorName(line.Author), 16, ""),
			stringutil.TruncateRunes(shortCheckpointSession(line), checkpointColumnWidth, ""),
			renderAttributionContent(sty, line),
		)
	}

	summary := result.Summary
	fmt.Fprintf(w, "  %s\n", sty.render(sty.dim, strings.Repeat("─", lineWidth+92)))
	parts := []string{
		sty.render(sty.green, fmt.Sprintf("AI: %d (%d%%)", summary.AILines, summary.AIPercentage)),
		fmt.Sprintf("Human: %d (%d%%)", summary.HumanLines, summary.HumanPercentage),
		sty.render(sty.yellow, fmt.Sprintf("Mixed: %d (%d%%)", summary.MixedLines, summary.MixedPercentage)),
	}
	if summary.UncommittedLines > 0 {
		parts = append(parts, sty.render(sty.dim, fmt.Sprintf("Uncommitted: %d", summary.UncommittedLines)))
	}
	if lineFlag != "" {
		fmt.Fprintf(w, "  %s %s %s\n\n", sty.render(sty.bold, "Summary:"), strings.Join(parts, sty.render(sty.dim, " · ")), sty.render(sty.dim, "(filtered)"))
		return
	}
	fmt.Fprintf(w, "  %s %s\n\n", sty.render(sty.bold, "Summary:"), strings.Join(parts, sty.render(sty.dim, " · ")))
}

func renderAttributionLineWhy(w io.Writer, file string, line attributionLine) {
	sty := newStatusStyles(w)
	fmt.Fprintf(w, "\n  %s %d in %s\n", sty.render(sty.bold, "Line"), line.LineNumber, sty.render(sty.bold, file))
	if line.Content != "" {
		fmt.Fprintf(w, "  %s\n\n", sty.render(sty.dim, strings.TrimRight(line.Content, "\r")))
	}

	switch line.Authorship {
	case attributionUncommitted:
		fmt.Fprintf(w, "  %s\n\n", sty.render(sty.yellow, "This line is not committed yet, so Entire cannot attribute it."))
	case attributionHuman:
		fmt.Fprintf(w, "  Written by %s", sty.render(sty.cyan, fallbackString(shortAuthorName(line.Author), "unknown")))
		if line.ShortCommitSHA != "" {
			fmt.Fprintf(w, " %s commit %s", sty.render(sty.dim, "·"), sty.render(sty.dim, line.ShortCommitSHA))
		}
		if line.AuthorTime != nil {
			fmt.Fprintf(w, " %s %s", sty.render(sty.dim, "·"), line.AuthorTime.Format("2006-01-02"))
		}
		fmt.Fprintf(w, "\n  %s\n\n", sty.render(sty.dim, "No Entire checkpoint is linked to the commit that last touched this line."))
	case attributionAI, attributionMixed:
		fmt.Fprintf(w, "  %s by %s", renderAttributionTag(sty, line.Authorship), sty.render(sty.agent, fallbackString(line.Agent, "Entire-tracked agent")))
		if line.Model != "" {
			fmt.Fprintf(w, " %s %s", sty.render(sty.dim, "·"), sty.render(sty.dim, line.Model))
		}
		if line.CheckpointID != "" {
			fmt.Fprintf(w, " %s checkpoint %s", sty.render(sty.dim, "·"), sty.render(sty.cyan, line.CheckpointID))
		}
		if line.SessionID != "" {
			fmt.Fprintf(w, " %s session %s", sty.render(sty.dim, "·"), sty.render(sty.dim, shortSessionID(line.SessionID)))
		}
		if line.ShortCommitSHA != "" {
			fmt.Fprintf(w, " %s commit %s", sty.render(sty.dim, "·"), sty.render(sty.dim, line.ShortCommitSHA))
		}
		fmt.Fprintln(w)
		if line.Prompt != "" {
			fmt.Fprintf(w, "  %s %q\n", sty.render(sty.bold, "Prompt:"), stringutil.TruncateRunes(stringutil.CollapseWhitespace(line.Prompt), 160, "..."))
		}
		if line.Intent != "" && line.Intent != line.Prompt {
			fmt.Fprintf(w, "  %s %q\n", sty.render(sty.bold, "Intent:"), stringutil.TruncateRunes(stringutil.CollapseWhitespace(line.Intent), 160, "..."))
		}
		if line.MetadataMissing {
			fmt.Fprintf(w, "  %s\n", sty.render(sty.yellow, "Checkpoint metadata was not found locally; showing trailer-level attribution only."))
		}
		if len(line.Candidates) > 1 {
			fmt.Fprintf(w, "\n  %s\n", sty.render(sty.bold, "Candidate checkpoints:"))
			for _, candidate := range line.Candidates {
				fmt.Fprintf(w, "  - %s", candidate.CheckpointID)
				if candidate.SessionID != "" {
					fmt.Fprintf(w, " session %s", shortSessionID(candidate.SessionID))
				}
				if candidate.Agent != "" {
					fmt.Fprintf(w, " · %s", candidate.Agent)
				}
				if candidate.Prompt != "" {
					fmt.Fprintf(w, " · %q", stringutil.TruncateRunes(stringutil.CollapseWhitespace(candidate.Prompt), 80, "..."))
				}
				fmt.Fprintln(w)
			}
		}
		if line.CheckpointID != "" {
			fmt.Fprintf(w, "\n  %s %s\n\n", sty.render(sty.dim, "Full context:"), sty.render(sty.cyan, "entire checkpoint explain "+line.CheckpointID))
		} else {
			fmt.Fprintln(w)
		}
	}
}

func renderAttributionFileWhy(w io.Writer, result *fileAttributionResult) {
	sty := newStatusStyles(w)
	summary := result.Summary
	fmt.Fprintf(w, "\n  %s\n", sty.render(sty.bold, result.File))
	fmt.Fprintf(w, "  %d lines %s %s %s %s",
		summary.TotalLines,
		sty.render(sty.dim, "·"),
		sty.render(sty.green, fmt.Sprintf("%d%% AI (%d)", summary.AIPercentage, summary.AILines)),
		sty.render(sty.dim, "·"),
		fmt.Sprintf("%d%% human (%d)", summary.HumanPercentage, summary.HumanLines),
	)
	if summary.MixedLines > 0 {
		fmt.Fprintf(w, " %s %s", sty.render(sty.dim, "·"), sty.render(sty.yellow, fmt.Sprintf("%d%% mixed (%d)", summary.MixedPercentage, summary.MixedLines)))
	}
	fmt.Fprintln(w)

	counts := checkpointLineCounts(result.Lines)
	if len(counts) == 0 {
		fmt.Fprintf(w, "\n  %s\n\n", sty.render(sty.dim, "No Entire checkpoints are linked to this file's current lines."))
		return
	}

	fmt.Fprintf(w, "\n  %s\n", sty.render(sty.bold, "Top checkpoints:"))
	for _, count := range counts {
		ctx := result.Checkpoints[count.CheckpointID]
		fmt.Fprintf(w, "  - %s  %d lines", sty.render(sty.cyan, count.CheckpointID), count.Lines)
		if ctx.Agent != "" {
			fmt.Fprintf(w, " %s %s", sty.render(sty.dim, "·"), ctx.Agent)
		}
		if ctx.SessionID != "" {
			fmt.Fprintf(w, " %s session %s", sty.render(sty.dim, "·"), shortSessionID(ctx.SessionID))
		}
		if ctx.Prompt != "" {
			fmt.Fprintf(w, " %s %q", sty.render(sty.dim, "·"), stringutil.TruncateRunes(stringutil.CollapseWhitespace(ctx.Prompt), 90, "..."))
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "\n  %s\n\n", sty.render(sty.dim, "Tip: entire why "+result.File+":<line> shows the prompt behind a specific line."))
}

type checkpointLineCount struct {
	CheckpointID string
	Lines        int
}

func checkpointLineCounts(lines []attributionLine) []checkpointLineCount {
	counts := make(map[string]int)
	for _, line := range lines {
		if line.CheckpointID != "" {
			counts[line.CheckpointID]++
		}
	}
	out := make([]checkpointLineCount, 0, len(counts))
	for cpID, count := range counts {
		out = append(out, checkpointLineCount{CheckpointID: cpID, Lines: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Lines == out[j].Lines {
			return out[i].CheckpointID < out[j].CheckpointID
		}
		return out[i].Lines > out[j].Lines
	})
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}

func renderAttributionTag(sty statusStyles, authorship attributionAuthorship) string {
	tag := attributionTag(authorship)
	switch authorship {
	case attributionAI:
		return sty.render(sty.green, tag)
	case attributionMixed:
		return sty.render(sty.yellow, tag)
	case attributionUncommitted:
		return sty.render(sty.dim, tag)
	case attributionHuman:
		return tag
	default:
		return tag
	}
}

func renderAttributionContent(sty statusStyles, line attributionLine) string {
	content := stringutil.TruncateRunes(line.Content, 120, "...")
	switch line.Authorship {
	case attributionAI:
		return sty.render(sty.green, content)
	case attributionMixed:
		return sty.render(sty.yellow, content)
	case attributionUncommitted:
		return sty.render(sty.dim, content)
	case attributionHuman:
		return content
	default:
		return content
	}
}

func maxAttributionLineNumber(lines []attributionLine) int {
	maxLine := 1
	for _, line := range lines {
		if line.LineNumber > maxLine {
			maxLine = line.LineNumber
		}
	}
	return maxLine
}

func attributionTag(authorship attributionAuthorship) string {
	switch authorship {
	case attributionAI:
		return "[AI]"
	case attributionMixed:
		return "[MX]"
	case attributionUncommitted:
		return "[??]"
	case attributionHuman:
		return "[HU]"
	default:
		return "[HU]"
	}
}

func preferredAttributionCandidate(candidates []attributionCandidate, file string) *attributionCandidate {
	if len(candidates) == 0 {
		return nil
	}
	for i := range candidates {
		if pathsContainFile(candidates[i].FilesTouched, file) {
			return &candidates[i]
		}
	}
	return &candidates[0]
}

func pathsContainFile(paths []string, file string) bool {
	normalizedFile := normalizeGitPath(file)
	for _, p := range paths {
		if normalizeGitPath(p) == normalizedFile {
			return true
		}
	}
	return false
}

func normalizePathSlice(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if normalized := normalizeGitPath(p); normalized != "" {
			out = appendUniqueString(out, normalized)
		}
	}
	return out
}

func normalizeGitPath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.TrimPrefix(path, "/")
	return filepath.ToSlash(path)
}

func attributionIsMixed(attr *checkpoint.InitialAttribution) bool {
	if attr == nil {
		return false
	}
	agentChanged := attr.AgentLines+attr.AgentRemoved > 0
	humanChanged := attr.HumanAdded+attr.HumanModified+attr.HumanRemoved > 0
	return agentChanged && humanChanged
}

func shortCheckpointSession(line attributionLine) string {
	if line.CheckpointID == "" {
		return ""
	}
	if line.SessionID == "" {
		return line.CheckpointID
	}
	return line.CheckpointID + "/" + shortSessionID(line.SessionID)
}

func shortSessionID(sessionID string) string {
	if len(sessionID) <= 8 {
		return sessionID
	}
	return sessionID[:8]
}

func shortSHA(sha string) string {
	if len(sha) <= 8 {
		return sha
	}
	return sha[:8]
}

func shortAuthorName(author string) string {
	author = strings.TrimSpace(author)
	if before, _, ok := strings.Cut(author, "<"); ok {
		author = strings.TrimSpace(before)
	}
	return author
}

func fallbackString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func appendUniqueString(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func isZeroCommit(sha string) bool {
	return sha == "" || sha == "0000000000000000000000000000000000000000"
}

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	return nil
}

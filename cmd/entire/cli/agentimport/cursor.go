package agentimport

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/cursor"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// cursorImporter imports Cursor transcripts. Cursor uses the same JSONL line
// format as Claude Code (role-tagged), so it reuses the shared user-prompt
// detection and content extraction. Cursor records neither model nor token
// usage, so imported turns carry an empty model and nil tokens.
type cursorImporter struct{}

func (cursorImporter) Name() string { return string(agent.AgentNameCursor) }

func (cursorImporter) AgentType() types.AgentType { return agent.AgentTypeCursor }

// Discover returns Cursor transcript files for the repo modified within the
// lookback window. Cursor stores sessions either flat (<dir>/<id>.jsonl) or
// nested (<dir>/<id>/<id>.jsonl, the IDE layout); both are discovered.
func (cursorImporter) Discover(repoRoot, overridePath string, now time.Time, sessionFilter []string) ([]SessionFile, error) {
	dir := overridePath
	if dir == "" {
		ag := &cursor.CursorAgent{}
		d, err := ag.GetSessionDir(repoRoot)
		if err != nil {
			return nil, fmt.Errorf("resolve cursor session dir: %w", err)
		}
		dir = d
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read cursor session dir: %w", err)
	}
	cutoff := now.AddDate(0, 0, -LookbackDays)
	var out []SessionFile
	for _, e := range entries {
		stem, path := cursorSessionFile(dir, e)
		if path == "" {
			continue
		}
		if len(sessionFilter) > 0 && !slices.Contains(sessionFilter, stem) {
			continue
		}
		info, statErr := os.Stat(path)
		if statErr != nil || info.ModTime().Before(cutoff) {
			continue
		}
		out = append(out, SessionFile{Path: path, SessionID: stem})
	}
	slices.SortFunc(out, func(a, b SessionFile) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// cursorSessionFile maps a directory entry to a (sessionID, transcript path),
// resolving both the flat and nested Cursor layouts. Returns an empty path for
// entries that are not Cursor transcripts.
func cursorSessionFile(dir string, e os.DirEntry) (sessionID, path string) {
	if e.IsDir() {
		nested := filepath.Join(dir, e.Name(), e.Name()+".jsonl")
		if _, err := os.Stat(nested); err == nil {
			return e.Name(), nested
		}
		return "", ""
	}
	if !strings.HasSuffix(e.Name(), ".jsonl") {
		return "", ""
	}
	return strings.TrimSuffix(e.Name(), ".jsonl"), filepath.Join(dir, e.Name())
}

// SplitTurns produces one Turn per user-prompt line, bounded by the next. It
// reuses the package's shared JSONL helpers; Cursor carries no token usage or
// model, so those fields are left zero.
func (cursorImporter) SplitTurns(_ SessionFile, full []byte) ([]Turn, error) {
	rawLines := splitRawLines(full)
	var starts []int
	for i, raw := range rawLines {
		if isUserPromptLine(raw) {
			starts = append(starts, i)
		}
	}
	turns := make([]Turn, 0, len(starts))
	for k, start := range starts {
		end := len(rawLines)
		if k+1 < len(starts) {
			end = starts[k+1]
		}
		var rec struct {
			UUID      string          `json:"uuid"`
			Message   json.RawMessage `json:"message"`
			Timestamp string          `json:"timestamp"`
		}
		if err := json.Unmarshal(rawLines[start], &rec); err != nil {
			continue
		}
		ts, parseErr := time.Parse(time.RFC3339, rec.Timestamp)
		if parseErr != nil {
			ts = time.Time{}
		}
		turns = append(turns, Turn{
			LineStart: start,
			LineEnd:   end,
			UUID:      rec.UUID,
			Prompt:    transcript.ExtractUserContent(rec.Message),
			CreatedAt: ts,
		})
	}
	return turns, nil
}

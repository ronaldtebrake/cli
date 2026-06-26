package agentimport

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// geminiImporter imports Gemini CLI transcripts
// (~/.gemini/tmp/<project-hash>/chats/session-*.json). Unlike the JSONL agents,
// a Gemini transcript is a single JSON document whose offsets are message
// indices, not line numbers, so import is per-session: one checkpoint covering
// the whole transcript rather than per-turn.
type geminiImporter struct{}

func (geminiImporter) Name() string { return string(agent.AgentNameGemini) }

func (geminiImporter) AgentType() types.AgentType { return agent.AgentTypeGemini }

// Discover returns Gemini transcript files for the repo modified within the
// lookback window. The session ID is the file stem (session-<date>-<shortid>).
func (geminiImporter) Discover(repoRoot, overridePath string, now time.Time, sessionFilter []string) ([]SessionFile, error) {
	dir := overridePath
	if dir == "" {
		ag := &geminicli.GeminiCLIAgent{}
		d, err := ag.GetSessionDir(repoRoot)
		if err != nil {
			return nil, fmt.Errorf("resolve gemini session dir: %w", err)
		}
		dir = d
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read gemini session dir: %w", err)
	}
	cutoff := now.AddDate(0, 0, -LookbackDays)
	var out []SessionFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		stem := strings.TrimSuffix(e.Name(), ".json")
		if len(sessionFilter) > 0 && !slices.Contains(sessionFilter, stem) {
			continue
		}
		info, statErr := e.Info()
		if statErr != nil || info.ModTime().Before(cutoff) {
			continue
		}
		out = append(out, SessionFile{Path: filepath.Join(dir, e.Name()), SessionID: stem})
	}
	slices.SortFunc(out, func(a, b SessionFile) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// SplitTurns returns a single Turn covering the whole session. Offsets are
// message indices (the native Gemini space): LineStart 0, LineEnd the message
// count. Token usage is the whole-session total and the prompt is the first
// user message. The turn UUID is the session ID so re-imports stay idempotent.
func (geminiImporter) SplitTurns(sf SessionFile, full []byte) ([]Turn, error) {
	tr, err := geminicli.ParseTranscript(full)
	if err != nil {
		return nil, fmt.Errorf("parse gemini transcript: %w", err)
	}
	if len(tr.Messages) == 0 {
		return nil, nil
	}

	ag := &geminicli.GeminiCLIAgent{}
	tokens, err := ag.CalculateTokenUsage(full, 0)
	if err != nil {
		return nil, fmt.Errorf("token usage: %w", err)
	}

	prompt := ""
	if prompts := geminicli.ExtractAllUserPromptsFromTranscript(tr); len(prompts) > 0 {
		prompt = prompts[0]
	}
	// Gemini messages carry no per-message timestamp; the file modtime is the
	// best available session time.
	var createdAt time.Time
	if info, statErr := os.Stat(sf.Path); statErr == nil {
		createdAt = info.ModTime()
	}

	return []Turn{{
		LineStart: 0,
		LineEnd:   len(tr.Messages),
		UUID:      sf.SessionID,
		Prompt:    prompt,
		CreatedAt: createdAt,
		Tokens:    tokens,
	}}, nil
}

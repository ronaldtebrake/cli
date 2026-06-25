package importclaude

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// Turn is one user-prompt turn extracted from a session transcript. Line
// offsets are in raw-line space (newline-counted), matching the offsets used by
// transcript.SliceFromLine and the agent token-usage helpers.
type Turn struct {
	LineStart, LineEnd int
	UUID, ParentUUID   string
	Prompt, Model      string
	CreatedAt          time.Time
	Tokens             *types.TokenUsage
	ContentHash        string
}

// extraFields are line fields not modeled by transcript.Line.
type extraFields struct {
	ParentUUID string `json:"parentUuid"`
	Timestamp  string `json:"timestamp"`
	Message    struct {
		Model string `json:"model"`
	} `json:"message"`
}

// SplitTurns produces one Turn per user-prompt line. Token usage for each turn
// is computed on the slice [LineStart, LineEnd) so turns don't double-count
// later turns. tool_result lines (Type == "user" but no text content) do not
// start a turn.
func SplitTurns(full []byte, subagentsDir string) ([]Turn, error) {
	rawLines := splitRawLines(full)

	// Identify user-prompt turn starts in raw-line space.
	var starts []int
	for i, raw := range rawLines {
		if isUserPromptLine(raw) {
			starts = append(starts, i)
		}
	}

	ag := &claudecode.ClaudeCodeAgent{}
	turns := make([]Turn, 0, len(starts))
	for k, start := range starts {
		end := len(rawLines)
		if k+1 < len(starts) {
			end = starts[k+1]
		}

		// Bound token usage to [start, end): truncate to the first `end` lines,
		// then let the agent helper slice from `start`.
		truncated := joinLines(rawLines[:end])
		tokens, err := ag.CalculateTotalTokenUsage(truncated, start, subagentsDir)
		if err != nil {
			return nil, fmt.Errorf("token usage for turn %d: %w", k, err)
		}

		var rec struct {
			UUID       string          `json:"uuid"`
			Message    json.RawMessage `json:"message"`
			ParentUUID string          `json:"parentUuid"`
			Timestamp  string          `json:"timestamp"`
		}
		if err := json.Unmarshal(rawLines[start], &rec); err != nil {
			// Already validated as a user-prompt line in isUserPromptLine; skip
			// defensively if it somehow fails to parse here.
			continue
		}
		ts, parseErr := time.Parse(time.RFC3339, rec.Timestamp)
		if parseErr != nil {
			ts = time.Time{}
		}

		slice := joinLines(rawLines[start:end])
		sum := sha256.Sum256(slice)

		turns = append(turns, Turn{
			LineStart:   start,
			LineEnd:     end,
			UUID:        rec.UUID,
			ParentUUID:  rec.ParentUUID,
			Prompt:      transcript.ExtractUserContent(rec.Message),
			Model:       modelInRange(rawLines, start, end),
			CreatedAt:   ts,
			Tokens:      tokens,
			ContentHash: "sha256:" + hex.EncodeToString(sum[:]),
		})
	}
	return turns, nil
}

// modelInRange returns the model from the first assistant message within
// [start, end), or "" when none carries one. The model lives on assistant
// lines, not the user-prompt line.
func modelInRange(rawLines [][]byte, start, end int) string {
	for i := start; i < end && i < len(rawLines); i++ {
		var line transcript.Line
		if err := json.Unmarshal(rawLines[i], &line); err != nil {
			continue
		}
		if line.Type != "assistant" {
			continue
		}
		var ex extraFields
		if err := json.Unmarshal(rawLines[i], &ex); err != nil {
			continue
		}
		if ex.Message.Model != "" {
			return ex.Message.Model
		}
	}
	return ""
}

// isUserPromptLine reports whether a raw JSONL line is a genuine user-prompt
// turn start: type "user" (or role "user") with non-empty extractable text.
// tool_result lines are type "user" but carry no text, so they return false.
func isUserPromptLine(raw []byte) bool {
	var line transcript.Line
	if err := json.Unmarshal(raw, &line); err != nil {
		return false
	}
	typ := line.Type
	if typ == "" {
		typ = line.Role
	}
	if typ != "user" {
		return false
	}
	return transcript.ExtractUserContent(line.Message) != ""
}

// splitRawLines splits content into raw lines in the same index space as
// transcript.SliceFromLine (newline-counted). Trailing empty segment from a
// final newline is dropped.
func splitRawLines(content []byte) [][]byte {
	if len(content) == 0 {
		return nil
	}
	parts := strings.Split(string(content), "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	out := make([][]byte, len(parts))
	for i, p := range parts {
		out[i] = []byte(p)
	}
	return out
}

// joinLines reassembles raw lines into newline-terminated bytes.
func joinLines(lines [][]byte) []byte {
	if len(lines) == 0 {
		return nil
	}
	strs := make([]string, len(lines))
	for i, l := range lines {
		strs[i] = string(l)
	}
	return []byte(strings.Join(strs, "\n") + "\n")
}

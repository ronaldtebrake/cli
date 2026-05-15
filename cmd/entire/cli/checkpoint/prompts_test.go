package checkpoint

import (
	"context"
	"testing"

	"github.com/entireio/cli/redact"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJoinAndSplitPrompts_RoundTrip(t *testing.T) {
	t.Parallel()

	original := []string{
		"first line\nwith newline",
		"second prompt",
	}

	joined := JoinPrompts(original)
	split := SplitPromptContent(joined)

	require.Len(t, split, 2)
	assert.Equal(t, original, split)
}

func TestSplitPromptContent_EmptyContent(t *testing.T) {
	t.Parallel()

	assert.Nil(t, SplitPromptContent(""))
}

// TestRedactedJoinedPrompts_PreRedactedIsTrustedVerbatim verifies that when
// the caller supplies a set RedactedJoinedPrompts the helper unwraps it
// untouched and never re-invokes the redaction pipeline. The pre-redacted
// path is what finalizeAllTurnCheckpoints relies on to avoid running OPF
// once per checkpoint over identical joined-prompt strings.
func TestRedactedJoinedPrompts_PreRedactedIsTrustedVerbatim(t *testing.T) {
	t.Parallel()

	const preRedacted = "[REDACTED_PERSON] asked about [REDACTED_EMAIL]"
	got := redactedJoinedPrompts(
		context.Background(),
		[]string{"raw prompt text"},
		redact.AlreadyRedactedJoinedPrompts(preRedacted),
	)
	assert.Equal(t, preRedacted, got, "preRedacted should pass through verbatim")
}

// TestRedactedJoinedPrompts_ZeroValueFallsBackToLegacyRedaction verifies
// that when the typed preRedacted is the zero value the helper joins the
// prompts and runs the 7-layer pipeline as a safety net. Critically, the
// fallback is JoinedPromptsLegacy (no OPF) — OPF runs exclusively in the
// pre-push rewrite path, never here in the writer's hot path.
func TestRedactedJoinedPrompts_ZeroValueFallsBackToLegacyRedaction(t *testing.T) {
	t.Parallel()

	got := redactedJoinedPrompts(context.Background(), []string{"hello", "world"}, redact.RedactedJoinedPrompts{})
	assert.NotEmpty(t, got, "zero-value preRedacted should fall back to running the legacy 7-layer pipeline")
	assert.Contains(t, got, PromptSeparator, "fallback output should preserve the prompt separator")
}

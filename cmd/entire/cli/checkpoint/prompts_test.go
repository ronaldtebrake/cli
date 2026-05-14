package checkpoint

import (
	"context"
	"testing"

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
// the caller supplies a non-empty preRedacted string the helper returns it
// untouched and never re-invokes the redaction pipeline. The pre-redacted
// path is what finalizeAllTurnCheckpoints relies on to avoid running OPF
// once per checkpoint over identical joined-prompt strings.
func TestRedactedJoinedPrompts_PreRedactedIsTrustedVerbatim(t *testing.T) {
	t.Parallel()

	const preRedacted = "[REDACTED_PERSON] asked about [REDACTED_EMAIL]"
	got := redactedJoinedPrompts(context.Background(), []string{"raw prompt text"}, preRedacted)
	assert.Equal(t, preRedacted, got, "preRedacted should pass through verbatim")
}

// TestRedactedJoinedPrompts_EmptyFallsBackToRedaction verifies that when
// preRedacted is empty the helper joins the prompts and runs the full
// pipeline as a safety net.
func TestRedactedJoinedPrompts_EmptyFallsBackToRedaction(t *testing.T) {
	t.Parallel()

	got := redactedJoinedPrompts(context.Background(), []string{"hello", "world"}, "")
	assert.NotEmpty(t, got, "empty preRedacted should fall back to running the redaction pipeline")
	assert.Contains(t, got, PromptSeparator, "fallback output should preserve the prompt separator")
}

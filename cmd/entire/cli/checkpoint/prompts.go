package checkpoint

import (
	"context"
	"strings"

	"github.com/entireio/cli/redact"
)

// PromptSeparator is the canonical separator used in prompt.txt when multiple
// prompts are stored in a single file.
const PromptSeparator = "\n\n---\n\n"

// JoinPrompts serializes prompts to prompt.txt format.
func JoinPrompts(prompts []string) string {
	return strings.Join(prompts, PromptSeparator)
}

// SplitPromptContent deserializes prompt.txt content into individual prompts.
func SplitPromptContent(content string) []string {
	if content == "" {
		return nil
	}

	prompts := strings.Split(content, PromptSeparator)
	for len(prompts) > 0 && prompts[len(prompts)-1] == "" {
		prompts = prompts[:len(prompts)-1]
	}
	return prompts
}

// redactedJoinedPrompts returns the redacted prompt-blob content for the
// supplied prompts. When preRedacted is set it is unwrapped verbatim;
// otherwise the prompts are joined and run through the full 8-layer
// pipeline as a safety net. Callers that share the same prompts across
// multiple checkpoint writes (finalizeAllTurnCheckpoints) should compute
// the redacted blob once via redact.JoinedPrompts and pass it through to
// avoid running OPF repeatedly over identical input.
func redactedJoinedPrompts(ctx context.Context, prompts []string, preRedacted redact.RedactedJoinedPrompts) string {
	if preRedacted.IsSet() {
		return preRedacted.String()
	}
	return redact.JoinedPrompts(ctx, prompts, PromptSeparator).String()
}

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
// otherwise the prompts are joined and run through the 7-layer pipeline
// as a safety net.
//
// The safety net is intentionally 7-layer-only (no OPF), even when OPF
// is enabled globally. OPF runs exclusively in the pre-push rewrite
// path so per-commit condensation stays fast and predictable; the
// safety net never drags OPF into a hot path. Callers that have
// already produced an 8-layer blob (e.g. the pre-push rewrite itself)
// pass it as preRedacted so this function returns it verbatim.
//
// ctx is retained on the signature for future extensions; the 7-layer
// pipeline doesn't consume it today.
func redactedJoinedPrompts(_ context.Context, prompts []string, preRedacted redact.RedactedJoinedPrompts) string {
	if preRedacted.IsSet() {
		return preRedacted.String()
	}
	return redact.JoinedPromptsLegacy(prompts, PromptSeparator).String()
}

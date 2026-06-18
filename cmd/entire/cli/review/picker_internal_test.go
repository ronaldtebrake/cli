package review

import (
	"context"
	"testing"
)

func TestReviewModelSelectOptionsPreservesCurrentCustomModel(t *testing.T) {
	t.Parallel()
	const current = "my-custom-model"
	options, picked := reviewModelSelectOptions(context.Background(), "unknown-agent", current)
	if picked != current {
		t.Fatalf("picked = %q, want current custom model %q", picked, current)
	}
	values := make(map[string]bool, len(options))
	for _, opt := range options {
		values[opt.Value] = true
	}
	if !values[reviewModelDefaultSentinel] {
		t.Fatal("default model option missing")
	}
	if !values[current] {
		t.Fatalf("current custom model option %q missing", current)
	}
	if !values[reviewModelCustomSentinel] {
		t.Fatal("custom model option missing")
	}
}

package redact

import (
	"strings"
	"testing"
)

// TestJoinedPromptsLegacy_ParityWithStringPlusJoin pins down that the legacy
// (7-layer, no-OPF) constructor produces the same content as the pre-PR
// behavior of `redact.String(strings.Join(prompts, sep))`. The writer's
// safety net switched from running OPF-augmented redaction to the legacy
// variant; this parity check is what keeps existing post-commit checkpoint
// blobs byte-identical to the pre-PR output once OPF is gated to pre-push.
func TestJoinedPromptsLegacy_ParityWithStringPlusJoin(t *testing.T) {
	t.Parallel()
	const sep = "\n\n---\n\n"
	cases := []struct {
		name    string
		prompts []string
	}{
		{"single_simple", []string{"hello world"}},
		{"multi_simple", []string{"first prompt", "second prompt", "third prompt"}},
		{"with_high_entropy_secret", []string{"set API_KEY=" + highEntropySecret, "ok"}},
		{"with_db_password", []string{"connect to postgres://app:pwd123@db.example.com:5432/app", "done"}},
		{"single_with_email_no_pii_config", []string{"contact alice@example.com please"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			joined := strings.Join(tc.prompts, sep)
			want := String(joined)
			got := JoinedPromptsLegacy(tc.prompts, sep).String()
			if got != want {
				t.Errorf("parity mismatch:\n got=%q\nwant=%q", got, want)
			}
		})
	}
}

// TestJoinedPromptsLegacy_EmptyReturnsZeroValue verifies the constructor
// short-circuits an empty input to the zero value, so IsSet() reports
// false and the writer's safety net can re-run rather than persist an
// empty prompt.txt blob.
func TestJoinedPromptsLegacy_EmptyReturnsZeroValue(t *testing.T) {
	t.Parallel()
	if got := JoinedPromptsLegacy(nil, "sep"); got.IsSet() {
		t.Errorf("nil prompts: IsSet() = true, want false (content=%q)", got.String())
	}
	if got := JoinedPromptsLegacy([]string{}, "sep"); got.IsSet() {
		t.Errorf("empty prompts: IsSet() = true, want false (content=%q)", got.String())
	}
}

// TestJoinedPromptsLegacy_DoesNotInvokeOPF is the regression guard for the
// architectural promise: even with OPF configured globally, the legacy
// constructor MUST run only the 7-layer pipeline. Pairs with
// TestPlainEntryPointsNeverInvokeOPF in opf_test.go — that test pins
// String/Bytes/JSONLBytes; this one pins JoinedPromptsLegacy.
func TestJoinedPromptsLegacy_DoesNotInvokeOPF(t *testing.T) {
	resetOPFConfig()
	t.Cleanup(resetOPFConfig)

	fake := &fakeRuntime{spans: []Span{{Start: 0, End: 5, Label: "private_person"}}}
	ConfigurePrivacyFilterWithRuntime(OPFConfig{
		Enabled:    true,
		Categories: map[string]bool{"private_person": true},
	}, fake)

	_ = JoinedPromptsLegacy([]string{"Alice met Bob in the lobby", "Charlie was there too"}, "\n---\n")

	if fake.calls+fake.batchCalls != 0 {
		t.Errorf("legacy constructor invoked OPF: calls=%d batchCalls=%d", fake.calls, fake.batchCalls)
	}
}

package pi

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

var _ reviewtypes.AgentReviewer = (*reviewtypes.ReviewerTemplate)(nil)

func TestPiReviewer_NameMatchesRegistryKey(t *testing.T) {
	t.Parallel()
	if got := NewReviewer().Name(); got != string(agent.AgentNamePi) {
		t.Fatalf("Name() = %q, want %q", got, agent.AgentNamePi)
	}
}

func TestPiReviewer_BuildCmd(t *testing.T) {
	t.Parallel()
	cfg := reviewtypes.RunConfig{
		Model:        "anthropic/claude-sonnet-4-5:high",
		Task:         "Review the change.",
		AlwaysPrompt: "Focus on API regressions.",
		StartingSHA:  "abc123",
	}
	cmd := buildPiReviewCmd(context.Background(), cfg)

	if cmd.Args[0] != "pi" {
		t.Fatalf("Args[0] = %q, want pi; args=%v", cmd.Args[0], cmd.Args)
	}
	wantPrefix := []string{"pi", "--mode", "json", "--print", "--model", "anthropic/claude-sonnet-4-5:high"}
	if len(cmd.Args) != len(wantPrefix)+1 {
		t.Fatalf("args len = %d, want %d: %v", len(cmd.Args), len(wantPrefix)+1, cmd.Args)
	}
	for i, want := range wantPrefix {
		if cmd.Args[i] != want {
			t.Fatalf("Args[%d] = %q, want %q; args=%v", i, cmd.Args[i], want, cmd.Args)
		}
	}
	if prompt := cmd.Args[len(cmd.Args)-1]; !strings.Contains(prompt, "Review the change.") || !strings.Contains(prompt, "Focus on API regressions.") {
		t.Fatalf("prompt arg missing composed review content: %q", prompt)
	}

	env := envMap(cmd.Env)
	if env[review.EnvSession] != "1" {
		t.Errorf("%s = %q, want 1", review.EnvSession, env[review.EnvSession])
	}
	if env[review.EnvAgent] != string(agent.AgentNamePi) {
		t.Errorf("%s = %q, want %q", review.EnvAgent, env[review.EnvAgent], agent.AgentNamePi)
	}
	if env[review.EnvStartingSHA] != "abc123" {
		t.Errorf("%s = %q, want abc123", review.EnvStartingSHA, env[review.EnvStartingSHA])
	}
}

func TestPiReviewer_ParseJSONEventStream(t *testing.T) {
	t.Parallel()
	input := strings.Join([]string{
		`{"type":"session","version":3,"id":"s1","cwd":"/repo"}`,
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"message_update","message":{"role":"assistant"},"assistantMessageEvent":{"type":"text_delta","delta":"Finding "}}`,
		`{"type":"tool_execution_start","toolName":"bash","args":{"command":"git diff --stat"}}`,
		`{"type":"message_update","message":{"role":"assistant"},"assistantMessageEvent":{"type":"text_delta","delta":"one"}}`,
		`{"type":"message_end","message":{"role":"assistant","usage":{"input":10,"output":4,"cacheRead":2,"cacheWrite":3},"stopReason":"stop"}}`,
		`{"type":"agent_end","messages":[]}`,
	}, "\n")

	events := collectPiReviewEvents(input)
	if len(events) != 6 {
		t.Fatalf("events len = %d, want 6: %#v", len(events), events)
	}
	if _, ok := events[0].(reviewtypes.Started); !ok {
		t.Fatalf("events[0] = %T, want Started", events[0])
	}
	if got, ok := events[1].(reviewtypes.AssistantText); !ok || got.Text != "Finding " {
		t.Fatalf("events[1] = %#v, want AssistantText{Finding }", events[1])
	}
	tool, ok := events[2].(reviewtypes.ToolCall)
	if !ok || tool.Name != "bash" || !strings.Contains(tool.Args, "git diff --stat") {
		t.Fatalf("events[2] = %#v, want ToolCall(bash)", events[2])
	}
	if got, ok := events[3].(reviewtypes.AssistantText); !ok || got.Text != "one" {
		t.Fatalf("events[3] = %#v, want AssistantText{one}", events[3])
	}
	tokens, ok := events[4].(reviewtypes.Tokens)
	if !ok || tokens.In != 15 || tokens.Out != 4 {
		t.Fatalf("events[4] = %#v, want Tokens{In:15 Out:4}", events[4])
	}
	finished, ok := events[5].(reviewtypes.Finished)
	if !ok || !finished.Success {
		t.Fatalf("events[5] = %#v, want Finished{Success:true}", events[5])
	}
}

func TestPiReviewer_ParseMessageEndTextWithoutDeltas(t *testing.T) {
	t.Parallel()
	input := strings.Join([]string{
		`{"type":"agent_start"}`,
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Final review text"}],"stopReason":"stop"}}`,
		`{"type":"agent_end"}`,
	}, "\n")

	events := collectPiReviewEvents(input)
	var found bool
	for _, ev := range events {
		text, ok := ev.(reviewtypes.AssistantText)
		if ok && text.Text == "Final review text" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected AssistantText from message_end content, got %#v", events)
	}
}

func collectPiReviewEvents(input string) []reviewtypes.Event {
	ch := parsePiReviewOutput(strings.NewReader(input))
	var events []reviewtypes.Event
	for ev := range ch {
		events = append(events, ev)
	}
	return events
}

func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		idx := strings.IndexByte(kv, '=')
		if idx < 0 {
			continue
		}
		out[kv[:idx]] = kv[idx+1:]
	}
	return out
}

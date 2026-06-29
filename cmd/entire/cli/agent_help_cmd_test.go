package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// commandNames returns the Use-name of each command, for assertions.
func commandNames(cmds []*cobra.Command) []string {
	names := make([]string, 0, len(cmds))
	for _, c := range cmds {
		names = append(names, c.Name())
	}
	return names
}

func contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// agent-help advertises visible commands plus hidden commands that explicitly
// opt in via the agentHelpAnnotation (e.g. trail), but never plain-hidden
// commands or the help command itself.
func TestAgentHelpCommands_IncludesAnnotatedHiddenOnly(t *testing.T) {
	t.Parallel()

	root := &cobra.Command{Use: "entire"}
	root.AddCommand(&cobra.Command{Use: "status", Short: "Show status"})
	root.AddCommand(&cobra.Command{Use: "secret", Hidden: true})
	root.AddCommand(&cobra.Command{
		Use:         "trail",
		Short:       "Manage trails",
		Hidden:      true,
		Annotations: map[string]string{agentHelpAnnotation: "true"},
	})
	root.AddCommand(&cobra.Command{Use: "reset", Short: "old", Deprecated: "use clean"})

	got := commandNames(agentHelpCommands(root))

	if !contains(got, "status") {
		t.Errorf("expected visible command 'status' to be advertised, got %v", got)
	}
	if !contains(got, "trail") {
		t.Errorf("expected annotated-hidden command 'trail' to be advertised, got %v", got)
	}
	if contains(got, "secret") {
		t.Errorf("plain-hidden command 'secret' must not be advertised, got %v", got)
	}
	if contains(got, "help") {
		t.Errorf("help command must not be advertised, got %v", got)
	}
	if contains(got, "reset") {
		t.Errorf("deprecated command 'reset' must not be advertised, got %v", got)
	}
}

// Drilling into a command renders its description, its live flags (with their
// usage text), its subcommands, and the auto-detected repo line.
func TestRenderAgentHelpCommand_ShowsFlagsAndSubcommands(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{
		Use:   "trail",
		Short: "Manage trails for your branches",
		Long:  "A trail ties together the context for a branch.",
	}
	cmd.PersistentFlags().String("repo", "", "Target repository as forge/owner/repo; defaults to the origin remote")
	cmd.PersistentFlags().String("branch", "", "Branch to resolve the trail for; defaults to the current branch")
	cmd.PersistentFlags().Bool("insecure-http-auth", false, "internal")
	if err := cmd.PersistentFlags().MarkHidden("insecure-http-auth"); err != nil {
		t.Fatal(err)
	}
	cmd.AddCommand(&cobra.Command{Use: "show", Short: "Show a trail"})
	cmd.AddCommand(&cobra.Command{Use: "list", Short: "List trails"})

	out := renderAgentHelpCommand(cmd, "gh/acme/app")

	for _, want := range []string{
		"trail",
		"Manage trails for your branches",
		"--repo",
		"defaults to the origin remote", // live flag usage text
		"--branch",
		"show",
		"list",
		"gh/acme/app",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("agent-help command output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "insecure-http-auth") {
		t.Errorf("hidden flag must not be rendered:\n%s", out)
	}
}

// The top-level rendering lists the live command map (including the revealed
// trail command), states the auto-detected repo, and carries the standing rule.
func TestRenderAgentHelpTop_ListsCommandsRepoAndRule(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	out := renderAgentHelpTop(root, "gh/acme/app")

	for _, want := range []string{
		"trail",             // hidden but revealed via annotation
		"checkpoint",        // visible
		"status",            // visible
		"gh/acme/app",       // auto-detected repo
		"entire agent-help", // drill-down pointer
		"never ask",         // the standing repo-inference rule
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("agent-help top output missing %q:\n%s", want, out)
		}
	}
}

// runAgentHelp dispatches: no args -> top overview; a command path -> that
// command's drill-down; --json -> structured output; unknown path -> error.
func TestRunAgentHelp_Dispatch(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()

	top, err := runAgentHelp(root, nil, "gh/acme/app", false)
	if err != nil {
		t.Fatalf("top: unexpected error: %v", err)
	}
	if !strings.Contains(top, "When to use entire") || !strings.Contains(top, "trail") {
		t.Fatalf("top output unexpected:\n%s", top)
	}

	drill, err := runAgentHelp(root, []string{"trail"}, "gh/acme/app", false)
	if err != nil {
		t.Fatalf("drill: unexpected error: %v", err)
	}
	if !strings.Contains(drill, "Manage trails for your branches") || !strings.Contains(drill, "--repo") {
		t.Fatalf("drill output unexpected:\n%s", drill)
	}

	jsonOut, err := runAgentHelp(root, []string{"trail"}, "gh/acme/app", true)
	if err != nil {
		t.Fatalf("json: unexpected error: %v", err)
	}
	var parsed struct {
		Command string `json:"command"`
		Repo    string `json:"repo"`
		Flags   []struct {
			Name string `json:"name"`
		} `json:"flags"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &parsed); err != nil {
		t.Fatalf("json output not valid JSON: %v\n%s", err, jsonOut)
	}
	if parsed.Command != "entire trail" {
		t.Errorf("json command = %q, want %q", parsed.Command, "entire trail")
	}
	if parsed.Repo != "gh/acme/app" {
		t.Errorf("json repo = %q, want %q", parsed.Repo, "gh/acme/app")
	}
	var hasRepoFlag bool
	for _, f := range parsed.Flags {
		if f.Name == "repo" {
			hasRepoFlag = true
		}
	}
	if !hasRepoFlag {
		t.Errorf("json flags missing --repo: %s", jsonOut)
	}

	if _, err := runAgentHelp(root, []string{"definitely-not-a-command"}, "", false); err == nil {
		t.Errorf("expected error for unknown command path")
	}
}

// End-to-end through cobra Execute: the --json flag is parsed, the RunE closure
// runs, repo resolution degrades gracefully (temp dir has no origin), and output
// is written to OutOrStdout.
func TestAgentHelpCmd_Execute(t *testing.T) {
	t.Chdir(t.TempDir()) // no origin here -> repo line degrades gracefully; deterministic

	root := NewRootCmd()

	top := newAgentHelpCmd(root)
	var out bytes.Buffer
	top.SetOut(&out)
	top.SetErr(io.Discard)
	top.SetArgs(nil)
	if err := top.Execute(); err != nil {
		t.Fatalf("agent-help execute: %v", err)
	}
	for _, want := range []string{"When to use entire", "trail", "checkpoint"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("agent-help output missing %q:\n%s", want, out.String())
		}
	}

	drill := newAgentHelpCmd(root)
	var jbuf bytes.Buffer
	drill.SetOut(&jbuf)
	drill.SetErr(io.Discard)
	drill.SetArgs([]string{"trail", "--json"})
	if err := drill.Execute(); err != nil {
		t.Fatalf("agent-help trail --json execute: %v", err)
	}
	var parsed struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(jbuf.Bytes(), &parsed); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, jbuf.String())
	}
	if parsed.Command != "entire trail" {
		t.Errorf("json command = %q, want %q", parsed.Command, "entire trail")
	}
}

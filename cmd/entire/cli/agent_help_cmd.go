package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	flag "github.com/spf13/pflag"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
)

// agentHelpAnnotation marks an otherwise-hidden command as worth advertising to
// coding agents through `entire agent-help`. Hidden commands (e.g. trail) opt in
// by setting Annotations[agentHelpAnnotation] = "true".
const agentHelpAnnotation = "entire_agent_help"

// agentHelpOverview is the only hand-maintained prose in agent-help: a terse,
// high-level "what entire is for" plus the standing repo-inference rule. It names
// no flags or subcommands — those are rendered live from the installed command
// tree — so it changes only when a whole capability area lands, not when a flag
// is added.
const agentHelpOverview = `Entire's CLI is the source of truth for its own usage. Do not guess flags or
subcommands — read them from this command. You are already inside the repo:
entire auto-detects it from the git origin remote, so never ask the user for the
repo name. Pass --repo only to target a DIFFERENT repo.`

// newAgentHelpCmd builds the `entire agent-help` command. It is visible in
// `entire help` (so agents on transports without context injection can still
// find it) and renders agent-facing usage live from rootCmd's command tree.
func newAgentHelpCmd(rootCmd *cobra.Command) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "agent-help [command...]",
		Short: "Machine-readable usage for coding agents (always matches the installed CLI)",
		Long: `Prints agent-facing usage for the Entire CLI, generated live from the installed
command tree so it always matches this binary. With no arguments it prints a
high-level map of when to use entire and which subcommand; pass a command path
(e.g. "agent-help trail") to see that command's exact, current flags.`,
		DisableFlagParsing: false,
		RunE: func(c *cobra.Command, args []string) error {
			repoLine := agentHelpRepoLine(c.Context())
			out, err := runAgentHelp(rootCmd, args, repoLine, asJSON)
			if err != nil {
				return err
			}
			fmt.Fprint(c.OutOrStdout(), out)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit structured JSON instead of text")
	return cmd
}

// agentHelpRepoLine resolves the current repo as forge/owner/repo from the
// origin remote, returning "" when it can't be determined (no origin / detached
// HEAD) so the renderer degrades gracefully rather than erroring.
func agentHelpRepoLine(ctx context.Context) string {
	forge, owner, repo, err := gitremote.ResolveRemoteRepo(ctx, "origin")
	if err != nil || forge == "" || owner == "" || repo == "" {
		return ""
	}
	return strings.Join([]string{forge, owner, repo}, "/")
}

// runAgentHelp resolves args to a command node and renders it. It is pure (no
// git / IO): the caller passes the already-resolved repoLine.
func runAgentHelp(rootCmd *cobra.Command, args []string, repoLine string, asJSON bool) (string, error) {
	target := rootCmd
	for _, name := range args {
		child := agentHelpFindChild(target, name)
		if child == nil {
			return "", fmt.Errorf("unknown command %q; run `entire agent-help` for the list of commands", name)
		}
		target = child
	}
	if asJSON {
		return renderAgentHelpJSON(rootCmd, target, repoLine)
	}
	if target == rootCmd {
		return renderAgentHelpTop(rootCmd, repoLine), nil
	}
	return renderAgentHelpCommand(target, repoLine), nil
}

// agentHelpFindChild finds a direct child of parent by name or alias, including
// hidden commands (so drill-down into e.g. trail works).
func agentHelpFindChild(parent *cobra.Command, name string) *cobra.Command {
	for _, sub := range parent.Commands() {
		if sub.Name() == name {
			return sub
		}
		for _, alias := range sub.Aliases {
			if alias == name {
				return sub
			}
		}
	}
	return nil
}

type agentHelpFlagJSON struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand,omitempty"`
	Type      string `json:"type"`
	Default   string `json:"default,omitempty"`
	Usage     string `json:"usage"`
}

type agentHelpSubcommandJSON struct {
	Name  string `json:"name"`
	Short string `json:"short"`
}

type agentHelpJSON struct {
	Command     string                    `json:"command"`
	Short       string                    `json:"short,omitempty"`
	Long        string                    `json:"long,omitempty"`
	Repo        string                    `json:"repo,omitempty"`
	Flags       []agentHelpFlagJSON       `json:"flags,omitempty"`
	Subcommands []agentHelpSubcommandJSON `json:"subcommands,omitempty"`
}

// renderAgentHelpJSON renders the structured form of a command node.
func renderAgentHelpJSON(rootCmd, target *cobra.Command, repoLine string) (string, error) {
	doc := agentHelpJSON{
		Command: target.CommandPath(),
		Short:   target.Short,
		Long:    strings.TrimSpace(target.Long),
		Repo:    repoLine,
	}
	if target != rootCmd {
		collect := func(fs *flag.FlagSet) {
			fs.VisitAll(func(f *flag.Flag) {
				if f.Hidden {
					return
				}
				doc.Flags = append(doc.Flags, agentHelpFlagJSON{
					Name:      f.Name,
					Shorthand: f.Shorthand,
					Type:      f.Value.Type(),
					Default:   f.DefValue,
					Usage:     f.Usage,
				})
			})
		}
		collect(target.LocalFlags())
		collect(target.InheritedFlags())
	}
	for _, sub := range agentHelpCommands(target) {
		doc.Subcommands = append(doc.Subcommands, agentHelpSubcommandJSON{Name: sub.Name(), Short: sub.Short})
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal agent-help json: %w", err)
	}
	return string(b) + "\n", nil
}

// agentHelpCommands returns the child commands to advertise to agents: every
// visible command plus any hidden command that opts in via agentHelpAnnotation.
// The help command itself is never advertised.
func agentHelpCommands(parent *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	for _, sub := range parent.Commands() {
		if sub.Name() == "help" || sub.Deprecated != "" {
			continue
		}
		if sub.Hidden && sub.Annotations[agentHelpAnnotation] != "true" {
			continue
		}
		out = append(out, sub)
	}
	return out
}

// agentHelpRepoBlock formats the auto-detected repo line, degrading gracefully
// when the repo can't be resolved (no origin / detached HEAD) rather than
// implying a repo that isn't there.
func agentHelpRepoBlock(repoLine string) string {
	if strings.TrimSpace(repoLine) == "" {
		return "Current repo: not auto-detectable here (no origin remote / detached HEAD); pass --repo explicitly.\n"
	}
	return "Current repo: " + repoLine + "  (auto-detected from origin; pass --repo only for a DIFFERENT repo)\n"
}

// renderAgentHelpCommand renders one resolved command node for an agent: its
// path + Short, its Long description, the auto-detected repo line, its live flag
// usages (hidden flags are skipped by cobra), and its advertised subcommands.
func renderAgentHelpCommand(cmd *cobra.Command, repoLine string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s\n", cmd.CommandPath(), cmd.Short)
	if long := strings.TrimSpace(cmd.Long); long != "" && long != strings.TrimSpace(cmd.Short) {
		b.WriteString(long)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(agentHelpRepoBlock(repoLine))

	// LocalFlags()/InheritedFlags() trigger cobra's persistent-flag merge (plain
	// Flags() does not without Execute) and skip hidden flags in FlagUsages.
	if usages := strings.TrimRight(cmd.LocalFlags().FlagUsages(), "\n"); usages != "" {
		b.WriteString("\nFlags:\n")
		b.WriteString(usages)
		b.WriteString("\n")
	}
	if usages := strings.TrimRight(cmd.InheritedFlags().FlagUsages(), "\n"); usages != "" {
		b.WriteString("\nInherited flags:\n")
		b.WriteString(usages)
		b.WriteString("\n")
	}

	if subs := agentHelpCommands(cmd); len(subs) > 0 {
		names := make([]string, 0, len(subs))
		for _, sub := range subs {
			names = append(names, sub.Name())
		}
		fmt.Fprintf(&b, "\nSubcommands: %s\n", strings.Join(names, " · "))
		fmt.Fprintf(&b, "Next:  entire agent-help %s <subcommand>\n", strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" "))
	}
	return b.String()
}

// renderAgentHelpTop renders the top-level agent-facing overview: the curated
// intro + rule, the auto-detected repo line, and a live map of the advertised
// commands (their Short help), ending with the drill-down pointer.
func renderAgentHelpTop(rootCmd *cobra.Command, repoLine string) string {
	var b strings.Builder
	b.WriteString(agentHelpOverview)
	b.WriteString("\n\n")
	b.WriteString(agentHelpRepoBlock(repoLine))
	b.WriteString("\nWhen to use entire:\n")
	for _, sub := range agentHelpCommands(rootCmd) {
		fmt.Fprintf(&b, "  %-12s %s\n", sub.Name(), sub.Short)
	}
	b.WriteString("\nDrill in for exact, currently-installed flags:  entire agent-help <command>")
	b.WriteString("  (e.g. entire agent-help trail)\n")
	b.WriteString("Add --json for structured output.\n")
	return b.String()
}

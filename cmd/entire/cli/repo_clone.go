package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/internal/coreapi"
)

// mirrorCloneRefRe parses the clone-ref shape `entire repo clone` accepts:
// the `/gh/<owner>/<repo>` path of a mirror's clone URL, with or without the
// leading slash. owner/repo reuse the GitHub identifier charsets from
// parseGitHubURL so the same metacharacter vectors are closed at the boundary
// (owner/repo flow unescaped into the synthesised entire:// clone URL).
var mirrorCloneRefRe = regexp.MustCompile(`^/?gh/` + gitHubOwnerPat + `/` + gitHubRepoPat + `$`)

// parseMirrorCloneRef turns a clone ref like `/gh/entirehq/entire-api` into the
// API provider ("github") and the lowercased owner/repo. The `gh` token is the
// path provider used in entire:// clone URLs; it maps to the "github" upstream
// provider the control plane records.
func parseMirrorCloneRef(ref string) (provider, owner, repo string, err error) {
	m := mirrorCloneRefRe.FindStringSubmatch(strings.TrimSpace(ref))
	if m == nil {
		return "", "", "", fmt.Errorf("expected /gh/<owner>/<repo>, got %q", ref)
	}
	owner, repo = strings.ToLower(m[1]), strings.ToLower(m[2])
	if gitHubDotOnlyRe.MatchString(repo) {
		return "", "", "", fmt.Errorf("repo cannot be dot-only: %s", ref)
	}
	return checkpointProviderGitHub, owner, repo, nil
}

// newCloneAliasCmd is the top-level `entire clone` alias for `entire repo
// clone`. Cobra forbids attaching one command to two parents, so this builds a
// fresh clone command and adds the control-plane flags (--json,
// --insecure-http-auth) the repo group otherwise supplies to its children as
// persistent flags.
func newCloneAliasCmd() *cobra.Command {
	cmd := newRepoCloneCmd()
	addControlPlaneFlags(cmd)
	return cmd
}

func newRepoCloneCmd() *cobra.Command {
	var cluster string
	cmd := &cobra.Command{
		Use:   "clone <repo> [target-dir]",
		Short: "Clone a mirrored repository",
		Long: "Clone a GitHub mirror by its `/gh/<owner>/<repo>` ref. Looks up where " +
			"the repo is mirrored: if it's on a single cluster, clones it directly; " +
			"if it's mirrored on more than one, prompts you to pick which to clone " +
			"from (or pass --cluster to choose non-interactively). The optional " +
			"[target-dir] is passed straight through to `git clone`.",
		Example: "  entire repo clone /gh/entirehq/entire-api\n" +
			"  entire repo clone /gh/entirehq/entire-api ./entire-api\n" +
			"  entire repo clone /gh/entirehq/entire-api --cluster aws-us-east-2.entire.io",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			provider, owner, repo, err := parseMirrorCloneRef(args[0])
			if err != nil {
				return fmt.Errorf("invalid <repo>: %w", err)
			}
			var targetDir string
			if len(args) > 1 {
				targetDir = args[1]
			}

			var mirrors []coreapi.Mirror
			lister := func(ctx context.Context, c *coreapi.Client) error {
				ms, err := listMirrorsForRepo(ctx, c, provider, owner, repo)
				if err != nil {
					return err
				}
				mirrors = ms
				return nil
			}
			// An explicit --cluster may name a cluster in a different federation
			// than the active context, whose mirrors the active-context core can't
			// see (the original bug: cloning a royalcanin.partial.to mirror while a
			// different context is active failed with "not mirrored on ..."). Dial
			// the core fronting that cluster — discovered from its well-known and
			// authenticated with the matching local context, the same path
			// `mirror create <url> [cluster]` uses — so the lookup resolves against
			// the right federation. With no --cluster, list from the active context.
			runWithCore := runCore
			if cluster != "" {
				if err := validateClusterHost(cluster); err != nil {
					return fmt.Errorf("invalid --cluster: %w", err)
				}
				runWithCore = func(cmd *cobra.Command, fn func(context.Context, *coreapi.Client) error) error {
					return runCoreForCluster(cmd, cluster, fn)
				}
			}
			if err := runWithCore(cmd, lister); err != nil {
				return err
			}

			if len(mirrors) == 0 {
				return fmt.Errorf("no mirror found for gh/%s/%s; run 'entire repo mirror create github.com/%s/%s' to onboard it", owner, repo, owner, repo)
			}

			chosen, err := selectCloneTarget(cmd, mirrors, cluster)
			if err != nil {
				return err
			}

			cloneURL := fmt.Sprintf("entire://%s/gh/%s/%s", chosen.ClusterHost, owner, repo)
			return runGitClone(cmd.Context(), cmd, cloneURL, targetDir)
		},
	}
	cmd.Flags().StringVar(&cluster, "cluster", "", "cluster host to clone from when the repo is mirrored on more than one (may belong to another auth context)")
	return cmd
}

// listMirrorsForRepo returns every mirror placement of one upstream repo across
// clusters. The list API filters by provider+owner server-side but has no repo
// filter, so the repo match is applied client-side (owner is already lowercased
// to match what the server persists).
func listMirrorsForRepo(ctx context.Context, c *coreapi.Client, provider, owner, repo string) ([]coreapi.Mirror, error) {
	all, err := fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]coreapi.Mirror, string, error) {
		params := coreapi.ListMirrorsParams{
			Provider: coreapi.NewOptString(provider),
			Owner:    coreapi.NewOptString(owner),
		}
		if cursor != "" {
			params.PageToken = coreapi.NewOptString(cursor)
		}
		out, err := c.ListMirrors(ctx, params)
		if err != nil {
			return nil, "", err
		}
		return out.Mirrors, out.NextPageToken.Or(""), nil
	})
	if err != nil {
		return nil, err
	}
	matched := make([]coreapi.Mirror, 0, len(all))
	for _, m := range all {
		if strings.EqualFold(m.Repo, repo) {
			matched = append(matched, m)
		}
	}
	return matched, nil
}

// selectCloneTarget resolves which mirror placement to clone from. With one
// placement it returns it directly. With --cluster it picks the matching one (or
// errors listing the available hosts). With more than one and no flag it prompts
// interactively, failing fast with a --cluster pointer when there's no terminal.
func selectCloneTarget(cmd *cobra.Command, mirrors []coreapi.Mirror, clusterFlag string) (coreapi.Mirror, error) {
	// Dedupe by cluster host: one placement per cluster is what a clone targets,
	// and the same host appearing twice would only confuse the picker.
	byHost := make(map[string]coreapi.Mirror, len(mirrors))
	hosts := make([]string, 0, len(mirrors))
	for _, m := range mirrors {
		if _, seen := byHost[m.ClusterHost]; seen {
			continue
		}
		byHost[m.ClusterHost] = m
		hosts = append(hosts, m.ClusterHost)
	}
	sort.Strings(hosts)

	if clusterFlag != "" {
		m, ok := byHost[clusterFlag]
		if !ok {
			return coreapi.Mirror{}, fmt.Errorf("repo is not mirrored on %q; available: %s", clusterFlag, strings.Join(hosts, ", "))
		}
		return m, nil
	}

	if len(hosts) == 1 {
		return byHost[hosts[0]], nil
	}

	if !interactive.CanPromptInteractively() {
		return coreapi.Mirror{}, fmt.Errorf("repo is mirrored on %d clusters; pass --cluster to choose one of: %s", len(hosts), strings.Join(hosts, ", "))
	}

	options := make([]huh.Option[string], len(hosts))
	for i, h := range hosts {
		options[i] = huh.NewOption(mirrorCellLabel(byHost[h]), h)
	}
	var selected string
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("This repo is mirrored on more than one cluster — pick one to clone from").
				Options(options...).
				Value(&selected),
		),
	)
	if err := form.RunWithContext(cmd.Context()); err != nil {
		// handleFormCancellation prints "Clone cancelled." and returns nil for a
		// Ctrl+C / cancelled-context abort. Surface that as a SilentError so the
		// caller stops instead of falling through to clone a zero-value target
		// (the `entire:///gh/...` empty-host bug); a real form error propagates.
		if cerr := handleFormCancellation(cmd.ErrOrStderr(), "Clone", err); cerr != nil {
			return coreapi.Mirror{}, cerr
		}
		return coreapi.Mirror{}, NewSilentError(errors.New("clone cancelled"))
	}
	m, ok := byHost[selected]
	if !ok {
		return coreapi.Mirror{}, NewSilentError(errors.New("clone cancelled"))
	}
	return m, nil
}

// mirrorCellLabel is the human label for a mirror placement in the clone picker:
// the physical cell and jurisdiction when known, always anchored by the cluster
// host that goes into the clone URL.
func mirrorCellLabel(m coreapi.Mirror) string {
	cell := strings.TrimSpace(m.Cell.Or(""))
	jur := strings.TrimSpace(m.Jurisdiction.Or(""))
	switch {
	case cell != "" && jur != "":
		return fmt.Sprintf("%s (%s) — %s", cell, jur, m.ClusterHost)
	case cell != "":
		return fmt.Sprintf("%s — %s", cell, m.ClusterHost)
	default:
		return m.ClusterHost
	}
}

// runGitClone shells out to `git clone <cloneURL> [target-dir]`, wiring the
// child's stdio through so git-remote-entire's auth prompts and clone progress
// reach the user. A clone failure is wrapped as a SilentError: git already
// printed its own diagnostics, so main.go shouldn't reprint the wrapper.
func runGitClone(ctx context.Context, cmd *cobra.Command, cloneURL, targetDir string) error {
	args := []string{"clone", cloneURL}
	if targetDir != "" {
		args = append(args, targetDir)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Cloning %s\n", cloneURL)
	gitCmd := exec.CommandContext(ctx, "git", args...)
	gitCmd.Stdin = cmd.InOrStdin()
	gitCmd.Stdout = cmd.OutOrStdout()
	gitCmd.Stderr = cmd.ErrOrStderr()
	if err := gitCmd.Run(); err != nil {
		return NewSilentError(fmt.Errorf("git clone failed: %w", err))
	}
	return nil
}

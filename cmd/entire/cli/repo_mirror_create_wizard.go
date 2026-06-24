package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/coreapi"
)

// mirrorCreateConcurrency bounds how many (repo, region) mirror creations run
// at once. The slow phase is the per-mirror clone wait, which is I/O-bound on
// the cluster, so a modest fan-out keeps the wizard responsive without
// hammering the control plane or the user's STS.
const mirrorCreateConcurrency = 8

// Per-mirror outcome labels shown in the results table's STATUS column.
const (
	mirrorStatusReady      = "ready"      // clone landed, ready to use
	mirrorStatusRegistered = "registered" // placement created, clone in progress (--no-wait)
	mirrorStatusEmpty      = "empty"      // upstream has no commits, nothing to clone
	mirrorStatusSuspended  = "suspended"  // placement exists but the cluster won't issue clone tokens
	mirrorStatusTimedOut   = "timed out"  // clone didn't finish within --wait-timeout
	mirrorStatusError      = "error"      // create or probe failed
)

// regionChoice is one mirrorable region offered by the create wizard's region
// picker, sourced from the control plane's cluster catalog
// (GET /api/v1/clusters via availableRegions).
type regionChoice struct {
	slug         string
	jurisdiction string
	host         string // bare cluster host passed to CreateMirror / validateClusterHost
	isDefault    bool
}

// availableRegions lists the data-plane clusters the user may mirror into,
// fetched from the control plane's cluster catalog. A cluster whose advertised
// public URL can't be safely reduced to a bare host (hostFromPublicURL) is
// skipped rather than failing the whole wizard, so a single malformed entry
// can't block onboarding into the others.
func availableRegions(ctx context.Context, c *coreapi.Client) ([]regionChoice, error) {
	out, err := c.ListClusters(ctx)
	if err != nil {
		return nil, renderCoreError(err)
	}
	return clustersToRegions(out.Clusters), nil
}

// clustersToRegions maps the catalog's clusters to picker choices, dropping any
// whose advertised public URL can't be safely reduced to a bare host.
func clustersToRegions(clusters []coreapi.Cluster) []regionChoice {
	regions := make([]regionChoice, 0, len(clusters))
	for _, cl := range clusters {
		host, herr := hostFromPublicURL(cl.PublicUrl)
		if herr != nil {
			continue
		}
		regions = append(regions, regionChoice{
			slug:         cl.Slug,
			jurisdiction: cl.Jurisdiction,
			host:         host,
			isDefault:    cl.IsDefault,
		})
	}
	return regions
}

// hostFromPublicURL extracts the bare cluster host from a cluster's public_url
// (with or without a scheme) and runs it through validateClusterHost, the same
// anti-token-leak guard the positional <cluster-host> arg uses. Kept separate
// so the ListClusters → regionChoice mapping is unit-testable without a live
// catalog.
func hostFromPublicURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("empty public_url")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("parse public_url %q: %w", raw, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("public_url %q has no host", raw)
	}
	// Reject anything beyond scheme://host[:port]. url.Parse demotes the
	// `host@evil.com` userinfo trick into u.User (leaving u.Host=evil.com) and
	// stashes a trailing path in u.Path, neither of which validateClusterHost
	// would otherwise see. Same belt-and-suspenders the positional <cluster-host>
	// arg gets — the host flows into clone URLs and the STS audience.
	if u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("public_url %q must be scheme://host[:port] only", raw)
	}
	if err := validateClusterHost(u.Host); err != nil {
		return "", err
	}
	return u.Host, nil
}

// selectableAvailableRepos narrows the ListAvailableMirrors result to repos the
// wizard should offer: status "available" (not already mirrored or owner-only)
// with write or admin access (read-only can't be onboarded). Sorted by
// owner/repo for a stable picker order.
func selectableAvailableRepos(avail []coreapi.AvailableMirror) []coreapi.AvailableMirror {
	out := make([]coreapi.AvailableMirror, 0, len(avail))
	for _, m := range avail {
		if m.Status != coreapi.AvailableMirrorStatusAvailable {
			continue
		}
		if m.Access != coreapi.AvailableMirrorAccessWrite && m.Access != coreapi.AvailableMirrorAccessAdmin {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Owner != out[j].Owner {
			return out[i].Owner < out[j].Owner
		}
		return out[i].Repo < out[j].Repo
	})
	return out
}

// clusterChoices maps regions to multi-select options (value = bare host) and
// the hosts that should start checked (the is-default regions).
func clusterChoices(regions []regionChoice) (opts []huh.Option[string], defaults []string) {
	opts = make([]huh.Option[string], 0, len(regions))
	for _, r := range regions {
		opts = append(opts, huh.NewOption(regionLabel(r), r.host))
		if r.isDefault {
			defaults = append(defaults, r.host)
		}
	}
	return opts, defaults
}

// regionLabel is the human label for a region in the picker and the results
// table: "slug (jurisdiction)" when both are known, else whatever identifier we
// have, falling back to the bare host.
func regionLabel(r regionChoice) string {
	switch {
	case r.slug != "" && r.jurisdiction != "":
		return fmt.Sprintf("%s (%s)", r.slug, r.jurisdiction)
	case r.slug != "":
		return r.slug
	default:
		return r.host
	}
}

// mirrorTarget is one unit of work: a selected repo to be mirrored into a
// selected region. The wizard creates the cross-product of repos × regions.
type mirrorTarget struct {
	owner  string
	repo   string
	region regionChoice
}

// mirrorTargets expands the selected repos and regions into the full
// cross-product of (repo, region) pairs.
func mirrorTargets(repos []coreapi.AvailableMirror, regions []regionChoice) []mirrorTarget {
	targets := make([]mirrorTarget, 0, len(repos)*len(regions))
	for _, r := range repos {
		for _, reg := range regions {
			targets = append(targets, mirrorTarget{owner: r.Owner, repo: r.Repo, region: reg})
		}
	}
	return targets
}

// mirrorResult is the outcome of creating one (repo, region) mirror.
type mirrorResult struct {
	owner       string
	repo        string
	regionLabel string
	cloneURL    string
	status      string // ready | registered | empty | suspended | timed out | error
	err         error
}

var mirrorCreateResultColumns = []string{"REPO", "REGION", "STATUS", "CLONE URL"}

func mirrorCreateResultRow(r mirrorResult) []string {
	url := r.cloneURL
	if url == "" {
		url = placeholderDash
	}
	return []string{r.owner + "/" + r.repo, r.regionLabel, r.status, url}
}

// runMirrorCreateWizard is the zero-argument `entire repo mirror create` flow:
// verify auth, pick repos, pick regions, then create the cross-product of
// mirrors in parallel and report the clone URLs. noWait/waitTimeout carry the
// same meaning as the positional-arg create path.
func runMirrorCreateWizard(cmd *cobra.Command, noWait bool, waitTimeout time.Duration) error {
	cmd.SilenceUsage = true
	ctx := cmd.Context()
	outW := cmd.OutOrStdout()
	errW := cmd.ErrOrStderr()

	insecure := insecureHTTPRequested(cmd)
	if insecure {
		auth.EnableInsecureHTTP()
	}

	if err := ensureMirrorWizardAuth(ctx, errW, insecure); err != nil {
		return err
	}

	client, err := coreapi.New()
	if err != nil {
		return fmt.Errorf("connect to Entire control plane: %w", err)
	}

	// --- pick repos ---------------------------------------------------------
	stopRepos := startSpinner(errW, "Fetching available repos")
	avail, err := client.ListAvailableMirrors(ctx, coreapi.ListAvailableMirrorsParams{})
	if err != nil {
		stopRepos(false)
		return renderCoreError(err)
	}
	stopRepos(true)
	repos := selectableAvailableRepos(avail.Available)
	if len(repos) == 0 {
		fmt.Fprintln(errW, "No GitHub repos available to mirror (you need write access to a repo that isn't mirrored yet).")
		fmt.Fprintln(errW, "Run 'entire repo mirror list --show-available' to see what's onboardable.")
		return nil
	}
	selectedRepos, err := pickRepos(outW, repos)
	if err != nil || len(selectedRepos) == 0 {
		return err
	}

	// --- pick regions -------------------------------------------------------
	stopRegions := startSpinner(errW, "Fetching regions")
	regions, err := availableRegions(ctx, client)
	if err != nil {
		stopRegions(false)
		return fmt.Errorf("list regions: %w", err)
	}
	stopRegions(true)
	if len(regions) == 0 {
		return errors.New("no regions available to mirror into")
	}
	selectedRegions, err := pickRegions(outW, regions)
	if err != nil || len(selectedRegions) == 0 {
		return err
	}

	// --- create + poll ------------------------------------------------------
	targets := mirrorTargets(selectedRepos, selectedRegions)
	results := createMirrors(ctx, errW, targets, noWait, waitTimeout)

	return reportMirrorResults(outW, errW, results)
}

// ensureMirrorWizardAuth mirrors `entire auth status`: resolve the active
// target (honouring ENTIRE_TOKEN), enforce TLS on the core we'll dial, and
// validate the token with a /me probe so the wizard fails fast with a re-login
// hint rather than deep inside the first API call.
func ensureMirrorWizardAuth(ctx context.Context, errW io.Writer, insecure bool) error {
	target, err := resolveAuthStatusTarget(ctx, auth.Contexts, auth.RefreshedLoginToken)
	if err != nil {
		return err
	}
	if target.token == "" {
		fmt.Fprintln(errW, "Not logged in. Run 'entire login' to authenticate.")
		return NewSilentError(errors.New("not logged in"))
	}
	if !insecure && target.coreURL != "" {
		if err := api.RequireSecureURL(target.coreURL); err != nil {
			return fmt.Errorf("login server URL check: %w", err)
		}
	}
	profile, err := defaultFetchProfile(ctx, target.coreURL, target.token)
	if err != nil {
		if isKeychainTokenRejected(err) {
			fmt.Fprintf(errW, "Login for %s is no longer valid. Run 'entire login' to re-authenticate.\n", target.coreURL)
			return NewSilentError(errors.New("login no longer valid"))
		}
		return fmt.Errorf("validate auth: %w", err)
	}
	fmt.Fprintf(errW, "Signed in as %s via %s\n", profile.Handle, target.coreURL)
	return nil
}

// pickRepos runs the repo multi-select and returns the chosen available
// mirrors. A clean cancel (Ctrl+C) returns (nil, nil).
func pickRepos(w io.Writer, repos []coreapi.AvailableMirror) ([]coreapi.AvailableMirror, error) {
	repoByKey := make(map[string]coreapi.AvailableMirror, len(repos))
	options := make([]huh.Option[string], len(repos))
	for i, m := range repos {
		key := m.Owner + "/" + m.Repo
		repoByKey[key] = m
		options[i] = huh.NewOption(key, key)
	}

	var selected []string
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Select repos to mirror").
				Description("Space to select, enter to confirm.").
				Options(options...).
				Validate(func(s []string) error {
					if len(s) == 0 {
						return errors.New("select at least one repo")
					}
					return nil
				}).
				Value(&selected),
		),
	)
	if err := form.Run(); err != nil {
		return nil, handleFormCancellation(w, "Mirror create", err)
	}

	chosen := make([]coreapi.AvailableMirror, 0, len(selected))
	for _, key := range selected {
		if m, ok := repoByKey[key]; ok {
			chosen = append(chosen, m)
		}
	}
	return chosen, nil
}

// pickRegions runs the region multi-select, pre-selecting the default
// region(s). A clean cancel returns (nil, nil).
func pickRegions(w io.Writer, regions []regionChoice) ([]regionChoice, error) {
	opts, defaults := clusterChoices(regions)
	regionByHost := make(map[string]regionChoice, len(regions))
	for _, r := range regions {
		regionByHost[r.host] = r
	}

	// Pre-fill with the default hosts so they start checked.
	selected := append([]string(nil), defaults...)
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Select regions to mirror into").
				Description("Each selected repo is mirrored into every selected region. Space to select, enter to confirm.").
				Options(opts...).
				Validate(func(s []string) error {
					if len(s) == 0 {
						return errors.New("select at least one region")
					}
					return nil
				}).
				Value(&selected),
		),
	)
	if err := form.Run(); err != nil {
		return nil, handleFormCancellation(w, "Mirror create", err)
	}

	chosen := make([]regionChoice, 0, len(selected))
	for _, host := range selected {
		if r, ok := regionByHost[host]; ok {
			chosen = append(chosen, r)
		}
	}
	return chosen, nil
}

// createMirrors fans out CreateMirror (and the clone-readiness poll) across all
// targets in parallel, returning one result per target in input order. One
// cluster client is built per region and shared across that region's repos; a
// region the active login can't reach fails every pair in that region rather
// than aborting the whole run.
func createMirrors(ctx context.Context, errW io.Writer, targets []mirrorTarget, noWait bool, waitTimeout time.Duration) []mirrorResult {
	// One client per distinct region, built once.
	clientByHost := make(map[string]*coreapi.Client)
	clientErrByHost := make(map[string]error)
	for _, t := range targets {
		if _, seen := clientByHost[t.region.host]; seen {
			continue
		}
		if _, seen := clientErrByHost[t.region.host]; seen {
			continue
		}
		c, err := coreapi.NewForCluster(ctx, t.region.host)
		if err != nil {
			clientErrByHost[t.region.host] = err
		} else {
			clientByHost[t.region.host] = c
		}
	}

	stop := startSpinner(errW, fmt.Sprintf("Creating %d mirror(s)…", len(targets)))
	results := make([]mirrorResult, len(targets))
	g := new(errgroup.Group)
	g.SetLimit(mirrorCreateConcurrency)
	for i, t := range targets {
		g.Go(func() error {
			results[i] = createOneMirror(ctx, t, clientByHost[t.region.host], clientErrByHost[t.region.host], noWait, waitTimeout)
			return nil
		})
	}
	// createOneMirror folds every failure into results and never returns an
	// error, so Wait is structurally always nil; check it anyway to satisfy
	// errcheck and stay correct if that invariant ever changes.
	if err := g.Wait(); err != nil {
		fmt.Fprintf(errW, "mirror creation: %v\n", err)
	}
	stop(true)
	return results
}

// createOneMirror registers a single (repo, region) mirror and, unless noWait
// or the upstream is empty, waits for its initial clone. It never returns an
// error: every outcome is folded into the mirrorResult so a single failure
// can't sink the batch.
func createOneMirror(ctx context.Context, t mirrorTarget, c *coreapi.Client, clientErr error, noWait bool, waitTimeout time.Duration) mirrorResult {
	res := mirrorResult{owner: t.owner, repo: t.repo, regionLabel: regionLabel(t.region)}
	if clientErr != nil {
		res.status, res.err = mirrorStatusError, clientErr
		return res
	}
	created, err := c.CreateMirror(ctx, &coreapi.CreateMirrorInputBody{
		Provider:    coreapi.CreateMirrorInputBodyProviderGithub,
		Owner:       t.owner,
		Repo:        t.repo,
		ClusterHost: t.region.host,
	})
	if err != nil {
		res.status, res.err = mirrorStatusError, renderCoreError(err)
		return res
	}
	res.cloneURL = created.MirrorUrl

	switch {
	case created.Empty:
		res.status = mirrorStatusEmpty
	case noWait:
		res.status = mirrorStatusRegistered
	default:
		// Discard the per-mirror heartbeat: concurrent waits would interleave
		// their dots on a shared writer. The aggregate spinner shows liveness.
		werr := waitForMirrorClone(ctx, io.Discard, t.region.host, t.owner, t.repo, waitTimeout)
		switch {
		case werr == nil:
			res.status = mirrorStatusReady
		case errors.Is(werr, auth.ErrRepoTargetUnknown):
			res.status, res.err = mirrorStatusSuspended, werr
		case errors.Is(werr, context.DeadlineExceeded):
			res.status, res.err = mirrorStatusTimedOut, werr
		default:
			res.status, res.err = mirrorStatusError, werr
		}
	}
	return res
}

// reportMirrorResults renders the results table, a copy-pasteable git-clone
// block for the ready mirrors, and per-failure detail. It returns a
// SilentError (so the table isn't reprinted) when any mirror failed, giving the
// command a non-zero exit while still showing what succeeded.
func reportMirrorResults(outW, errW io.Writer, results []mirrorResult) error {
	if len(results) == 0 {
		return nil
	}
	if err := printTable(outW, mirrorCreateResultColumns, results, mirrorCreateResultRow); err != nil {
		return err
	}

	var readyURLs []string
	var failures int
	for _, r := range results {
		if r.status == mirrorStatusReady && r.cloneURL != "" {
			readyURLs = append(readyURLs, r.cloneURL)
		}
		if r.err != nil {
			failures++
		}
	}
	if len(readyURLs) > 0 {
		fmt.Fprintln(outW, "\nClone them:")
		for _, u := range readyURLs {
			fmt.Fprintf(outW, "  git clone %s\n", u)
		}
	}
	if failures > 0 {
		for _, r := range results {
			if r.err != nil {
				fmt.Fprintf(errW, "%s/%s @ %s: %v\n", r.owner, r.repo, r.regionLabel, r.err)
			}
		}
		return NewSilentError(fmt.Errorf("%d mirror(s) failed", failures))
	}
	return nil
}

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-git/go-git/v6/plumbing/protocol/packp"

	"github.com/entireio/cli/cmd/entire/cli/auth"
)

// gitHubHTTPSRe / gitHubSSHRe / gitHubBareRe parse the GitHub URL shapes
// `mirror create`/`remove` accept, mirroring the standalone entiredb CLI:
//
//	https://github.com/<owner>/<repo>(.git)
//	git@github.com:<owner>/<repo>(.git)
//	(github.com/)<owner>/<repo>
//
// owner/repo are lowercased so the synthesised /gh/<owner>/<repo> slug
// matches what the server persists.
//
// The owner/repo capture groups are restricted to GitHub's real identifier
// charset rather than a permissive "anything but slash". owner/repo flow
// unescaped into the STS audience (auth.RepoScopedToken) and the smart-HTTP
// probe URL (waitForMirrorClone); a loose pattern would admit ?, #, %, .. and
// control chars, letting a name like `repo?bypass=1` smuggle a query string
// or `repo#x` truncate the path. GitHub owners are [A-Za-z0-9-] and repos are
// [A-Za-z0-9._-], so matching upstream reality closes those vectors at the
// boundary instead of relying on whatever the server does with weird strings.
const (
	gitHubOwnerPat = `([A-Za-z0-9-]+)`
	gitHubRepoPat  = `([A-Za-z0-9._-]+?)`
)

var (
	gitHubHTTPSRe = regexp.MustCompile(`^https?://github\.com/` + gitHubOwnerPat + `/` + gitHubRepoPat + `(?:\.git)?$`)
	gitHubSSHRe   = regexp.MustCompile(`^git@github\.com:` + gitHubOwnerPat + `/` + gitHubRepoPat + `(?:\.git)?$`)
	gitHubBareRe  = regexp.MustCompile(`^(?:github\.com/)?` + gitHubOwnerPat + `/` + gitHubRepoPat + `(?:\.git)?$`)

	// gitHubDotOnlyRe matches repo segments that are entirely dots
	// (".", "..", ...). The tightened owner charset already excludes
	// dots, but gitHubRepoPat allows ".", and a dot-only repo name would
	// embed a literal ".." in both /gh/<owner>/<repo> and the
	// token-exchange audience. Reject at the boundary.
	gitHubDotOnlyRe = regexp.MustCompile(`^\.+$`)
)

func parseGitHubURL(rawURL string) (owner, repo string, err error) {
	for _, re := range []*regexp.Regexp{gitHubHTTPSRe, gitHubSSHRe, gitHubBareRe} {
		m := re.FindStringSubmatch(rawURL)
		if m == nil {
			continue
		}
		owner, repo = strings.ToLower(m[1]), strings.ToLower(m[2])
		if gitHubDotOnlyRe.MatchString(repo) {
			return "", "", fmt.Errorf("invalid GitHub URL: repo cannot be dot-only: %s", rawURL)
		}
		return owner, repo, nil
	}
	return "", "", fmt.Errorf("not a recognized GitHub URL: %s", rawURL)
}

// probeInterval is the cadence between info/refs probes during the
// clone wait. minReauthInterval floors how often we'll re-mint a
// repo-scoped token after a 401: STS rate-limits or auth flapping
// during a long wait shouldn't be amplified by the 2s probe cadence.
const (
	probeInterval     = 2 * time.Second
	minReauthInterval = 30 * time.Second
)

// maxProbeBytes bounds the smart-HTTP info/refs body read so a
// pathological or misbehaving server can't make us allocate without
// limit. Real ref advertisements for the largest repos sit well under
// this; the cap is sized for headroom, not snugness.
const maxProbeBytes = 8 << 20 // 8 MiB

// probeClient is the HTTP client used for the clone-readiness loop. It is
// purpose-built (own Transport, explicit timeouts, no redirect surprises)
// instead of http.DefaultClient: DefaultClient is process-global and can
// be poisoned by other code, and its zero-Timeout default would let a
// stuck connection consume one ticker slot indefinitely. Each probe is
// already context-bound by the loop's deadline, but Timeout is a belt to
// the context's braces for the connection/TLS phase.
var probeClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		// One mirror, repeated probes — a tiny idle pool is enough to
		// reuse the TLS session between ticks without leaking conns.
		MaxIdleConns:        2,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	},
	// info/refs is a single GET; a redirect would indicate misconfiguration
	// (e.g. cluster host fronted by something that 30x's to a login page).
	// Surface it as an error rather than silently following.
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// waitForMirrorClone blocks until the mirror at /gh/<owner>/<repo> on
// clusterHost advertises a resolvable HEAD (the initial GitHub→EntireDB
// clone has landed) or the deadline expires. It probes the data plane's
// smart-HTTP info/refs endpoint every 2s, printing a heartbeat so a long
// clone doesn't look hung.
//
// Repo-scoped pull tokens are short-lived (minutes) while the default wait
// is 30m, so a single token can't cover the whole wait. The loop re-mints
// from the (long-lived) login token whenever a probe comes back 401,
// rather than minting once up front. Re-mints are floored at
// minReauthInterval so a flapping STS can't be amplified into a re-mint
// every probeInterval ticks.
func waitForMirrorClone(ctx context.Context, out io.Writer, clusterHost, owner, repo string, timeout time.Duration) error {
	repoSlug := "/gh/" + owner + "/" + repo
	checkURL := fmt.Sprintf("https://%s%s/info/refs?service=git-upload-pack", clusterHost, repoSlug)

	mintToken := func() (string, error) {
		return auth.RepoScopedToken(ctx, "https://"+clusterHost, repoSlug, "pull")
	}
	token, err := mintToken()
	if err != nil {
		return fmt.Errorf("authorize clone probe: %w", err)
	}
	lastMint := time.Now()

	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()

	cloning := false
	for {
		ready, status := mirrorAdvertisesHead(ctx, probeClient, checkURL, token)
		switch {
		case ready:
			if cloning {
				fmt.Fprintln(out, " ready")
			} else {
				fmt.Fprintln(out, "  ready to use")
			}
			return nil
		case status == http.StatusUnauthorized:
			// The repo-scoped token expired mid-wait; mint a fresh one from
			// the login token (no dot, since this is a token refresh, not
			// clone progress). Skip the re-mint if we just refreshed —
			// otherwise an STS hiccup that 401s every probe would mint on
			// every probeInterval tick.
			if since := time.Since(lastMint); since < minReauthInterval {
				break
			}
			newToken, mintErr := mintToken()
			if mintErr != nil {
				if cloning {
					fmt.Fprintln(out)
				}
				return fmt.Errorf("re-authorize clone probe: %w", mintErr)
			}
			token = newToken
			lastMint = time.Now()
		default:
			if !cloning {
				fmt.Fprint(out, "  cloning")
				cloning = true
			}
			fmt.Fprint(out, ".")
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(out)
			// User cancellation (Ctrl+C) should exit quietly, not print a
			// "timed out…: context canceled" line. NewSilentError signals
			// main.go to skip printing; a real deadline still reports.
			if errors.Is(ctx.Err(), context.Canceled) {
				return NewSilentError(ctx.Err())
			}
			return fmt.Errorf("timed out waiting for initial clone: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// mirrorAdvertisesHead fetches the smart-HTTP ref advertisement and reports
// whether HEAD resolves to a real commit, plus the HTTP status so the
// caller can distinguish an expired token (401, re-mint) from a
// still-cloning mirror (200 with no resolvable HEAD, or 404/503). status is
// 0 for transport/build/decode failures, treated as "not ready, keep
// waiting". Auth is the repo-scoped token as HTTP basic-auth password, the
// same shape git presents over the entire:// transport.
func mirrorAdvertisesHead(ctx context.Context, client *http.Client, checkURL, token string) (ready bool, status int) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checkURL, nil)
	if err != nil {
		return false, 0
	}
	req.SetBasicAuth("entire-cli", token)
	resp, err := client.Do(req)
	if err != nil {
		return false, 0
	}
	// Drain before close so the transport can return the connection to the
	// idle pool and reuse the TLS session on the next tick. Go only recycles
	// a conn whose body was read to EOF before Close, and the Decode error
	// returns below leave the body partially read. Drain to EOF, uncapped:
	// the maxProbeBytes cap on the read below bounds the Decode *allocation*,
	// but draining to io.Discard is O(1) memory, and a LimitReader cap
	// shorter than the body would stop before EOF and silently defeat reuse.
	// The client Timeout still bounds how long the drain can run.
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // best-effort drain to enable conn reuse; copy errors are irrelevant
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return false, resp.StatusCode
	}
	// Cap the body to prevent unbounded reads; ogen's api/client.go does
	// the same for JSON. Smart-HTTP wraps the advertisement in a "#
	// service=..." pkt-line header + flush; AdvRefs.Decode expects to
	// start at the first ref line, so strip the wrapper first.
	body := io.LimitReader(resp.Body, maxProbeBytes)
	var sr packp.SmartReply
	if err := sr.Decode(body); err != nil {
		return false, 0
	}
	var adv packp.AdvRefs
	if err := adv.Decode(body); err != nil {
		return false, 0
	}
	if _, err := adv.ResolvedHead(); err != nil {
		return false, http.StatusOK // reachable, clone still in progress
	}
	return true, http.StatusOK
}

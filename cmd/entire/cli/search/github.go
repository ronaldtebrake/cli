// Package search provides search functionality via the Entire search service.
package search

import (
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
)

// ParseGitHubRemote extracts owner and repo from a git remote URL that resolves
// to GitHub. It accepts direct GitHub remotes (SCP-style SSH, ssh://, and
// https://) as well as Entire mirror remotes (entire://host/gh/owner/repo),
// whose forge prefix maps back to github.com. Remotes resolving to any other
// host are rejected.
func ParseGitHubRemote(remoteURL string) (owner, repo string, err error) {
	info, err := gitremote.ParseURL(remoteURL)
	if err != nil {
		return "", "", fmt.Errorf("parsing remote URL: %w", err)
	}
	if host := info.CanonicalHost(); host != "github.com" {
		return "", "", fmt.Errorf("remote is not a GitHub repository (host: %s)", host)
	}
	return info.Owner, info.Repo, nil
}

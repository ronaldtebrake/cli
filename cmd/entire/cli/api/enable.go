package api

import (
	"context"
	"fmt"
)

// EnableRepoRequest is the body of POST /api/v1/cli/enable. The server parses
// the raw remote URL itself, so the CLI only needs to send what it knows.
type EnableRepoRequest struct {
	RemoteURL string `json:"remote_url"`
}

// EnableRepoResponse is the result of recording an `entire enable`. Connected
// reports whether the GitHub App can currently reach the repo; when it can't,
// InstallURL points at the App installation page.
type EnableRepoResponse struct {
	Connected  bool   `json:"connected"`
	InstallURL string `json:"install_url,omitempty"`
	Repo       *struct {
		FullName string `json:"full_name"`
		GithubID int64  `json:"github_id"`
		Private  bool   `json:"private"`
	} `json:"repo,omitempty"`
}

// ReportEnable records that the authenticated user ran `entire enable` for the
// repo identified by remoteURL, and returns whether the App can reach it.
func (c *Client) ReportEnable(ctx context.Context, remoteURL string) (*EnableRepoResponse, error) {
	resp, err := c.Post(ctx, "/api/v1/cli/enable", EnableRepoRequest{RemoteURL: remoteURL})
	if err != nil {
		return nil, fmt.Errorf("report enable: %w", err)
	}
	defer resp.Body.Close()

	if err := CheckResponse(resp); err != nil {
		return nil, err
	}

	var out EnableRepoResponse
	if err := DecodeJSON(resp, &out); err != nil {
		return nil, fmt.Errorf("report enable: %w", err)
	}
	return &out, nil
}

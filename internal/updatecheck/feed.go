package updatecheck

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// LatestReleaseURL is the GitHub endpoint that reports the newest published
// release. The endpoint skips prereleases, which matters because the release
// workflow marks them automatically.
const LatestReleaseURL = "https://api.github.com/repos/cafecito-games/grafo/releases/latest"

// responseLimit bounds how much of a feed response is read. The useful payload
// is a few kilobytes; the limit keeps a misrouted response from being buffered
// in full.
const responseLimit = 1 << 20

// Feed reports the newest published release tag.
type Feed interface {
	LatestRelease(ctx context.Context) (string, error)
}

// GitHubFeed reads the latest release from the GitHub API.
type GitHubFeed struct {
	URL    string
	Client *http.Client
}

func (f GitHubFeed) LatestRelease(ctx context.Context) (string, error) {
	endpoint := f.URL
	if endpoint == "" {
		endpoint = LatestReleaseURL
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("release feed returned %s", response.Status)
	}
	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, responseLimit)).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode release feed: %w", err)
	}
	tag := strings.TrimSpace(payload.TagName)
	if Comparable(tag) == "" {
		return "", fmt.Errorf("release feed reported unusable tag %q", payload.TagName)
	}
	return tag, nil
}

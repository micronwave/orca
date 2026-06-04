// Package intake fetches external issues and converts them to plain-text goal
// input for the intent compiler. Only GitHub REST API is supported in Phase 5.
//
// Dependency contract:
//
//	Reads:  GitHub REST API (network)
//	Writes: none
//
//	May import:   internal/config
//	Must NOT import: internal/intent, internal/store, or any runtime component
package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/micronwave/orca/internal/config"
)

const defaultAPIBase = "https://api.github.com"

// Fetcher fetches GitHub issues. Zero value uses the real GitHub API.
// Set BaseURL to an httptest server URL in tests.
type Fetcher struct {
	// BaseURL overrides the GitHub API base URL (default https://api.github.com).
	// Used in tests to point at a local mock server.
	BaseURL string
	// Client overrides the HTTP client used for requests. If nil, a client with
	// a 30-second timeout is used.
	Client *http.Client
}

func (f *Fetcher) apiBase() string {
	if f.BaseURL != "" {
		return strings.TrimRight(f.BaseURL, "/")
	}
	return defaultAPIBase
}

func (f *Fetcher) httpClient() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Fetch fetches the GitHub issue at issueNumber and returns its title and body
// as plain text. The title is the first line; the body follows after a blank
// line when non-empty. Code blocks in the body are preserved as-is.
//
// Token resolution order: cfg.GitHubToken → GITHUB_TOKEN env var.
func (f *Fetcher) Fetch(ctx context.Context, cfg config.IntakeConfig, issueNumber int) (string, error) {
	token := cfg.GitHubToken
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	if token == "" {
		return "", fmt.Errorf("intake: github token required (set intake.github_token or GITHUB_TOKEN)")
	}
	if cfg.Repo == "" {
		return "", fmt.Errorf("intake: intake.repo is required")
	}
	if issueNumber <= 0 {
		return "", fmt.Errorf("intake: issue number must be positive, got %d", issueNumber)
	}

	url := fmt.Sprintf("%s/repos/%s/issues/%d", f.apiBase(), cfg.Repo, issueNumber)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("intake: build request for issue %d: %w", issueNumber, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := f.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("intake: fetch issue %d: %w", issueNumber, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("intake: read response for issue %d: %w", issueNumber, err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("intake: github api returned %d for issue %d: %s",
			resp.StatusCode, issueNumber, strings.TrimSpace(string(body)))
	}

	var issue struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.Unmarshal(body, &issue); err != nil {
		return "", fmt.Errorf("intake: parse issue %d response: %w", issueNumber, err)
	}
	if issue.Title == "" {
		return "", fmt.Errorf("intake: issue %d has no title", issueNumber)
	}

	if issue.Body == "" {
		return issue.Title, nil
	}
	return issue.Title + "\n\n" + issue.Body, nil
}

// Fetch fetches a GitHub issue using the default HTTP client and API base URL.
// It is a convenience wrapper around a zero-value Fetcher.
func Fetch(ctx context.Context, cfg config.IntakeConfig, issueNumber int) (string, error) {
	return (&Fetcher{}).Fetch(ctx, cfg, issueNumber)
}

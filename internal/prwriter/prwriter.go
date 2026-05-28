// Package prwriter creates GitHub pull requests from accepted patches.
//
// It receives fully resolved branch names and body text from cmd/orca.
// It must not read the artifact store, derive merge eligibility, or decide
// when a PR is allowed — all of that is the orchestrator's responsibility.
//
// Dependency contract:
//
//	Reads:  GitHub REST API (network)
//	Writes: none (returns an unsaved PRRecord for cmd/orca to persist)
//
//	May import:   internal/schema, internal/idgen
//	Must NOT import: reconciler, verifier, projector, budget, gate, store
package prwriter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/micronwave/orca/internal/idgen"
	"github.com/micronwave/orca/internal/schema"
)

const defaultAPIBase = "https://api.github.com"

// Config holds the GitHub connection settings and PR defaults.
type Config struct {
	// Repo is the owner/repo string, e.g. "acme/myrepo".
	Repo string
	// GitHubToken is the bearer token for the GitHub API.
	GitHubToken string
	// Draft creates the PR in draft state.
	Draft bool
	// Label is the label to apply (ignored when empty).
	Label string
	// BaseURL overrides the GitHub API base URL. If empty, https://api.github.com is used.
	// Set to an httptest server URL in tests.
	BaseURL string
}

func (c Config) apiBase() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return defaultAPIBase
}

// CreateInput holds the fully resolved parameters for one PR creation call.
// All branches must already be resolved by cmd/orca before calling Create.
type CreateInput struct {
	GoalID     string
	PatchID    string
	BaseBranch string
	HeadBranch string
	Title      string
	Body       string
	Draft      bool
}

// Create calls the GitHub REST API to create a pull request and returns an
// unsaved PRRecord. The caller (cmd/orca) is responsible for persisting the
// record via store.SavePRRecord after this function returns successfully.
func Create(ctx context.Context, cfg Config, in CreateInput) (*schema.PRRecord, error) {
	if cfg.Repo == "" {
		return nil, fmt.Errorf("prwriter: repo is required")
	}
	if cfg.GitHubToken == "" {
		return nil, fmt.Errorf("prwriter: github token is required")
	}
	if in.BaseBranch == "" {
		return nil, fmt.Errorf("prwriter: base branch is required")
	}
	if in.HeadBranch == "" {
		return nil, fmt.Errorf("prwriter: head branch is required")
	}
	if in.Title == "" {
		return nil, fmt.Errorf("prwriter: title is required")
	}

	type prRequest struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Draft bool   `json:"draft"`
	}
	reqBody := prRequest{
		Title: in.Title,
		Body:  in.Body,
		Head:  in.HeadBranch,
		Base:  in.BaseBranch,
		Draft: in.Draft,
	}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("prwriter: marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/repos/%s/pulls", cfg.apiBase(), cfg.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("prwriter: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prwriter: create pr: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("prwriter: read response: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("prwriter: github api returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var created struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(respBody, &created); err != nil {
		return nil, fmt.Errorf("prwriter: parse response: %w", err)
	}
	if created.HTMLURL == "" {
		return nil, fmt.Errorf("prwriter: github response missing html_url")
	}

	return &schema.PRRecord{
		PRID:       idgen.New("PR"),
		GoalID:     in.GoalID,
		PatchID:    in.PatchID,
		PRURL:      created.HTMLURL,
		BaseBranch: in.BaseBranch,
		HeadBranch: in.HeadBranch,
		Draft:      in.Draft,
		CreatedAt:  time.Now().UTC(),
	}, nil
}

// FetchDefaultBranch calls the GitHub repos API to get the repository's
// default branch. Used when pr.base_branch is empty and git symbolic-ref fails.
func FetchDefaultBranch(ctx context.Context, cfg Config) (string, error) {
	if cfg.Repo == "" {
		return "", fmt.Errorf("prwriter: repo is required")
	}
	if cfg.GitHubToken == "" {
		return "", fmt.Errorf("prwriter: github token is required")
	}

	url := fmt.Sprintf("%s/repos/%s", cfg.apiBase(), cfg.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("prwriter: build default-branch request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("prwriter: fetch repo info: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("prwriter: read repo response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("prwriter: github api returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(body, &repo); err != nil {
		return "", fmt.Errorf("prwriter: parse repo response: %w", err)
	}
	return repo.DefaultBranch, nil
}

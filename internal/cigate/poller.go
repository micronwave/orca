// Package cigate polls a CI provider for pipeline status and persists the
// result as a CIStatusRecord. Only github_actions is supported in Phase 5.
//
// Dependency contract:
//
//	Reads  (store): none directly — callers may load active goal before calling Wait
//	Writes (store): none — callers write CIStatusRecord after Wait returns
//	Writes (log):   none
//
//	Must NOT import: internal/planner, internal/runner, internal/verifier,
//	                 internal/reconciler, internal/projector, internal/budget,
//	                 internal/gate
package cigate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/micronwave/orca/internal/config"
)

const (
	defaultPollInterval = 30 * time.Second
	defaultTimeout      = 600 * time.Second
	defaultAPIBase      = "https://api.github.com"
)

// Poller polls a CI provider until a run reaches a terminal state.
type Poller struct {
	cfg     config.CIConfig
	token   string
	repo    string
	apiBase string
	httpDo  func(*http.Request) (*http.Response, error)
}

// Option configures a Poller. Primarily used in tests to inject mock
// HTTP transports or alternative API base URLs.
type Option func(*Poller)

// WithAPIBase overrides the GitHub API base URL (default: https://api.github.com).
func WithAPIBase(base string) Option {
	return func(p *Poller) { p.apiBase = base }
}

// WithHTTPDo replaces the HTTP transport used for all API calls.
func WithHTTPDo(fn func(*http.Request) (*http.Response, error)) Option {
	return func(p *Poller) { p.httpDo = fn }
}

// New returns a Poller. token is the GitHub API token (may be empty for
// public repos); repo is "owner/repo".
func New(cfg config.CIConfig, token, repo string, opts ...Option) *Poller {
	p := &Poller{
		cfg:     cfg,
		token:   token,
		repo:    repo,
		apiBase: defaultAPIBase,
		httpDo:  http.DefaultClient.Do,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func (p *Poller) pollInterval() time.Duration {
	if p.cfg.PollIntervalSeconds > 0 {
		return time.Duration(p.cfg.PollIntervalSeconds) * time.Second
	}
	return defaultPollInterval
}

type workflowRunsResponse struct {
	TotalCount   int           `json:"total_count"`
	WorkflowRuns []workflowRun `json:"workflow_runs"`
}

type workflowRun struct {
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
	HeadBranch string `json:"head_branch"`
}

// Wait polls the CI provider until the latest run for branch reaches a
// terminal state, ctx is cancelled, or timeout elapses.
//
// Return values:
//   - status: "success" or "failure"
//   - runURL: web URL for the CI run (may be empty if no run was found)
//   - summary: human-readable description
//   - rawLogPath: always empty in Phase 5
//   - err: non-nil only on hard errors (HTTP failure, context cancel)
//
// Timeout is not a hard error: it returns status="failure" and
// summary="CI status poll timed out" with err=nil so callers can persist
// the record before exiting non-zero.
func (p *Poller) Wait(ctx context.Context, goalID, capsuleID, branch string, timeout time.Duration) (status, runURL, summary, rawLogPath string, err error) {
	if branch == "" {
		branch = p.cfg.Branch
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	deadline := time.Now().Add(timeout)
	interval := p.pollInterval()

	for {
		run, pollErr := p.latestRun(ctx, branch)
		if pollErr != nil {
			return "failure", "", pollErr.Error(), "", pollErr
		}
		if run != nil && run.Status == "completed" {
			if run.Conclusion == "success" {
				return "success", run.HTMLURL, "CI passed", "", nil
			}
			return "failure", run.HTMLURL, fmt.Sprintf("CI %s: %s", run.Status, run.Conclusion), "", nil
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "failure", "", "CI status poll timed out", "", nil
		}

		wait := interval
		if wait > remaining {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return "failure", "", ctx.Err().Error(), "", ctx.Err()
		case <-time.After(wait):
		}

		if time.Until(deadline) <= 0 {
			return "failure", "", "CI status poll timed out", "", nil
		}
	}
}

func (p *Poller) latestRun(ctx context.Context, branch string) (*workflowRun, error) {
	url := fmt.Sprintf("%s/repos/%s/actions/runs?branch=%s&per_page=1", p.apiBase, p.repo, branch)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("cigate: create request: %w", err)
	}
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := p.httpDo(req)
	if err != nil {
		return nil, fmt.Errorf("cigate: http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cigate: github api returned %d", resp.StatusCode)
	}

	var result workflowRunsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("cigate: decode response: %w", err)
	}

	if len(result.WorkflowRuns) == 0 {
		return nil, nil
	}
	return &result.WorkflowRuns[0], nil
}

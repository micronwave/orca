package cigate_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/cigate"
	"github.com/micronwave/orca/internal/config"
)

// newTestPoller returns a Poller wired to server.
func newTestPoller(t *testing.T, server *httptest.Server, cfg config.CIConfig) *cigate.Poller {
	t.Helper()
	return cigate.New(cfg, "tok", "owner/repo", nil, nil,
		cigate.WithAPIBase(server.URL),
		cigate.WithHTTPDo(server.Client().Do),
	)
}

func runResponse(status, conclusion, htmlURL string) map[string]any {
	return map[string]any{
		"total_count": 1,
		"workflow_runs": []map[string]any{
			{
				"id":          1,
				"status":      status,
				"conclusion":  conclusion,
				"html_url":    htmlURL,
				"head_branch": "main",
			},
		},
	}
}

func emptyRunsResponse() map[string]any {
	return map[string]any{
		"total_count":   0,
		"workflow_runs": []any{},
	}
}

func TestWait_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(runResponse("completed", "success", "https://github.com/run/1"))
	}))
	defer srv.Close()

	cfg := config.CIConfig{Provider: "github_actions", PollIntervalSeconds: 1}
	p := newTestPoller(t, srv, cfg)

	status, runURL, summary, rawLogPath, err := p.Wait(context.Background(), "G-1", "CAP-1", "main", 10*time.Second)
	if err != nil {
		t.Fatalf("Wait error = %v", err)
	}
	if status != "success" {
		t.Errorf("status = %q, want %q", status, "success")
	}
	if runURL == "" {
		t.Error("runURL is empty")
	}
	if summary == "" {
		t.Error("summary is empty")
	}
	_ = rawLogPath
}

func TestWait_Failure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(runResponse("completed", "failure", "https://github.com/run/2"))
	}))
	defer srv.Close()

	cfg := config.CIConfig{Provider: "github_actions", PollIntervalSeconds: 1}
	p := newTestPoller(t, srv, cfg)

	status, _, _, _, err := p.Wait(context.Background(), "G-1", "CAP-1", "main", 10*time.Second)
	if err != nil {
		t.Fatalf("Wait error = %v", err)
	}
	if status != "failure" {
		t.Errorf("status = %q, want %q", status, "failure")
	}
}

func TestWait_Timeout(t *testing.T) {
	// Server always returns in_progress so polling never terminates.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(runResponse("in_progress", "", "https://github.com/run/3"))
	}))
	defer srv.Close()

	cfg := config.CIConfig{Provider: "github_actions", PollIntervalSeconds: 1}
	p := newTestPoller(t, srv, cfg)

	status, _, summary, _, err := p.Wait(context.Background(), "G-1", "CAP-1", "main", 2*time.Second)
	if err != nil {
		t.Fatalf("Wait error = %v (want nil — timeout is non-fatal)", err)
	}
	if status != "failure" {
		t.Errorf("status = %q, want %q", status, "failure")
	}
	if summary != "CI status poll timed out" {
		t.Errorf("summary = %q, want %q", summary, "CI status poll timed out")
	}
}

func TestWait_NoRunsYet_ThenSuccess(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		calls++
		if calls < 2 {
			_ = json.NewEncoder(w).Encode(emptyRunsResponse())
			return
		}
		_ = json.NewEncoder(w).Encode(runResponse("completed", "success", "https://github.com/run/4"))
	}))
	defer srv.Close()

	cfg := config.CIConfig{Provider: "github_actions", PollIntervalSeconds: 1}
	p := newTestPoller(t, srv, cfg)

	status, _, _, _, err := p.Wait(context.Background(), "G-1", "CAP-1", "main", 10*time.Second)
	if err != nil {
		t.Fatalf("Wait error = %v", err)
	}
	if status != "success" {
		t.Errorf("status = %q, want %q", status, "success")
	}
}

func TestWait_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	cfg := config.CIConfig{Provider: "github_actions", PollIntervalSeconds: 1}
	p := newTestPoller(t, srv, cfg)

	status, _, _, _, err := p.Wait(context.Background(), "G-1", "CAP-1", "main", 10*time.Second)
	if err == nil {
		t.Fatal("Wait error = nil, want non-nil on HTTP error")
	}
	if status != "failure" {
		t.Errorf("status = %q, want %q", status, "failure")
	}
}

func TestWait_ContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(runResponse("in_progress", "", ""))
	}))
	defer srv.Close()

	cfg := config.CIConfig{Provider: "github_actions", PollIntervalSeconds: 30}
	p := newTestPoller(t, srv, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	status, _, _, _, err := p.Wait(ctx, "G-1", "CAP-1", "main", 60*time.Second)
	if err == nil {
		t.Fatal("Wait error = nil, want non-nil on context cancel")
	}
	if status != "failure" {
		t.Errorf("status = %q, want %q", status, "failure")
	}
}

func TestWait_UsesBranchFromConfig(t *testing.T) {
	var capturedBranch string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBranch = r.URL.Query().Get("branch")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(runResponse("completed", "success", ""))
	}))
	defer srv.Close()

	cfg := config.CIConfig{Provider: "github_actions", PollIntervalSeconds: 1, Branch: "feat/x"}
	p := newTestPoller(t, srv, cfg)

	// Pass empty branch so config branch is used.
	_, _, _, _, _ = p.Wait(context.Background(), "G-1", "CAP-1", "", 5*time.Second)
	if capturedBranch != "feat/x" {
		t.Errorf("branch = %q, want %q", capturedBranch, "feat/x")
	}
}

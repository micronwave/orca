package intake_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/config"
	"github.com/micronwave/orca/internal/intake"
)

func TestFetch_ReturnsTitleAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/issues/42" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer mytoken" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"title": "Fix the auth middleware rounding defect",
			"body":  "The middleware rounds incorrectly on edge cases.",
		})
	}))
	defer srv.Close()

	f := &intake.Fetcher{BaseURL: srv.URL}
	cfg := config.IntakeConfig{GitHubToken: "mytoken", Repo: "owner/repo"}

	got, err := f.Fetch(context.Background(), cfg, 42)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(got, "Fix the auth middleware rounding defect") {
		t.Errorf("result %q does not contain title", got)
	}
	if !strings.Contains(got, "The middleware rounds incorrectly on edge cases.") {
		t.Errorf("result %q does not contain body", got)
	}
}

func TestFetch_PreservesCodeBlocks(t *testing.T) {
	body := "Steps to reproduce:\n\n```go\nfmt.Println(1/3)\n```\n\nExpected: 0.333"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"title": "Division bug",
			"body":  body,
		})
	}))
	defer srv.Close()

	f := &intake.Fetcher{BaseURL: srv.URL}
	cfg := config.IntakeConfig{GitHubToken: "tok", Repo: "o/r"}

	got, err := f.Fetch(context.Background(), cfg, 1)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(got, "```go\nfmt.Println(1/3)\n```") {
		t.Errorf("code block not preserved in result:\n%s", got)
	}
}

func TestFetch_TokenFromEnv(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "envtoken")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer envtoken" {
			http.Error(w, "expected envtoken", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"title": "Issue from env token",
			"body":  "",
		})
	}))
	defer srv.Close()

	f := &intake.Fetcher{BaseURL: srv.URL}
	cfg := config.IntakeConfig{GitHubToken: "", Repo: "o/r"}

	got, err := f.Fetch(context.Background(), cfg, 7)
	if err != nil {
		t.Fatalf("Fetch with GITHUB_TOKEN: %v", err)
	}
	if got != "Issue from env token" {
		t.Errorf("result = %q, want title only when body is empty", got)
	}
}

func TestFetch_ConfigTokenTakesPriorityOverEnv(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "envtoken")

	var receivedToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedToken = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"title": "t", "body": ""})
	}))
	defer srv.Close()

	f := &intake.Fetcher{BaseURL: srv.URL}
	cfg := config.IntakeConfig{GitHubToken: "cfgtoken", Repo: "o/r"}

	if _, err := f.Fetch(context.Background(), cfg, 1); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if receivedToken != "Bearer cfgtoken" {
		t.Errorf("token sent = %q, want cfg token to take priority over env", receivedToken)
	}
}

func TestFetch_MissingTokenReturnsError(t *testing.T) {
	os.Unsetenv("GITHUB_TOKEN")
	f := &intake.Fetcher{}
	cfg := config.IntakeConfig{Repo: "o/r"}
	_, err := f.Fetch(context.Background(), cfg, 1)
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("expected token error, got %v", err)
	}
}

func TestFetch_MissingRepoReturnsError(t *testing.T) {
	f := &intake.Fetcher{}
	cfg := config.IntakeConfig{GitHubToken: "tok"}
	_, err := f.Fetch(context.Background(), cfg, 1)
	if err == nil || !strings.Contains(err.Error(), "repo") {
		t.Fatalf("expected repo error, got %v", err)
	}
}

func TestFetch_InvalidIssueNumberReturnsError(t *testing.T) {
	f := &intake.Fetcher{}
	cfg := config.IntakeConfig{GitHubToken: "tok", Repo: "o/r"}
	_, err := f.Fetch(context.Background(), cfg, 0)
	if err == nil || !strings.Contains(err.Error(), "positive") {
		t.Fatalf("expected positive-number error, got %v", err)
	}
}

func TestFetch_HTTPErrorReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	f := &intake.Fetcher{BaseURL: srv.URL}
	cfg := config.IntakeConfig{GitHubToken: "tok", Repo: "o/r"}
	_, err := f.Fetch(context.Background(), cfg, 99)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected 404 error, got %v", err)
	}
}

func TestFetch_NoTitleReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"title": "", "body": "some body"})
	}))
	defer srv.Close()

	f := &intake.Fetcher{BaseURL: srv.URL}
	cfg := config.IntakeConfig{GitHubToken: "tok", Repo: "o/r"}
	_, err := f.Fetch(context.Background(), cfg, 1)
	if err == nil || !strings.Contains(err.Error(), "no title") {
		t.Fatalf("expected no-title error, got %v", err)
	}
}

func TestFetch_TitleOnlyWhenBodyEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"title": "Just the title", "body": ""})
	}))
	defer srv.Close()

	f := &intake.Fetcher{BaseURL: srv.URL}
	cfg := config.IntakeConfig{GitHubToken: "tok", Repo: "o/r"}
	got, err := f.Fetch(context.Background(), cfg, 1)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got != "Just the title" {
		t.Errorf("result = %q, want title only", got)
	}
}

// TestFetch_HangingServerClientTimeout verifies that Fetch returns an error
// when the server never responds and the client timeout fires. This proves the
// timeout mechanism actually fires, not just that the Timeout field is set.
func TestFetch_HangingServerClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until the client drops the connection; the timeout fires first.
		<-r.Context().Done()
	}))
	defer srv.Close()

	f := &intake.Fetcher{
		BaseURL: srv.URL,
		Client:  &http.Client{Timeout: 150 * time.Millisecond},
	}
	cfg := config.IntakeConfig{GitHubToken: "tok", Repo: "o/r"}

	start := time.Now()
	_, err := f.Fetch(context.Background(), cfg, 1)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Fetch against hanging server: expected error, got nil")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Fetch took %v; expected timeout in ~150ms", elapsed)
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "timeout") && !strings.Contains(msg, "deadline") && !strings.Contains(msg, "context") {
		t.Errorf("Fetch error = %q; expected a timeout or deadline error", err)
	}
}

// TestFetch_ContextCancelDuringHang verifies that a caller-cancelled context
// terminates Fetch promptly even when the server never responds — exercising
// the http.NewRequestWithContext cancellation path in Fetcher.Fetch.
func TestFetch_ContextCancelDuringHang(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	f := &intake.Fetcher{BaseURL: srv.URL}
	cfg := config.IntakeConfig{GitHubToken: "tok", Repo: "o/r"}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := f.Fetch(ctx, cfg, 1)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Fetch with cancelled context against hanging server: expected error, got nil")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Fetch took %v; expected cancellation in ~150ms", elapsed)
	}
}

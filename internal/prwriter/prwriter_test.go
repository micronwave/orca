package prwriter_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/prwriter"
)

func TestCreate_SendsCorrectFields(t *testing.T) {
	var gotReq struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Draft bool   `json:"draft"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "want POST", http.StatusMethodNotAllowed)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/pulls") {
			http.Error(w, "want /pulls", http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{
			"html_url": "https://github.com/owner/repo/pull/1",
		})
	}))
	defer srv.Close()

	cfg := prwriter.Config{
		Repo:        "owner/repo",
		GitHubToken: "tok",
		BaseURL:     srv.URL,
	}
	in := prwriter.CreateInput{
		GoalID:     "G-1",
		PatchID:    "PATCH-1",
		BaseBranch: "main",
		HeadBranch: "feature/fix",
		Title:      "Fix the bug",
		Body:       "Detailed body here.",
		Draft:      true,
	}

	record, err := prwriter.Create(context.Background(), cfg, in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if gotReq.Base != "main" {
		t.Errorf("base = %q, want main", gotReq.Base)
	}
	if gotReq.Head != "feature/fix" {
		t.Errorf("head = %q, want feature/fix", gotReq.Head)
	}
	if gotReq.Title != "Fix the bug" {
		t.Errorf("title = %q, want Fix the bug", gotReq.Title)
	}
	if !strings.Contains(gotReq.Body, "Detailed body") {
		t.Errorf("body = %q does not contain expected content", gotReq.Body)
	}
	if !gotReq.Draft {
		t.Error("draft = false, want true")
	}

	// PR record must be complete and unsaved (no PRID prefix check beyond non-empty)
	if record.PRID == "" {
		t.Error("PRID is empty")
	}
	if !strings.HasPrefix(record.PRID, "PR-") {
		t.Errorf("PRID = %q, want PR- prefix", record.PRID)
	}
	if record.GoalID != "G-1" {
		t.Errorf("GoalID = %q, want G-1", record.GoalID)
	}
	if record.PatchID != "PATCH-1" {
		t.Errorf("PatchID = %q, want PATCH-1", record.PatchID)
	}
	if record.PRURL != "https://github.com/owner/repo/pull/1" {
		t.Errorf("PRURL = %q, want github pull URL", record.PRURL)
	}
	if record.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want main", record.BaseBranch)
	}
	if record.HeadBranch != "feature/fix" {
		t.Errorf("HeadBranch = %q, want feature/fix", record.HeadBranch)
	}
	if !record.Draft {
		t.Error("Draft = false, want true")
	}
	if record.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
	if record.CreatedAt.After(time.Now().Add(time.Second)) {
		t.Error("CreatedAt is in the future")
	}
}

func TestCreate_ReturnsUnsavedPRRecord(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{
			"html_url": "https://github.com/o/r/pull/2",
		})
	}))
	defer srv.Close()

	cfg := prwriter.Config{Repo: "o/r", GitHubToken: "tok", BaseURL: srv.URL}
	in := prwriter.CreateInput{
		GoalID: "G-2", PatchID: "P-2",
		BaseBranch: "main", HeadBranch: "fix-branch",
		Title: "title", Body: "body",
	}

	record, err := prwriter.Create(context.Background(), cfg, in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if record == nil {
		t.Fatal("Create returned nil record")
	}
	// The record is unsaved — it has all fields set but no persistence side effects.
	if record.PRID == "" || record.GoalID == "" || record.PRURL == "" {
		t.Errorf("record missing fields: %+v", record)
	}
}

func TestCreate_MissingRepoReturnsError(t *testing.T) {
	cfg := prwriter.Config{GitHubToken: "tok"}
	in := prwriter.CreateInput{BaseBranch: "main", HeadBranch: "feat", Title: "t"}
	_, err := prwriter.Create(context.Background(), cfg, in)
	if err == nil || !strings.Contains(err.Error(), "repo") {
		t.Fatalf("expected repo error, got %v", err)
	}
}

func TestCreate_MissingTokenReturnsError(t *testing.T) {
	cfg := prwriter.Config{Repo: "o/r"}
	in := prwriter.CreateInput{BaseBranch: "main", HeadBranch: "feat", Title: "t"}
	_, err := prwriter.Create(context.Background(), cfg, in)
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("expected token error, got %v", err)
	}
}

func TestCreate_MissingBaseBranchReturnsError(t *testing.T) {
	cfg := prwriter.Config{Repo: "o/r", GitHubToken: "tok"}
	in := prwriter.CreateInput{HeadBranch: "feat", Title: "t"}
	_, err := prwriter.Create(context.Background(), cfg, in)
	if err == nil || !strings.Contains(err.Error(), "base branch") {
		t.Fatalf("expected base-branch error, got %v", err)
	}
}

func TestCreate_MissingHeadBranchReturnsError(t *testing.T) {
	cfg := prwriter.Config{Repo: "o/r", GitHubToken: "tok"}
	in := prwriter.CreateInput{BaseBranch: "main", Title: "t"}
	_, err := prwriter.Create(context.Background(), cfg, in)
	if err == nil || !strings.Contains(err.Error(), "head branch") {
		t.Fatalf("expected head-branch error, got %v", err)
	}
}

func TestCreate_GitHubAPIErrorReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Unprocessable Entity"}`, http.StatusUnprocessableEntity)
	}))
	defer srv.Close()

	cfg := prwriter.Config{Repo: "o/r", GitHubToken: "tok", BaseURL: srv.URL}
	in := prwriter.CreateInput{
		GoalID: "G-1", PatchID: "P-1",
		BaseBranch: "main", HeadBranch: "feat",
		Title: "t", Body: "b",
	}
	_, err := prwriter.Create(context.Background(), cfg, in)
	if err == nil || !strings.Contains(err.Error(), "422") {
		t.Fatalf("expected 422 error, got %v", err)
	}
}

func TestCreate_SendsBearerToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"html_url": "https://github.com/o/r/pull/3"})
	}))
	defer srv.Close()

	cfg := prwriter.Config{Repo: "o/r", GitHubToken: "secrettoken", BaseURL: srv.URL}
	in := prwriter.CreateInput{
		BaseBranch: "main", HeadBranch: "feat",
		Title: "t", Body: "b",
	}
	if _, err := prwriter.Create(context.Background(), cfg, in); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if gotAuth != "Bearer secrettoken" {
		t.Errorf("Authorization = %q, want Bearer secrettoken", gotAuth)
	}
}

func TestFetchDefaultBranch_ReturnsDefaultBranch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/owner/repo") {
			http.Error(w, "want GET /repos/owner/repo", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"default_branch": "trunk"})
	}))
	defer srv.Close()

	cfg := prwriter.Config{Repo: "owner/repo", GitHubToken: "tok", BaseURL: srv.URL}
	branch, err := prwriter.FetchDefaultBranch(context.Background(), cfg)
	if err != nil {
		t.Fatalf("FetchDefaultBranch: %v", err)
	}
	if branch != "trunk" {
		t.Errorf("branch = %q, want trunk", branch)
	}
}

func TestFetchDefaultBranch_MissingRepoReturnsError(t *testing.T) {
	cfg := prwriter.Config{GitHubToken: "tok"}
	_, err := prwriter.FetchDefaultBranch(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "repo") {
		t.Fatalf("expected repo error, got %v", err)
	}
}

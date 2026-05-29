// Phase 5 integration tests: MCP, intake, CI gate, PR creation, replay,
// and prior-phase isolation. All tests run without a real GitHub token or
// SSH host by using httptest servers and in-process fakes.
//
// Build constraint: same package as the existing integration tests (no tag).
// Run with: go test -tags integration ./internal/integration/...
package integration_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/cigate"
	"github.com/micronwave/orca/internal/config"
	"github.com/micronwave/orca/internal/idgen"
	"github.com/micronwave/orca/internal/intake"
	"github.com/micronwave/orca/internal/intent"
	"github.com/micronwave/orca/internal/mcp"
	"github.com/micronwave/orca/internal/prwriter"
	"github.com/micronwave/orca/internal/reconciler"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

// ── MCP helpers ──────────────────────────────────────────────────────────────

// mcpToolCall performs a JSON-RPC 2.0 tools/call request against ts.
func mcpToolCall(t *testing.T, ts *httptest.Server, toolName string, args any) map[string]any {
	t.Helper()
	argBytes, _ := json.Marshal(args)
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": json.RawMessage(argBytes),
		},
	})
	resp, err := http.Post(ts.URL+"/", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("mcpToolCall %s: %v", toolName, err)
	}
	defer resp.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("mcpToolCall %s: decode response: %v", toolName, err)
	}
	return result
}

// mcpResultText extracts the text content from a tools/call response.
// Fails the test if isError is true.
func mcpResultText(t *testing.T, resp map[string]any) string {
	t.Helper()
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("mcpResultText: no result field: %v", resp)
	}
	if isErr, _ := result["isError"].(bool); isErr {
		content, _ := result["content"].([]any)
		if len(content) > 0 {
			item, _ := content[0].(map[string]any)
			t.Fatalf("mcpResultText: tool returned error: %v", item["text"])
		}
		t.Fatal("mcpResultText: tool returned error (no content)")
	}
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	item, _ := content[0].(map[string]any)
	text, _ := item["text"].(string)
	return text
}

// ── Phase 5 tests ─────────────────────────────────────────────────────────────

// TestPhase5_MCPServer_ReadsGoalAndObligations starts an MCP server against a
// temp store, calls orca_get_goal and orca_list_open_obligations, and asserts
// that the correct artifacts are returned and that no store/log writes occurred.
func TestPhase5_MCPServer_ReadsGoalAndObligations(t *testing.T) {
	env := newIntegEnv(t)
	ctx := env.ctx
	now := time.Now().UTC()

	const (
		goalID = "G-MCP-P5"
		condID = "GC-MCP-P5"
		obl1   = "OB-MCP-P5-1"
		obl2   = "OB-MCP-P5-2"
	)

	if err := env.st.SaveGoal(ctx, &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "mcp server read test",
		GoalConditions: []schema.GoalCondition{{
			ID: condID, Description: "c", EffectiveDescription: "c",
			Status: schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow, Status: schema.GoalStatusActive, CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	for _, oblID := range []string{obl1, obl2} {
		if err := env.st.SaveObligation(ctx, &schema.Obligation{
			ObligationID: oblID, GoalConditionID: condID,
			Description: oblID, Blocking: true,
			RiskLevel: schema.RiskLow, Status: schema.ObligationOpen,
		}); err != nil {
			t.Fatalf("SaveObligation %s: %v", oblID, err)
		}
	}

	server := mcp.New(env.st, env.log)
	ts := httptest.NewServer(server)
	defer ts.Close()

	countBefore := len(mustReadAllEvents(t, env))

	// orca_get_goal
	goalResp := mcpToolCall(t, ts, "orca_get_goal", map[string]any{})
	text := mcpResultText(t, goalResp)
	var goal schema.GoalIR
	if err := json.Unmarshal([]byte(text), &goal); err != nil {
		t.Fatalf("unmarshal goal: %v", err)
	}
	if goal.GoalID != goalID {
		t.Errorf("GoalID = %q, want %q", goal.GoalID, goalID)
	}

	// orca_list_open_obligations
	oblResp := mcpToolCall(t, ts, "orca_list_open_obligations", map[string]any{"goal_id": goalID})
	oblText := mcpResultText(t, oblResp)
	var obligations []*schema.Obligation
	if err := json.Unmarshal([]byte(oblText), &obligations); err != nil {
		t.Fatalf("unmarshal obligations: %v", err)
	}
	if len(obligations) != 2 {
		t.Errorf("obligation count = %d, want 2", len(obligations))
	}
	gotIDs := make(map[string]bool)
	for _, o := range obligations {
		gotIDs[o.ObligationID] = true
	}
	for _, want := range []string{obl1, obl2} {
		if !gotIDs[want] {
			t.Errorf("obligation %q missing from MCP response", want)
		}
	}

	// No writes must have occurred
	countAfter := len(mustReadAllEvents(t, env))
	if countAfter != countBefore {
		t.Errorf("event count changed: before=%d after=%d (MCP server must be read-only)",
			countBefore, countAfter)
	}
}

// TestPhase5_IntakeIngestion_CreatesGoalIR mocks the GitHub issue API, fetches
// an issue via intake.Fetcher, compiles a GoalIR, persists an IntakeRecord, and
// asserts that both goal_created and intake_issue_ingested events appear.
func TestPhase5_IntakeIngestion_CreatesGoalIR(t *testing.T) {
	env := newIntegEnv(t)
	ctx := env.ctx

	issueTitle := "Add rate limiting to API endpoints"
	issueBody := "Rate limits should be enforced per user token to prevent abuse."

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"title": issueTitle,
			"body":  issueBody,
		})
	}))
	defer mockSrv.Close()

	fetcher := &intake.Fetcher{BaseURL: mockSrv.URL}
	cfg := config.IntakeConfig{GitHubToken: "test-token", Repo: "owner/repo"}

	text, err := fetcher.Fetch(ctx, cfg, 42)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(text, issueTitle) {
		t.Fatalf("fetched text %q does not contain issue title", text)
	}
	if !strings.Contains(text, issueBody) {
		t.Fatalf("fetched text %q does not contain issue body", text)
	}

	compiler := intent.New(env.st)
	goal, err := compiler.Compile(ctx, text)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if goal.GoalID == "" {
		t.Fatal("Compile returned goal with empty GoalID")
	}

	intakeRec := &schema.IntakeRecord{
		RecordID:    idgen.New("INTAKE"),
		GoalID:      goal.GoalID,
		Source:      "github_issue",
		ExternalID:  "42",
		ExternalURL: "https://github.com/owner/repo/issues/42",
		IngestedAt:  time.Now().UTC(),
	}
	if err := env.st.SaveIntakeRecord(ctx, goal.GoalID, intakeRec); err != nil {
		t.Fatalf("SaveIntakeRecord: %v", err)
	}

	// Verify goal artifact reconstructed
	loaded, err := env.st.LoadGoal(ctx, goal.GoalID)
	if err != nil {
		t.Fatalf("LoadGoal: %v", err)
	}
	if loaded.GoalID != goal.GoalID {
		t.Errorf("loaded GoalID = %q, want %q", loaded.GoalID, goal.GoalID)
	}

	// Verify both events exist in log
	events := mustReadAllEvents(t, env)
	var foundGoalCreated, foundIntakeIngested bool
	for _, ev := range events {
		switch ev.Type {
		case schema.EventGoalCreated:
			if ev.GoalID == goal.GoalID {
				foundGoalCreated = true
			}
		case schema.EventIntakeIssueIngested:
			if ev.GoalID == goal.GoalID {
				foundIntakeIngested = true
			}
		}
	}
	if !foundGoalCreated {
		t.Error("goal_created event not found after intake ingestion")
	}
	if !foundIntakeIngested {
		t.Error("intake_issue_ingested event not found after SaveIntakeRecord")
	}

	// Verify intake record reloads correctly
	loaded2, err := env.st.LoadIntakeRecord(ctx, goal.GoalID, intakeRec.RecordID)
	if err != nil {
		t.Fatalf("LoadIntakeRecord: %v", err)
	}
	if loaded2.ExternalID != "42" {
		t.Errorf("ExternalID = %q, want 42", loaded2.ExternalID)
	}
}

// TestPhase5_CIGate_CommandSuccess mocks GitHub Actions returning success, runs
// the poller, saves the CIStatusRecord, and asserts the record has Status "success".
func TestPhase5_CIGate_CommandSuccess(t *testing.T) {
	env := newIntegEnv(t)
	ctx := env.ctx
	now := time.Now().UTC()

	const goalID = "G-CI-SUCCESS-P5"
	if err := env.st.SaveGoal(ctx, &schema.GoalIR{
		GoalID: goalID, OriginalIntent: "ci gate success test",
		GoalConditions: []schema.GoalCondition{{
			ID: "GC-CI-SUCCESS-P5", Description: "c", EffectiveDescription: "c",
			Status: schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow, Status: schema.GoalStatusActive, CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{{
				"id": 1, "status": "completed", "conclusion": "success",
				"html_url":    "https://github.com/owner/repo/actions/runs/1",
				"head_branch": "feature",
			}},
		})
	}))
	defer mockSrv.Close()

	poller := cigate.New(
		config.CIConfig{Provider: "github_actions", PollIntervalSeconds: 1},
		"test-token", "owner/repo",
		cigate.WithAPIBase(mockSrv.URL),
		cigate.WithHTTPDo(mockSrv.Client().Do),
	)

	status, runURL, summary, _, waitErr := poller.Wait(ctx, goalID, "CAP-CI-1", "feature", 10*time.Second)
	if waitErr != nil {
		t.Fatalf("Wait: %v", waitErr)
	}
	if status != "success" {
		t.Errorf("status = %q, want success", status)
	}

	record := &schema.CIStatusRecord{
		RecordID:   idgen.New("CISTAT"),
		GoalID:     goalID,
		CapsuleID:  "CAP-CI-1",
		Provider:   "github_actions",
		Branch:     "feature",
		Status:     status,
		RunURL:     runURL,
		Summary:    summary,
		RecordedAt: time.Now().UTC(),
	}
	if err := env.st.SaveCIStatusRecord(ctx, goalID, record); err != nil {
		t.Fatalf("SaveCIStatusRecord: %v", err)
	}

	loaded, err := env.st.LoadCIStatusRecord(ctx, goalID, record.RecordID)
	if err != nil {
		t.Fatalf("LoadCIStatusRecord: %v", err)
	}
	if loaded.Status != "success" {
		t.Errorf("CIStatusRecord.Status = %q, want success", loaded.Status)
	}

	// Confirm ci_status_received event exists
	events := mustReadAllEvents(t, env)
	var found bool
	for _, ev := range events {
		if ev.Type == schema.EventCIStatusReceived && ev.GoalID == goalID {
			found = true
		}
	}
	if !found {
		t.Error("ci_status_received event not found")
	}
}

// TestPhase5_CIGate_CommandFailure mocks GitHub Actions returning failure, runs
// the poller, saves the record, and asserts status is "failure".
func TestPhase5_CIGate_CommandFailure(t *testing.T) {
	env := newIntegEnv(t)
	ctx := env.ctx
	now := time.Now().UTC()

	const goalID = "G-CI-FAIL-P5"
	if err := env.st.SaveGoal(ctx, &schema.GoalIR{
		GoalID: goalID, OriginalIntent: "ci gate failure test",
		GoalConditions: []schema.GoalCondition{{
			ID: "GC-CI-FAIL-P5", Description: "c", EffectiveDescription: "c",
			Status: schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow, Status: schema.GoalStatusActive, CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}

	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{{
				"id": 2, "status": "completed", "conclusion": "failure",
				"html_url":    "https://github.com/owner/repo/actions/runs/2",
				"head_branch": "feature",
			}},
		})
	}))
	defer mockSrv.Close()

	poller := cigate.New(
		config.CIConfig{Provider: "github_actions", PollIntervalSeconds: 1},
		"test-token", "owner/repo",
		cigate.WithAPIBase(mockSrv.URL),
		cigate.WithHTTPDo(mockSrv.Client().Do),
	)

	status, runURL, summary, _, waitErr := poller.Wait(ctx, goalID, "CAP-CI-2", "feature", 10*time.Second)
	if waitErr != nil {
		t.Fatalf("Wait unexpected hard error: %v", waitErr)
	}
	if status != "failure" {
		t.Errorf("status = %q, want failure", status)
	}

	record := &schema.CIStatusRecord{
		RecordID:   idgen.New("CISTAT"),
		GoalID:     goalID,
		CapsuleID:  "CAP-CI-2",
		Provider:   "github_actions",
		Branch:     "feature",
		Status:     status,
		RunURL:     runURL,
		Summary:    summary,
		RecordedAt: time.Now().UTC(),
	}
	if err := env.st.SaveCIStatusRecord(ctx, goalID, record); err != nil {
		t.Fatalf("SaveCIStatusRecord: %v", err)
	}

	loaded, err := env.st.LoadCIStatusRecord(ctx, goalID, record.RecordID)
	if err != nil {
		t.Fatalf("LoadCIStatusRecord: %v", err)
	}
	if loaded.Status != "failure" {
		t.Errorf("CIStatusRecord.Status = %q, want failure", loaded.Status)
	}

	// Confirm ci_status_received event exists for the failure case.
	events := mustReadAllEvents(t, env)
	var found bool
	for _, ev := range events {
		if ev.Type == schema.EventCIStatusReceived && ev.GoalID == goalID {
			found = true
		}
	}
	if !found {
		t.Error("ci_status_received event not found for failure case")
	}
}

// TestPhase5_PRCreation_AfterMergeRecommendation mocks the GitHub PR API, calls
// prwriter.Create, saves the PRRecord, and asserts a pr_created event is logged.
func TestPhase5_PRCreation_AfterMergeRecommendation(t *testing.T) {
	env := newIntegEnv(t)
	ctx := env.ctx
	now := time.Now().UTC()

	const (
		goalID  = "G-PR-P5"
		condID  = "GC-PR-P5"
		oblID   = "OB-PR-P5"
		capsID  = "CAP-PR-P5"
		patchID = "PATCH-PR-P5"
		evID    = "EV-PR-P5"
	)

	if err := env.st.SaveGoal(ctx, &schema.GoalIR{
		GoalID: goalID, OriginalIntent: "pr creation test",
		GoalConditions: []schema.GoalCondition{{
			ID: condID, Description: "c", EffectiveDescription: "c",
			Status: schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow, Status: schema.GoalStatusActive, CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	if err := env.st.SaveObligation(ctx, &schema.Obligation{
		ObligationID: oblID, GoalConditionID: condID,
		Description: "implement feature", Blocking: true,
		RiskLevel: schema.RiskLow, Status: schema.ObligationOpen,
	}); err != nil {
		t.Fatalf("SaveObligation: %v", err)
	}
	if err := env.st.SaveCapsule(ctx, &schema.ExecutionCapsule{
		CapsuleID: capsID, ObligationIDs: []string{oblID},
		Agent: schema.AgentCodex, Role: schema.RoleExecutor,
		State: schema.CapsuleStateCompleted,
	}); err != nil {
		t.Fatalf("SaveCapsule: %v", err)
	}
	if err := env.st.SaveEvidence(ctx, &schema.EvidenceArtifact{
		EvidenceID: evID, Type: schema.EvidenceTestResult,
		ExitCode: 0, Supports: []string{oblID}, CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveEvidence: %v", err)
	}
	if err := env.st.SavePatch(ctx, &schema.PatchArtifact{
		PatchID: patchID, CapsuleID: capsID,
		ObligationIDsClaimed: []string{oblID},
		Status:               schema.PatchAccepted,
	}); err != nil {
		t.Fatalf("SavePatch: %v", err)
	}

	// Mock GitHub PR creation
	const wantPRURL = "https://github.com/owner/repo/pull/42"
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]string{
				"html_url": wantPRURL,
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer mockSrv.Close()

	prCfg := prwriter.Config{
		Repo:        "owner/repo",
		GitHubToken: "test-token",
		BaseURL:     mockSrv.URL,
	}
	in := prwriter.CreateInput{
		GoalID:     goalID,
		PatchID:    patchID,
		BaseBranch: "main",
		HeadBranch: "feature/pr-test",
		Title:      "Add rate limiting",
		Body:       "Implements rate limiting per obligation OB-PR-P5.",
	}

	prRecord, err := prwriter.Create(ctx, prCfg, in)
	if err != nil {
		t.Fatalf("prwriter.Create: %v", err)
	}
	if prRecord.PRURL == "" {
		t.Fatal("PRURL is empty")
	}
	if prRecord.PRURL != wantPRURL {
		t.Errorf("PRURL = %q, want %q", prRecord.PRURL, wantPRURL)
	}

	if err := env.st.SavePRRecord(ctx, goalID, prRecord); err != nil {
		t.Fatalf("SavePRRecord: %v", err)
	}

	// Assert pr_created event with PRURL
	events := mustReadAllEvents(t, env)
	var foundPRCreated bool
	for _, ev := range events {
		if ev.Type == schema.EventPRCreated && ev.GoalID == goalID {
			var pr schema.PRRecord
			if err := json.Unmarshal(ev.Payload, &pr); err != nil {
				t.Fatalf("unmarshal pr_created payload: %v", err)
			}
			if pr.PRURL != wantPRURL {
				t.Errorf("pr_created payload PRURL = %q, want %q", pr.PRURL, wantPRURL)
			}
			foundPRCreated = true
		}
	}
	if !foundPRCreated {
		t.Fatal("pr_created event not found")
	}

	// Confirm LoadPRRecord works
	loaded, err := env.st.LoadPRRecord(ctx, goalID, prRecord.PRID)
	if err != nil {
		t.Fatalf("LoadPRRecord: %v", err)
	}
	if loaded.PRURL != wantPRURL {
		t.Errorf("loaded PRURL = %q, want %q", loaded.PRURL, wantPRURL)
	}
}

// TestPhase5_Replay_PhaseArtifactsReconstructFromEventLog runs a full Phase 5
// cycle (intake + CI + PR creation), wipes artifact files, replays, and asserts
// that IntakeRecord, CIStatusRecord, and PRRecord all reconstruct correctly.
func TestPhase5_Replay_PhaseArtifactsReconstructFromEventLog(t *testing.T) {
	env := newIntegEnv(t)
	ctx := env.ctx
	now := time.Now().UTC()

	const (
		goalID    = "G-PHASE5-REPLAY"
		condID    = "GC-PHASE5-REPLAY"
		intakeID  = "INTAKE-PHASE5-REPLAY"
		ciStatID  = "CISTAT-PHASE5-REPLAY"
		prID      = "PR-PHASE5-REPLAY"
		wantPRURL = "https://github.com/owner/repo/pull/99"
	)

	// Seed goal
	if err := env.st.SaveGoal(ctx, &schema.GoalIR{
		GoalID: goalID, OriginalIntent: "phase5 replay test",
		GoalConditions: []schema.GoalCondition{{
			ID: condID, Description: "c", EffectiveDescription: "c",
			Status: schema.GoalConditionUnmet,
		}},
		RiskLevel: schema.RiskLow, Status: schema.GoalStatusActive, CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}

	// Save intake record
	intakeRec := &schema.IntakeRecord{
		RecordID:    intakeID,
		GoalID:      goalID,
		Source:      "github_issue",
		ExternalID:  "99",
		ExternalURL: "https://github.com/owner/repo/issues/99",
		IngestedAt:  now,
	}
	if err := env.st.SaveIntakeRecord(ctx, goalID, intakeRec); err != nil {
		t.Fatalf("SaveIntakeRecord: %v", err)
	}

	// Save CI status record
	ciRec := &schema.CIStatusRecord{
		RecordID:   ciStatID,
		GoalID:     goalID,
		CapsuleID:  "CAP-PHASE5-REPLAY",
		Provider:   "github_actions",
		Branch:     "feature/replay",
		Status:     "success",
		RunURL:     "https://github.com/owner/repo/actions/runs/99",
		RecordedAt: now,
	}
	if err := env.st.SaveCIStatusRecord(ctx, goalID, ciRec); err != nil {
		t.Fatalf("SaveCIStatusRecord: %v", err)
	}

	// Save PR record
	prRec := &schema.PRRecord{
		PRID:       prID,
		GoalID:     goalID,
		PatchID:    "PATCH-PHASE5-REPLAY",
		PRURL:      wantPRURL,
		BaseBranch: "main",
		HeadBranch: "feature/replay",
		CreatedAt:  now,
	}
	if err := env.st.SavePRRecord(ctx, goalID, prRec); err != nil {
		t.Fatalf("SavePRRecord: %v", err)
	}

	// Count events before wipe
	preEventCount := len(mustReadAllEvents(t, env))

	// Wipe all artifact files, replay from scratch
	wipeArtifactFiles(t, env)

	if err := store.Replay(ctx, env.log, env.st, 0); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	// Replay must not append new events
	postEventCount := len(mustReadAllEvents(t, env))
	if postEventCount != preEventCount {
		t.Errorf("event count changed during replay: before=%d after=%d (must be read-only)",
			preEventCount, postEventCount)
	}

	// Assert IntakeRecord reconstructed
	loadedIntake, err := env.st.LoadIntakeRecord(ctx, goalID, intakeID)
	if err != nil {
		t.Fatalf("LoadIntakeRecord after replay: %v", err)
	}
	if loadedIntake.ExternalID != "99" {
		t.Errorf("IntakeRecord.ExternalID = %q, want 99", loadedIntake.ExternalID)
	}
	if loadedIntake.Source != "github_issue" {
		t.Errorf("IntakeRecord.Source = %q, want github_issue", loadedIntake.Source)
	}

	// Assert CIStatusRecord reconstructed
	loadedCI, err := env.st.LoadCIStatusRecord(ctx, goalID, ciStatID)
	if err != nil {
		t.Fatalf("LoadCIStatusRecord after replay: %v", err)
	}
	if loadedCI.Status != "success" {
		t.Errorf("CIStatusRecord.Status = %q, want success", loadedCI.Status)
	}
	if loadedCI.Provider != "github_actions" {
		t.Errorf("CIStatusRecord.Provider = %q, want github_actions", loadedCI.Provider)
	}

	// Assert PRRecord reconstructed
	loadedPR, err := env.st.LoadPRRecord(ctx, goalID, prID)
	if err != nil {
		t.Fatalf("LoadPRRecord after replay: %v", err)
	}
	if loadedPR.PRURL != wantPRURL {
		t.Errorf("PRRecord.PRURL = %q, want %q", loadedPR.PRURL, wantPRURL)
	}
}

// TestPhase5_PriorPhasesUnaffected verifies that running a Phase 1-4 scenario
// with all Phase 5 config absent leaves reconciler behavior unchanged.
// It reuses buildCompleteScenario and confirms PatchAccepted is still true.
func TestPhase5_PriorPhasesUnaffected(t *testing.T) {
	env := newIntegEnv(t)

	// Build a standard Phase 1 scenario (no Phase 5 artifacts involved)
	ids := buildCompleteScenario(t, env)

	// Run reconciler — same as TestCrashResumableRun Phase B
	// No MCP, intake, CI, PR, or remote config is active.
	rec := reconciler.New(env.st, env.log, reconciler.Config{})
	result, err := rec.Reconcile(env.ctx, ids.PatchID)
	if err != nil {
		t.Fatalf("Reconcile with Phase 5 artifacts absent: %v", err)
	}
	if !result.PatchAccepted {
		t.Errorf("PatchAccepted = false; Phase 5 additions must not break Phase 1-4 reconcile")
	}

	// Confirm no Phase 5 event types leaked into the log
	events := mustReadAllEvents(t, env)
	for _, ev := range events {
		switch ev.Type {
		case schema.EventPRCreated, schema.EventCIStatusReceived, schema.EventIntakeIssueIngested:
			t.Errorf("Phase 5 event type %s found in Phase 1-4-only scenario", ev.Type)
		}
	}
}

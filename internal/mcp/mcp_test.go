package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/mcp"
	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

// ── test helpers ──────────────────────────────────────────────────────────────

type testEnv struct {
	log *eventlog.FileLog
	st  *store.FileStore
	srv *httptest.Server
	ctx context.Context
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	l, err := eventlog.Open(filepath.Join(dir, "events.log"))
	if err != nil {
		t.Fatalf("open eventlog: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	st, err := store.New(dir, l)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	s := mcp.New(st, l)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return &testEnv{log: l, st: st, srv: ts, ctx: context.Background()}
}

// rpcCall performs a JSON-RPC 2.0 POST and unmarshals the response.
func (e *testEnv) rpcCall(t *testing.T, method string, params any) map[string]any {
	t.Helper()
	paramBytes, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  json.RawMessage(paramBytes),
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	resp, err := http.Post(e.srv.URL+"/", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return result
}

// toolCall calls a named MCP tool with the given arguments.
func (e *testEnv) toolCall(t *testing.T, toolName string, args any) map[string]any {
	t.Helper()
	argBytes, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return e.rpcCall(t, "tools/call", map[string]any{
		"name":      toolName,
		"arguments": json.RawMessage(argBytes),
	})
}

// toolText extracts the text content from a tools/call result.
// It fails if the result has no content or isError is true.
func toolText(t *testing.T, resp map[string]any) string {
	t.Helper()
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in response: %v", resp)
	}
	if isErr, _ := result["isError"].(bool); isErr {
		content := firstContentText(result)
		t.Fatalf("tool returned error: %s", content)
	}
	return firstContentText(result)
}

// toolError extracts the error text from a tools/call result with isError=true.
func toolError(t *testing.T, resp map[string]any) string {
	t.Helper()
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in response: %v", resp)
	}
	if isErr, _ := result["isError"].(bool); !isErr {
		t.Fatalf("expected tool error but got success: %v", result)
	}
	return firstContentText(result)
}

func firstContentText(result map[string]any) string {
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	item, _ := content[0].(map[string]any)
	text, _ := item["text"].(string)
	return text
}

// eventCount returns the total number of events in the log.
func (e *testEnv) eventCount(t *testing.T) int {
	t.Helper()
	events, err := e.log.ReadAfter(e.ctx, 0, 0)
	if err != nil {
		t.Fatalf("ReadAfter: %v", err)
	}
	return len(events)
}

// seeding helpers

func seedGoal(t *testing.T, st *store.FileStore, ctx context.Context, goalID string) *schema.GoalIR {
	t.Helper()
	g := &schema.GoalIR{
		GoalID:         goalID,
		OriginalIntent: "test goal for mcp",
		GoalConditions: []schema.GoalCondition{
			{ID: "GC-1", Description: "condition 1", EffectiveDescription: "condition 1", Status: schema.GoalConditionUnmet},
		},
		RiskLevel: schema.RiskLow,
		CreatedAt: time.Now().UTC(),
		Status:    schema.GoalStatusActive,
	}
	if err := st.SaveGoal(ctx, g); err != nil {
		t.Fatalf("SaveGoal: %v", err)
	}
	return g
}

func seedObligation(t *testing.T, st *store.FileStore, ctx context.Context, oblID, condID string) *schema.Obligation {
	t.Helper()
	o := &schema.Obligation{
		ObligationID:    oblID,
		GoalConditionID: condID,
		Description:     "obligation " + oblID,
		Blocking:        true,
		RiskLevel:       schema.RiskLow,
		Status:          schema.ObligationOpen,
	}
	if err := st.SaveObligation(ctx, o); err != nil {
		t.Fatalf("SaveObligation %s: %v", oblID, err)
	}
	return o
}

func seedCapsule(t *testing.T, st *store.FileStore, ctx context.Context, capsuleID string, oblIDs ...string) *schema.ExecutionCapsule {
	t.Helper()
	c := &schema.ExecutionCapsule{
		CapsuleID:     capsuleID,
		ObligationIDs: oblIDs,
		Agent:         schema.AgentClaude,
		Role:          schema.RoleExecutor,
		State:         schema.CapsuleStatePending,
	}
	if err := st.SaveCapsule(ctx, c); err != nil {
		t.Fatalf("SaveCapsule %s: %v", capsuleID, err)
	}
	return c
}

func seedPatch(t *testing.T, st *store.FileStore, ctx context.Context, patchID, capsuleID string, oblIDs ...string) *schema.PatchArtifact {
	t.Helper()
	p := &schema.PatchArtifact{
		PatchID:              patchID,
		CapsuleID:            capsuleID,
		Status:               schema.PatchCandidate,
		ObligationIDsClaimed: oblIDs,
	}
	if err := st.SavePatch(ctx, p); err != nil {
		t.Fatalf("SavePatch %s: %v", patchID, err)
	}
	return p
}

func seedEvidence(t *testing.T, st *store.FileStore, ctx context.Context, evidenceID string, supports ...string) *schema.EvidenceArtifact {
	t.Helper()
	ev := &schema.EvidenceArtifact{
		EvidenceID: evidenceID,
		Type:       schema.EvidenceTestResult,
		Source:     "gate",
		ExitCode:   0,
		Summary:    "tests passed",
		Supports:   supports,
		CreatedAt:  time.Now().UTC(),
	}
	if err := st.SaveEvidence(ctx, ev); err != nil {
		t.Fatalf("SaveEvidence %s: %v", evidenceID, err)
	}
	return ev
}

func seedVerifierResult(t *testing.T, st *store.FileStore, ctx context.Context, resultID, patchID, capsuleID string) *schema.VerifierResult {
	t.Helper()
	vr := &schema.VerifierResult{
		VerifierResultID:  resultID,
		PatchID:           patchID,
		CapsuleID:         capsuleID,
		RecommendedAction: schema.ActionAccept,
		CreatedAt:         time.Now().UTC(),
	}
	if err := st.SaveVerifierResult(ctx, vr); err != nil {
		t.Fatalf("SaveVerifierResult %s: %v", resultID, err)
	}
	return vr
}

func seedBudgetRecord(t *testing.T, st *store.FileStore, ctx context.Context, budgetID, goalID string) *schema.BudgetRecord {
	t.Helper()
	b := &schema.BudgetRecord{
		BudgetID:    budgetID,
		GoalID:      goalID,
		TokensSpent: 1000,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := st.SaveBudgetRecord(ctx, b); err != nil {
		t.Fatalf("SaveBudgetRecord %s: %v", budgetID, err)
	}
	return b
}

// ── tests ─────────────────────────────────────────────────────────────────────

func TestMCP_Initialize(t *testing.T) {
	e := newEnv(t)
	resp := e.rpcCall(t, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0"},
	})
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result, got: %v", resp)
	}
	if got := result["protocolVersion"]; got != "2024-11-05" {
		t.Errorf("protocolVersion = %q, want 2024-11-05", got)
	}
	caps, _ := result["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Errorf("expected tools capability in capabilities: %v", caps)
	}
}

func TestMCP_ToolsList_ReturnsAllTools(t *testing.T) {
	e := newEnv(t)
	resp := e.rpcCall(t, "tools/list", map[string]any{})
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result, got: %v", resp)
	}
	tools, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("expected tools array, got: %v", result)
	}
	want := []string{
		"orca_get_goal",
		"orca_list_open_obligations",
		"orca_list_capsules",
		"orca_get_patch",
		"orca_list_patch_evidence",
		"orca_get_verifier_result_for_patch",
		"orca_get_budget_for_goal",
		"orca_get_merge_readiness",
		"orca_repo_status",
		"orca_repo_diff",
	}
	if len(tools) != len(want) {
		t.Fatalf("tool count = %d, want %d", len(tools), len(want))
	}
	names := make(map[string]bool)
	for _, item := range tools {
		tool, _ := item.(map[string]any)
		name, _ := tool["name"].(string)
		names[name] = true
	}
	for _, w := range want {
		if !names[w] {
			t.Errorf("missing tool %q", w)
		}
	}
}

func TestMCP_GetGoal_ActiveGoal(t *testing.T) {
	e := newEnv(t)
	seedGoal(t, e.st, e.ctx, "G-100")

	resp := e.toolCall(t, "orca_get_goal", map[string]any{})
	text := toolText(t, resp)

	var goal schema.GoalIR
	if err := json.Unmarshal([]byte(text), &goal); err != nil {
		t.Fatalf("unmarshal goal: %v", err)
	}
	if goal.GoalID != "G-100" {
		t.Errorf("GoalID = %q, want G-100", goal.GoalID)
	}
}

func TestMCP_GetGoal_ByID(t *testing.T) {
	e := newEnv(t)
	seedGoal(t, e.st, e.ctx, "G-200")

	resp := e.toolCall(t, "orca_get_goal", map[string]any{"goal_id": "G-200"})
	text := toolText(t, resp)

	var goal schema.GoalIR
	if err := json.Unmarshal([]byte(text), &goal); err != nil {
		t.Fatalf("unmarshal goal: %v", err)
	}
	if goal.GoalID != "G-200" {
		t.Errorf("GoalID = %q, want G-200", goal.GoalID)
	}
}

func TestMCP_GetGoal_NoActiveGoal_ReturnsError(t *testing.T) {
	e := newEnv(t)
	resp := e.toolCall(t, "orca_get_goal", map[string]any{})
	errText := toolError(t, resp)
	if !strings.Contains(errText, "no active goal") {
		t.Errorf("error text %q does not contain 'no active goal'", errText)
	}
}

func TestMCP_ListOpenObligations(t *testing.T) {
	e := newEnv(t)
	g := seedGoal(t, e.st, e.ctx, "G-300")
	seedObligation(t, e.st, e.ctx, "OB-1", g.GoalConditions[0].ID)
	seedObligation(t, e.st, e.ctx, "OB-2", g.GoalConditions[0].ID)

	resp := e.toolCall(t, "orca_list_open_obligations", map[string]any{"goal_id": "G-300"})
	text := toolText(t, resp)

	var obligations []*schema.Obligation
	if err := json.Unmarshal([]byte(text), &obligations); err != nil {
		t.Fatalf("unmarshal obligations: %v", err)
	}
	if len(obligations) != 2 {
		t.Errorf("obligation count = %d, want 2", len(obligations))
	}
}

func TestMCP_ListCapsules(t *testing.T) {
	e := newEnv(t)
	g := seedGoal(t, e.st, e.ctx, "G-400")
	obl1 := seedObligation(t, e.st, e.ctx, "OB-400-1", g.GoalConditions[0].ID)
	obl2 := seedObligation(t, e.st, e.ctx, "OB-400-2", g.GoalConditions[0].ID)
	seedCapsule(t, e.st, e.ctx, "CAP-1", obl1.ObligationID)
	seedCapsule(t, e.st, e.ctx, "CAP-2", obl2.ObligationID)
	if err := e.st.AppendRuntimeEvent(e.ctx, &schema.CapsuleRuntimeEvent{
		CapsuleID: "CAP-1",
		GoalID:    g.GoalID,
		Source:    "runner",
		Status:    schema.RuntimeStatusOutputCollecting,
	}); err != nil {
		t.Fatalf("AppendRuntimeEvent: %v", err)
	}

	resp := e.toolCall(t, "orca_list_capsules", map[string]any{"goal_id": g.GoalID})
	text := toolText(t, resp)

	var capsules []struct {
		CapsuleID     string                      `json:"capsule_id"`
		RuntimeStatus *schema.CapsuleRuntimeEvent `json:"runtime_status,omitempty"`
	}
	if err := json.Unmarshal([]byte(text), &capsules); err != nil {
		t.Fatalf("unmarshal capsules: %v", err)
	}
	if len(capsules) != 2 {
		t.Errorf("capsule count = %d, want 2", len(capsules))
	}
	if capsules[0].CapsuleID != "CAP-1" || capsules[0].RuntimeStatus == nil || capsules[0].RuntimeStatus.Status != schema.RuntimeStatusOutputCollecting {
		t.Fatalf("first capsule runtime status = %+v", capsules[0])
	}
}

func TestMCP_GetPatch(t *testing.T) {
	e := newEnv(t)
	g := seedGoal(t, e.st, e.ctx, "G-500")
	seedObligation(t, e.st, e.ctx, "OB-P1", g.GoalConditions[0].ID)
	seedCapsule(t, e.st, e.ctx, "CAP-P1", "OB-P1")
	seedPatch(t, e.st, e.ctx, "PA-1", "CAP-P1", "OB-P1")

	resp := e.toolCall(t, "orca_get_patch", map[string]any{"patch_id": "PA-1"})
	text := toolText(t, resp)

	var patch schema.PatchArtifact
	if err := json.Unmarshal([]byte(text), &patch); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	if patch.PatchID != "PA-1" {
		t.Errorf("PatchID = %q, want PA-1", patch.PatchID)
	}
}

func TestMCP_GetPatch_Missing_ReturnsError(t *testing.T) {
	e := newEnv(t)
	resp := e.toolCall(t, "orca_get_patch", map[string]any{"patch_id": "PA-MISSING"})
	errText := toolError(t, resp)
	if errText == "" {
		t.Error("expected non-empty error text for missing patch")
	}
}

func TestMCP_ListPatchEvidence(t *testing.T) {
	e := newEnv(t)
	g := seedGoal(t, e.st, e.ctx, "G-600")
	seedObligation(t, e.st, e.ctx, "OB-E1", g.GoalConditions[0].ID)
	seedObligation(t, e.st, e.ctx, "OB-E2", g.GoalConditions[0].ID)
	seedCapsule(t, e.st, e.ctx, "CAP-E1", "OB-E1", "OB-E2")
	seedPatch(t, e.st, e.ctx, "PA-E1", "CAP-E1", "OB-E1", "OB-E2")
	seedEvidence(t, e.st, e.ctx, "EV-1", "OB-E1")
	seedEvidence(t, e.st, e.ctx, "EV-2", "OB-E2")

	resp := e.toolCall(t, "orca_list_patch_evidence", map[string]any{"patch_id": "PA-E1"})
	text := toolText(t, resp)

	var evidence []*schema.EvidenceArtifact
	if err := json.Unmarshal([]byte(text), &evidence); err != nil {
		t.Fatalf("unmarshal evidence: %v", err)
	}
	if len(evidence) != 2 {
		t.Errorf("evidence count = %d, want 2", len(evidence))
	}
}

func TestMCP_GetVerifierResultForPatch(t *testing.T) {
	e := newEnv(t)
	g := seedGoal(t, e.st, e.ctx, "G-700")
	seedObligation(t, e.st, e.ctx, "OB-V1", g.GoalConditions[0].ID)
	seedCapsule(t, e.st, e.ctx, "CAP-V1", "OB-V1")
	seedPatch(t, e.st, e.ctx, "PA-V1", "CAP-V1")
	seedVerifierResult(t, e.st, e.ctx, "VR-1", "PA-V1", "CAP-V1")

	resp := e.toolCall(t, "orca_get_verifier_result_for_patch", map[string]any{"patch_id": "PA-V1"})
	text := toolText(t, resp)

	var vr schema.VerifierResult
	if err := json.Unmarshal([]byte(text), &vr); err != nil {
		t.Fatalf("unmarshal verifier result: %v", err)
	}
	if vr.VerifierResultID != "VR-1" {
		t.Errorf("VerifierResultID = %q, want VR-1", vr.VerifierResultID)
	}
	if vr.PatchID != "PA-V1" {
		t.Errorf("PatchID = %q, want PA-V1", vr.PatchID)
	}
}

func TestMCP_GetVerifierResult_Missing_ReturnsError(t *testing.T) {
	e := newEnv(t)
	g := seedGoal(t, e.st, e.ctx, "G-710")
	seedObligation(t, e.st, e.ctx, "OB-VX1", g.GoalConditions[0].ID)
	seedCapsule(t, e.st, e.ctx, "CAP-VX", "OB-VX1")
	seedPatch(t, e.st, e.ctx, "PA-VX", "CAP-VX")

	resp := e.toolCall(t, "orca_get_verifier_result_for_patch", map[string]any{"patch_id": "PA-VX"})
	errText := toolError(t, resp)
	if errText == "" {
		t.Error("expected non-empty error text for missing verifier result")
	}
}

func TestMCP_GetBudgetForGoal(t *testing.T) {
	e := newEnv(t)
	seedGoal(t, e.st, e.ctx, "G-800")
	seedBudgetRecord(t, e.st, e.ctx, "BUD-1", "G-800")
	seedBudgetRecord(t, e.st, e.ctx, "BUD-2", "G-800")

	resp := e.toolCall(t, "orca_get_budget_for_goal", map[string]any{"goal_id": "G-800"})
	text := toolText(t, resp)

	var records []*schema.BudgetRecord
	if err := json.Unmarshal([]byte(text), &records); err != nil {
		t.Fatalf("unmarshal budget records: %v", err)
	}
	if len(records) != 2 {
		t.Errorf("budget record count = %d, want 2", len(records))
	}
	for _, r := range records {
		if r.TokensSpent != 1000 {
			t.Errorf("TokensSpent = %d, want 1000", r.TokensSpent)
		}
	}
}

func TestMCP_GetMergeReadiness_NoVerifierResult(t *testing.T) {
	e := newEnv(t)
	seedGoal(t, e.st, e.ctx, "G-900")

	resp := e.toolCall(t, "orca_get_merge_readiness", map[string]any{"goal_id": "G-900"})
	text := toolText(t, resp)

	var result map[string]any
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatalf("unmarshal readiness: %v", err)
	}
	if result["status"] != "unknown" {
		t.Errorf("status = %q, want unknown", result["status"])
	}
}

func TestMCP_GetMergeReadiness_BlockedByObligations(t *testing.T) {
	e := newEnv(t)
	g := seedGoal(t, e.st, e.ctx, "G-901")
	seedObligation(t, e.st, e.ctx, "OB-R1", g.GoalConditions[0].ID)
	seedCapsule(t, e.st, e.ctx, "CAP-R1", "OB-R1")
	seedPatch(t, e.st, e.ctx, "PA-R1", "CAP-R1")
	seedVerifierResult(t, e.st, e.ctx, "VR-R1", "PA-R1", "CAP-R1")

	resp := e.toolCall(t, "orca_get_merge_readiness", map[string]any{
		"goal_id":  "G-901",
		"patch_id": "PA-R1",
	})
	text := toolText(t, resp)

	var result map[string]any
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatalf("unmarshal readiness: %v", err)
	}
	if result["status"] != "blocked" {
		t.Errorf("status = %q, want blocked (open blocking obligation)", result["status"])
	}
	if count, _ := result["open_blocking_obligations"].(float64); count != 1 {
		t.Errorf("open_blocking_obligations = %v, want 1", result["open_blocking_obligations"])
	}
}

func TestMCP_GetMergeReadiness_Ready(t *testing.T) {
	e := newEnv(t)
	g := seedGoal(t, e.st, e.ctx, "G-902")
	seedObligation(t, e.st, e.ctx, "OB-R2", g.GoalConditions[0].ID)
	seedCapsule(t, e.st, e.ctx, "CAP-R2", "OB-R2")
	seedPatch(t, e.st, e.ctx, "PA-R2", "CAP-R2")
	seedVerifierResult(t, e.st, e.ctx, "VR-R2", "PA-R2", "CAP-R2")

	// Satisfy the obligation so LoadOpenObligations returns empty.
	satisfiedBy := []string{"EV-R2"}
	if err := e.st.UpdateObligationStatus(e.ctx, "OB-R2", schema.ObligationSatisfied, &satisfiedBy); err != nil {
		t.Fatalf("UpdateObligationStatus: %v", err)
	}
	// Accept the patch so it transitions from candidate to accepted.
	if err := e.st.UpdatePatchStatus(e.ctx, "PA-R2", schema.PatchAccepted); err != nil {
		t.Fatalf("UpdatePatchStatus: %v", err)
	}

	resp := e.toolCall(t, "orca_get_merge_readiness", map[string]any{
		"goal_id":  "G-902",
		"patch_id": "PA-R2",
	})
	text := toolText(t, resp)

	var result map[string]any
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatalf("unmarshal readiness: %v", err)
	}
	if result["status"] != "ready" {
		t.Errorf("status = %q, want ready", result["status"])
	}
}

func TestMCP_UnknownTool_ReturnsError(t *testing.T) {
	e := newEnv(t)
	resp := e.toolCall(t, "orca_does_not_exist", map[string]any{})
	errText := toolError(t, resp)
	if !strings.Contains(errText, "unknown tool") {
		t.Errorf("error %q does not contain 'unknown tool'", errText)
	}
}

func TestMCP_Notification_Returns204(t *testing.T) {
	e := newEnv(t)
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
		"params":  map[string]any{},
	})
	resp, err := http.Post(e.srv.URL+"/", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
}

func TestMCP_UnknownMethod_ReturnsMethodNotFound(t *testing.T) {
	e := newEnv(t)
	resp := e.rpcCall(t, "orca/no_such_method", map[string]any{})
	rpcErr, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error field in response, got: %v", resp)
	}
	if code, _ := rpcErr["code"].(float64); code != -32601 {
		t.Errorf("error code = %v, want -32601", rpcErr["code"])
	}
}

func TestMCP_ReadOnly_NoWritesToStoreOrLog(t *testing.T) {
	e := newEnv(t)
	g := seedGoal(t, e.st, e.ctx, "G-RO")
	seedObligation(t, e.st, e.ctx, "OB-RO1", g.GoalConditions[0].ID)
	seedCapsule(t, e.st, e.ctx, "CAP-RO1", "OB-RO1")
	seedPatch(t, e.st, e.ctx, "PA-RO1", "CAP-RO1", "OB-RO1")
	seedEvidence(t, e.st, e.ctx, "EV-RO1", "OB-RO1")
	seedVerifierResult(t, e.st, e.ctx, "VR-RO1", "PA-RO1", "CAP-RO1")
	seedBudgetRecord(t, e.st, e.ctx, "BUD-RO1", "G-RO")

	countBefore := e.eventCount(t)

	// Call all read tools and verify no new events are written.
	e.toolCall(t, "orca_get_goal", map[string]any{})
	e.toolCall(t, "orca_list_open_obligations", map[string]any{"goal_id": "G-RO"})
	e.toolCall(t, "orca_list_capsules", map[string]any{"goal_id": "G-RO"})
	e.toolCall(t, "orca_get_patch", map[string]any{"patch_id": "PA-RO1"})
	e.toolCall(t, "orca_list_patch_evidence", map[string]any{"patch_id": "PA-RO1"})
	e.toolCall(t, "orca_get_verifier_result_for_patch", map[string]any{"patch_id": "PA-RO1"})
	e.toolCall(t, "orca_get_budget_for_goal", map[string]any{"goal_id": "G-RO"})
	e.toolCall(t, "orca_get_merge_readiness", map[string]any{"goal_id": "G-RO", "patch_id": "PA-RO1"})

	countAfter := e.eventCount(t)
	if countAfter != countBefore {
		t.Errorf("event count changed: before=%d after=%d — MCP tools must not write to the log", countBefore, countAfter)
	}
}

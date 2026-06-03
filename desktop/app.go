package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is the Wails application backend. All exported methods are bound to the
// frontend and must not import internal/ packages. State is read directly from
// the .orca/ JSON artifact files.
type App struct {
	mu         sync.RWMutex
	ctx        context.Context
	cancelTail context.CancelFunc
	orcaDir    string
}

// NewApp creates the App with the given .orca directory path.
func NewApp(orcaDir string) *App {
	return &App{orcaDir: orcaDir}
}

func (a *App) startup(ctx context.Context) {
	if a.cancelTail != nil {
		a.cancelTail()
	}
	a.ctx = ctx
	tailCtx, cancel := context.WithCancel(ctx)
	a.cancelTail = cancel
	go a.tailEventLog(tailCtx)
}

func (a *App) shutdown(_ context.Context) {
	if a.cancelTail != nil {
		a.cancelTail()
		a.cancelTail = nil
	}
}

// SetOrcaDir updates the .orca directory and emits a state refresh.
func (a *App) SetOrcaDir(dir string) {
	a.mu.Lock()
	a.orcaDir = dir
	a.mu.Unlock()
	runtime.EventsEmit(a.ctx, "state:refresh")
}

// GetOrcaDir returns the current .orca directory path.
func (a *App) GetOrcaDir() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.orcaDir
}

func (a *App) dir() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.orcaDir
}

// ── Backend methods ───────────────────────────────────────────────────────────

// GetGoal returns the active goal, or the most-recently-created goal when no
// active goal exists. Returns nil when no goals have been created.
func (a *App) GetGoal() (*GoalView, error) {
	goals, err := scanDir[goalDisk](orcaPath(a.dir(), relGoals))
	if err != nil {
		return nil, err
	}
	// Prefer the active goal; fall back to the most recent.
	for i := range goals {
		if goals[i].Status == "active" {
			v := toGoalView(goals[i])
			return &v, nil
		}
	}
	if len(goals) == 0 {
		return nil, nil
	}
	sort.Slice(goals, func(i, j int) bool {
		return goals[i].CreatedAt.After(goals[j].CreatedAt)
	})
	v := toGoalView(goals[0])
	return &v, nil
}

// ListObligations returns obligations for the active or most recent goal, sorted by ID.
func (a *App) ListObligations() ([]ObligationView, error) {
	dir := a.dir()
	scope, err := loadActiveGoalScopeFromDir(dir)
	if err != nil {
		return nil, err
	}
	obs := scope.obligations
	if scope.goalID == "" {
		obs, err = scanDir[obligationDisk](orcaPath(dir, relObligations))
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(obs, func(i, j int) bool {
		return obs[i].ObligationID < obs[j].ObligationID
	})
	out := make([]ObligationView, 0, len(obs))
	for _, o := range obs {
		out = append(out, toObligationView(o))
	}
	return out, nil
}

// ListCapsules returns capsules for the active or most recent goal, sorted by ID.
func (a *App) ListCapsules() ([]CapsuleView, error) {
	dir := a.dir()
	scope, err := loadActiveGoalScopeFromDir(dir)
	if err != nil {
		return nil, err
	}
	caps := scope.capsules
	if scope.goalID == "" {
		caps, err = scanDir[capsuleDisk](orcaPath(dir, relCapsules))
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(caps, func(i, j int) bool {
		return caps[i].CapsuleID < caps[j].CapsuleID
	})
	out := make([]CapsuleView, 0, len(caps))
	for _, c := range caps {
		runtimeStatus, err := readFile[capsuleRuntimeDisk](filepath.Join(orcaPath(dir, relCapsuleRuntime), c.CapsuleID+".json"))
		if err != nil {
			return nil, err
		}
		out = append(out, toCapsuleView(c, runtimeStatus))
	}
	return out, nil
}

// ListPatches returns patches for the active or most recent goal, sorted by patch ID.
func (a *App) ListPatches() ([]PatchView, error) {
	dir := a.dir()
	scope, err := loadActiveGoalScopeFromDir(dir)
	if err != nil {
		return nil, err
	}
	patches, err := scanDir[patchDisk](orcaPath(dir, relPatches))
	if err != nil {
		return nil, err
	}
	patches = filterPatchesByScope(patches, scope)
	sort.Slice(patches, func(i, j int) bool {
		return patches[i].PatchID < patches[j].PatchID
	})
	out := make([]PatchView, 0, len(patches))
	for _, p := range patches {
		out = append(out, toPatchView(p))
	}
	return out, nil
}

// ListEvidence returns evidence artifacts that support obligations claimed by
// the given patch. Returns an empty slice when the patch does not exist.
func (a *App) ListEvidence(patchID string) ([]EvidenceView, error) {
	if patchID == "" {
		return []EvidenceView{}, nil
	}
	dir := a.dir() // snapshot once so both reads use the same directory
	patch, err := readFile[patchDisk](filepath.Join(orcaPath(dir, relPatches), patchID+".json"))
	if err != nil || patch == nil {
		return []EvidenceView{}, nil
	}

	claimed := make(map[string]bool, len(patch.ObligationIDsClaimed))
	for _, id := range patch.ObligationIDsClaimed {
		claimed[id] = true
	}

	allEvidence, err := scanDir[evidenceDisk](orcaPath(dir, relEvidence))
	if err != nil {
		return nil, err
	}

	out := make([]EvidenceView, 0)
	for _, e := range allEvidence {
		if evidenceSupportsClaimed(e.Supports, claimed) {
			out = append(out, toEvidenceView(e))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].EvidenceID < out[j].EvidenceID
	})
	return out, nil
}

func evidenceSupportsClaimed(supports []string, claimed map[string]bool) bool {
	for _, id := range supports {
		if claimed[id] {
			return true
		}
	}
	return false
}

// ListFailures returns failure fingerprints for the active or most recent goal,
// sorted by failure ID.
func (a *App) ListFailures() ([]FailureView, error) {
	dir := a.dir()
	scope, err := loadActiveGoalScopeFromDir(dir)
	if err != nil {
		return nil, err
	}
	failures, err := scanDir[failureDisk](orcaPath(dir, relFailures))
	if err != nil {
		return nil, err
	}
	failures = filterFailuresByScope(failures, scope)
	sort.Slice(failures, func(i, j int) bool {
		return failures[i].FailureID < failures[j].FailureID
	})
	out := make([]FailureView, 0, len(failures))
	for _, f := range failures {
		out = append(out, toFailureView(f))
	}
	return out, nil
}

// GetBudget returns budget records for a specific capsule, or all records when
// capsuleID is empty. Returns nil when no matching records exist.
func (a *App) GetBudget(capsuleID string) (*BudgetView, error) {
	records, err := scanDir[budgetDisk](orcaPath(a.dir(), relBudgets))
	if err != nil {
		return nil, err
	}
	// Aggregate records for the requested capsule.
	agg := BudgetView{CapsuleID: capsuleID}
	found := false
	for _, r := range records {
		if capsuleID != "" && r.CapsuleID != capsuleID {
			continue
		}
		if !found {
			agg.BudgetID = r.BudgetID
			agg.GoalID = r.GoalID
			found = true
		}
		agg.TokensSpent += r.TokensSpent
		agg.WallTimeSeconds += r.WallTimeSeconds
		agg.ToolCalls += r.ToolCalls
		agg.Retries += r.Retries
		agg.ObligationsDischarged += r.ObligationsDischarged
		agg.PatchesAccepted += r.PatchesAccepted
		agg.PatchesRejected += r.PatchesRejected
	}
	if !found {
		return nil, nil
	}
	return &agg, nil
}

// GetBudgetSummary returns aggregated budget totals for the active or most recent goal.
func (a *App) GetBudgetSummary() (*BudgetSummary, error) {
	dir := a.dir()
	scope, err := loadActiveGoalScopeFromDir(dir)
	if err != nil {
		return nil, err
	}
	records, err := scanDir[budgetDisk](orcaPath(dir, relBudgets))
	if err != nil {
		return nil, err
	}
	s := &BudgetSummary{}
	for _, r := range records {
		if scope.goalID != "" && r.GoalID != scope.goalID {
			continue
		}
		s.TotalTokensSpent += r.TokensSpent
		s.TotalWallTimeSeconds += r.WallTimeSeconds
		s.TotalToolCalls += r.ToolCalls
		s.TotalRetries += r.Retries
		s.TotalDischarged += r.ObligationsDischarged
		s.TotalPatchesAccepted += r.PatchesAccepted
		s.TotalPatchesRejected += r.PatchesRejected
	}
	return s, nil
}

// GetMergeReadiness derives the merge-readiness string from the current
// artifact state. Mirrors the logic in cmd/orca.printStatus.
func (a *App) GetMergeReadiness() (string, error) {
	dir := a.dir() // snapshot once for a consistent read across all scans
	scope, err := loadActiveGoalScopeFromDir(dir)
	if err != nil {
		return "unknown", err
	}
	verifierResults, err := scanDir[verifierResultDisk](orcaPath(dir, relVerifierResults))
	if err != nil {
		return "unknown", err
	}
	verifierResults = filterVerifierResultsByScope(verifierResults, scope)
	patches, err := scanDir[patchDisk](orcaPath(dir, relPatches))
	if err != nil {
		return "unknown", err
	}
	decisions, err := scanDir[decisionDisk](orcaPath(dir, relDecisions))
	if err != nil {
		return "unknown", err
	}
	return deriveMergeReadiness(scope.obligations, verifierResults, patches, decisions), nil
}

// deriveMergeReadiness is the pure merge-readiness logic, extracted for testing.
func deriveMergeReadiness(
	obligations []obligationDisk,
	verifierResults []verifierResultDisk,
	patches []patchDisk,
	decisions []decisionDisk,
) string {
	if len(verifierResults) == 0 {
		return "unknown"
	}
	// Pick the latest verifier result by sequence (approximated by slice position;
	// files are written monotonically and ReadDir returns them sorted by name,
	// which for UUID-based IDs does not guarantee order — use CreatedAt instead).
	latest := latestVerifierResult(verifierResults)

	// Blocked if any open blocking obligation or verifier blocking failures.
	if hasOpenBlockingObligation(obligations) || len(latest.BlockingFailures) > 0 {
		return "blocked"
	}

	// Find the patch for the latest verifier result.
	patch := findPatch(patches, latest.PatchID)
	if patch == nil {
		return "pending_reconciliation"
	}
	switch patch.Status {
	case "accepted":
		// Need human review if any open gate decision is still pending.
		// Pass nil capsules: at merge-readiness check time capsules are completed,
		// so projection_review gates cannot be pending; only merge_review matters here.
		blocked := deriveBlockedDecisions(obligations, nil, patches, verifierResults, decisions)
		if len(blocked) > 0 {
			return "needs_human_review"
		}
		return "ready"
	case "candidate":
		return "pending_reconciliation"
	default:
		return "blocked"
	}
}

// GetBlockedDecisions derives the set of human-gate decisions that are needed
// but have not yet been made.
func (a *App) GetBlockedDecisions() ([]PendingGate, error) {
	dir := a.dir() // snapshot once for a consistent read across all scans
	scope, err := loadActiveGoalScopeFromDir(dir)
	if err != nil {
		return nil, err
	}
	patches, err := scanDir[patchDisk](orcaPath(dir, relPatches))
	if err != nil {
		return nil, err
	}
	verifierResults, err := scanDir[verifierResultDisk](orcaPath(dir, relVerifierResults))
	if err != nil {
		return nil, err
	}
	verifierResults = filterVerifierResultsByScope(verifierResults, scope)
	decisions, err := scanDir[decisionDisk](orcaPath(dir, relDecisions))
	if err != nil {
		return nil, err
	}
	return deriveBlockedDecisions(scope.obligations, scope.capsules, patches, verifierResults, decisions), nil
}

// ListDecisions returns all decision records, sorted by creation time descending.
func (a *App) ListDecisions() ([]DecisionView, error) {
	decs, err := scanDir[decisionDisk](orcaPath(a.dir(), relDecisions))
	if err != nil {
		return nil, err
	}
	sort.Slice(decs, func(i, j int) bool {
		return decs[i].CreatedAt.After(decs[j].CreatedAt)
	})
	out := make([]DecisionView, 0, len(decs))
	for _, d := range decs {
		out = append(out, toDecisionView(d))
	}
	return out, nil
}

// ── Derivation helpers ────────────────────────────────────────────────────────

func latestVerifierResult(results []verifierResultDisk) verifierResultDisk {
	latest := results[0]
	for _, r := range results[1:] {
		if r.CreatedAt.After(latest.CreatedAt) {
			latest = r
		}
	}
	return latest
}

func hasOpenBlockingObligation(obligations []obligationDisk) bool {
	for _, o := range obligations {
		if o.Blocking && o.Status == "open" {
			return true
		}
	}
	return false
}

func findPatch(patches []patchDisk, patchID string) *patchDisk {
	for i := range patches {
		if patches[i].PatchID == patchID {
			return &patches[i]
		}
	}
	return nil
}

// deriveBlockedDecisions computes pending gate decisions needed based on current state.
// Mirrors the blockingHumanDecisions logic from cmd/orca/main.go.
func deriveBlockedDecisions(
	obligations []obligationDisk,
	capsules []capsuleDisk,
	patches []patchDisk,
	verifierResults []verifierResultDisk,
	decisions []decisionDisk,
) []PendingGate {
	var out []PendingGate

	// projection_review: active executor capsules whose topology+risk requires
	// human sign-off before the agent runs.
	maxRisk := maxObligationRisk(obligations)
	for _, cap := range capsules {
		if cap.Role != "executor" || !isActiveCapsuleState(cap.State) {
			continue
		}
		if cap.TopologyDecisionID == "" {
			continue
		}
		topo := findDecisionTopology(decisions, cap.TopologyDecisionID)
		if topo == "" {
			continue
		}
		if shouldReviewProjection(topo, maxRisk) {
			if !hasGateDecision(decisions, "projection_review", cap.CapsuleID) {
				out = append(out, PendingGate{
					GateType:  "projection_review",
					RelatedID: cap.CapsuleID,
					Reason:    "executor capsule requires human projection review before running",
				})
			}
		}
	}

	// merge_review: accepted patches with high-risk obligations awaiting human merge approval.
	if len(verifierResults) > 0 {
		latest := latestVerifierResult(verifierResults)
		if !hasOpenBlockingObligation(obligations) && len(latest.BlockingFailures) == 0 {
			patch := findPatch(patches, latest.PatchID)
			if patch != nil && patch.Status == "accepted" {
				if resultHasHighRiskObligation(latest, obligations) {
					if !hasGateDecision(decisions, "merge_review", latest.PatchID) {
						out = append(out, PendingGate{
							GateType:  "merge_review",
							RelatedID: latest.PatchID,
							Reason:    "patch accepted with high-risk obligation; human merge approval required",
						})
					}
				}
			}
		}
	}

	return out
}

// maxObligationRisk returns the highest risk level among all open obligations.
func maxObligationRisk(obligations []obligationDisk) string {
	max := "low"
	for _, o := range obligations {
		if o.Status != "open" {
			continue
		}
		switch o.RiskLevel {
		case "high":
			return "high"
		case "medium":
			max = "medium"
		}
	}
	return max
}

// shouldReviewProjection mirrors gate.ShouldReviewProjection without importing
// the internal/gate package.
func shouldReviewProjection(topology, maxRisk string) bool {
	switch topology {
	case "human_gated":
		return true
	case "implementer_reviewer":
		return maxRisk == "medium" || maxRisk == "high"
	case "single", "parallel", "test_first", "investigate_then_implement":
		return true
	default:
		return false
	}
}

// findDecisionTopology finds the topology string for a topology_selection
// decision record by its ID.
func findDecisionTopology(decisions []decisionDisk, decisionID string) string {
	for _, d := range decisions {
		if d.DecisionID == decisionID && d.Context == "topology_selection" {
			return d.Decision
		}
	}
	return ""
}

// isActiveCapsuleState reports whether a capsule is in a pre-completion state
// where a projection_review gate could still be pending.
func isActiveCapsuleState(state string) bool {
	switch state {
	case "pending", "worktree_created", "workspace_attached", "setup_run", "agent_running":
		return true
	default:
		return false
	}
}

func resultHasHighRiskObligation(result verifierResultDisk, obligations []obligationDisk) bool {
	oblByID := make(map[string]obligationDisk, len(obligations))
	for _, o := range obligations {
		oblByID[o.ObligationID] = o
	}
	for _, verdict := range result.ObligationResults {
		if obl, ok := oblByID[verdict.ObligationID]; ok {
			if obl.RiskLevel == "high" {
				return true
			}
		}
	}
	return false
}

func hasGateDecision(decisions []decisionDisk, gateCtx, relatedID string) bool {
	for _, d := range decisions {
		if d.Context != gateCtx {
			continue
		}
		for _, id := range d.RelatedIDs {
			if id == relatedID {
				return true
			}
		}
	}
	return false
}

// ── Goal scoping ──────────────────────────────────────────────────────────────

// goalScope holds the active-goal artifact ID sets used to filter disk scans
// so that GetMergeReadiness and GetBlockedDecisions only consider the active
// goal's records, mirroring the goal-scoped queries in cmd/orca printStatus.
type goalScope struct {
	goalID        string
	conditionIDs  map[string]bool
	obligationIDs map[string]bool
	capsuleIDs    map[string]bool
	// Pre-loaded filtered slices avoid re-scanning the same directories twice.
	obligations []obligationDisk
	capsules    []capsuleDisk
}

// loadActiveGoalScopeFromDir identifies the active (or most-recently-created)
// goal from dir and builds a goalScope from its conditions, obligations, and
// capsules. dir is the .orca/ root, snapshotted by the caller once per
// operation to avoid mixed-directory reads if SetOrcaDir is called concurrently.
// Returns a zero goalScope (empty maps, nil slices) when no goals exist.
func loadActiveGoalScopeFromDir(dir string) (goalScope, error) {
	goals, err := scanDir[goalDisk](orcaPath(dir, relGoals))
	if err != nil {
		return goalScope{}, err
	}
	active := findActiveGoalDisk(goals)
	if active == nil {
		return goalScope{}, nil
	}
	scope := goalScope{
		goalID:        active.GoalID,
		conditionIDs:  make(map[string]bool, len(active.GoalConditions)),
		obligationIDs: make(map[string]bool),
		capsuleIDs:    make(map[string]bool),
	}
	for _, c := range active.GoalConditions {
		scope.conditionIDs[c.ID] = true
	}
	allObs, err := scanDir[obligationDisk](orcaPath(dir, relObligations))
	if err != nil {
		return goalScope{}, err
	}
	for _, o := range allObs {
		if scope.conditionIDs[o.GoalConditionID] {
			scope.obligationIDs[o.ObligationID] = true
			scope.obligations = append(scope.obligations, o)
		}
	}
	allCaps, err := scanDir[capsuleDisk](orcaPath(dir, relCapsules))
	if err != nil {
		return goalScope{}, err
	}
	for _, c := range allCaps {
		for _, oblID := range c.ObligationIDs {
			if scope.obligationIDs[oblID] {
				scope.capsuleIDs[c.CapsuleID] = true
				scope.capsules = append(scope.capsules, c)
				break
			}
		}
	}
	return scope, nil
}

// findActiveGoalDisk returns the active goal from a slice, or the most-recently-
// created one if no goal has status "active". Returns nil for an empty slice.
func findActiveGoalDisk(goals []goalDisk) *goalDisk {
	for i := range goals {
		if goals[i].Status == "active" {
			return &goals[i]
		}
	}
	if len(goals) == 0 {
		return nil
	}
	sort.Slice(goals, func(i, j int) bool {
		return goals[i].CreatedAt.After(goals[j].CreatedAt)
	})
	return &goals[0]
}

// filterVerifierResultsByScope returns the subset of vrs whose CapsuleID is in
// the active goal's capsule set. When there is no active goal (conditionIDs is
// empty), all results are returned. When there is an active goal but no capsules
// have been created yet (capsuleIDs is empty), an empty slice is returned so
// that historical verifier results from other goals are not mixed in.
func filterVerifierResultsByScope(vrs []verifierResultDisk, scope goalScope) []verifierResultDisk {
	if scope.goalID == "" {
		return vrs // no active goal — no filter
	}
	if len(scope.capsuleIDs) == 0 {
		return nil // active goal has no capsules yet; no valid results exist
	}
	out := vrs[:0:0]
	for _, r := range vrs {
		if scope.capsuleIDs[r.CapsuleID] {
			out = append(out, r)
		}
	}
	return out
}

func filterPatchesByScope(patches []patchDisk, scope goalScope) []patchDisk {
	if scope.goalID == "" {
		return patches
	}
	if len(scope.capsuleIDs) == 0 {
		return nil
	}
	out := patches[:0:0]
	for _, p := range patches {
		if scope.capsuleIDs[p.CapsuleID] {
			out = append(out, p)
		}
	}
	return out
}

func filterFailuresByScope(failures []failureDisk, scope goalScope) []failureDisk {
	if scope.goalID == "" {
		return failures
	}
	if len(scope.capsuleIDs) == 0 {
		return nil
	}
	out := failures[:0:0]
	for _, f := range failures {
		if scope.capsuleIDs[f.SourceCapsuleID] {
			out = append(out, f)
		}
	}
	return out
}

// ── Timeline and setup health ─────────────────────────────────────────────────

// rawEventLogLine is the minimal JSON shape of one line in events.log.
type rawEventLogLine struct {
	Type       string          `json:"type"`
	GoalID     string          `json:"goal_id"`
	ArtifactID string          `json:"artifact_id,omitempty"`
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  time.Time       `json:"created_at"`
}

// GetTimeline reads the events.log JSONL and returns a filtered, ordered list
// of lifecycle timeline entries for the primary screen stepper. Routine
// housekeeping events are omitted. Returns at most 200 entries. Returns an
// empty slice when the log does not exist.
func (a *App) GetTimeline() ([]TimelineEntry, error) {
	dir := a.dir()
	scope, err := loadActiveGoalScopeFromDir(dir)
	if err != nil {
		return nil, err
	}
	logPath := filepath.Join(dir, "events.log")
	f, err := os.Open(logPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []TimelineEntry{}, nil
		}
		return nil, err
	}
	defer f.Close()

	var entries []TimelineEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20) // allow large artifact payload events
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev rawEventLogLine
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue // skip malformed lines
		}
		if scope.goalID != "" && ev.GoalID != scope.goalID {
			continue
		}
		if !timelineSignificant(ev.Type) {
			continue
		}
		entries = append(entries, timelineEntryFromEvent(ev))
	}
	if err := sc.Err(); err != nil {
		return entries, err
	}
	const maxEntries = 200
	if len(entries) > maxEntries {
		entries = entries[len(entries)-maxEntries:]
	}
	return entries, nil
}

// GetSetupHealth returns a lightweight disk-based setup health check from the
// .orca/ directory. Full doctor diagnostics are CLI-only (orca doctor).
func (a *App) GetSetupHealth() (*SetupHealthView, error) {
	dir := a.dir()
	_, configErr := os.Stat(filepath.Join(dir, "config.yaml"))
	configExists := configErr == nil
	_, logErr := os.Stat(filepath.Join(dir, "events.log"))
	eventLogExists := logErr == nil

	h := &SetupHealthView{
		ConfigExists:   configExists,
		EventLogExists: eventLogExists,
	}
	if !configExists {
		h.Warning = "config.yaml not found — run orca init to set up"
	}
	return h, nil
}

// timelineSignificant reports whether an event type is worth showing in the
// timeline stepper. Routine housekeeping events are excluded to keep the
// timeline focused on meaningful lifecycle steps.
func timelineSignificant(evType string) bool {
	switch evType {
	case "state_snapshot_saved",
		"budget_record_saved", "budget_record_updated",
		"capsule_projection_linked",
		"artifact_invalidated",
		"claim_status_updated", "claim_superseded",
		"topology_outcome_recorded",
		"pr_created", "ci_status_received", "intake_issue_ingested":
		return false
	}
	return true
}

// timelineEntryFromEvent converts a rawEventLogLine into a TimelineEntry.
func timelineEntryFromEvent(ev rawEventLogLine) TimelineEntry {
	summary, status := eventTypeLabel(ev.Type, ev.Payload)
	return TimelineEntry{
		At:      ev.CreatedAt,
		Type:    ev.Type,
		Summary: summary,
		Status:  status,
	}
}

// eventTypeLabel maps an events.log type string to a human-readable summary
// and a status indicator ("ok" | "error" | "warning" | ""). Unknown types
// fall back to their raw type string with underscores replaced by spaces.
func eventTypeLabel(evType string, payload json.RawMessage) (string, string) {
	switch evType {
	case "goal_created":
		return "Goal created", "ok"
	case "obligation_created":
		return "Obligation planned", ""
	case "topology_selected":
		if topo := payloadString(payload, "topology"); topo != "" {
			return "Topology: " + topo, "ok"
		}
		return "Topology selected", "ok"
	case "context_projection_created":
		return "Context compiled", ""
	case "capsule_created":
		return "Capsule created", ""
	case "capsule_started":
		return "Capsule started", ""
	case "capsule_state_updated":
		if state := payloadString(payload, "state"); state != "" {
			return "Capsule: " + strings.ReplaceAll(state, "_", " "), ""
		}
		return "Capsule state updated", ""
	case "capsule_completed":
		return "Capsule completed", "ok"
	case "patch_artifact_created":
		return "Patch produced", ""
	case "evidence_artifact_created":
		return "Evidence collected", ""
	case "failure_fingerprint_created":
		return "Failure recorded", "warning"
	case "verifier_result_created":
		return "Verification complete", ""
	case "obligation_status_updated":
		if s := payloadString(payload, "status"); s != "" {
			return "Obligation: " + s, ""
		}
		return "Obligation updated", ""
	case "patch_accepted":
		return "Patch accepted", "ok"
	case "patch_rejected":
		return "Patch rejected", "error"
	case "merge_applied":
		return "Merge applied", "ok"
	case "goal_status_updated":
		if s := payloadString(payload, "status"); s != "" {
			return "Goal: " + s, ""
		}
		return "Goal updated", ""
	case "decision_record_created":
		return "Decision recorded", ""
	case "claim_created":
		return "Claim created", ""
	default:
		return strings.ReplaceAll(evType, "_", " "), ""
	}
}

// payloadString extracts a single string field from a JSON payload object.
// Returns an empty string if the payload is invalid or the field is absent.
func payloadString(payload json.RawMessage, field string) string {
	if len(payload) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return ""
	}
	raw, ok := m[field]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

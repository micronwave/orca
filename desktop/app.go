package main

import (
	"context"
	"path/filepath"
	"sort"
	"sync"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is the Wails application backend. All exported methods are bound to the
// frontend and must not import internal/ packages. State is read directly from
// the .orca/ JSON artifact files.
type App struct {
	mu      sync.RWMutex
	ctx     context.Context
	orcaDir string
	stop    chan struct{}
}

// NewApp creates the App with the given .orca directory path.
func NewApp(orcaDir string) *App {
	return &App{
		orcaDir: orcaDir,
		stop:    make(chan struct{}, 1), // buffered so shutdown() never drops the signal
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	go a.tailEventLog()
}

func (a *App) shutdown(_ context.Context) {
	select {
	case a.stop <- struct{}{}:
	default:
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

// ListObligations returns all obligations in the store, sorted by ID.
func (a *App) ListObligations() ([]ObligationView, error) {
	obs, err := scanDir[obligationDisk](orcaPath(a.dir(), relObligations))
	if err != nil {
		return nil, err
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

// ListCapsules returns all capsules, sorted by ID.
func (a *App) ListCapsules() ([]CapsuleView, error) {
	caps, err := scanDir[capsuleDisk](orcaPath(a.dir(), relCapsules))
	if err != nil {
		return nil, err
	}
	sort.Slice(caps, func(i, j int) bool {
		return caps[i].CapsuleID < caps[j].CapsuleID
	})
	out := make([]CapsuleView, 0, len(caps))
	for _, c := range caps {
		out = append(out, toCapsuleView(c))
	}
	return out, nil
}

// ListPatches returns all patches, sorted by patch ID.
func (a *App) ListPatches() ([]PatchView, error) {
	patches, err := scanDir[patchDisk](orcaPath(a.dir(), relPatches))
	if err != nil {
		return nil, err
	}
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

// ListFailures returns all failure fingerprints, sorted by failure ID.
func (a *App) ListFailures() ([]FailureView, error) {
	failures, err := scanDir[failureDisk](orcaPath(a.dir(), relFailures))
	if err != nil {
		return nil, err
	}
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

// GetBudgetSummary returns aggregated budget totals across all records.
func (a *App) GetBudgetSummary() (*BudgetSummary, error) {
	records, err := scanDir[budgetDisk](orcaPath(a.dir(), relBudgets))
	if err != nil {
		return nil, err
	}
	s := &BudgetSummary{}
	for _, r := range records {
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
	if len(scope.conditionIDs) == 0 {
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


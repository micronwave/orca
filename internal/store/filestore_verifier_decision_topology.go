package store

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/micronwave/orca/internal/schema"
)

// ── Verifier Results ─────────────────────────────────────────────────────────

func (s *FileStore) SaveVerifierResult(ctx context.Context, r *schema.VerifierResult) error {
	if r == nil {
		return fmt.Errorf("store: verifier result is required")
	}
	if err := validateArtifactID("verifier result", r.VerifierResultID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("verifier result", s.artifactPath(dirVerifierResults, r.VerifierResultID), r.VerifierResultID); err != nil {
		return err
	}
	goalID, err := s.goalIDForCapsule(ctx, r.CapsuleID)
	if err != nil {
		return fmt.Errorf("store: SaveVerifierResult: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventVerifierResultCreated, goalID, r.VerifierResultID, r)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(s.artifactPath(dirVerifierResults, r.VerifierResultID), r))
}

func (s *FileStore) LoadVerifierResult(ctx context.Context, resultID string) (*schema.VerifierResult, error) {
	if err := validateArtifactID("verifier result", resultID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.VerifierResult](s.artifactPath(dirVerifierResults, resultID))
}

func (s *FileStore) LoadVerifierResultForPatch(ctx context.Context, patchID string) (*schema.VerifierResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all, err := scanDir[schema.VerifierResult](ctx, filepath.Join(s.root, dirVerifierResults))
	if err != nil {
		return nil, err
	}
	for _, r := range all {
		if r.PatchID == patchID {
			return r, nil
		}
	}
	return nil, ErrNotFound
}

// ── Decision Records ─────────────────────────────────────────────────────────

func (s *FileStore) SaveDecision(ctx context.Context, d *schema.DecisionRecord) error {
	if d == nil {
		return fmt.Errorf("store: decision record is required")
	}
	if err := validateArtifactID("decision", d.DecisionID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("decision", s.artifactPath(dirDecisions, d.DecisionID), d.DecisionID); err != nil {
		return err
	}
	goalID, err := s.goalIDForRelatedIDs(ctx, d.RelatedIDs)
	if err != nil {
		return fmt.Errorf("store: SaveDecision: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventDecisionRecordCreated, goalID, d.DecisionID, d)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(s.artifactPath(dirDecisions, d.DecisionID), d))
}

func (s *FileStore) LoadDecision(ctx context.Context, decisionID string) (*schema.DecisionRecord, error) {
	if err := validateArtifactID("decision", decisionID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.DecisionRecord](s.artifactPath(dirDecisions, decisionID))
}

// ── Topology Outcomes ────────────────────────────────────────────────────────

func (s *FileStore) SaveTopologyOutcome(ctx context.Context, r *schema.TopologyOutcomeRecord) error {
	if r == nil {
		return fmt.Errorf("store: topology outcome is required")
	}
	if err := validateArtifactID("topology outcome", r.OutcomeID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("topology outcome", s.artifactPath(dirTopologyOutcomes, r.OutcomeID), r.OutcomeID); err != nil {
		return err
	}
	if err := s.requireExistingGoal(r.GoalID); err != nil {
		return fmt.Errorf("store: SaveTopologyOutcome: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventTopologyOutcomeRecorded, r.GoalID, r.OutcomeID, r)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(s.artifactPath(dirTopologyOutcomes, r.OutcomeID), r))
}

func (s *FileStore) LoadTopologyOutcomesForGoal(ctx context.Context, goalID string) ([]*schema.TopologyOutcomeRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.requireExistingGoal(goalID); err != nil {
		return nil, err
	}
	all, err := scanDir[schema.TopologyOutcomeRecord](ctx, filepath.Join(s.root, dirTopologyOutcomes))
	if err != nil {
		return nil, err
	}
	out := make([]*schema.TopologyOutcomeRecord, 0, len(all))
	for _, r := range all {
		if r.GoalID == goalID {
			out = append(out, r)
		}
	}
	sortTopologyOutcomes(out)
	return out, nil
}

func (s *FileStore) LoadTopologyOutcomes(ctx context.Context, topology schema.Topology, maxRisk schema.RiskLevel) ([]*schema.TopologyOutcomeRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all, err := scanDir[schema.TopologyOutcomeRecord](ctx, filepath.Join(s.root, dirTopologyOutcomes))
	if err != nil {
		return nil, err
	}
	out := make([]*schema.TopologyOutcomeRecord, 0, len(all))
	for _, r := range all {
		if r.Topology == topology && r.MaxRiskLevel == maxRisk {
			out = append(out, r)
		}
	}
	sortTopologyOutcomes(out)
	return out, nil
}

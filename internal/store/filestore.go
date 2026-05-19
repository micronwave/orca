package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/schema"
)

// ErrNotFound is returned by Load methods when the requested artifact does not exist.
var ErrNotFound = errors.New("artifact not found")

var windowsReservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// MaterializationError means the authoritative event was durably appended, but
// updating the file-backed materialized view failed afterward. The caller must
// not retry the same semantic save as a new event; run replay or inspect Event.
type MaterializationError struct {
	Event schema.Event
	Err   error
}

func (e *MaterializationError) Error() string {
	return fmt.Sprintf("store: materialize event seq=%d type=%s: %v", e.Event.SequenceNum, e.Event.Type, e.Err)
}

func (e *MaterializationError) Unwrap() error {
	return e.Err
}

// Directory layout under the store root. Each constant is a path relative to root.
const (
	dirGoals           = "state/goals"
	dirObligations     = "state/obligations"
	dirCapsules        = "state/capsules"
	dirSnapshots       = "state/snapshots"
	dirPatches         = "artifacts/patches"
	dirEvidence        = "artifacts/evidence"
	dirClaims          = "artifacts/claims"
	dirProjExecutor    = "artifacts/projections/executor"
	dirProjHuman       = "artifacts/projections/human_summary"
	dirProjReviewer    = "artifacts/projections/reviewer"
	dirProjTester      = "artifacts/projections/tester"
	dirFailures        = "artifacts/failures"
	dirDecisions       = "artifacts/decisions"
	dirBudgets         = "artifacts/budgets"
	dirVerifierResults = "artifacts/verifier_results"
)

// FileStore is the file-backed JSON implementation of ArtifactStore.
//
// Each artifact is stored as an individual JSON file named {id}.json inside
// the appropriate subdirectory under root. Save methods append the
// corresponding event to the EventLog before writing the artifact file,
// ensuring the log is the authoritative history from which state can be
// replayed. Lifecycle/status update methods write mutated artifacts in place;
// callers (reconciler, capsule_runner) are responsible for appending any
// status-change events directly to the EventLog before calling them.
//
// All exported methods are safe for concurrent use.
type FileStore struct {
	root string
	log  eventlog.EventLog
	mu   sync.RWMutex
}

// New creates or opens the FileStore at root, building the required directory
// hierarchy. root is typically the .orca directory for the repository.
func New(root string, log eventlog.EventLog) (*FileStore, error) {
	dirs := []string{
		dirGoals, dirObligations, dirCapsules, dirSnapshots,
		dirPatches, dirEvidence, dirClaims,
		dirProjExecutor, dirProjHuman, dirProjReviewer, dirProjTester,
		dirFailures, dirDecisions, dirBudgets, dirVerifierResults,
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, fmt.Errorf("store: create dir %s: %w", d, err)
		}
	}
	return &FileStore{root: root, log: log}, nil
}

// ── low-level helpers (no locking; callers hold appropriate lock) ────────────

// artifactPath returns the JSON file path for an artifact ID in a directory.
func (s *FileStore) artifactPath(dir, id string) string {
	return filepath.Join(s.root, dir, id+".json")
}

func validateArtifactID(kind, id string) error {
	if id == "" {
		return fmt.Errorf("store: %s id is required", kind)
	}
	if id == "." || id == ".." || filepath.IsAbs(id) ||
		strings.Contains(id, "/") || strings.Contains(id, "\\") || strings.Contains(id, ":") {
		return fmt.Errorf("store: invalid %s id %q", kind, id)
	}
	name := id
	if base, _, ok := strings.Cut(id, "."); ok {
		name = base
	}
	if windowsReservedNames[strings.ToUpper(name)] {
		return fmt.Errorf("store: %s id %q is a reserved device name on Windows", kind, id)
	}
	return nil
}

// writeFile marshals v to JSON and atomically writes it to path via a
// temp-file rename.
func (s *FileStore) writeFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("store: write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("store: rename to %s: %w", path, err)
	}
	return nil
}

// readFile reads and JSON-unmarshals the file at path into a new T.
// Returns ErrNotFound if the file does not exist.
func readFile[T any](path string) (*T, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: read %s: %w", path, err)
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("store: unmarshal %s: %w", path, err)
	}
	return &v, nil
}

// scanDir reads every .json file in the absolute directory dir and returns
// the unmarshaled values. Returns nil slice (not error) if dir is missing.
func scanDir[T any](dir string) ([]*T, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: readdir %s: %w", dir, err)
	}
	var out []*T
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		v, err := readFile[T](filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// appendEvent builds and appends an event to the log, returning the durable
// record assigned by the log.
// Callers must hold s.mu for writing before calling this.
func (s *FileStore) appendEvent(ctx context.Context, eventType schema.EventType, goalID, artifactID string, payload any) (schema.Event, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return schema.Event{}, fmt.Errorf("store: marshal event payload: %w", err)
	}
	return s.log.Append(ctx, schema.Event{
		Type:       eventType,
		GoalID:     goalID,
		ArtifactID: artifactID,
		Payload:    data,
	})
}

func materializationError(e schema.Event, err error) error {
	if err == nil {
		return nil
	}
	return &MaterializationError{Event: e, Err: err}
}

func ensureArtifactAbsent(kind, path, id string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("store: %s id %q already exists", kind, id)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("store: stat %s: %w", path, err)
	}
	return nil
}

func (s *FileStore) ensureProjectionAbsent(id string) error {
	if err := ensureArtifactAbsent("context projection", s.artifactPath(dirProjExecutor, id), id); err != nil {
		return err
	}
	if err := ensureArtifactAbsent("context projection", s.artifactPath(dirProjHuman, id), id); err != nil {
		return err
	}
	if err := ensureArtifactAbsent("context projection", s.artifactPath(dirProjReviewer, id), id); err != nil {
		return err
	}
	return ensureArtifactAbsent("context projection", s.artifactPath(dirProjTester, id), id)
}

// ── GoalID resolution helpers (no locking) ──────────────────────────────────

// findGoalIDForCondition scans all goal files and returns the GoalID of the
// goal whose GoalConditions list contains conditionID.
func (s *FileStore) findGoalIDForCondition(conditionID string) (string, error) {
	goals, err := scanDir[schema.GoalIR](filepath.Join(s.root, dirGoals))
	if err != nil {
		return "", err
	}
	for _, g := range goals {
		for _, c := range g.GoalConditions {
			if c.ID == conditionID {
				return g.GoalID, nil
			}
		}
	}
	return "", fmt.Errorf("store: no goal contains condition %s: %w", conditionID, ErrNotFound)
}

func (s *FileStore) goalExists(goalID string) (bool, error) {
	_, err := readFile[schema.GoalIR](s.artifactPath(dirGoals, goalID))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return false, err
}

func (s *FileStore) requireExistingGoal(goalID string) error {
	if err := validateArtifactID("goal", goalID); err != nil {
		return err
	}
	exists, err := s.goalExists(goalID)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("store: goal %s: %w", goalID, ErrNotFound)
	}
	return nil
}

func (s *FileStore) goalIDForObligation(obligationID string) (string, error) {
	obl, err := readFile[schema.Obligation](s.artifactPath(dirObligations, obligationID))
	if err != nil {
		return "", fmt.Errorf("store: load obligation %s: %w", obligationID, err)
	}
	return s.findGoalIDForCondition(obl.GoalConditionID)
}

// goalIDForCapsule follows capsule → obligation → condition → goal.
func (s *FileStore) goalIDForCapsule(capsuleID string) (string, error) {
	c, err := readFile[schema.ExecutionCapsule](s.artifactPath(dirCapsules, capsuleID))
	if err != nil {
		return "", fmt.Errorf("store: load capsule %s: %w", capsuleID, err)
	}
	if len(c.ObligationIDs) == 0 {
		return "", fmt.Errorf("store: capsule %s has no obligation IDs", capsuleID)
	}
	return s.goalIDForObligation(c.ObligationIDs[0])
}

func (s *FileStore) goalIDForEvidence(ev *schema.EvidenceArtifact) (string, error) {
	for _, obligationID := range append(append([]string{}, ev.Supports...), ev.Weakens...) {
		if obligationID == "" {
			continue
		}
		goalID, err := s.goalIDForObligation(obligationID)
		if err == nil {
			return goalID, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return "", err
		}
	}
	return "", fmt.Errorf("store: evidence %s has no resolvable obligation reference", ev.EvidenceID)
}

func (s *FileStore) goalIDForProjectionSources(sourceIDs []string) (string, error) {
	if len(sourceIDs) == 0 {
		return "", fmt.Errorf("store: projection source_artifact_ids are required to resolve goal")
	}
	for _, id := range sourceIDs {
		if err := validateArtifactID("source artifact", id); err != nil {
			return "", err
		}
	}
	for _, id := range sourceIDs {
		goalID, err := s.goalIDForCapsule(id)
		if err == nil {
			return goalID, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return "", err
		}
	}
	for _, id := range sourceIDs {
		goalID, err := s.goalIDForObligation(id)
		if err == nil {
			return goalID, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return "", err
		}
	}
	for _, id := range sourceIDs {
		exists, err := s.goalExists(id)
		if err != nil {
			return "", err
		}
		if exists {
			return id, nil
		}
	}
	return "", fmt.Errorf("store: no projection source resolves to a goal")
}

func (s *FileStore) goalIDForRelatedIDs(relatedIDs []string) (string, error) {
	if len(relatedIDs) == 0 {
		return "", fmt.Errorf("store: related_ids are required to resolve goal")
	}
	for _, id := range relatedIDs {
		if err := validateArtifactID("related artifact", id); err != nil {
			return "", err
		}
	}
	resolvers := []func(string) (string, error){
		func(id string) (string, error) {
			exists, err := s.goalExists(id)
			if err != nil {
				return "", err
			}
			if !exists {
				return "", ErrNotFound
			}
			return id, nil
		},
		s.goalIDForObligation,
		s.goalIDForCapsule,
		func(id string) (string, error) {
			p, err := readFile[schema.PatchArtifact](s.artifactPath(dirPatches, id))
			if err != nil {
				return "", err
			}
			return s.goalIDForCapsule(p.CapsuleID)
		},
		func(id string) (string, error) {
			c, err := readFile[schema.ClaimArtifact](s.artifactPath(dirClaims, id))
			if err != nil {
				return "", err
			}
			return s.goalIDForCapsule(c.SourceCapsuleID)
		},
		func(id string) (string, error) {
			f, err := readFile[schema.FailureFingerprint](s.artifactPath(dirFailures, id))
			if err != nil {
				return "", err
			}
			return s.goalIDForCapsule(f.SourceCapsuleID)
		},
		func(id string) (string, error) {
			r, err := readFile[schema.VerifierResult](s.artifactPath(dirVerifierResults, id))
			if err != nil {
				return "", err
			}
			return s.goalIDForCapsule(r.CapsuleID)
		},
		func(id string) (string, error) {
			ev, err := readFile[schema.EvidenceArtifact](s.artifactPath(dirEvidence, id))
			if err != nil {
				return "", err
			}
			return s.goalIDForEvidence(ev)
		},
	}
	for _, resolve := range resolvers {
		for _, id := range relatedIDs {
			goalID, err := resolve(id)
			if err == nil {
				return goalID, nil
			}
			if !errors.Is(err, ErrNotFound) {
				return "", err
			}
		}
	}
	return "", fmt.Errorf("store: no related ID resolves to a goal")
}

func projectionDir(role schema.ProjectionRole) (string, error) {
	switch role {
	case schema.ProjectionRoleExecutor:
		return dirProjExecutor, nil
	case schema.ProjectionRoleReviewer:
		return dirProjReviewer, nil
	case schema.ProjectionRoleTester:
		return dirProjTester, nil
	default:
		return "", fmt.Errorf("store: unsupported context projection role %q", role)
	}
}

// ── Goal IR ──────────────────────────────────────────────────────────────────

func (s *FileStore) SaveGoal(ctx context.Context, g *schema.GoalIR) error {
	if err := validateArtifactID("goal", g.GoalID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("goal", s.artifactPath(dirGoals, g.GoalID), g.GoalID); err != nil {
		return err
	}
	ev, err := s.appendEvent(ctx, schema.EventGoalCreated, g.GoalID, g.GoalID, g)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(s.artifactPath(dirGoals, g.GoalID), g))
}

func (s *FileStore) LoadGoal(ctx context.Context, goalID string) (*schema.GoalIR, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.GoalIR](s.artifactPath(dirGoals, goalID))
}

// LoadActiveGoal scans all goal files and returns the one with status "active".
// Returns (nil, nil) when no active goal exists. The MVP enforces one active goal
// per repo; the IntentCompiler calls this before creating a new goal.
func (s *FileStore) LoadActiveGoal(ctx context.Context) (*schema.GoalIR, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	goals, err := scanDir[schema.GoalIR](filepath.Join(s.root, dirGoals))
	if err != nil {
		return nil, err
	}
	for _, g := range goals {
		if g.Status == schema.GoalStatusActive {
			return g, nil
		}
	}
	return nil, nil
}

func (s *FileStore) UpdateGoalStatus(ctx context.Context, goalID string, status schema.GoalStatus) error {
	if err := validateArtifactID("goal", goalID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := readFile[schema.GoalIR](s.artifactPath(dirGoals, goalID))
	if err != nil {
		return err
	}
	g.Status = status
	return s.writeFile(s.artifactPath(dirGoals, goalID), g)
}

func (s *FileStore) LoadGoalCondition(ctx context.Context, conditionID string) (*schema.GoalCondition, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	goals, err := scanDir[schema.GoalIR](filepath.Join(s.root, dirGoals))
	if err != nil {
		return nil, err
	}
	for _, g := range goals {
		for i := range g.GoalConditions {
			if g.GoalConditions[i].ID == conditionID {
				c := g.GoalConditions[i]
				return &c, nil
			}
		}
	}
	return nil, ErrNotFound
}

// ── Obligations ──────────────────────────────────────────────────────────────

func (s *FileStore) SaveObligation(ctx context.Context, o *schema.Obligation) error {
	if err := validateArtifactID("obligation", o.ObligationID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("obligation", s.artifactPath(dirObligations, o.ObligationID), o.ObligationID); err != nil {
		return err
	}
	goalID, err := s.findGoalIDForCondition(o.GoalConditionID)
	if err != nil {
		return fmt.Errorf("store: SaveObligation: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventObligationCreated, goalID, o.ObligationID, o)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(s.artifactPath(dirObligations, o.ObligationID), o))
}

func (s *FileStore) LoadObligation(ctx context.Context, obligationID string) (*schema.Obligation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.Obligation](s.artifactPath(dirObligations, obligationID))
}

func (s *FileStore) LoadOpenObligations(ctx context.Context, goalID string) ([]*schema.Obligation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Build the set of condition IDs that belong to this goal.
	g, err := readFile[schema.GoalIR](s.artifactPath(dirGoals, goalID))
	if err != nil {
		return nil, fmt.Errorf("store: LoadOpenObligations: %w", err)
	}
	conditionIDs := make(map[string]bool, len(g.GoalConditions))
	for _, c := range g.GoalConditions {
		conditionIDs[c.ID] = true
	}
	all, err := scanDir[schema.Obligation](filepath.Join(s.root, dirObligations))
	if err != nil {
		return nil, err
	}
	var out []*schema.Obligation
	for _, o := range all {
		if o.Status == schema.ObligationOpen && conditionIDs[o.GoalConditionID] {
			out = append(out, o)
		}
	}
	return out, nil
}

func (s *FileStore) LoadObligationsForCondition(ctx context.Context, conditionID string) ([]*schema.Obligation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all, err := scanDir[schema.Obligation](filepath.Join(s.root, dirObligations))
	if err != nil {
		return nil, err
	}
	var out []*schema.Obligation
	for _, o := range all {
		if o.GoalConditionID == conditionID {
			out = append(out, o)
		}
	}
	return out, nil
}

func (s *FileStore) UpdateObligationStatus(ctx context.Context, obligationID string, status schema.ObligationStatus, satisfiedBy []string) error {
	if err := validateArtifactID("obligation", obligationID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, err := readFile[schema.Obligation](s.artifactPath(dirObligations, obligationID))
	if err != nil {
		return err
	}
	o.Status = status
	if satisfiedBy != nil {
		o.SatisfiedBy = satisfiedBy
	}
	return s.writeFile(s.artifactPath(dirObligations, obligationID), o)
}

// ── Execution Capsules ───────────────────────────────────────────────────────

func (s *FileStore) SaveCapsule(ctx context.Context, c *schema.ExecutionCapsule) error {
	if err := validateArtifactID("capsule", c.CapsuleID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("capsule", s.artifactPath(dirCapsules, c.CapsuleID), c.CapsuleID); err != nil {
		return err
	}
	goalID, err := s.goalIDForCapsuleFromObligation(c)
	if err != nil {
		return fmt.Errorf("store: SaveCapsule: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventCapsuleCreated, goalID, c.CapsuleID, c)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(s.artifactPath(dirCapsules, c.CapsuleID), c))
}

// goalIDForCapsuleFromObligation resolves the goalID from the capsule's
// first ObligationID without loading the capsule from disk (it's not saved yet).
func (s *FileStore) goalIDForCapsuleFromObligation(c *schema.ExecutionCapsule) (string, error) {
	if len(c.ObligationIDs) == 0 {
		return "", fmt.Errorf("store: capsule %s has no obligation IDs", c.CapsuleID)
	}
	return s.goalIDForObligation(c.ObligationIDs[0])
}

func (s *FileStore) LoadCapsule(ctx context.Context, capsuleID string) (*schema.ExecutionCapsule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.ExecutionCapsule](s.artifactPath(dirCapsules, capsuleID))
}

func (s *FileStore) UpdateCapsuleState(ctx context.Context, capsuleID string, state schema.CapsuleState) error {
	if err := validateArtifactID("capsule", capsuleID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := readFile[schema.ExecutionCapsule](s.artifactPath(dirCapsules, capsuleID))
	if err != nil {
		return err
	}
	c.State = state
	return s.writeFile(s.artifactPath(dirCapsules, capsuleID), c)
}

func (s *FileStore) UpdateCapsuleProjectionID(ctx context.Context, capsuleID, projectionID string) error {
	if err := validateArtifactID("capsule", capsuleID); err != nil {
		return err
	}
	if err := validateArtifactID("projection", projectionID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := readFile[schema.ExecutionCapsule](s.artifactPath(dirCapsules, capsuleID))
	if err != nil {
		return err
	}
	c.ContextProjectionID = projectionID
	return s.writeFile(s.artifactPath(dirCapsules, capsuleID), c)
}

// ── Context Projections ──────────────────────────────────────────────────────

func (s *FileStore) SaveProjection(ctx context.Context, p *schema.ContextProjection) error {
	if err := validateArtifactID("context projection", p.ContextProjectionID); err != nil {
		return err
	}
	dir, err := projectionDir(p.Role)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureProjectionAbsent(p.ContextProjectionID); err != nil {
		return err
	}
	saved := *p
	goalID, err := s.goalIDForProjectionSources(saved.SourceArtifactIDs)
	if err != nil {
		return fmt.Errorf("store: SaveProjection: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventContextProjectionCreated, goalID, saved.ContextProjectionID, &saved)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(s.artifactPath(dir, saved.ContextProjectionID), &saved))
}

func (s *FileStore) SaveHumanSummaryProjection(ctx context.Context, p *schema.HumanSummaryProjection) error {
	if err := validateArtifactID("context projection", p.ContextProjectionID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureProjectionAbsent(p.ContextProjectionID); err != nil {
		return err
	}
	saved := *p
	saved.Role = schema.ProjectionRoleHumanSummary
	goalID, err := s.goalIDForProjectionSources(saved.SourceArtifactIDs)
	if err != nil {
		return fmt.Errorf("store: SaveHumanSummaryProjection: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventContextProjectionCreated, goalID, saved.ContextProjectionID, &saved)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(s.artifactPath(dirProjHuman, saved.ContextProjectionID), &saved))
}

func (s *FileStore) LoadProjection(ctx context.Context, projectionID string) (*schema.ContextProjection, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, dir := range []string{dirProjExecutor, dirProjReviewer, dirProjTester} {
		projection, err := readFile[schema.ContextProjection](s.artifactPath(dir, projectionID))
		if err == nil {
			return projection, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	return nil, ErrNotFound
}

func (s *FileStore) LoadHumanSummaryProjection(ctx context.Context, projectionID string) (*schema.HumanSummaryProjection, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.HumanSummaryProjection](s.artifactPath(dirProjHuman, projectionID))
}

func (s *FileStore) LoadHumanSummaryProjectionForCapsule(ctx context.Context, capsuleID string) (*schema.HumanSummaryProjection, error) {
	if err := validateArtifactID("capsule", capsuleID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	all, err := scanDir[schema.HumanSummaryProjection](filepath.Join(s.root, dirProjHuman))
	if err != nil {
		return nil, err
	}
	for _, p := range all {
		for _, sourceID := range p.SourceArtifactIDs {
			if sourceID == capsuleID {
				return p, nil
			}
		}
	}
	return nil, ErrNotFound
}

// ── Patch Artifacts ──────────────────────────────────────────────────────────

func (s *FileStore) SavePatch(ctx context.Context, p *schema.PatchArtifact) error {
	if err := validateArtifactID("patch", p.PatchID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("patch", s.artifactPath(dirPatches, p.PatchID), p.PatchID); err != nil {
		return err
	}
	goalID, err := s.goalIDForCapsule(p.CapsuleID)
	if err != nil {
		return fmt.Errorf("store: SavePatch: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventPatchArtifactCreated, goalID, p.PatchID, p)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(s.artifactPath(dirPatches, p.PatchID), p))
}

func (s *FileStore) LoadPatch(ctx context.Context, patchID string) (*schema.PatchArtifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.PatchArtifact](s.artifactPath(dirPatches, patchID))
}

func (s *FileStore) UpdatePatchStatus(ctx context.Context, patchID string, status schema.PatchStatus) error {
	if err := validateArtifactID("patch", patchID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := readFile[schema.PatchArtifact](s.artifactPath(dirPatches, patchID))
	if err != nil {
		return err
	}
	p.Status = status
	return s.writeFile(s.artifactPath(dirPatches, patchID), p)
}

func (s *FileStore) LoadPatchesForCapsule(ctx context.Context, capsuleID string) ([]*schema.PatchArtifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all, err := scanDir[schema.PatchArtifact](filepath.Join(s.root, dirPatches))
	if err != nil {
		return nil, err
	}
	var out []*schema.PatchArtifact
	for _, p := range all {
		if p.CapsuleID == capsuleID {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *FileStore) LoadPatchesForObligation(ctx context.Context, obligationID string) ([]*schema.PatchArtifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := validateArtifactID("obligation", obligationID); err != nil {
		return nil, err
	}
	all, err := scanDir[schema.PatchArtifact](filepath.Join(s.root, dirPatches))
	if err != nil {
		return nil, err
	}
	var out []*schema.PatchArtifact
	for _, p := range all {
		if containsString(p.ObligationIDsClaimed, obligationID) {
			out = append(out, p)
		}
	}
	return out, nil
}

// ── Evidence Artifacts ───────────────────────────────────────────────────────

func (s *FileStore) SaveEvidence(ctx context.Context, e *schema.EvidenceArtifact) error {
	if err := validateArtifactID("evidence", e.EvidenceID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("evidence", s.artifactPath(dirEvidence, e.EvidenceID), e.EvidenceID); err != nil {
		return err
	}
	goalID, err := s.goalIDForEvidence(e)
	if err != nil {
		return fmt.Errorf("store: SaveEvidence: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventEvidenceArtifactCreated, goalID, e.EvidenceID, e)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(s.artifactPath(dirEvidence, e.EvidenceID), e))
}

func (s *FileStore) LoadEvidence(ctx context.Context, evidenceID string) (*schema.EvidenceArtifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.EvidenceArtifact](s.artifactPath(dirEvidence, evidenceID))
}

func (s *FileStore) LoadEvidenceForObligation(ctx context.Context, obligationID string) ([]*schema.EvidenceArtifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all, err := scanDir[schema.EvidenceArtifact](filepath.Join(s.root, dirEvidence))
	if err != nil {
		return nil, err
	}
	var out []*schema.EvidenceArtifact
	for _, ev := range all {
		if containsString(ev.Supports, obligationID) || containsString(ev.Weakens, obligationID) {
			out = append(out, ev)
		}
	}
	return out, nil
}

// ── Claim Artifacts ──────────────────────────────────────────────────────────

func (s *FileStore) SaveClaim(ctx context.Context, c *schema.ClaimArtifact) error {
	if err := validateArtifactID("claim", c.ClaimID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("claim", s.artifactPath(dirClaims, c.ClaimID), c.ClaimID); err != nil {
		return err
	}
	goalID, err := s.goalIDForCapsule(c.SourceCapsuleID)
	if err != nil {
		return fmt.Errorf("store: SaveClaim: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventClaimCreated, goalID, c.ClaimID, c)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(s.artifactPath(dirClaims, c.ClaimID), c))
}

func (s *FileStore) LoadClaim(ctx context.Context, claimID string) (*schema.ClaimArtifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.ClaimArtifact](s.artifactPath(dirClaims, claimID))
}

func (s *FileStore) LoadVerifiedClaimsForFiles(ctx context.Context, files []string) ([]*schema.ClaimArtifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(files) == 0 {
		return nil, nil
	}
	fileSet := make(map[string]bool, len(files))
	for _, f := range files {
		if normalized := normalizeArtifactPath(f); normalized != "" {
			fileSet[normalized] = true
		}
	}
	all, err := scanDir[schema.ClaimArtifact](filepath.Join(s.root, dirClaims))
	if err != nil {
		return nil, err
	}
	var out []*schema.ClaimArtifact
	for _, c := range all {
		if c.Status != schema.ClaimVerified {
			continue
		}
		for _, af := range c.AffectedFiles {
			if fileSet[normalizeArtifactPath(af)] {
				out = append(out, c)
				break
			}
		}
	}
	return out, nil
}

func (s *FileStore) LoadClaimsForCapsule(ctx context.Context, capsuleID string) ([]*schema.ClaimArtifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all, err := scanDir[schema.ClaimArtifact](filepath.Join(s.root, dirClaims))
	if err != nil {
		return nil, err
	}
	var out []*schema.ClaimArtifact
	for _, c := range all {
		if c.SourceCapsuleID == capsuleID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *FileStore) LoadClaimsForGoal(ctx context.Context, goalID string) ([]*schema.ClaimArtifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if goalID == "" {
		return nil, ErrNotFound
	}
	exists, err := s.goalExists(goalID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	all, err := scanDir[schema.ClaimArtifact](filepath.Join(s.root, dirClaims))
	if err != nil {
		return nil, err
	}
	out := make([]*schema.ClaimArtifact, 0, len(all))
	for _, claim := range all {
		claimGoalID, err := s.goalIDForCapsule(claim.SourceCapsuleID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if claimGoalID == goalID {
			out = append(out, claim)
		}
	}
	return out, nil
}

func (s *FileStore) UpdateClaimStatus(ctx context.Context, claimID string, status schema.ClaimStatus) error {
	if err := validateArtifactID("claim", claimID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := readFile[schema.ClaimArtifact](s.artifactPath(dirClaims, claimID))
	if err != nil {
		return err
	}
	c.Status = status
	return s.writeFile(s.artifactPath(dirClaims, claimID), c)
}

// ── Failure Fingerprints ─────────────────────────────────────────────────────

func (s *FileStore) SaveFailure(ctx context.Context, f *schema.FailureFingerprint) error {
	if err := validateArtifactID("failure", f.FailureID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("failure", s.artifactPath(dirFailures, f.FailureID), f.FailureID); err != nil {
		return err
	}
	goalID, err := s.goalIDForCapsule(f.SourceCapsuleID)
	if err != nil {
		return fmt.Errorf("store: SaveFailure: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventFailureFingerprintCreated, goalID, f.FailureID, f)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(s.artifactPath(dirFailures, f.FailureID), f))
}

func (s *FileStore) LoadFailure(ctx context.Context, failureID string) (*schema.FailureFingerprint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.FailureFingerprint](s.artifactPath(dirFailures, failureID))
}

func (s *FileStore) LoadFailuresForFiles(ctx context.Context, files []string) ([]*schema.FailureFingerprint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(files) == 0 {
		return nil, nil
	}
	fileSet := make(map[string]bool, len(files))
	for _, f := range files {
		if normalized := normalizeArtifactPath(f); normalized != "" {
			fileSet[normalized] = true
		}
	}
	all, err := scanDir[schema.FailureFingerprint](filepath.Join(s.root, dirFailures))
	if err != nil {
		return nil, err
	}
	var out []*schema.FailureFingerprint
	for _, f := range all {
		for _, af := range f.AffectedFiles {
			if fileSet[normalizeArtifactPath(af)] {
				out = append(out, f)
				break
			}
		}
	}
	return out, nil
}

func (s *FileStore) LoadFailuresForCapsule(ctx context.Context, capsuleID string) ([]*schema.FailureFingerprint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all, err := scanDir[schema.FailureFingerprint](filepath.Join(s.root, dirFailures))
	if err != nil {
		return nil, err
	}
	var out []*schema.FailureFingerprint
	for _, f := range all {
		if f.SourceCapsuleID == capsuleID {
			out = append(out, f)
		}
	}
	return out, nil
}

// LoadAllFailures returns FailureFingerprints associated with goalID.
// FailureFingerprint has no GoalID field, so the MVP implementation resolves
// SourceCapsuleID → ObligationID → GoalConditionID → GoalID.
func (s *FileStore) LoadAllFailures(_ context.Context, goalID string) ([]*schema.FailureFingerprint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if goalID == "" {
		return nil, ErrNotFound
	}
	exists, err := s.goalExists(goalID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	all, err := scanDir[schema.FailureFingerprint](filepath.Join(s.root, dirFailures))
	if err != nil {
		return nil, err
	}
	var out []*schema.FailureFingerprint
	for _, f := range all {
		resolvedGoalID, err := s.goalIDForCapsule(f.SourceCapsuleID)
		if err != nil {
			// Skip orphaned failures (capsule or obligation files missing) but
			// propagate genuine I/O or corruption errors so callers are not
			// silently given a truncated result.
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("store: LoadAllFailures: resolve goal for failure %s: %w", f.FailureID, err)
		}
		if resolvedGoalID != goalID {
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

// ── Verifier Results ─────────────────────────────────────────────────────────

func (s *FileStore) SaveVerifierResult(ctx context.Context, r *schema.VerifierResult) error {
	if err := validateArtifactID("verifier result", r.VerifierResultID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("verifier result", s.artifactPath(dirVerifierResults, r.VerifierResultID), r.VerifierResultID); err != nil {
		return err
	}
	goalID, err := s.goalIDForCapsule(r.CapsuleID)
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.VerifierResult](s.artifactPath(dirVerifierResults, resultID))
}

func (s *FileStore) LoadVerifierResultForPatch(ctx context.Context, patchID string) (*schema.VerifierResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all, err := scanDir[schema.VerifierResult](filepath.Join(s.root, dirVerifierResults))
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
	if err := validateArtifactID("decision", d.DecisionID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("decision", s.artifactPath(dirDecisions, d.DecisionID), d.DecisionID); err != nil {
		return err
	}
	goalID, err := s.goalIDForRelatedIDs(d.RelatedIDs)
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.DecisionRecord](s.artifactPath(dirDecisions, decisionID))
}

// ── Budget Records ───────────────────────────────────────────────────────────

func (s *FileStore) SaveBudgetRecord(ctx context.Context, b *schema.BudgetRecord) error {
	if err := validateArtifactID("budget", b.BudgetID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("budget", s.artifactPath(dirBudgets, b.BudgetID), b.BudgetID); err != nil {
		return err
	}
	if err := s.requireExistingGoal(b.GoalID); err != nil {
		return fmt.Errorf("store: SaveBudgetRecord: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventBudgetRecordSaved, b.GoalID, b.BudgetID, b)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(s.artifactPath(dirBudgets, b.BudgetID), b))
}

func (s *FileStore) LoadBudgetRecord(ctx context.Context, budgetID string) (*schema.BudgetRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readFile[schema.BudgetRecord](s.artifactPath(dirBudgets, budgetID))
}

func (s *FileStore) LoadBudgetForGoal(ctx context.Context, goalID string) ([]*schema.BudgetRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all, err := scanDir[schema.BudgetRecord](filepath.Join(s.root, dirBudgets))
	if err != nil {
		return nil, err
	}
	var out []*schema.BudgetRecord
	for _, b := range all {
		if b.GoalID == goalID {
			out = append(out, b)
		}
	}
	return out, nil
}

// UpdateBudgetRecord overwrites the stored BudgetRecord with b after appending
// a replayable budget_record_updated event.
func (s *FileStore) UpdateBudgetRecord(ctx context.Context, b *schema.BudgetRecord) error {
	if err := validateArtifactID("budget", b.BudgetID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.artifactPath(dirBudgets, b.BudgetID)
	if _, err := readFile[schema.BudgetRecord](path); err != nil {
		return err
	}
	if err := s.requireExistingGoal(b.GoalID); err != nil {
		return fmt.Errorf("store: UpdateBudgetRecord: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventBudgetRecordUpdated, b.GoalID, b.BudgetID, b)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(path, b))
}

// ── State Snapshots ──────────────────────────────────────────────────────────

// SaveSnapshot persists a StateSnapshot and records it in the log so replay can
// reconstruct checkpoint metadata. The snapshot's SequenceNum remains the last
// domain event included in the snapshot, not the sequence assigned to this save.
func (s *FileStore) SaveSnapshot(ctx context.Context, snap *schema.StateSnapshot) error {
	if err := validateArtifactID("snapshot", snap.SnapshotID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureArtifactAbsent("snapshot", s.artifactPath(dirSnapshots, snap.SnapshotID), snap.SnapshotID); err != nil {
		return err
	}
	if err := s.requireExistingGoal(snap.GoalID); err != nil {
		return fmt.Errorf("store: SaveSnapshot: %w", err)
	}
	ev, err := s.appendEvent(ctx, schema.EventStateSnapshotSaved, snap.GoalID, snap.SnapshotID, snap)
	if err != nil {
		return err
	}
	return materializationError(ev, s.writeFile(s.artifactPath(dirSnapshots, snap.SnapshotID), snap))
}

// LoadLatestSnapshot returns the StateSnapshot for goalID with the highest
// SequenceNum, representing the most recent checkpoint.
func (s *FileStore) LoadLatestSnapshot(ctx context.Context, goalID string) (*schema.StateSnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all, err := scanDir[schema.StateSnapshot](filepath.Join(s.root, dirSnapshots))
	if err != nil {
		return nil, err
	}
	var latest *schema.StateSnapshot
	for _, snap := range all {
		if snap.GoalID != goalID {
			continue
		}
		if latest == nil || snap.SequenceNum > latest.SequenceNum {
			latest = snap
		}
	}
	if latest == nil {
		return nil, ErrNotFound
	}
	return latest, nil
}

// ── utility ──────────────────────────────────────────────────────────────────

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func normalizeArtifactPath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.ReplaceAll(path, "\\", "/")
	path = strings.TrimPrefix(path, "./")
	path = strings.Trim(path, "/")
	return strings.ToLower(path)
}

// compile-time interface satisfaction check
var _ ArtifactStore = (*FileStore)(nil)

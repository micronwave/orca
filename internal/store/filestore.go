package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/micronwave/orca/internal/eventlog"
	"github.com/micronwave/orca/internal/schema"
)

// ErrNotFound is returned by Load methods when the requested artifact does not exist.
var ErrNotFound = errors.New("artifact not found")

// ErrInvalidCapsuleTransition is returned when a requested capsule state
// transition violates the documented lifecycle order.
var ErrInvalidCapsuleTransition = errors.New("invalid capsule state transition")

// ErrStoreIO wraps OS-level file read/write errors from the store so callers
// can distinguish them from logical errors (ErrNotFound, validation failures).
// Context cancellation errors are NOT wrapped with ErrStoreIO — they remain
// distinguishable as context.Canceled / context.DeadlineExceeded.
var ErrStoreIO = errors.New("store: I/O error")

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
	dirGoals            = "state/goals"
	dirObligations      = "state/obligations"
	dirCapsules         = "state/capsules"
	dirSnapshots        = "state/snapshots"
	dirPatches          = "artifacts/patches"
	dirEvidence         = "artifacts/evidence"
	dirClaims           = "artifacts/claims"
	dirProjExecutor     = "artifacts/projections/executor"
	dirProjHuman        = "artifacts/projections/human_summary"
	dirProjReviewer     = "artifacts/projections/reviewer"
	dirProjTester       = "artifacts/projections/tester"
	dirFailures         = "artifacts/failures"
	dirDecisions        = "artifacts/decisions"
	dirBudgets          = "artifacts/budgets"
	dirVerifierResults  = "artifacts/verifier_results"
	dirTopologyOutcomes = "artifacts/topology_outcomes"
	dirProjReuse        = "artifacts/projections/reuse"
	dirPRs              = "artifacts/prs"
	dirCIStatus         = "artifacts/ci_status"
	dirIntake           = "artifacts/intake"
	dirCapsuleRuntime   = "state/capsule_runtime"
	dirStartupBundles   = "artifacts/startup_bundles"
	dirRecoveryLedger   = "artifacts/recovery"
	dirRepoStatus       = "artifacts/repo_status"
)

// FileStore is the file-backed JSON implementation of ArtifactStore.
//
// Each artifact is stored as an individual JSON file named {id}.json inside
// the appropriate subdirectory under root. Save methods and status mutation
// methods all append the corresponding event to the EventLog before writing
// the artifact file, ensuring the log is the authoritative history from which
// state can be replayed.
//
// Status mutation methods that are store-owned (emit the event internally):
//   - UpdateGoalStatus       → goal_status_updated
//   - UpdateObligationStatus → obligation_status_updated
//   - UpdatePatchStatus      → patch_accepted / patch_rejected
//   - UpdateClaimStatus      → claim_status_updated
//   - UpdateClaimDispute     → claim_status_updated
//   - UpdateClaimValidation  → claim_status_updated
//   - UpdateClaimSupersession → claim_superseded
//   - UpdateCapsuleProjectionID → capsule_projection_linked
//
// Capsule lifecycle transitions (UpdateCapsuleState) remain caller-owned:
// the runner appends capsule_started / capsule_state_updated / capsule_completed
// before calling UpdateCapsuleState, as the runner owns the lifecycle semantics.
//
// All exported methods are safe for concurrent use.
type FileStore struct {
	root string
	log  *eventlog.FileLog
	mu   sync.RWMutex

	// openOblIdx maps goalID → set of obligationIDs whose status is ObligationOpen.
	// Maintained under mu.Lock for writes; read under mu.RLock.
	// Built from disk on New and updated on every obligation save/status change.
	openOblIdx map[string]map[string]bool
}

// New creates or opens the FileStore at root, building the required directory
// hierarchy. root is typically the .orca directory for the repository.
func New(root string, log *eventlog.FileLog) (*FileStore, error) {
	if log == nil {
		return nil, fmt.Errorf("store: event log is required")
	}
	dirs := []string{
		dirGoals, dirObligations, dirCapsules, dirSnapshots,
		dirPatches, dirEvidence, dirClaims,
		dirProjExecutor, dirProjHuman, dirProjReviewer, dirProjTester, dirProjReuse,
		dirFailures, dirDecisions, dirBudgets, dirVerifierResults, dirTopologyOutcomes,
		dirPRs, dirCIStatus, dirIntake,
		dirCapsuleRuntime, dirStartupBundles,
		dirRecoveryLedger, dirRepoStatus,
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, fmt.Errorf("store: create dir %s: %w", d, err)
		}
	}
	ctx := context.Background()
	s := &FileStore{root: root, log: log, openOblIdx: make(map[string]map[string]bool)}
	if err := s.initOpenObligationsIndex(ctx); err != nil {
		return nil, fmt.Errorf("store: build open-obligations index: %w", err)
	}
	return s, nil
}

// initOpenObligationsIndex scans the goals and obligations directories once at
// startup and populates openOblIdx with every obligation whose status is Open.
// Caller must NOT hold s.mu (called from New before the store is published).
func (s *FileStore) initOpenObligationsIndex(ctx context.Context) error {
	goals, err := scanDir[schema.GoalIR](ctx, filepath.Join(s.root, dirGoals))
	if err != nil {
		return err
	}
	condToGoal := make(map[string]string)
	for _, g := range goals {
		for _, c := range g.GoalConditions {
			condToGoal[c.ID] = g.GoalID
		}
	}
	obligations, err := scanDir[schema.Obligation](ctx, filepath.Join(s.root, dirObligations))
	if err != nil {
		return err
	}
	for _, o := range obligations {
		if o.Status == schema.ObligationOpen {
			goalID := condToGoal[o.GoalConditionID]
			if goalID == "" {
				return fmt.Errorf("store: open obligation %s references unknown condition %s: %w",
					o.ObligationID, o.GoalConditionID, ErrNotFound)
			}
			s.addToOpenIdx(goalID, o.ObligationID)
		}
	}
	return nil
}

// addToOpenIdx records obligationID as open for goalID.
// Caller must hold s.mu.Lock().
func (s *FileStore) addToOpenIdx(goalID, obligationID string) {
	if s.openOblIdx[goalID] == nil {
		s.openOblIdx[goalID] = make(map[string]bool)
	}
	s.openOblIdx[goalID][obligationID] = true
}

// removeFromOpenIdx removes obligationID from the open index for goalID.
// Caller must hold s.mu.Lock().
func (s *FileStore) removeFromOpenIdx(goalID, obligationID string) {
	delete(s.openOblIdx[goalID], obligationID)
}

// updateOpenIdx adds obligationID to the open index when status is Open;
// removes it otherwise. Caller must hold s.mu.Lock().
func (s *FileStore) updateOpenIdx(goalID, obligationID string, status schema.ObligationStatus) {
	if status == schema.ObligationOpen {
		s.addToOpenIdx(goalID, obligationID)
	} else {
		s.removeFromOpenIdx(goalID, obligationID)
	}
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
// temp-file rename. The temp file is synced before the rename so that a
// power loss between write and rename cannot leave a corrupt artifact.
// The operation runs in a goroutine; if ctx is canceled before it completes,
// writeFile returns immediately with a context error. The goroutine may still
// finish in the background — this is acceptable since Go cannot cancel OS calls.
func (s *FileStore) writeFile(ctx context.Context, path string, v any) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("store: write %s: %w", path, err)
	}

	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal: %w", err)
	}

	type result struct{ err error }
	ch := make(chan result, 1)
	go func() {
		tmp := path + ".tmp"
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			ch <- result{fmt.Errorf("%w: create tmp %s: %v", ErrStoreIO, path, err)}
			return
		}
		n, werr := f.Write(data)
		if werr == nil && n != len(data) {
			werr = io.ErrShortWrite
		}
		serr := f.Sync()
		cerr := f.Close()
		if werr != nil {
			_ = os.Remove(tmp)
			ch <- result{fmt.Errorf("%w: write tmp %s: %v", ErrStoreIO, path, werr)}
			return
		}
		if serr != nil {
			_ = os.Remove(tmp)
			ch <- result{fmt.Errorf("%w: sync tmp %s: %v", ErrStoreIO, path, serr)}
			return
		}
		if cerr != nil {
			_ = os.Remove(tmp)
			ch <- result{fmt.Errorf("%w: close tmp %s: %v", ErrStoreIO, path, cerr)}
			return
		}
		if err := os.Rename(tmp, path); err != nil {
			ch <- result{errors.Join(fmt.Errorf("%w: rename to %s: %v", ErrStoreIO, path, err), os.Remove(tmp))}
			return
		}
		ch <- result{}
	}()

	select {
	case <-ctx.Done():
		return fmt.Errorf("store: write %s: %w", path, ctx.Err())
	case r := <-ch:
		return r.err
	}
}

// writeFileReplay writes v as JSON directly to path without a temp file,
// rename, or explicit fsync. It is intended only for bulk replay where
// artifact files are being rebuilt from the authoritative event log.
//
// Crash-restart semantics: if replay is interrupted mid-run, artifact
// directories must be wiped and replay re-run from the latest snapshot
// sequence, since partially-written files are not guaranteed to be valid JSON.
func (s *FileStore) writeFileReplay(ctx context.Context, path string, v any) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("store: write %s: %w", path, err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("%w: write %s: %v", ErrStoreIO, path, err)
	}
	return nil
}

// readFile reads and JSON-unmarshals the file at path into a new T.
// Returns ErrNotFound if the file does not exist.
// The operation runs in a goroutine; if ctx is canceled before it completes,
// readFile returns immediately with a context error.
func readFile[T any](ctx context.Context, path string) (*T, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("store: read %s: %w", path, err)
	}

	type result struct {
		v   *T
		err error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			ch <- result{nil, ErrNotFound}
			return
		}
		if err != nil {
			ch <- result{nil, fmt.Errorf("%w: read %s: %v", ErrStoreIO, path, err)}
			return
		}
		var v T
		if err := json.Unmarshal(data, &v); err != nil {
			ch <- result{nil, fmt.Errorf("store: unmarshal %s: %w", path, err)}
			return
		}
		ch <- result{&v, nil}
	}()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("store: read %s: %w", path, ctx.Err())
	case r := <-ch:
		return r.v, r.err
	}
}

// scanDir reads every .json file in the absolute directory dir and returns
// the unmarshaled values. Returns nil slice (not error) if dir is missing.
func scanDir[T any](ctx context.Context, dir string) ([]*T, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: readdir %s: %w", dir, err)
	}
	var out []*T
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		v, err := readFile[T](ctx, filepath.Join(dir, e.Name()))
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
func (s *FileStore) findGoalIDForCondition(ctx context.Context, conditionID string) (string, error) {
	goals, err := scanDir[schema.GoalIR](ctx, filepath.Join(s.root, dirGoals))
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

func (s *FileStore) goalExists(ctx context.Context, goalID string) (bool, error) {
	_, err := readFile[schema.GoalIR](ctx, s.artifactPath(dirGoals, goalID))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return false, err
}

func (s *FileStore) requireExistingGoal(ctx context.Context, goalID string) error {
	if err := validateArtifactID("goal", goalID); err != nil {
		return err
	}
	exists, err := s.goalExists(ctx, goalID)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("store: goal %s: %w", goalID, ErrNotFound)
	}
	return nil
}

func (s *FileStore) goalIDForObligation(ctx context.Context, obligationID string) (string, error) {
	if err := validateArtifactID("obligation", obligationID); err != nil {
		return "", err
	}
	obl, err := readFile[schema.Obligation](ctx, s.artifactPath(dirObligations, obligationID))
	if err != nil {
		return "", fmt.Errorf("store: load obligation %s: %w", obligationID, err)
	}
	return s.findGoalIDForCondition(ctx, obl.GoalConditionID)
}

// goalIDForCapsule follows capsule → obligation → condition → goal.
func (s *FileStore) goalIDForCapsule(ctx context.Context, capsuleID string) (string, error) {
	if err := validateArtifactID("capsule", capsuleID); err != nil {
		return "", err
	}
	c, err := readFile[schema.ExecutionCapsule](ctx, s.artifactPath(dirCapsules, capsuleID))
	if err != nil {
		return "", fmt.Errorf("store: load capsule %s: %w", capsuleID, err)
	}
	if len(c.ObligationIDs) == 0 {
		return "", fmt.Errorf("store: capsule %s has no obligation IDs", capsuleID)
	}
	return s.goalIDForObligation(ctx, c.ObligationIDs[0])
}

func (s *FileStore) goalIDForEvidence(ctx context.Context, ev *schema.EvidenceArtifact) (string, error) {
	for _, obligationID := range append(append([]string{}, ev.Supports...), ev.Weakens...) {
		if obligationID == "" {
			continue
		}
		goalID, err := s.goalIDForObligation(ctx, obligationID)
		if err == nil {
			return goalID, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return "", err
		}
	}
	return "", fmt.Errorf("store: evidence %s has no resolvable obligation reference", ev.EvidenceID)
}

func (s *FileStore) goalIDForProjectionSources(ctx context.Context, sourceIDs []string) (string, error) {
	if len(sourceIDs) == 0 {
		return "", fmt.Errorf("store: projection source_artifact_ids are required to resolve goal")
	}
	for _, id := range sourceIDs {
		if err := validateArtifactID("source artifact", id); err != nil {
			return "", err
		}
	}
	for _, id := range sourceIDs {
		goalID, err := s.goalIDForCapsule(ctx, id)
		if err == nil {
			return goalID, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return "", err
		}
	}
	for _, id := range sourceIDs {
		goalID, err := s.goalIDForObligation(ctx, id)
		if err == nil {
			return goalID, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return "", err
		}
	}
	for _, id := range sourceIDs {
		exists, err := s.goalExists(ctx, id)
		if err != nil {
			return "", err
		}
		if exists {
			return id, nil
		}
	}
	return "", fmt.Errorf("store: no projection source resolves to a goal")
}

func (s *FileStore) goalIDForRelatedIDs(ctx context.Context, relatedIDs []string) (string, error) {
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
			exists, err := s.goalExists(ctx, id)
			if err != nil {
				return "", err
			}
			if !exists {
				return "", ErrNotFound
			}
			return id, nil
		},
		func(id string) (string, error) { return s.goalIDForObligation(ctx, id) },
		func(id string) (string, error) { return s.goalIDForCapsule(ctx, id) },
		func(id string) (string, error) {
			p, err := readFile[schema.PatchArtifact](ctx, s.artifactPath(dirPatches, id))
			if err != nil {
				return "", err
			}
			return s.goalIDForCapsule(ctx, p.CapsuleID)
		},
		func(id string) (string, error) {
			c, err := readFile[schema.ClaimArtifact](ctx, s.artifactPath(dirClaims, id))
			if err != nil {
				return "", err
			}
			return s.goalIDForCapsule(ctx, c.SourceCapsuleID)
		},
		func(id string) (string, error) {
			f, err := readFile[schema.FailureFingerprint](ctx, s.artifactPath(dirFailures, id))
			if err != nil {
				return "", err
			}
			return s.goalIDForCapsule(ctx, f.SourceCapsuleID)
		},
		func(id string) (string, error) {
			r, err := readFile[schema.VerifierResult](ctx, s.artifactPath(dirVerifierResults, id))
			if err != nil {
				return "", err
			}
			return s.goalIDForCapsule(ctx, r.CapsuleID)
		},
		func(id string) (string, error) {
			ev, err := readFile[schema.EvidenceArtifact](ctx, s.artifactPath(dirEvidence, id))
			if err != nil {
				return "", err
			}
			return s.goalIDForEvidence(ctx, ev)
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

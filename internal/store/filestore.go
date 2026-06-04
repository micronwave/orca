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

	// condToGoal maps conditionID → goalID. Conditions are write-once at goal
	// creation time, so this cache is valid for the store's lifetime.
	// Populated at init; updated under mu.Lock on SaveGoal.
	condToGoal map[string]string

	// latestSnap maps goalID → snapshot with the highest SequenceNum.
	// Populated at init; updated under mu.Lock on SaveSnapshot.
	// Read under mu.RLock in LoadLatestSnapshot.
	latestSnap map[string]*schema.StateSnapshot

	// budgetsByGoal maps goalID → all BudgetRecords for that goal.
	// Populated at init; maintained under mu.Lock on SaveBudgetRecord/UpdateBudgetRecord.
	// Read under mu.RLock in LoadBudgetForGoal.
	budgetsByGoal map[string][]*schema.BudgetRecord

	// knownGoals is the set of goal IDs that exist on disk. Monotonically grows —
	// goals are never deleted. Populated at init; updated under mu.Lock on SaveGoal.
	knownGoals map[string]bool
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
	var mkWg sync.WaitGroup
	mkErrs := make([]error, len(dirs))
	for i, d := range dirs {
		mkWg.Add(1)
		go func() {
			defer mkWg.Done()
			mkErrs[i] = os.MkdirAll(filepath.Join(root, d), 0o755)
		}()
	}
	mkWg.Wait()
	for i, err := range mkErrs {
		if err != nil {
			return nil, fmt.Errorf("store: create dir %s: %w", dirs[i], err)
		}
	}
	ctx := context.Background()
	s := &FileStore{
		root:          root,
		log:           log,
		openOblIdx:    make(map[string]map[string]bool),
		condToGoal:    make(map[string]string),
		latestSnap:    make(map[string]*schema.StateSnapshot),
		budgetsByGoal: make(map[string][]*schema.BudgetRecord),
		knownGoals:    make(map[string]bool),
	}
	if err := s.initOpenObligationsIndex(ctx); err != nil {
		return nil, fmt.Errorf("store: build open-obligations index: %w", err)
	}
	if err := s.initLatestSnapshotIndex(ctx); err != nil {
		return nil, fmt.Errorf("store: build snapshot index: %w", err)
	}
	if err := s.initBudgetIndex(ctx); err != nil {
		return nil, fmt.Errorf("store: build budget index: %w", err)
	}
	return s, nil
}

// initOpenObligationsIndex scans the goals and obligations directories once at
// startup and populates openOblIdx, condToGoal, and knownGoals.
// Caller must NOT hold s.mu (called from New before the store is published).
func (s *FileStore) initOpenObligationsIndex(ctx context.Context) error {
	var goals []*schema.GoalIR
	var obligations []*schema.Obligation
	var errGoals, errObl error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		goals, errGoals = scanDir[schema.GoalIR](ctx, filepath.Join(s.root, dirGoals))
	}()
	go func() {
		defer wg.Done()
		obligations, errObl = scanDir[schema.Obligation](ctx, filepath.Join(s.root, dirObligations))
	}()
	wg.Wait()
	if errGoals != nil {
		return errGoals
	}
	if errObl != nil {
		return errObl
	}
	for _, g := range goals {
		s.knownGoals[g.GoalID] = true
		for _, c := range g.GoalConditions {
			s.condToGoal[c.ID] = g.GoalID
		}
	}
	for _, o := range obligations {
		if o.Status == schema.ObligationOpen {
			goalID := s.condToGoal[o.GoalConditionID]
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
// ctx is checked before the blocking I/O; true mid-write cancellation is not
// achievable at the OS level on any platform, so no goroutine is used.
// Once the rename succeeds the write is committed, so reporting a later context
// error here would misclassify a successful materialization as a failure.
func (s *FileStore) writeFile(ctx context.Context, path string, v any) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("store: write %s: %w", path, err)
	}

	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("store: marshal: %w", err)
	}

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("%w: create tmp %s: %v", ErrStoreIO, path, err)
	}
	n, werr := f.Write(data)
	if werr == nil && n != len(data) {
		werr = io.ErrShortWrite
	}
	serr := f.Sync()
	cerr := f.Close()
	if werr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("%w: write tmp %s: %v", ErrStoreIO, path, werr)
	}
	if serr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("%w: sync tmp %s: %v", ErrStoreIO, path, serr)
	}
	if cerr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("%w: close tmp %s: %v", ErrStoreIO, path, cerr)
	}
	if err := os.Rename(tmp, path); err != nil {
		return errors.Join(fmt.Errorf("%w: rename to %s: %v", ErrStoreIO, path, err), os.Remove(tmp))
	}
	return nil
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
// ctx is checked before and after the blocking read; true mid-read cancellation
// is not achievable at the OS level, so no goroutine is used.
func readFile[T any](ctx context.Context, path string) (*T, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("store: read %s: %w", path, err)
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %v", ErrStoreIO, path, err)
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("store: unmarshal %s: %w", path, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("store: read %s: %w", path, err)
	}
	return &v, nil
}

// scanDir reads every .json file in the absolute directory dir and returns
// the unmarshaled values. Returns nil slice (not error) if dir is missing.
// Reads are issued concurrently via a bounded semaphore (16 max in-flight).
// Result order is unspecified; callers must not rely on directory-listing order.
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
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	out := make([]*T, len(paths))
	errs := make([]error, len(paths))
	const maxConcurrent = 16
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			out[i], errs[i] = readFile[T](ctx, p)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
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

// findGoalIDForCondition returns the goalID for conditionID from the in-memory
// cache, falling back to a disk scan only when the cache misses (should not
// occur in normal operation). Safe for callers that do not already hold s.mu.
func (s *FileStore) findGoalIDForCondition(ctx context.Context, conditionID string) (string, error) {
	s.mu.RLock()
	goalID, ok := s.condToGoal[conditionID]
	s.mu.RUnlock()
	if ok {
		return goalID, nil
	}
	goalID, err := s.findGoalIDForConditionSlow(ctx, conditionID)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	if _, ok := s.condToGoal[conditionID]; !ok {
		s.condToGoal[conditionID] = goalID
	}
	s.mu.Unlock()
	return goalID, nil
}

// findGoalIDForConditionLocked is the lock-held variant of
// findGoalIDForCondition. Caller must hold s.mu (read or write).
func (s *FileStore) findGoalIDForConditionLocked(ctx context.Context, conditionID string) (string, error) {
	if goalID, ok := s.condToGoal[conditionID]; ok {
		return goalID, nil
	}
	return s.findGoalIDForConditionSlow(ctx, conditionID)
}

// findGoalIDForConditionSlow is the O(N) fallback for findGoalIDForCondition.
// Should not be reached in normal operation.
func (s *FileStore) findGoalIDForConditionSlow(ctx context.Context, conditionID string) (string, error) {
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

// goalExists reports whether goalID exists, checking the in-memory cache first.
// Safe for callers that do not already hold s.mu.
func (s *FileStore) goalExists(ctx context.Context, goalID string) (bool, error) {
	s.mu.RLock()
	ok := s.knownGoals[goalID]
	s.mu.RUnlock()
	if ok {
		return true, nil
	}
	exists, err := s.goalExistsSlow(ctx, goalID)
	if err != nil || !exists {
		return exists, err
	}
	s.mu.Lock()
	s.knownGoals[goalID] = true
	s.mu.Unlock()
	return true, nil
}

// goalExistsLocked is the lock-held variant of goalExists.
// Caller must hold s.mu (read or write).
func (s *FileStore) goalExistsLocked(ctx context.Context, goalID string) (bool, error) {
	if s.knownGoals[goalID] {
		return true, nil
	}
	return s.goalExistsSlow(ctx, goalID)
}

// goalExistsSlow checks the disk when the knownGoals cache misses.
// Should not be reached in normal operation.
func (s *FileStore) goalExistsSlow(ctx context.Context, goalID string) (bool, error) {
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
	exists, err := s.goalExistsLocked(ctx, goalID)
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

func (s *FileStore) goalIDForObligationLocked(ctx context.Context, obligationID string) (string, error) {
	if err := validateArtifactID("obligation", obligationID); err != nil {
		return "", err
	}
	obl, err := readFile[schema.Obligation](ctx, s.artifactPath(dirObligations, obligationID))
	if err != nil {
		return "", fmt.Errorf("store: load obligation %s: %w", obligationID, err)
	}
	return s.findGoalIDForConditionLocked(ctx, obl.GoalConditionID)
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

func (s *FileStore) goalIDForCapsuleLocked(ctx context.Context, capsuleID string) (string, error) {
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
	return s.goalIDForObligationLocked(ctx, c.ObligationIDs[0])
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

func (s *FileStore) goalIDForEvidenceLocked(ctx context.Context, ev *schema.EvidenceArtifact) (string, error) {
	for _, obligationID := range append(append([]string{}, ev.Supports...), ev.Weakens...) {
		if obligationID == "" {
			continue
		}
		goalID, err := s.goalIDForObligationLocked(ctx, obligationID)
		if err == nil {
			return goalID, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return "", err
		}
	}
	return "", fmt.Errorf("store: evidence %s has no resolvable obligation reference", ev.EvidenceID)
}

// goalIDForProjectionSources resolves a goal from a projection's source IDs.
// Caller must hold s.mu (read or write).
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
		goalID, err := s.goalIDForCapsuleLocked(ctx, id)
		if err == nil {
			return goalID, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return "", err
		}
	}
	for _, id := range sourceIDs {
		goalID, err := s.goalIDForObligationLocked(ctx, id)
		if err == nil {
			return goalID, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return "", err
		}
	}
	for _, id := range sourceIDs {
		exists, err := s.goalExistsLocked(ctx, id)
		if err != nil {
			return "", err
		}
		if exists {
			return id, nil
		}
	}
	return "", fmt.Errorf("store: no projection source resolves to a goal")
}

// goalIDForRelatedIDs resolves a goal from a decision record's related IDs.
// Caller must hold s.mu (read or write).
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
			exists, err := s.goalExistsLocked(ctx, id)
			if err != nil {
				return "", err
			}
			if !exists {
				return "", ErrNotFound
			}
			return id, nil
		},
		func(id string) (string, error) { return s.goalIDForObligationLocked(ctx, id) },
		func(id string) (string, error) { return s.goalIDForCapsuleLocked(ctx, id) },
		func(id string) (string, error) {
			p, err := readFile[schema.PatchArtifact](ctx, s.artifactPath(dirPatches, id))
			if err != nil {
				return "", err
			}
			return s.goalIDForCapsuleLocked(ctx, p.CapsuleID)
		},
		func(id string) (string, error) {
			c, err := readFile[schema.ClaimArtifact](ctx, s.artifactPath(dirClaims, id))
			if err != nil {
				return "", err
			}
			return s.goalIDForCapsuleLocked(ctx, c.SourceCapsuleID)
		},
		func(id string) (string, error) {
			f, err := readFile[schema.FailureFingerprint](ctx, s.artifactPath(dirFailures, id))
			if err != nil {
				return "", err
			}
			return s.goalIDForCapsuleLocked(ctx, f.SourceCapsuleID)
		},
		func(id string) (string, error) {
			r, err := readFile[schema.VerifierResult](ctx, s.artifactPath(dirVerifierResults, id))
			if err != nil {
				return "", err
			}
			return s.goalIDForCapsuleLocked(ctx, r.CapsuleID)
		},
		func(id string) (string, error) {
			ev, err := readFile[schema.EvidenceArtifact](ctx, s.artifactPath(dirEvidence, id))
			if err != nil {
				return "", err
			}
			return s.goalIDForEvidenceLocked(ctx, ev)
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

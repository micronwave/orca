package reconciler

import "errors"

// ErrInvalidWaiver is returned when a waiver token does not match a stored
// DecisionRecord with Context "waiver_review".
var ErrInvalidWaiver = errors.New("reconciler: invalid waiver")

// ErrNoActiveGoal is returned when Reconcile is called and no active goal exists.
var ErrNoActiveGoal = errors.New("reconciler: no active goal")

// ErrMissingVerifierResult is returned when no verifier result exists for the patch.
var ErrMissingVerifierResult = errors.New("reconciler: verifier result not found")

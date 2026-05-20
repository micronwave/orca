package failurehistory

import (
	"context"
	"fmt"
	"strings"

	"github.com/micronwave/orca/internal/schema"
	"github.com/micronwave/orca/internal/store"
)

const maxPriorCapsules = 10

// Prepare normalizes a new failure and attaches prior-attempt history before it
// is saved as a distinct fingerprint artifact.
func Prepare(ctx context.Context, st store.ArtifactStore, goalID string, failure *schema.FailureFingerprint, noLearning bool) error {
	if failure == nil {
		return fmt.Errorf("failurehistory: failure is required")
	}
	failure.ErrorSignature = NormalizeSignature(failure.ErrorSignature)
	if failure.ErrorSignature == "" {
		failure.ErrorSignature = NormalizeSignature(failure.Summary)
	}
	failure.PriorAttemptCount = 0
	failure.PriorCapsuleIDs = nil
	if noLearning {
		failure.RecommendedNextAction = ""
		return nil
	}
	matches, err := st.LoadFailuresBySignature(ctx, goalID, failure.ErrorSignature)
	if err != nil {
		return fmt.Errorf("failurehistory: load prior failures for signature %q: %w", failure.ErrorSignature, err)
	}
	failure.PriorAttemptCount = len(matches)
	for _, match := range matches {
		if match == nil || strings.TrimSpace(match.SourceCapsuleID) == "" {
			continue
		}
		failure.PriorCapsuleIDs = append(failure.PriorCapsuleIDs, match.SourceCapsuleID)
		if len(failure.PriorCapsuleIDs) == maxPriorCapsules {
			break
		}
	}
	failure.RecommendedNextAction = RecommendedNextAction(failure)
	return nil
}

func NormalizeSignature(signature string) string {
	signature = strings.ReplaceAll(signature, "\r\n", "\n")
	signature = strings.ReplaceAll(signature, "\r", "\n")
	for strings.Contains(signature, "\n\n") {
		signature = strings.ReplaceAll(signature, "\n\n", "\n")
	}
	return strings.ToLower(strings.TrimSpace(signature))
}

func RecommendedNextAction(failure *schema.FailureFingerprint) string {
	if failure == nil {
		return ""
	}
	signature := NormalizeSignature(failure.ErrorSignature)
	summary := NormalizeSignature(failure.Summary)
	switch {
	case failure.PriorAttemptCount >= 2:
		return "stop retrying the same approach; route through human review with the prior capsule history"
	case failure.FailureType == schema.FailureTest:
		return "reproduce the failing test locally, isolate the regression, then retry with targeted test evidence"
	case failure.FailureType == schema.FailureLint || failure.FailureType == schema.FailureTypecheck:
		return "fix the static gate failure and rerun the exact verifier gate"
	case strings.Contains(signature, "timeout") || strings.Contains(summary, "timeout") || strings.Contains(summary, "deadline exceeded"):
		return "inspect the timeout cause before retrying; narrow the command or raise the capsule budget only with evidence"
	case failure.FailureType == schema.FailureInfra:
		return "fix the local infrastructure or tool setup before rerunning the capsule"
	case failure.FailureType == schema.FailurePolicy:
		return "adjust the patch to satisfy the capsule policy before retrying"
	default:
		return "investigate the normalized failure signature before retrying"
	}
}

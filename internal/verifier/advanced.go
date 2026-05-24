package verifier

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/micronwave/orca/internal/schema"
)

func (s *Engine) runMutationGate(
	ctx context.Context,
	goalID, capsuleID string,
	workingDir string,
	obligationRefs []string,
	changedFiles []string,
) ([]string, bool, error) {
	if !s.advanced.Enabled || !s.advanced.Mutation {
		return nil, false, nil
	}
	command := strings.TrimSpace(s.advanced.MutationCommand)
	if command == "" {
		return []string{"mutation gate skipped: mutation_command not configured"}, false, nil
	}

	timeout := 120 * time.Second
	if s.advanced.MutationTimeoutSeconds > 0 {
		timeout = time.Duration(s.advanced.MutationTimeoutSeconds) * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	exitCode, output, err := s.runner.Run(runCtx, command, workingDir)
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded)
	if err != nil && !timedOut {
		return nil, false, fmt.Errorf("verifier: mutation gate goal %s capsule %s: %w", goalID, capsuleID, err)
	}
	if timedOut {
		exitCode = -1
	}

	summary := "mutation testing passed: no survivors found"
	warnings := []string(nil)
	blocking := false
	if timedOut {
		summary = "mutation testing timed out"
		warnings = append(warnings, "[mutation] mutation gate timed out")
		if s.advanced.MutationBlocking {
			if _, err := s.saveGateFailure(ctx, goalID, capsuleID, schema.FailureTest, command, summary, changedFiles); err != nil {
				return nil, false, err
			}
		}
	} else if exitCode != 0 {
		summary = fmt.Sprintf("mutation testing found survivors (exit_code=%d): %s", exitCode, summarizeAdvancedOutput(output, 200))
		warnings = append(warnings, fmt.Sprintf("[mutation] survivor found: test gap candidate for %s", advancedGateFiles(changedFiles)))
		blocking = s.advanced.MutationBlocking
	} else {
		warnings = append(warnings, "[mutation] gate passed: no survivors found")
	}

	if _, err := s.saveAdvancedEvidence(ctx, schema.EvidenceMutationResult, "verifier", command, exitCode, summary, output, obligationRefs); err != nil {
		return nil, false, err
	}
	return warnings, blocking, nil
}

func (s *Engine) runAdversarialGate(
	ctx context.Context,
	goalID, capsuleID string,
	workingDir string,
	obligationRefs []string,
	changedFiles []string,
) ([]string, bool, error) {
	if !s.advanced.Enabled || !s.advanced.AdversarialTests {
		return nil, false, nil
	}
	command := strings.TrimSpace(s.advanced.AdversarialCommand)
	if command == "" {
		return []string{"adversarial gate skipped: adversarial_command not configured"}, false, nil
	}

	timeout := 60 * time.Second
	if s.advanced.AdversarialTimeoutSeconds > 0 {
		timeout = time.Duration(s.advanced.AdversarialTimeoutSeconds) * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	exitCode, output, err := s.runner.Run(runCtx, command, workingDir)
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded)
	if err != nil && !timedOut {
		return nil, false, fmt.Errorf("verifier: adversarial gate goal %s capsule %s: %w", goalID, capsuleID, err)
	}
	if timedOut {
		exitCode = -1
	}

	summary := "adversarial gate passed: no challenge failures"
	warnings := []string(nil)
	blocking := false
	if timedOut {
		summary = "adversarial gate timed out"
		warnings = append(warnings, "[adversarial] gate timed out")
	} else if exitCode != 0 {
		summary = fmt.Sprintf("adversarial challenge failed (exit_code=%d): %s", exitCode, summarizeAdvancedOutput(output, 200))
		warnings = append(warnings, "[adversarial] challenge failed: test gap candidate")
		blocking = s.advanced.AdversarialBlocking
	} else {
		warnings = append(warnings, "[adversarial] gate passed: no challenge failures")
	}

	if _, err := s.saveAdvancedEvidence(ctx, schema.EvidenceTestResult, "adversarial gate", command, exitCode, summary, output, obligationRefs); err != nil {
		return nil, false, err
	}
	_ = changedFiles
	return warnings, blocking, nil
}

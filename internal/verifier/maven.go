package verifier

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/micronwave/orca/internal/schema"
)

type reviewFindings struct {
	warnings            []string
	requiresHumanReview bool
	claimIDs            []string
	claims              []*schema.ClaimArtifact
}

func (r reviewFindings) rationale() string {
	if len(r.claimIDs) == 0 {
		return "supplemental reviewer claims require human review"
	}
	return fmt.Sprintf("supplemental reviewer claims require human review: %s", strings.Join(r.claimIDs, ", "))
}

func (s *Engine) reviewFindingsFromClaims(ctx context.Context, claimIDs []string) (reviewFindings, error) {
	claimIDs = uniqueStrings(claimIDs)
	findings := reviewFindings{}
	for _, claimID := range claimIDs {
		claim, err := s.store.LoadClaim(ctx, claimID)
		if err != nil {
			return reviewFindings{}, fmt.Errorf("verifier: load supplemental claim %s: %w", claimID, err)
		}
		findings.claims = append(findings.claims, claim)
		switch claim.ClaimType {
		case schema.ClaimRisk, schema.ClaimOpenQuestion, schema.ClaimTestGap:
			if claim.Status != schema.ClaimVerified {
				findings.requiresHumanReview = true
				findings.claimIDs = append(findings.claimIDs, claim.ClaimID)
			}
			findings.warnings = append(findings.warnings, fmt.Sprintf("review claim %s (%s) status=%s", claim.ClaimID, claim.ClaimType, claim.Status))
		case schema.ClaimAssumption:
			if claim.Status != schema.ClaimVerified {
				findings.requiresHumanReview = true
				findings.claimIDs = append(findings.claimIDs, claim.ClaimID)
			}
		}
	}
	findings.claimIDs = uniqueStrings(findings.claimIDs)
	findings.warnings = uniqueStrings(findings.warnings)
	return findings, nil
}

type mavenFindings struct {
	warnings            []string
	requiresHumanReview bool
}

func (s *Engine) runMAVEN(
	patch *schema.PatchArtifact,
	obligations []*schema.Obligation,
	obligationResults []schema.ObligationVerdict,
	allEvidenceByID map[string]*schema.EvidenceArtifact,
	supplementalClaims []*schema.ClaimArtifact,
) mavenFindings {
	findings := mavenFindings{}
	allEvidence := make([]*schema.EvidenceArtifact, 0, len(allEvidenceByID))
	for _, ev := range allEvidenceByID {
		if ev != nil {
			allEvidence = append(allEvidence, ev)
		}
	}

	expectedFilesByObligation := make(map[string][]string, len(obligations))
	for _, obligation := range obligations {
		if obligation == nil {
			continue
		}
		expectedFilesByObligation[obligation.ObligationID] = append([]string(nil), obligation.ExpectedFiles...)
		for _, required := range obligation.EvidenceRequired {
			if !hasEvidenceForObligationType(allEvidence, obligation.ObligationID, required) {
				findings.warnings = append(findings.warnings, fmt.Sprintf("[maven] factual: obligation %s missing evidence type %s", obligation.ObligationID, required))
				findings.requiresHumanReview = true
			}
		}
	}

	for _, result := range obligationResults {
		if result.Verdict != schema.VerdictSatisfied {
			continue
		}
		for _, evidenceID := range result.EvidenceIDs {
			evidence := allEvidenceByID[evidenceID]
			if evidence != nil && evidence.ExitCode != 0 {
				findings.warnings = append(findings.warnings, fmt.Sprintf("[maven] logical: obligation %s verdict=satisfied but evidence exit_code != 0", result.ObligationID))
				findings.requiresHumanReview = true
				break
			}
		}
	}

	outOfScope := patchFilesOutsideMAVENScope(patch.ChangedFiles, expectedFilesByObligation, allEvidence)
	if len(outOfScope) > 0 {
		findings.warnings = append(findings.warnings, fmt.Sprintf("[maven] causal: patch changed files outside obligation scope: %s", strings.Join(outOfScope, ", ")))
	}

	for _, claim := range supplementalClaims {
		if claim == nil || claim.Status == schema.ClaimVerified {
			continue
		}
		switch claim.ClaimType {
		case schema.ClaimAssumption, schema.ClaimRisk, schema.ClaimOpenQuestion, schema.ClaimTestGap:
			findings.warnings = append(findings.warnings, fmt.Sprintf("[maven] assumption: unverified %s claim %s is still relevant", claim.ClaimType, claim.ClaimID))
			findings.requiresHumanReview = true
		}
	}

	findings.warnings = uniqueStrings(findings.warnings)
	return findings
}

func hasEvidenceForObligationType(evidence []*schema.EvidenceArtifact, obligationID string, evidenceType string) bool {
	for _, item := range evidence {
		if item == nil || string(item.Type) != evidenceType {
			continue
		}
		if evidenceMatchesObligation(item, obligationID) {
			return true
		}
	}
	return false
}

func patchFilesOutsideMAVENScope(
	changedFiles []string,
	expectedFilesByObligation map[string][]string,
	evidence []*schema.EvidenceArtifact,
) []string {
	scopedFiles := make(map[string]bool)
	for _, files := range expectedFilesByObligation {
		addNormalizedFiles(scopedFiles, files)
	}
	for _, item := range evidence {
		if item == nil {
			continue
		}
		for _, obligationID := range item.Supports {
			addNormalizedFiles(scopedFiles, expectedFilesByObligation[obligationID])
		}
	}
	var out []string
	for _, file := range changedFiles {
		normalized := normalizeMAVENPath(file)
		if normalized == "" {
			continue
		}
		if !scopedFiles[normalized] {
			out = append(out, normalized)
		}
	}
	sort.Strings(out)
	return uniqueStrings(out)
}

func addNormalizedFiles(dst map[string]bool, files []string) {
	for _, file := range files {
		normalized := normalizeMAVENPath(file)
		if normalized != "" {
			dst[normalized] = true
		}
	}
}

func normalizeMAVENPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if path == "." {
		return ""
	}
	return strings.ReplaceAll(path, "\\", "/")
}

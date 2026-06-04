package runner

import (
	"strings"
	"testing"

	"github.com/micronwave/orca/internal/schema"
)

func TestSerializeExecutorProjection(t *testing.T) {
	md, err := SerializeExecutorProjection(&schema.ContextProjection{
		ContextProjectionID: "CTX-1",
		Role:                schema.ProjectionRoleExecutor,
		SourceArtifactIDs:   []string{"OB-1", "CLM-1"},
		IncludedSections:    []string{"goal_conditions", "obligations"},
		OmittedSections:     []string{"raw_transcript"},
		TokenBudget:         2048,
		FreshnessBase:       "SNAP-1",
	})
	if err != nil {
		t.Fatalf("SerializeExecutorProjection: %v", err)
	}
	for _, want := range []string{
		"# Orca Executor Briefing",
		"Freshness Base: `SNAP-1`",
		"goal_conditions",
		"obligations",
		"obligations_addressed",
		"evidence_paths",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("serialized markdown missing %q\n%s", want, md)
		}
	}
	for _, absent := range []string{
		"Context Projection ID",
		"Token Budget",
		"Included Sections",
		"Omitted Sections",
		"Source Artifact IDs",
	} {
		if strings.Contains(md, absent) {
			t.Fatalf("serialized markdown should not contain %q\n%s", absent, md)
		}
	}
}

func TestSerializeExecutorProjectionNoFreshnessBase(t *testing.T) {
	md, err := SerializeExecutorProjection(&schema.ContextProjection{
		ContextProjectionID: "CTX-2",
		Role:                schema.ProjectionRoleExecutor,
		FreshnessBase:       "",
	})
	if err != nil {
		t.Fatalf("SerializeExecutorProjection: %v", err)
	}
	if strings.Contains(md, "## Projection") {
		t.Fatalf("## Projection section should be omitted when FreshnessBase is empty\n%s", md)
	}
}

func TestSerializeExecutorProjectionRejectsWrongRole(t *testing.T) {
	_, err := SerializeExecutorProjection(&schema.ContextProjection{
		ContextProjectionID: "CTX-2",
		Role:                schema.ProjectionRoleHumanSummary,
	})
	if err == nil {
		t.Fatal("SerializeExecutorProjection returned nil error for human_summary role")
	}
}

func TestSerializeExecutorProjectionAcceptsReviewerRole(t *testing.T) {
	md, err := SerializeExecutorProjection(&schema.ContextProjection{
		ContextProjectionID: "CTX-reviewer",
		Role:                schema.ProjectionRoleReviewer,
		FreshnessBase:       "SNAP-r1",
	})
	if err != nil {
		t.Fatalf("SerializeExecutorProjection reviewer: %v", err)
	}
	for _, want := range []string{
		"# Orca Reviewer Briefing",
		"Freshness Base: `SNAP-r1`",
		"obligations_addressed",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("serialized reviewer markdown missing %q\n%s", want, md)
		}
	}
}

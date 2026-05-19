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
		"Context Projection ID: `CTX-1`",
		"goal_conditions",
		"obligations_addressed",
		"evidence_paths",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("serialized markdown missing %q\n%s", want, md)
		}
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

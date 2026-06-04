package runner

import (
	"fmt"
	"strings"

	"github.com/micronwave/orca/internal/schema"
)

// SerializeExecutorProjection converts an agent projection into a markdown
// briefing consumable by CLI adapters. The historical function name is kept for
// adapter compatibility; Phase 2.2 allows executor, reviewer, and tester roles.
func SerializeExecutorProjection(p *schema.ContextProjection) (string, error) {
	if p == nil {
		return "", fmt.Errorf("runner: projection is required")
	}
	if !isAgentProjectionRole(p.Role) {
		return "", fmt.Errorf("runner: expected agent projection role, got %q", p.Role)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Orca %s Briefing\n\n", projectionRoleTitle(p.Role))

	if strings.TrimSpace(p.FreshnessBase) != "" {
		b.WriteString("## Projection\n")
		fmt.Fprintf(&b, "- Freshness Base: `%s`\n", p.FreshnessBase)
		b.WriteString("\n")
	}

	for _, section := range p.IncludedSections {
		if strings.TrimSpace(section) == "" {
			continue
		}
		b.WriteString(section)
		b.WriteString("\n\n")
	}

	b.WriteString("## Required Output Contract\n")
	b.WriteString("- Return sidecar-equivalent output with these keys:\n")
	b.WriteString("  - obligations_addressed\n")
	b.WriteString("  - files_changed\n")
	b.WriteString("  - commands_run\n")
	b.WriteString("  - assumptions\n")
	b.WriteString("  - claims (each claim may include contradicts and invalidates claim-ID arrays)\n")
	b.WriteString("  - risks\n")
	b.WriteString("  - follow_up_needed\n")
	b.WriteString("  - evidence_paths\n")
	return b.String(), nil
}

func isAgentProjectionRole(role schema.ProjectionRole) bool {
	switch role {
	case schema.ProjectionRoleExecutor, schema.ProjectionRoleReviewer, schema.ProjectionRoleTester:
		return true
	default:
		return false
	}
}

func projectionRoleTitle(role schema.ProjectionRole) string {
	switch role {
	case schema.ProjectionRoleReviewer:
		return "Reviewer"
	case schema.ProjectionRoleTester:
		return "Tester"
	default:
		return "Executor"
	}
}


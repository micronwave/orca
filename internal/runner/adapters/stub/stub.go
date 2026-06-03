// Package stub provides placeholder adapters for agent types that have no
// implementation yet. Copy this package as a starting template for new adapters.
package stub

import (
	"context"
	"errors"
	"fmt"

	"github.com/micronwave/orca/internal/schema"
)

// ErrNotImplemented is returned by Execute and ExtractFromTranscript to signal
// that this agent type has no implementation yet.
var ErrNotImplemented = errors.New("stub: agent type not implemented")

// Adapter is a placeholder for agent types with no implementation.
// Preflight succeeds so capsule creation proceeds; Execute and
// ExtractFromTranscript return ErrNotImplemented so the failure is informative.
type Adapter struct {
	agentType schema.AgentType
}

// New returns a stub Adapter registered for agentType.
func New(agentType schema.AgentType) *Adapter {
	return &Adapter{agentType: agentType}
}

func (a *Adapter) AgentType() schema.AgentType { return a.agentType }

func (a *Adapter) Preflight(_ context.Context, _ *schema.ExecutionCapsule) error {
	return nil
}

func (a *Adapter) Execute(_ context.Context, _ *schema.ExecutionCapsule, _ *schema.ContextProjection) (*schema.AgentSidecarOutput, error) {
	return nil, fmt.Errorf("stub: agent %q: %w", a.agentType, ErrNotImplemented)
}

func (a *Adapter) ExtractFromTranscript(_ context.Context, _ *schema.ExecutionCapsule, _ string) (*schema.AgentSidecarOutput, error) {
	return nil, fmt.Errorf("stub: agent %q: %w", a.agentType, ErrNotImplemented)
}

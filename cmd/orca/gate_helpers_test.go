package main

import (
	"context"
	"time"

	"github.com/micronwave/orca/internal/gate"
)

// stubGate implements gateService with configurable merge decisions for testing.
// All non-merge gate methods auto-approve.
type stubGate struct {
	mergeApproved bool
	mergeNotes    string
}

func (g *stubGate) ReviewProjection(_ context.Context, _ string, _ time.Duration) (gate.GateDecision, error) {
	return gate.GateDecision{Approved: true}, nil
}

func (g *stubGate) ReviewMerge(_ context.Context, _ string) (gate.GateDecision, error) {
	return gate.GateDecision{Approved: g.mergeApproved, Notes: g.mergeNotes}, nil
}

func (g *stubGate) ReviewWaiver(_ context.Context, _, _ string) (gate.GateDecision, error) {
	return gate.GateDecision{Approved: true}, nil
}

func (g *stubGate) Close() {}

// closedTracker wraps a gateService and records whether Close was called.
// Used to verify that callers properly close the original gatekeeper.
type closedTracker struct {
	inner  gateService
	closed bool
}

func (c *closedTracker) ReviewProjection(ctx context.Context, capsuleID string, d time.Duration) (gate.GateDecision, error) {
	return c.inner.ReviewProjection(ctx, capsuleID, d)
}

func (c *closedTracker) ReviewMerge(ctx context.Context, patchID string) (gate.GateDecision, error) {
	return c.inner.ReviewMerge(ctx, patchID)
}

func (c *closedTracker) ReviewWaiver(ctx context.Context, obligationID string, reason string) (gate.GateDecision, error) {
	return c.inner.ReviewWaiver(ctx, obligationID, reason)
}

func (c *closedTracker) Close() { c.closed = true }

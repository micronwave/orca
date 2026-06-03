package stub_test

import (
	"context"
	"errors"
	"testing"

	"github.com/micronwave/orca/internal/runner"
	"github.com/micronwave/orca/internal/runner/adapters/stub"
	"github.com/micronwave/orca/internal/schema"
)

// compile-time interface check
var _ runner.Adapter = (*stub.Adapter)(nil)

func TestNew_AgentType(t *testing.T) {
	for _, agentType := range []schema.AgentType{schema.AgentCopilot, schema.AgentTool} {
		a := stub.New(agentType)
		if got := a.AgentType(); got != agentType {
			t.Errorf("AgentType() = %q, want %q", got, agentType)
		}
	}
}

func TestPreflight_ReturnsNil(t *testing.T) {
	a := stub.New(schema.AgentCopilot)
	if err := a.Preflight(context.Background(), &schema.ExecutionCapsule{}); err != nil {
		t.Errorf("Preflight() error = %v, want nil", err)
	}
}

func TestExecute_ReturnsErrNotImplemented(t *testing.T) {
	a := stub.New(schema.AgentCopilot)
	out, err := a.Execute(context.Background(), &schema.ExecutionCapsule{}, &schema.ContextProjection{})
	if out != nil {
		t.Errorf("Execute() output = %v, want nil", out)
	}
	if !errors.Is(err, stub.ErrNotImplemented) {
		t.Errorf("Execute() error = %v, want wrapping ErrNotImplemented", err)
	}
}

func TestExtractFromTranscript_ReturnsErrNotImplemented(t *testing.T) {
	a := stub.New(schema.AgentTool)
	out, err := a.ExtractFromTranscript(context.Background(), &schema.ExecutionCapsule{}, "/tmp/transcript.json")
	if out != nil {
		t.Errorf("ExtractFromTranscript() output = %v, want nil", out)
	}
	if !errors.Is(err, stub.ErrNotImplemented) {
		t.Errorf("ExtractFromTranscript() error = %v, want wrapping ErrNotImplemented", err)
	}
}

func TestExecute_ErrorContainsAgentType(t *testing.T) {
	a := stub.New(schema.AgentCopilot)
	_, err := a.Execute(context.Background(), &schema.ExecutionCapsule{}, &schema.ContextProjection{})
	if err == nil {
		t.Fatal("Execute() error = nil, want error")
	}
	want := `stub: agent "copilot": stub: agent type not implemented`
	if err.Error() != want {
		t.Errorf("Execute() error = %q, want %q", err.Error(), want)
	}
}

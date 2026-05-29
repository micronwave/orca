package runner

import (
	"bytes"
	"encoding/json"

	"github.com/micronwave/orca/internal/schema"
)

// ParseSidecarJSON parses a JSON-encoded AgentSidecarOutput blob.
// Returns ErrNoSidecar when data is empty, ErrInvalidSidecar when it is
// malformed or structurally empty (no files, commands, or obligations).
func ParseSidecarJSON(data []byte) (*schema.AgentSidecarOutput, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, ErrNoSidecar
	}
	out := &schema.AgentSidecarOutput{}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, ErrInvalidSidecar
	}
	if len(out.FilesChanged) == 0 && len(out.CommandsRun) == 0 && len(out.ObligationsAddressed) == 0 {
		return nil, ErrInvalidSidecar
	}
	return out, nil
}

// claudeEnvelope is the JSON wrapper that `claude -p --output-format json` produces.
type claudeEnvelope struct {
	Result  string `json:"result"`
	IsError bool   `json:"is_error"`
}

// ParseOutputJSON parses agent output that may be a raw AgentSidecarOutput JSON
// or a Claude JSON envelope ({"result": "<inner JSON>", "is_error": false}).
// Returns ErrNoSidecar for empty input, ErrInvalidSidecar for unrecognised formats.
func ParseOutputJSON(data []byte) (*schema.AgentSidecarOutput, error) {
	out, err := ParseSidecarJSON(data)
	if err == nil {
		return out, nil
	}
	// Try Claude envelope
	var env claudeEnvelope
	if jsonErr := json.Unmarshal(data, &env); jsonErr == nil && !env.IsError && env.Result != "" {
		return ParseSidecarJSON([]byte(env.Result))
	}
	return nil, err
}

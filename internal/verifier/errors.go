package verifier

import "errors"

// ErrGateExec is returned when a verifier gate cannot be executed (missing
// binary, permission denied, etc.) as distinct from the gate running and
// returning a non-zero exit code.
var ErrGateExec = errors.New("verifier: gate execution failed")

// ErrShellNotFound is returned when the platform shell required to run gates
// cannot be located on PATH.
var ErrShellNotFound = errors.New("verifier: shell not found")

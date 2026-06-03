package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	goos "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/micronwave/orca/internal/schema"
)

// newTestSupervisor builds a supervisor wired to in-memory pipes so tests
// never touch os.Stdin/Stdout. The returned Supervisor has its inner runtime
// and gatekeeper wired; callers must call closeFn() when done.
func newTestSupervisor(t *testing.T, orcaDir string, in io.Reader) (*Supervisor, func()) {
	t.Helper()
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	var outBuf, errBuf bytes.Buffer
	sup := newSupervisor(orcaDir, rt, in, &outBuf, &errBuf)
	return sup, func() {
		sup.stopOnce.Do(func() { close(sup.stop) })
		_ = sup.gateW.Close()
		closeFn()
	}
}

// TestSupervisor_StatusWhileGoalActive verifies that /status works while a
// fake long-running goal goroutine holds goalActive=true.
func TestSupervisor_StatusWhileGoalActive(t *testing.T) {
	orcaDir := seedOrcaDir(t, true) // includes active goal in store
	sup, cleanup := newTestSupervisor(t, orcaDir, strings.NewReader(""))
	defer cleanup()

	// Simulate an in-flight goal: set goalActive without actually running the pipeline.
	goalCtx, goalCancel := context.WithCancel(context.Background())
	defer goalCancel()
	sup.goalMu.Lock()
	sup.goalActive.Store(true)
	sup.goalCtx = goalCtx
	sup.goalCancel = goalCancel
	sup.goalMu.Unlock()

	var outBuf bytes.Buffer
	sup.out = &outBuf

	// /status must succeed and return data about the store's active goal.
	if err := sup.handleLine(context.Background(), "/status"); err != nil {
		t.Fatalf("handleLine(/status): %v", err)
	}
	got := outBuf.String()
	if !strings.Contains(got, "fix the auth middleware rounding defect") {
		t.Errorf("status output missing intent: %q", got)
	}
}

// TestSupervisor_CancelCancelsGoalContext verifies that /cancel cancels the
// active goal's context and updates the stored goal status.
func TestSupervisor_CancelCancelsGoalContext(t *testing.T) {
	orcaDir := seedOrcaDir(t, true) // includes active goal with capsule in store
	sup, cleanup := newTestSupervisor(t, orcaDir, strings.NewReader(""))
	defer cleanup()

	goalCtx, goalCancel := context.WithCancel(context.Background())
	sup.goalMu.Lock()
	sup.goalActive.Store(true)
	sup.goalCtx = goalCtx
	sup.goalCancel = goalCancel
	sup.goalMu.Unlock()

	// A fake "goal goroutine" that blocks until the context is cancelled.
	goalDone := make(chan struct{})
	go func() {
		defer close(goalDone)
		<-goalCtx.Done()
	}()

	var outBuf bytes.Buffer
	sup.out = &outBuf

	if err := sup.handleCancel(context.Background()); err != nil {
		t.Fatalf("handleCancel: %v", err)
	}

	select {
	case <-goalDone:
	case <-time.After(2 * time.Second):
		t.Fatal("goal context was not cancelled within 2s")
	}

	// Stored status must now be cancelled.
	if got := loadGoalStatus(t, orcaDir, "G-1"); got != schema.GoalStatusCancelled {
		t.Fatalf("goal status = %s, want cancelled", got)
	}
}

// TestSupervisor_GateInputRoutedThroughPipe verifies that non-command input
// is written to the gate pipe (not lineCh) when gateWaiting is set.
// This ensures there is exactly one stdin reader in the session.
func TestSupervisor_GateInputRoutedThroughPipe(t *testing.T) {
	// Create a controlled input source so the test drives timing.
	sessionInR, sessionInW := io.Pipe()
	t.Cleanup(func() { _ = sessionInW.Close(); _ = sessionInR.Close() })

	orcaDir := t.TempDir()
	writeTestConfig(t, filepath.Join(orcaDir, "config.yaml"))
	sup, cleanup := newTestSupervisor(t, orcaDir, sessionInR)
	defer cleanup()

	// Signal that a gate is currently waiting for input.
	sup.gateWaiting.Store(true)

	// Start the supervisor's readLines goroutine.
	lineCh := make(chan string, 4)
	go sup.readLines(lineCh)

	// Write an empty line (= approve) to the session's input.
	go func() {
		_, _ = io.WriteString(sessionInW, "\n")
	}()

	// The gate pipe must receive the approval within 1s.
	gateScanner := bufio.NewScanner(sup.gateR)
	gotLine := make(chan string, 1)
	go func() {
		if gateScanner.Scan() {
			gotLine <- gateScanner.Text()
		} else {
			close(gotLine)
		}
	}()

	select {
	case line, ok := <-gotLine:
		if !ok {
			t.Fatal("gate pipe closed without delivering any input")
		}
		// Empty line signals approve.
		if line != "" {
			t.Fatalf("gate received %q, want empty string (approve)", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gate pipe did not receive input within 2s")
	}

	// lineCh must NOT have received anything (it was routed to the gate).
	select {
	case got := <-lineCh:
		t.Fatalf("lineCh received %q, want no lines (should be routed to gate)", got)
	default:
	}
}

// TestSupervisor_CommandInputRemainsActiveDuringGate verifies that supervisor
// commands are not swallowed by the gate pipe while a gate is waiting.
func TestSupervisor_CommandInputRemainsActiveDuringGate(t *testing.T) {
	sessionInR, sessionInW := io.Pipe()
	t.Cleanup(func() { _ = sessionInW.Close(); _ = sessionInR.Close() })

	orcaDir := t.TempDir()
	writeTestConfig(t, filepath.Join(orcaDir, "config.yaml"))
	sup, cleanup := newTestSupervisor(t, orcaDir, sessionInR)
	defer cleanup()

	sup.gateWaiting.Store(true)
	lineCh := make(chan string, 4)
	go sup.readLines(lineCh)

	go func() {
		_, _ = io.WriteString(sessionInW, "/status\n")
	}()

	select {
	case got := <-lineCh:
		if got != "/status" {
			t.Fatalf("lineCh received %q, want /status", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("/status was not delivered to the supervisor command loop")
	}
}

func TestSupervisor_ClearCommandRemainsActiveDuringGate(t *testing.T) {
	if !isSupervisorCommand("clear") {
		t.Fatal("clear must remain a supervisor command while a gate is waiting")
	}
	if !isSupervisorCommand("/clear") {
		t.Fatal("/clear must remain a supervisor command while a gate is waiting")
	}
}

func TestSupervisor_ClearCommandClearsTerminalAndDoesNotStartGoal(t *testing.T) {
	orcaDir := t.TempDir()
	writeTestConfig(t, filepath.Join(orcaDir, "config.yaml"))
	sup, cleanup := newTestSupervisor(t, orcaDir, strings.NewReader(""))
	defer cleanup()

	tmp, err := os.CreateTemp(t.TempDir(), "clear-screen")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer func() { _ = tmp.Close() }()

	sup.out = tmp
	sup.errout = tmp
	if err := sup.handleLine(context.Background(), "/clear"); err != nil {
		t.Fatalf("handleLine(/clear): %v", err)
	}
	if sup.goalActive.Load() {
		t.Fatal("/clear started a goal")
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got, err := io.ReadAll(tmp)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if strings.Contains(string(got), "\x1b[2J") || strings.Contains(string(got), "\x1b[3J") {
		t.Fatalf("non-terminal clear should not emit ANSI data, got %q", string(got))
	}
	want := "≋ Orca  local proof runtime\nWorking directory: " + mustAbs(".") + "\nType a goal or /help for commands\n\n"
	if string(got) != want {
		t.Fatalf("clear output = %q, want %q", string(got), want)
	}
}

// TestSupervisor_ClearPassesOutToScreenWriter verifies that handleLine("/clear")
// passes s.out among the writers given to the clear function, guarding against
// the s.out → s.errout routing regression.
func TestSupervisor_ClearPassesOutToScreenWriter(t *testing.T) {
	orcaDir := t.TempDir()
	writeTestConfig(t, filepath.Join(orcaDir, "config.yaml"))
	sup, cleanup := newTestSupervisor(t, orcaDir, strings.NewReader(""))
	defer cleanup()

	var capturedWriters []io.Writer
	sup.clearScreen = func(writers ...io.Writer) {
		capturedWriters = writers
	}

	if err := sup.handleLine(context.Background(), "/clear"); err != nil {
		t.Fatalf("handleLine(/clear): %v", err)
	}

	var foundOut bool
	for _, w := range capturedWriters {
		if w == sup.out {
			foundOut = true
		}
	}
	if !foundOut {
		t.Fatalf("/clear did not pass s.out to clearScreen; writers = %v", capturedWriters)
	}
}

func TestSupervisor_DoctorCommandDoesNotStartGoal(t *testing.T) {
	orcaDir := t.TempDir()
	writeTestConfig(t, filepath.Join(orcaDir, "config.yaml"))
	sup, cleanup := newTestSupervisor(t, orcaDir, strings.NewReader(""))
	defer cleanup()

	var outBuf, errBuf bytes.Buffer
	sup.out = &outBuf
	sup.errout = &errBuf

	_ = sup.handleLine(context.Background(), "orca doctor")
	if sup.goalActive.Load() {
		t.Fatal("orca doctor started a goal")
	}
	if strings.Contains(errBuf.String(), "[orca] starting: orca doctor") {
		t.Fatalf("doctor command was treated as a goal:\n%s", errBuf.String())
	}
	if !strings.Contains(outBuf.String(), "Orca  doctor") {
		t.Fatalf("doctor output missing report:\n%s", outBuf.String())
	}
}

func TestSupervisor_DoctorCommandRemainsActiveDuringGate(t *testing.T) {
	if !isSupervisorCommand("orca doctor") {
		t.Fatal("orca doctor must remain a supervisor command while a gate is waiting")
	}
}

func TestSupervisor_UICommandDoesNotStartGoal(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("PATH", "")
	if goos.GOOS == "windows" {
		t.Setenv("LOCALAPPDATA", t.TempDir())
	} else {
		t.Setenv("HOME", t.TempDir())
	}

	orcaDir := t.TempDir()
	writeTestConfig(t, filepath.Join(orcaDir, "config.yaml"))
	sup, cleanup := newTestSupervisor(t, orcaDir, strings.NewReader(""))
	defer cleanup()

	var errBuf bytes.Buffer
	sup.errout = &errBuf

	err := sup.handleLine(context.Background(), "orca ui")
	if err == nil || !strings.Contains(err.Error(), "orca-desktop not found") {
		t.Fatalf("handleLine(orca ui) error = %v, want desktop lookup error", err)
	}
	if sup.goalActive.Load() {
		t.Fatal("orca ui started a goal")
	}
	if strings.Contains(errBuf.String(), "[orca] starting: orca ui") {
		t.Fatalf("ui command was treated as a goal:\n%s", errBuf.String())
	}
}

func TestSupervisor_UICommandRemainsActiveDuringGate(t *testing.T) {
	if !isSupervisorCommand("orca ui") {
		t.Fatal("orca ui must remain a supervisor command while a gate is waiting")
	}
}

// TestSupervisor_GateRejectionRoutedThroughPipe verifies that /reject writes
// "reject" to the gate pipe when a gate is waiting.
func TestSupervisor_GateRejectionRoutedThroughPipe(t *testing.T) {
	orcaDir := t.TempDir()
	writeTestConfig(t, filepath.Join(orcaDir, "config.yaml"))

	// Override gateW/gateR with a controllable pipe in the test.
	gateR, gateW := io.Pipe()
	t.Cleanup(func() { _ = gateR.Close(); _ = gateW.Close() })

	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	var outBuf bytes.Buffer
	sup := newSupervisor(orcaDir, rt, strings.NewReader(""), &outBuf, io.Discard)
	// Replace the default pipe with the test pipe.
	_ = sup.gateW.Close()
	_ = sup.gateR.Close()
	sup.gateW = gateW
	sup.gateR = gateR
	sup.gateWaiting.Store(true)
	defer func() { sup.stopOnce.Do(func() { close(sup.stop) }) }()

	received := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(gateR)
		if scanner.Scan() {
			received <- scanner.Text()
		}
	}()

	if err := sup.handleLine(context.Background(), "/reject"); err != nil {
		t.Fatalf("handleLine(/reject): %v", err)
	}

	select {
	case got := <-received:
		if got != "reject" {
			t.Fatalf("gate pipe received %q, want \"reject\"", got)
		}
	case <-time.After(time.Second):
		t.Fatal("gate pipe did not receive reject within 1s")
	}
}

// TestSupervisor_SignalFirstCancelsGoal verifies that the first OS interrupt
// cancels the active goal context and does not exit the session.
func TestSupervisor_SignalFirstCancelsGoal(t *testing.T) {
	orcaDir := seedOrcaDir(t, true)
	sup, cleanup := newTestSupervisor(t, orcaDir, strings.NewReader(""))
	defer cleanup()

	goalCtx, goalCancel := context.WithCancel(context.Background())
	defer goalCancel()
	sup.goalMu.Lock()
	sup.goalActive.Store(true)
	sup.goalCtx = goalCtx
	sup.goalCancel = goalCancel
	sup.goalMu.Unlock()

	// First signal: should cancel the goal, not close stop.
	sup.handleSignal()

	// Context must be cancelled.
	select {
	case <-goalCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("goal context not cancelled after first signal")
	}

	// Stop channel must still be open (session alive).
	select {
	case <-sup.stop:
		t.Fatal("session stop channel closed after first signal, want it open")
	default:
	}

	// cancelRequested must be set.
	if !sup.cancelRequested.Load() {
		t.Fatal("cancelRequested not set after first signal")
	}
	if got := loadGoalStatus(t, orcaDir, "G-1"); got != schema.GoalStatusCancelled {
		t.Fatalf("goal status = %s, want cancelled", got)
	}
}

// TestSupervisor_SignalSecondExitsSession verifies that a second OS interrupt
// closes the stop channel even when a goal is nominally still active.
func TestSupervisor_SignalSecondExitsSession(t *testing.T) {
	orcaDir := t.TempDir()
	writeTestConfig(t, filepath.Join(orcaDir, "config.yaml"))
	sup, cleanup := newTestSupervisor(t, orcaDir, strings.NewReader(""))
	defer cleanup()

	goalCtx, goalCancel := context.WithCancel(context.Background())
	defer goalCancel()
	sup.goalMu.Lock()
	sup.goalActive.Store(true)
	sup.goalCtx = goalCtx
	sup.goalCancel = goalCancel
	sup.goalMu.Unlock()

	// First signal cancels goal.
	sup.handleSignal()
	// Second signal must close stop.
	sup.handleSignal()

	select {
	case <-sup.stop:
	case <-time.After(time.Second):
		t.Fatal("session stop channel not closed after second signal")
	}
}

// TestRunGoal_NonInteractive_DoesNotStartREPL verifies that
// `orca goal --orca-dir ... <text>` returns (quickly) without entering the
// interactive supervisor loop. This is tested by checking that the run path
// for non-interactive goals exits on error (missing config) without hanging.
func TestRunGoal_NonInteractive_DoesNotStartREPL(t *testing.T) {
	// A fresh temp dir has no config.yaml → runGoal returns an error quickly
	// (config load failure), never entering the supervisor REPL loop.
	orcaDir := t.TempDir()

	done := make(chan error, 1)
	go func() {
		done <- run([]string{"goal", "--orca-dir", orcaDir, "some goal"})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error for missing config, got nil")
		}
		// Must not be a hang — just a quick config-load error.
	case <-time.After(5 * time.Second):
		t.Fatal("run goal with missing config hung (REPL loop likely started)")
	}
}

// TestSupervisor_StartGoalRejectsSecondGoal verifies that trying to start a
// second goal while one is running prints an error instead of panicking.
func TestSupervisor_StartGoalRejectsSecondGoal(t *testing.T) {
	orcaDir := t.TempDir()
	writeTestConfig(t, filepath.Join(orcaDir, "config.yaml"))

	sup, cleanup := newTestSupervisor(t, orcaDir, strings.NewReader(""))
	defer cleanup()

	var errBuf bytes.Buffer
	sup.errout = &errBuf

	// Mark a goal as active.
	sup.goalActive.Store(true)
	goalCtx, goalCancel := context.WithCancel(context.Background())
	defer goalCancel()
	sup.goalMu.Lock()
	sup.goalCtx = goalCtx
	sup.goalCancel = goalCancel
	sup.goalMu.Unlock()

	// Attempt to start another goal — must be rejected gracefully.
	if err := sup.startGoal(context.Background(), "second goal"); err != nil {
		t.Fatalf("startGoal unexpectedly returned error: %v", err)
	}
	if !strings.Contains(errBuf.String(), "already running") {
		t.Errorf("expected 'already running' message, got: %q", errBuf.String())
	}
}

// TestSupervisor_ExitCommandCancelsGoal verifies that the "exit" command
// cancels any running goal before closing the stop channel.
func TestSupervisor_ExitCommandCancelsGoal(t *testing.T) {
	orcaDir := t.TempDir()
	writeTestConfig(t, filepath.Join(orcaDir, "config.yaml"))

	sup, cleanup := newTestSupervisor(t, orcaDir, strings.NewReader(""))
	defer cleanup()

	goalCtx, goalCancel := context.WithCancel(context.Background())
	defer goalCancel()
	sup.goalMu.Lock()
	sup.goalActive.Store(true)
	sup.goalCtx = goalCtx
	sup.goalCancel = goalCancel
	sup.goalMu.Unlock()

	if err := sup.handleLine(context.Background(), "exit"); err != nil {
		t.Fatalf("handleLine(exit): %v", err)
	}

	// Goal context must be cancelled.
	select {
	case <-goalCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("goal context not cancelled on exit")
	}

	// Stop channel must be closed.
	select {
	case <-sup.stop:
	case <-time.After(time.Second):
		t.Fatal("stop channel not closed on exit")
	}
}

// TestSupervisor_RunExitsOnEOF verifies that Run returns nil when stdin is
// closed (EOF), which is the expected non-interactive termination path.
func TestSupervisor_RunExitsOnEOF(t *testing.T) {
	orcaDir := t.TempDir()
	writeTestConfig(t, filepath.Join(orcaDir, "config.yaml"))

	// stdin with nothing to read → immediate EOF.
	sup, cleanup := newTestSupervisor(t, orcaDir, strings.NewReader(""))
	defer cleanup()

	done := make(chan error, 1)
	go func() { done <- sup.Run(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit on EOF")
	}
}

// TestSupervisor_ConcurrentStatusDuringGoal verifies that /status can be
// handled from multiple goroutines without data races (supervisor fields are
// accessed concurrently by the goal goroutine and the command loop).
func TestSupervisor_ConcurrentStatusDuringGoal(t *testing.T) {
	orcaDir := seedOrcaDir(t, false)
	sup, cleanup := newTestSupervisor(t, orcaDir, strings.NewReader(""))
	defer cleanup()

	// Use out buffer that's goroutine-safe for this test.
	var mu sync.Mutex
	var outBuf bytes.Buffer
	sup.out = &syncWriter{mu: &mu, buf: &outBuf}

	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = sup.handleLine(ctx, "/status")
		}()
	}
	wg.Wait()
	// Verify that /status actually produced output — not just "no race".
	mu.Lock()
	n := outBuf.Len()
	mu.Unlock()
	if n == 0 {
		t.Error("concurrent /status calls produced no output")
	}
}

type syncWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// Satisfy io.Writer interface for *os.File detection in autoInitWithConfirmation.
var _ io.Writer = (*syncWriter)(nil)

// ── integration: gateService interface satisfied by *gate.Gatekeeper ─────────

// TestGateService_InterfaceSatisfied ensures *gate.Gatekeeper implements
// gateService so the interface definition does not drift from the gate package.
func TestGateService_InterfaceSatisfied(t *testing.T) {
	orcaDir := t.TempDir()
	writeTestConfig(t, filepath.Join(orcaDir, "config.yaml"))
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()
	// rt.gatekeeper is gateService; the compile-time check is sufficient.
	// Verify at runtime that the underlying type is not nil.
	if rt.gatekeeper == nil {
		t.Fatal("rt.gatekeeper is nil after openRuntime")
	}
}

// ── Ensure newSupervisor closes the original gatekeeper ──────────────────────

// TestNewSupervisor_ClosesOriginalGatekeeper verifies that newSupervisor
// closes the default os.Stdin gatekeeper before replacing it, so there is
// only one active stdin reader.
func TestNewSupervisor_ClosesOriginalGatekeeper(t *testing.T) {
	orcaDir := t.TempDir()
	writeTestConfig(t, filepath.Join(orcaDir, "config.yaml"))
	rt, closeFn, err := openRuntime(orcaDir, false)
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer closeFn()

	tracker := &closedTracker{inner: rt.gatekeeper}
	rt.gatekeeper = tracker

	sup := newSupervisor(orcaDir, rt, strings.NewReader(""), io.Discard, io.Discard)
	defer func() { sup.stopOnce.Do(func() { close(sup.stop) }); _ = sup.gateW.Close() }()

	// newSupervisor must call Close() on the original gatekeeper.
	if !tracker.closed {
		t.Fatal("newSupervisor did not call Close() on the original gatekeeper")
	}

	// rt.gatekeeper must be the sessionGate wrapping the new inner gatekeeper.
	sg, ok := rt.gatekeeper.(*sessionGate)
	if !ok {
		t.Fatalf("rt.gatekeeper is %T, want *sessionGate", rt.gatekeeper)
	}
	if sg.s != sup {
		t.Fatal("sessionGate.s does not point to the created supervisor")
	}
}

// Provide a minimal fmt.Fprintln equivalent used inside session.go tests
// to avoid pulling in fmt (already imported).
func init() {
	// Validate that session_test.go compiles with required packages.
	_ = os.DevNull
}

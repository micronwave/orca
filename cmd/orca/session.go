package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/micronwave/orca/internal/gate"
)

// gateService is the consumer-side interface for gate operations.
// Per module_boundaries.md, interfaces belong to the consumer package.
type gateService interface {
	ReviewProjection(ctx context.Context, capsuleID string, reviewWindow time.Duration) (gate.GateDecision, error)
	ReviewMerge(ctx context.Context, patchID string) (gate.GateDecision, error)
	ReviewWaiver(ctx context.Context, obligationID string, reason string) (gate.GateDecision, error)
	Close()
}

// sessionGate wraps *gate.Gatekeeper and signals the Supervisor when a gate
// call is blocking on input, so the supervisor routes stdin to the gate pipe.
type sessionGate struct {
	inner *gate.Gatekeeper
	s     *Supervisor
}

func (g *sessionGate) ReviewProjection(ctx context.Context, capsuleID string, reviewWindow time.Duration) (gate.GateDecision, error) {
	g.s.gateWaiting.Store(true)
	defer g.s.gateWaiting.Store(false)
	return g.inner.ReviewProjection(ctx, capsuleID, reviewWindow)
}

func (g *sessionGate) ReviewMerge(ctx context.Context, patchID string) (gate.GateDecision, error) {
	g.s.gateWaiting.Store(true)
	defer g.s.gateWaiting.Store(false)
	return g.inner.ReviewMerge(ctx, patchID)
}

func (g *sessionGate) ReviewWaiver(ctx context.Context, obligationID string, reason string) (gate.GateDecision, error) {
	g.s.gateWaiting.Store(true)
	defer g.s.gateWaiting.Store(false)
	return g.inner.ReviewWaiver(ctx, obligationID, reason)
}

func (g *sessionGate) Close() { g.inner.Close() }

// Supervisor owns stdin for the interactive session and routes input between
// the REPL command loop and any active gate. It runs one goal at a time in a
// background goroutine while keeping the command loop responsive.
type Supervisor struct {
	orcaDir string
	rt      *runtime

	in     io.Reader // typically os.Stdin
	out    io.Writer // gate prompts and REPL command output (stdout)
	errout io.Writer // progress notifications and the "> " prompt (stderr)

	// gateR is passed to the inner gatekeeper as its input source.
	// The supervisor writes non-command lines to gateW when gateWaiting is set.
	gateR *io.PipeReader
	gateW *io.PipeWriter

	// gateWaiting is set true while a gate call is blocking on input.
	// Atomically accessed from the supervisor input goroutine and the goal goroutine.
	gateWaiting atomic.Bool

	// goalCtx / goalCancel are set while a goal goroutine is running.
	goalCtx    context.Context //nolint:containedctx
	goalCancel context.CancelFunc
	goalActive atomic.Bool
	goalMu     sync.Mutex

	// cancelRequested tracks whether a cancellation has already been requested
	// for the current goal. Reset when the goal finishes.
	cancelRequested atomic.Bool

	sigCh    chan os.Signal
	stop     chan struct{}
	stopOnce sync.Once
}

// newSupervisor constructs a Supervisor and wires the gatekeeper to use the
// session's pipe instead of os.Stdin. The original gatekeeper created by
// newRuntime (which reads from os.Stdin) is closed before replacement.
func newSupervisor(orcaDir string, rt *runtime, in io.Reader, out, errout io.Writer) *Supervisor {
	// Close the default gatekeeper so it does not start a reader goroutine.
	rt.gatekeeper.Close()

	gateR, gateW := io.Pipe()
	s := &Supervisor{
		orcaDir: orcaDir,
		rt:      rt,
		in:      in,
		out:     out,
		errout:  errout,
		gateR:   gateR,
		gateW:   gateW,
		sigCh:   make(chan os.Signal, 1),
		stop:    make(chan struct{}),
	}
	// Replace the runtime's gatekeeper with the session-aware wrapper.
	rt.gatekeeper = &sessionGate{
		inner: gate.NewWithIO(rt.store, gateR, out),
		s:     s,
	}
	return s
}

// Run starts the interactive session loop. It owns stdin, dispatches REPL
// commands, and routes input to the active gate when one is waiting.
// Run blocks until stdin is closed, the stop channel is closed, or ctx is Done.
func (s *Supervisor) Run(ctx context.Context) error {
	lineCh := make(chan string, 4)

	go s.readLines(lineCh)

	signal.Notify(s.sigCh, os.Interrupt)
	defer signal.Stop(s.sigCh)

	fmt.Fprint(s.errout, "> ")
	for {
		select {
		case line, ok := <-lineCh:
			if !ok {
				return nil // stdin EOF
			}
			if err := s.handleLine(ctx, line); err != nil {
				return err
			}
			if !s.goalActive.Load() {
				fmt.Fprint(s.errout, "> ")
			}
		case <-s.sigCh:
			s.handleSignal()
		case <-s.stop:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// readLines reads from s.in line by line. Non-command lines are routed to the
// gate pipe when a gate is waiting; supervisor commands remain available.
// The goroutine exits on EOF or when s.stop is closed.
func (s *Supervisor) readLines(lineCh chan<- string) {
	scanner := bufio.NewScanner(s.in)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if s.gateWaiting.Load() && !isSupervisorCommand(line) {
			// Route to the gate pipe. fmt.Fprintln adds the newline that
			// gate.Gatekeeper's bufio.Reader expects before ReadString returns.
			fmt.Fprintln(s.gateW, line)
			continue
		}
		select {
		case lineCh <- line:
		case <-s.stop:
			close(lineCh)
			return
		}
	}
	close(lineCh)
}

func isSupervisorCommand(line string) bool {
	switch strings.TrimSpace(line) {
	case "exit", "quit", "status", "cancel", "help":
		return true
	}
	return strings.HasPrefix(line, "/")
}

// handleLine dispatches a single trimmed input line to the appropriate handler.
func (s *Supervisor) handleLine(ctx context.Context, line string) error {
	switch {
	case line == "":
		return nil
	case line == "exit" || line == "quit":
		s.goalMu.Lock()
		if s.goalCancel != nil {
			s.goalCancel()
		}
		s.goalMu.Unlock()
		s.stopOnce.Do(func() { close(s.stop) })
		return nil
	case line == "/status" || line == "status":
		return s.rt.printStatusConcise(ctx, s.out)
	case line == "/details":
		return s.rt.printStatus(ctx, s.out)
	case line == "/logs":
		fmt.Fprintln(s.out, "/logs: not yet implemented (Phase 3)")
		return nil
	case line == "/approve":
		if s.gateWaiting.Load() {
			fmt.Fprintln(s.gateW, "")
		} else {
			fmt.Fprintln(s.out, "No gate is currently waiting for input.")
		}
		return nil
	case line == "/reject":
		if s.gateWaiting.Load() {
			fmt.Fprintln(s.gateW, "reject")
		} else {
			fmt.Fprintln(s.out, "No gate is currently waiting for input.")
		}
		return nil
	case line == "/cancel" || line == "cancel":
		return s.handleCancel(ctx)
	case line == "/resume":
		return s.handleResume(ctx)
	case line == "/config":
		fmt.Fprintf(s.out, "Config: %s\n", filepath.Join(s.orcaDir, "config.yaml"))
		return nil
	case line == "/help" || line == "help":
		printHelp()
		return nil
	default:
		return s.startGoal(ctx, line)
	}
}

// startGoal launches the goal control loop in a background goroutine.
// If a goal is already running it prints a message and returns nil.
func (s *Supervisor) startGoal(ctx context.Context, goalText string) error {
	s.goalMu.Lock()
	defer s.goalMu.Unlock()

	if s.goalActive.Load() {
		fmt.Fprintln(s.errout, "A goal is already running. Use /cancel to cancel it first.")
		return nil
	}

	active, err := s.rt.store.LoadActiveGoal(ctx)
	if err != nil {
		return fmt.Errorf("load active goal: %w", err)
	}
	if active != nil {
		cp, cpErr := s.rt.deriveCheckpoint(ctx, active)
		if cpErr == nil {
			s.rt.showActiveGoalResumePrompt(ctx, s.errout, active, cp)
			fmt.Fprintln(s.errout, "\nType /resume to continue, /cancel to cancel, or /status for details.")
		} else {
			fmt.Fprintln(s.errout, activeGoalError(active).Error())
		}
		return nil
	}

	goalCtx, cancel := context.WithCancel(ctx)
	s.goalCtx = goalCtx
	s.goalCancel = cancel
	s.goalActive.Store(true)
	s.cancelRequested.Store(false)

	fmt.Fprintf(s.errout, "[orca] starting: %s\n", goalText)

	go func() {
		defer func() {
			cancel()
			s.goalMu.Lock()
			s.goalActive.Store(false)
			s.cancelRequested.Store(false)
			s.goalCtx = nil
			s.goalCancel = nil
			s.goalMu.Unlock()
			fmt.Fprint(s.errout, "\n> ")
		}()

		if runErr := s.rt.runControlLoop(goalCtx, goalText); runErr != nil {
			if !errors.Is(runErr, context.Canceled) {
				fmt.Fprintf(s.errout, "[orca] error: %v\n", runErr)
			}
		} else {
			fmt.Fprintln(s.errout, "[orca] goal completed.")
		}
	}()

	return nil
}

// handleResume derives the checkpoint for the active goal and resumes the
// pipeline in the same background-goroutine pattern as startGoal.
func (s *Supervisor) handleResume(ctx context.Context) error {
	s.goalMu.Lock()
	alreadyActive := s.goalActive.Load()
	s.goalMu.Unlock()
	if alreadyActive {
		fmt.Fprintln(s.errout, "A goal is already running. Use /cancel to cancel it first.")
		return nil
	}

	goal, err := s.rt.store.LoadActiveGoal(ctx)
	if err != nil {
		return fmt.Errorf("resume: load active goal: %w", err)
	}
	if goal == nil {
		fmt.Fprintln(s.out, "No active goal to resume.")
		return nil
	}

	cp, err := s.rt.deriveCheckpoint(ctx, goal)
	if err != nil {
		return fmt.Errorf("resume: derive checkpoint: %w", err)
	}

	s.rt.showActiveGoalResumePrompt(ctx, s.out, goal, cp)

	s.goalMu.Lock()
	goalCtx, cancel := context.WithCancel(ctx)
	s.goalCtx = goalCtx
	s.goalCancel = cancel
	s.goalActive.Store(true)
	s.cancelRequested.Store(false)
	s.goalMu.Unlock()

	go func() {
		defer func() {
			cancel()
			s.goalMu.Lock()
			s.goalActive.Store(false)
			s.cancelRequested.Store(false)
			s.goalCtx = nil
			s.goalCancel = nil
			s.goalMu.Unlock()
			fmt.Fprint(s.errout, "\n> ")
		}()
		if runErr := s.rt.resumeFromCheckpoint(goalCtx, cp); runErr != nil {
			if !errors.Is(runErr, context.Canceled) {
				fmt.Fprintf(s.errout, "[orca] resume error: %v\n", runErr)
			}
		} else {
			fmt.Fprintln(s.errout, "[orca] goal resumed and completed.")
		}
	}()
	return nil
}

// handleCancel cancels the running goal context and updates the stored status.
// It bypasses the interactive confirmation prompt since the user already typed
// /cancel in the session.
func (s *Supervisor) handleCancel(ctx context.Context) error {
	s.goalMu.Lock()
	cancel := s.goalCancel
	active := s.goalActive.Load()
	s.goalMu.Unlock()

	if active && cancel != nil {
		cancel()
	}
	// Update the stored goal status. Pass a pre-filled reader so
	// cancelActiveGoal's confirmation prompt is answered automatically.
	return s.rt.cancelActiveGoal(ctx, strings.NewReader("cancel\n"), s.out)
}

func (s *Supervisor) cancelActiveGoal(ctx context.Context) error {
	s.goalMu.Lock()
	cancel := s.goalCancel
	active := s.goalActive.Load()
	s.goalMu.Unlock()

	if active && cancel != nil {
		cancel()
	}
	return s.rt.cancelActiveGoal(ctx, strings.NewReader("cancel\n"), s.out)
}

// handleSignal handles OS interrupt signals. The first signal cancels the active
// goal; the second exits the session.
func (s *Supervisor) handleSignal() {
	s.goalMu.Lock()
	active := s.goalActive.Load()
	cancel := s.goalCancel
	s.goalMu.Unlock()

	if active && cancel != nil && !s.cancelRequested.Swap(true) {
		fmt.Fprintln(s.errout, "\n[Cancelling active goal... press Ctrl+C again to force exit]")
		cancel()
		if err := s.cancelActiveGoal(context.Background()); err != nil {
			fmt.Fprintf(s.errout, "[orca] cancel: %v\n", err)
		}
		return
	}
	fmt.Fprintln(s.errout, "\nExiting.")
	s.stopOnce.Do(func() { close(s.stop) })
}

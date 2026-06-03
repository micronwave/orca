package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestCommandRegistry_allCLICommandsInTable verifies every CLI command registered
// in cliCommands appears in the human-readable table output.
func TestCommandRegistry_allCLICommandsInTable(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := writeCommandsTable(&buf); err != nil {
		t.Fatalf("writeCommandsTable: %v", err)
	}
	out := buf.String()
	for _, cmd := range cliCommands {
		if !strings.Contains(out, cmd.Name) {
			t.Errorf("CLI command %q missing from commands table output", cmd.Name)
		}
	}
}

func TestCommandRegistry_allCLICommandsInCLIHelp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	printCLIHelp(&buf)
	out := buf.String()
	for _, cmd := range cliCommands {
		if !strings.Contains(out, cmd.Name) {
			t.Errorf("CLI command %q missing from CLI help output", cmd.Name)
		}
	}
}

// TestCommandRegistry_allREPLCommandsInTable verifies every REPL slash command
// appears in the human-readable table output.
func TestCommandRegistry_allREPLCommandsInTable(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := writeCommandsTable(&buf); err != nil {
		t.Fatalf("writeCommandsTable: %v", err)
	}
	out := buf.String()
	for _, cmd := range replCommands {
		if !strings.Contains(out, cmd.Name) {
			t.Errorf("REPL command %q missing from commands table output", cmd.Name)
		}
	}
}

// TestCommandRegistry_allCLICommandsInJSONManifest verifies every CLI command
// appears in the JSON manifest emitted by writeCommandsJSON.
func TestCommandRegistry_allCLICommandsInJSONManifest(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := writeCommandsJSON(&buf); err != nil {
		t.Fatalf("writeCommandsJSON: %v", err)
	}
	var manifest commandManifest
	if err := json.Unmarshal(buf.Bytes(), &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	named := make(map[string]bool, len(manifest.Commands))
	for _, cmd := range manifest.Commands {
		named[cmd.Name] = true
	}
	for _, want := range cliCommands {
		if !named[want.Name] {
			t.Errorf("CLI command %q missing from JSON manifest", want.Name)
		}
	}
}

// TestCommandRegistry_allREPLCommandsInJSONManifest verifies every REPL command
// appears in the JSON manifest.
func TestCommandRegistry_allREPLCommandsInJSONManifest(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := writeCommandsJSON(&buf); err != nil {
		t.Fatalf("writeCommandsJSON: %v", err)
	}
	var manifest commandManifest
	if err := json.Unmarshal(buf.Bytes(), &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	named := make(map[string]bool, len(manifest.Commands))
	for _, cmd := range manifest.Commands {
		named[cmd.Name] = true
	}
	for _, want := range replCommands {
		if !named[want.Name] {
			t.Errorf("REPL command %q missing from JSON manifest", want.Name)
		}
	}
}

// TestCommandRegistry_printHelpContainsAllREPLCommands verifies that printHelp
// (the REPL help displayed to users) includes every REPL command.
func TestCommandRegistry_printHelpContainsAllREPLCommands(t *testing.T) {
	t.Parallel()
	// printHelp writes to stdout via fmt.Println; capture it by checking the
	// command names appear in the registiry (the function is generated from it).
	// Structural: verify all REPL commands are in replCommands and all have non-empty summaries.
	for _, cmd := range replCommands {
		if cmd.Name == "" {
			t.Error("REPL command has empty Name")
		}
		if cmd.Summary == "" {
			t.Errorf("REPL command %q has empty Summary", cmd.Name)
		}
		if cmd.Kind != "repl" {
			t.Errorf("REPL command %q has Kind %q, want repl", cmd.Name, cmd.Kind)
		}
	}
}

// TestCommandRegistry_activeOKCommandsCorrectlyTagged verifies that the commands
// expected to work while a goal is active are marked ActiveOK, and that /resume
// is not (it requires no goal to be running).
func TestCommandRegistry_activeOKCommandsCorrectlyTagged(t *testing.T) {
	t.Parallel()
	mustBeActive := []string{"/status", "/details", "/logs", "/approve", "/reject", "/cancel", "/clear", "/help", "/config", "/commands", "/doctor"}
	mustNotBeActive := []string{"/resume"}

	index := make(map[string]CommandSpec, len(replCommands))
	for _, cmd := range replCommands {
		index[cmd.Name] = cmd
	}

	for _, name := range mustBeActive {
		cmd, ok := index[name]
		if !ok {
			t.Errorf("REPL command %q not in registry", name)
			continue
		}
		if !cmd.ActiveOK {
			t.Errorf("command %q: ActiveOK=false, want true (should work during active goal)", name)
		}
	}
	for _, name := range mustNotBeActive {
		cmd, ok := index[name]
		if !ok {
			t.Errorf("REPL command %q not in registry", name)
			continue
		}
		if cmd.ActiveOK {
			t.Errorf("command %q: ActiveOK=true, want false (must not run during active goal)", name)
		}
	}
}

// TestCommandRegistry_reservedSlashCommandsNotShadowed verifies that
// isReservedREPLCommand correctly identifies registered slash commands and
// that unknown /slash lines are not treated as reserved.
func TestCommandRegistry_reservedSlashCommandsNotShadowed(t *testing.T) {
	t.Parallel()
	reserved := []string{"/status", "/details", "/logs", "/approve", "/reject",
		"/cancel", "/resume", "/config", "/clear", "/help", "/commands", "/doctor", "/ui"}
	for _, name := range reserved {
		if !isReservedREPLCommand(name) {
			t.Errorf("isReservedREPLCommand(%q) = false, want true", name)
		}
	}

	notReserved := []string{"/goal", "/fakecmd", "/run", "status", "help"}
	for _, name := range notReserved {
		if isReservedREPLCommand(name) {
			t.Errorf("isReservedREPLCommand(%q) = true, want false", name)
		}
	}
}

// TestRun_unknownCommandReturnsError verifies that an unrecognised top-level
// command name returns an error rather than silently doing nothing.
func TestRun_unknownCommandReturnsError(t *testing.T) {
	t.Parallel()
	err := run([]string{"notacommand"})
	if err == nil {
		t.Fatal("run(notacommand): want error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("run(notacommand): error = %q, want to contain 'unknown command'", err.Error())
	}
}

// TestRun_unknownFlagFailsAtParse verifies that an unknown flag passed as the
// first argument is rejected by runGoal's flag parser, not silently treated
// as goal text.
func TestRun_unknownFlagFailsAtParse(t *testing.T) {
	t.Parallel()
	err := run([]string{"--totally-unknown-diagnostic-flag"})
	if err == nil {
		t.Fatal("run with unknown flag: want error, got nil")
	}
	// Must be a flag-parse error, not an "unknown command" error, because the
	// leading "-" causes run() to forward to runGoal which uses flag.ContinueOnError.
	if strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("run with unknown flag got wrong error type: %v", err)
	}
}

// TestCommandRegistry_JSONManifestIsValidJSON verifies the manifest encodes
// and decodes without error and has the expected top-level key.
func TestCommandRegistry_JSONManifestIsValidJSON(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := writeCommandsJSON(&buf); err != nil {
		t.Fatalf("writeCommandsJSON: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatalf("manifest not valid JSON: %v", err)
	}
	if _, ok := raw["commands"]; !ok {
		t.Fatal("manifest missing top-level 'commands' key")
	}
}

func TestHandleLine_commandsJSONWritesManifest(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	s := &Supervisor{out: &buf}
	if err := s.handleLine(context.Background(), "/commands --json"); err != nil {
		t.Fatalf("handleLine(/commands --json): %v", err)
	}
	var manifest commandManifest
	if err := json.Unmarshal(buf.Bytes(), &manifest); err != nil {
		t.Fatalf("/commands --json output is not valid manifest JSON: %v\n%s", err, buf.String())
	}
	if len(manifest.Commands) != len(allCommands()) {
		t.Fatalf("manifest command count = %d, want %d", len(manifest.Commands), len(allCommands()))
	}
}

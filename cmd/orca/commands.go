package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// CommandSpec describes a single CLI top-level command or REPL slash command.
// The registry is the single source of truth for help text, JSON manifests, and
// command validation — dispatch logic remains in run() and handleLine().
type CommandSpec struct {
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`              // "cli" or "repl"
	Aliases    []string `json:"aliases,omitempty"` // alternate names recognized by the dispatcher
	Summary    string   `json:"summary"`
	ArgHint    string   `json:"arg_hint,omitempty"` // e.g. "[--orca-dir dir]"
	ActiveOK   bool     `json:"active_ok"`          // may run while a goal is executing
	ResumeCtx  bool     `json:"resume_ctx,omitempty"`
	JSONOutput bool     `json:"json_output,omitempty"`
	NeedsInit  bool     `json:"needs_init,omitempty"` // requires .orca/ to exist
}

// cliCommands is the registry of top-level orca <cmd> commands.
var cliCommands = []CommandSpec{
	{
		Name:    "init",
		Kind:    "cli",
		Summary: "initialize the .orca directory for a project",
		ArgHint: "[--orca-dir dir]",
	},
	{
		Name:       "goal",
		Kind:       "cli",
		Summary:    "start a new goal",
		ArgHint:    "[--goal text] [--from-issue N] [--plain|--verbose|--json]",
		NeedsInit:  true,
		JSONOutput: true,
	},
	{
		Name:      "status",
		Kind:      "cli",
		Summary:   "show active goal status",
		ArgHint:   "[--raw]",
		NeedsInit: true,
		ActiveOK:  true,
	},
	{
		Name:      "cancel",
		Kind:      "cli",
		Summary:   "cancel the active goal",
		NeedsInit: true,
	},
	{
		Name:      "resume",
		Kind:      "cli",
		Summary:   "resume a paused or cancelled goal",
		NeedsInit: true,
		ResumeCtx: true,
	},
	{
		Name:      "ci",
		Kind:      "cli",
		Summary:   "CI integration subcommands (ci wait)",
		ArgHint:   "wait [--timeout N]",
		NeedsInit: true,
	},
	{
		Name:      "ui",
		Kind:      "cli",
		Summary:   "launch the Orca desktop UI",
		NeedsInit: true,
	},
	{
		Name:    "doctor",
		Kind:    "cli",
		Summary: "run environment diagnostics",
		ArgHint: "[--orca-dir dir]",
	},
	{
		Name:       "commands",
		Kind:       "cli",
		Summary:    "list available commands and their metadata",
		ArgHint:    "[--json]",
		ActiveOK:   true,
		JSONOutput: true,
	},
}

// replCommands is the registry of REPL slash commands recognized by the supervisor.
var replCommands = []CommandSpec{
	{
		Name:     "/status",
		Kind:     "repl",
		Aliases:  []string{"status"},
		Summary:  "show active goal status (concise)",
		ActiveOK: true,
	},
	{
		Name:     "/details",
		Kind:     "repl",
		Summary:  "show active goal status (full/raw)",
		ActiveOK: true,
	},
	{
		Name:     "/logs",
		Kind:     "repl",
		Summary:  "show agent or verifier logs",
		ActiveOK: true,
	},
	{
		Name:     "/approve",
		Kind:     "repl",
		Summary:  "approve the current waiting gate",
		ActiveOK: true,
	},
	{
		Name:     "/reject",
		Kind:     "repl",
		Summary:  "reject the current waiting gate",
		ActiveOK: true,
	},
	{
		Name:     "/cancel",
		Kind:     "repl",
		Aliases:  []string{"cancel"},
		Summary:  "cancel the active goal",
		ActiveOK: true,
	},
	{
		Name:      "/resume",
		Kind:      "repl",
		Summary:   "resume a paused goal",
		ActiveOK:  false,
		ResumeCtx: true,
	},
	{
		Name:     "/config",
		Kind:     "repl",
		Summary:  "show config file path",
		ActiveOK: true,
	},
	{
		Name:     "/help",
		Kind:     "repl",
		Aliases:  []string{"help"},
		Summary:  "show command help",
		ActiveOK: true,
	},
	{
		Name:       "/commands",
		Kind:       "repl",
		Summary:    "list available commands",
		ArgHint:    "[--json]",
		ActiveOK:   true,
		JSONOutput: true,
	},
	{
		Name:     "/doctor",
		Kind:     "repl",
		Aliases:  []string{"doctor", "orca doctor"},
		Summary:  "run environment diagnostics",
		ActiveOK: true,
	},
	{
		Name:    "/ui",
		Kind:    "repl",
		Aliases: []string{"ui", "orca ui"},
		Summary: "launch the Orca desktop UI",
	},
}

// commandManifest is the JSON-serialisable form of the full command manifest.
type commandManifest struct {
	Commands []CommandSpec `json:"commands"`
}

// allCommands returns the combined CLI and REPL command registry.
func allCommands() []CommandSpec {
	out := make([]CommandSpec, 0, len(cliCommands)+len(replCommands))
	out = append(out, cliCommands...)
	out = append(out, replCommands...)
	return out
}

// isReservedREPLCommand reports whether line is a registered slash command
// (canonical name or alias). Used by handleLine to guard the default case.
func isReservedREPLCommand(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "/") {
		return false
	}
	for _, spec := range replCommands {
		if spec.Name == trimmed {
			return true
		}
		for _, alias := range spec.Aliases {
			if alias == trimmed {
				return true
			}
		}
	}
	return false
}

// printHelp writes the REPL interactive-session help to stderr.
// It is generated from replCommands so the registry stays in sync.
func printHelp() {
	fmt.Println("Commands:")
	fmt.Println("  <goal text>   start a new goal (runs in background)")
	for _, cmd := range replCommands {
		name := cmd.Name
		if cmd.ArgHint != "" {
			name = name + " " + cmd.ArgHint
		}
		fmt.Printf("  %-22s%s\n", name, cmd.Summary)
	}
	fmt.Println("  exit / quit   exit orca")
}

// printCLIHelp writes the top-level orca CLI help to w.
func printCLIHelp(w io.Writer) {
	fmt.Fprintln(w, "Usage: orca <command> [args]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Commands:")
	for _, cmd := range cliCommands {
		hint := ""
		if cmd.ArgHint != "" {
			hint = " " + cmd.ArgHint
		}
		fmt.Fprintf(w, "  %-10s%s%s\n", cmd.Name, hint, cmd.Summary)
	}
}

// writeCommandsJSON writes the full command manifest as JSON to w.
func writeCommandsJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(commandManifest{Commands: allCommands()})
}

// writeCommandsTable writes a human-readable command table to w.
func writeCommandsTable(w io.Writer) error {
	fmt.Fprintln(w, "CLI commands (orca <cmd>):")
	for _, cmd := range cliCommands {
		hint := ""
		if cmd.ArgHint != "" {
			hint = " " + cmd.ArgHint
		}
		active := ""
		if cmd.ActiveOK {
			active = " [active-ok]"
		}
		fmt.Fprintf(w, "  %-12s%-30s%s%s\n", cmd.Name, hint, cmd.Summary, active)
	}
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "REPL slash commands (/cmd):")
	for _, cmd := range replCommands {
		hint := ""
		if cmd.ArgHint != "" {
			hint = " " + cmd.ArgHint
		}
		active := ""
		if cmd.ActiveOK {
			active = " [active-ok]"
		}
		fmt.Fprintf(w, "  %-14s%-28s%s%s\n", cmd.Name, hint, cmd.Summary, active)
	}
	return nil
}

// runCommands handles the `orca commands [--json]` subcommand.
func runCommands(args []string) error {
	fs := flag.NewFlagSet("orca commands", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "emit command manifest as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *jsonOut {
		return writeCommandsJSON(os.Stdout)
	}
	return writeCommandsTable(os.Stdout)
}

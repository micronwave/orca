package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/micronwave/orca/internal/ui"
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
		Name:    "update",
		Kind:    "cli",
		Summary: "rebuild and replace the current orca binary from local source",
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
		Name:     "/clear",
		Kind:     "repl",
		Aliases:  []string{"clear"},
		Summary:  "clear the visible REPL session",
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
	fmt.Println(ui.Colorize(os.Stdout, ui.Bold, "Interactive Commands:"))
	fmt.Printf("  %s%s%s\n",
		ui.Colorize(os.Stdout, ui.Cyan, "<goal text>"),
		strings.Repeat(" ", max(0, 22-len("<goal text>"))),
		"start a new goal")
	for _, cmd := range replCommands {
		nameStr := cmd.Name
		if cmd.ArgHint != "" {
			nameStr += " " + cmd.ArgHint
		}
		coloredName := ui.Colorize(os.Stdout, ui.Cyan, cmd.Name)
		if cmd.ArgHint != "" {
			coloredName += " " + ui.Colorize(os.Stdout, ui.Black+ui.Bold, cmd.ArgHint)
		}
		pad := strings.Repeat(" ", max(0, 32-len(nameStr)))
		fmt.Printf("  %s%s%s\n", coloredName, pad, cmd.Summary)
	}
	fmt.Printf("  %s%s%s\n",
		ui.Colorize(os.Stdout, ui.Cyan, "exit / quit"),
		strings.Repeat(" ", max(0, 32-len("exit / quit"))),
		"exit orca")
}

// printCLIHelp writes the top-level orca CLI help to w.
func printCLIHelp(w io.Writer) {
	fmt.Fprintf(w, "%s %s orca <command> [args]\n", ui.IconOrca, ui.Colorize(w, ui.Bold, "Usage:"))
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, ui.Colorize(w, ui.Bold, "Commands:"))
	for _, cmd := range cliCommands {
		hintPlain := ""
		hintColored := ""
		if cmd.ArgHint != "" {
			hintPlain = " " + cmd.ArgHint
			hintColored = " " + ui.Colorize(w, ui.Black+ui.Bold, cmd.ArgHint)
		}
		pad := strings.Repeat(" ", max(0, 20-len(cmd.Name+hintPlain)))
		fmt.Fprintf(w, "  %s%s%s%s\n",
			ui.Colorize(w, ui.Cyan, cmd.Name), hintColored, pad, cmd.Summary)
	}
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "Run %s for detailed environment health.\n", ui.Colorize(w, ui.Cyan, "orca doctor"))
}

// writeCommandsJSON writes the full command manifest as JSON to w.
func writeCommandsJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(commandManifest{Commands: allCommands()})
}

// writeCommandsTable writes a human-readable command table to w.
func writeCommandsTable(w io.Writer) error {
	fmt.Fprintln(w, ui.Colorize(w, ui.Bold, "CLI commands (orca <cmd>):"))
	for _, cmd := range cliCommands {
		hint := ""
		if cmd.ArgHint != "" {
			hint = " " + cmd.ArgHint
		}
		active := ""
		if cmd.ActiveOK {
			active = ui.Colorize(w, ui.Green, " [active-ok]")
		}
		coloredName := ui.Colorize(w, ui.Cyan, fmt.Sprintf("%-12s", cmd.Name))
		coloredHint := ui.Colorize(w, ui.Black+ui.Bold, fmt.Sprintf("%-30s", hint))
		fmt.Fprintf(w, "  %s%s%s%s\n", coloredName, coloredHint, cmd.Summary, active)
	}
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, ui.Colorize(w, ui.Bold, "REPL slash commands (/cmd):"))
	for _, cmd := range replCommands {
		hint := ""
		if cmd.ArgHint != "" {
			hint = " " + cmd.ArgHint
		}
		active := ""
		if cmd.ActiveOK {
			active = ui.Colorize(w, ui.Green, " [active-ok]")
		}
		coloredName := ui.Colorize(w, ui.Cyan, fmt.Sprintf("%-14s", cmd.Name))
		coloredHint := ui.Colorize(w, ui.Black+ui.Bold, fmt.Sprintf("%-28s", hint))
		fmt.Fprintf(w, "  %s%s%s%s\n", coloredName, coloredHint, cmd.Summary, active)
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

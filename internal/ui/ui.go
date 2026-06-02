package ui

import (
	"fmt"
	"io"
	"os"
	"runtime"

	"golang.org/x/term"
)

// ─── Theme Colors ────────────────────────────────────────────────────────────

const (
	Reset  = "\x1b[0m"
	Bold   = "\x1b[1m"
	Black  = "\x1b[30m"
	Red    = "\x1b[31m"
	Green  = "\x1b[32m"
	Yellow = "\x1b[33m"
	Blue   = "\x1b[34m"
	Cyan   = "\x1b[36m"
	White  = "\x1b[37m"

	// Orca Theme Aliases
	OrcaBlue  = Cyan
	OrcaWhite = White + Bold
)

// ─── Emojis & Icons ──────────────────────────────────────────────────────────

const (
	IconOrca      = "≋"
	IconPackage   = "❒"
	IconCheck     = "✓"
	IconCross     = "✗"
	IconWarning   = "⚠"
	IconRocket    = "✦"
	IconHammer    = "⚙"
	IconClipboard = "≡"
	IconFolder    = "▸"
	IconFile      = "▫"
	IconStep      = "›"
)

// ─── ASCII Art ───────────────────────────────────────────────────────────────

const Banner = `
   .            _
  - _ _  _  ( ) _
 - ( _ )( _ )( _ )| |( _ )
-  (_ ) | | |  _/| || _ |
 - ( _ ) |_| | _ | |_|( _ )
  -  -   -   -   -   -
`

// PrintBanner writes the Orca ASCII art to the given writer.
func PrintBanner(w io.Writer) {
	if UseColor(w) {
		fmt.Fprintf(w, "%s%s%s", Cyan, Banner, Reset)
	} else {
		fmt.Fprintln(w, "--- ORCA ---")
	}
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// UseColor returns true if the writer is a TTY and colors are not disabled.
func UseColor(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	if f, ok := w.(*os.File); ok {
		return term.IsTerminal(int(f.Fd()))
	}
	return false
}

// Colorize returns s wrapped in the given ANSI code if w is a TTY.
func Colorize(w io.Writer, code, s string) string {
	if UseColor(w) {
		return code + s + Reset
	}
	return s
}

// Error returns a formatted error string with a red prefix and cross icon.
func Error(msg string) string {
	return fmt.Sprintf("%s %serror:%s %s", IconCross, Red+Bold, Reset, msg)
}

// Success returns a formatted success string with a green check icon.
func Success(msg string) string {
	return fmt.Sprintf("%s %s", IconCheck, msg)
}

// Info returns a formatted info string with the Orca blue color.
func Info(w io.Writer, msg string) string {
	return Colorize(w, OrcaBlue, msg)
}

// TreePrefix returns the appropriate tree branch characters.
func TreePrefix(isLast bool) string {
	if isLast {
		return "└── "
	}
	return "├── "
}

// IsWindows returns true if the current OS is Windows.
func IsWindows() bool {
	return runtime.GOOS == "windows"
}

// ClearLine returns the ANSI code to clear the current line.
func ClearLine() string {
	return "\r\x1b[K"
}

// MoveUp returns the ANSI code to move the cursor up n lines.
func MoveUp(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("\x1b[%dA", n)
}

// ReplaceLine clears the current line and writes the new content.
func ReplaceLine(w io.Writer, content string) {
	fmt.Fprintf(w, "%s%s", ClearLine(), content)
}

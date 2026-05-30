# Orca 

Orca is a local runtime that turns coding goals into verified patches. You give it a goal, it wires the right agents together, runs your test suite against the result, and hands you proof that implementation was accurate. Gone are the days of "trust me bro, it works"

```text
$ orca goal "refactor the storage layer to use SQLite"

[COMPILING]  Goal -> 4 checkable obligations
[PLANNING]   Selected Topology: implementer_reviewer
[CAPSULE]    Starting CAP-1 (executor: claude-3-5-sonnet)
             - Allowed: internal/store/*.go
             - Gate: 'go test ./internal/store/...'
[RUNNING]    Agent is modifying 3 files...
[SUCCESS]    CAP-1 finished. Patch created.
[CAPSULE]    Starting CAP-2 (reviewer: gpt-4-codex)
             - Task: Review PATCH-1 for migration risks.
[SUCCESS]    CAP-2 finished. No risks found.
[VERIFYING]  Running final gates...
             - static_check: PASS
             - unit_tests:   PASS

[RESULT]     Merge Recommended. 
             Evidence: 12 test results attached to PATCH-1.
```

---

## Quickstart

### 1. Install

#### For end users

```bash
# macOS/Linux:
curl -fsSL https://raw.githubusercontent.com/micronwave/orca/main/install.sh | sh

# Windows (PowerShell):
iwr https://raw.githubusercontent.com/micronwave/orca/main/install.ps1 | iex
```

#### For contributors

```bash
go install ./cmd/orca
```

### 2. Initialize a Repository (optional)

> [!TIP]
> `orca goal` auto-initializes `.orca/` on first run — you only need this step if you want to inspect or edit `config.yaml` before delegating anything.

```bash
orca init
```
This creates a `.orca/config.yaml` pre-populated for your project type (`go.mod`, `package.json`, `pom.xml`, etc.). Open it and adjust the verifier gates if needed.

### 3. Delegate a Goal

You can provide a goal directly, pull from a GitHub issue, or use the interactive REPL.
```bash
# Direct delegation
orca goal "add a new endpoint to the API that returns the current system load"

# From a GitHub issue (requires GITHUB_TOKEN and intake.repo config)
orca goal --from-issue 42
```

> [!NOTE]
> Run `orca` with no arguments to drop into an interactive session. Auto-initializes `.orca/` on first use.
```text
$ orca
Orca  local proof runtime
Working directory: /my-project

> add a new endpoint to the API that returns the current system load
[COMPILING] ...

> /status
> /cancel
> /help
> exit
```

### 4. Monitor and Control

Orca runs in a loop. You can check the state at any time from another terminal.
```bash
# See what's running, what's blocked, and current budget spend
orca status

# Stop everything and cleanup worktrees
orca cancel

# Open the desktop UI (requires orca-desktop to be installed)
orca ui
```

---

## How it works

Orca treats agents like contractors: give it a goal and it handles the "how".

*   **Multi-Provider Wiring:** It automatically delegates steps to the right model. Maybe Claude handles the implementation while Codex reviews the risk.
*   **Obligations, Not Prompts:** Orca defines what "done" looks like (e.g., "Tests in `internal/reconciler` must pass") before any code is written.
*   **Execution Capsules:** Each agent run happens in a cage. We give it the exact files it needs (and nothing else), a token budget, and a set of gates it must pass to exit.
*   **Context Projections:** Instead of replaying 50kb of chat history, Orca compiles a fresh briefing for each step. It's faster, cheaper, and keeps the agent from wandering.

---

## How it's built

Orca isn't a wrapper, but a Go-based supervisor that manages a durable **Artifact Graph**.

1.  **The Event Log:** Everything that happens is saved to `events.log`. If your computer crashes or the API goes down, Orca picks up where it left off.
2.  **The Store:** All patches, test results, and "claims" (things the agent discovered) are stored as JSON files in `.orca/`.
3.  **The Reconciler:** This is the brain. It looks at the evidence (test logs, lint output) and matches it against the obligations. If the evidence doesn't match, the patch is rejected.

---

## Comparison

| Feature | The "Chat" Way | The Orca Way |
| :--- | :--- | :--- |
| **Workflow** | You chat, you check, you merge. | You set a goal. Orca proves it. |
| **Context** | Replays entire transcripts (expensive). | Compiles minimal "projections" (cheap). |
| **Verification** | "The agent said it passed." | "Here are the 12 signed test logs." |
| **Multi-Agent** | You copy-paste between windows. | Wired together automatically. |

---

## Supported Drivers

Orca is model-agnostic. Out of the box, it supports:
*   **Claude Code** (via `claude` CLI)
*   **Codex** (via `codex` CLI)
*   **Remote Adapters** (Any MCP-compatible endpoint)

Configure these in `.orca/config.yaml` by pointing to their paths, or ensure they are in your `$PATH`. For GitHub integration, set your `GITHUB_TOKEN` in your environment.

---

[MIT License](LICENSE)

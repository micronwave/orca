# Orca

Orca closes the gap between what agents SAY and what they actually DID. It defines what "done" looks like *before* any agent runs, holds the work to that standard, and only recommends merge when the evidence is there.

Give it a goal. Orca breaks it into checkable steps, delegates to the right agents, runs your gates, and hands you a merge recommendation attached to real artifacts. Not just "the agent said it passed."

```text
$ orca goal "refactor the storage layer to use SQLite"

[COMPILING]  Goal -> 4 checkable obligations
[PLANNING]   Selected Topology: implementer_reviewer
[CAPSULE]    Starting CAP-1 (executor: claude-sonnet-4-6)
             - Allowed: internal/store/*.go
             - Gate: 'go test ./internal/store/...'
[RUNNING]    Agent is modifying 3 files...
[SUCCESS]    CAP-1 finished. Patch created.
[CAPSULE]    Starting CAP-2 (reviewer: codex)
             - Task: Review PATCH-1 for migration risks.
[SUCCESS]    CAP-2 finished. No risks found.
[VERIFYING]  Running final gates...
             - static_check: PASS
             - unit_tests:   PASS

[RESULT]     Merge Recommended. 
             Evidence: 12 test results attached to PATCH-1.
```

---

## How it's different

| Feature | The "Chat" Way | The Orca Way |
| :--- | :--- | :--- |
| **Workflow** | You chat, you check, you merge. | You set a goal. Orca proves it. |
| **Context** | Replays entire transcripts (expensive). | Compiles minimal "projections" (cheap). |
| **Verification** | "The agent said it passed." | "Here are the 12 signed test logs." |
| **Multi-Agent** | You copy-paste between windows. | Wired together automatically. |
| **Crash recovery** | Start over. | Resume from the last checkpoint. |

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

After initializing, run `orca doctor` to confirm your environment is ready.

### 3. Delegate a Goal

You can provide a goal directly, pull from a GitHub issue, or use the interactive REPL.
```bash
# Direct delegation
orca goal "add a new endpoint to the API that returns the current system load"

# From a GitHub issue (requires GITHUB_TOKEN and intake.repo config)
orca goal --from-issue 42

# Output modes (auto-detected on TTY; override with a flag)
orca goal --plain "..."    # plain text, no ANSI
orca goal --verbose "..."  # plain text with extra detail
orca goal --json "..."     # newline-delimited JSON on stderr
```

> [!NOTE]
> Run `orca` with no arguments to drop into an interactive session. Auto-initializes `.orca/` on first use.
```text
$ orca
Orca  local proof runtime
Working directory: /my-project

> add a new endpoint to the API that returns the current system load
[COMPILING] ...

> /status         show active goal status
> /details        show full status dump
> /logs           show agent or verifier logs
> /approve        approve the current waiting gate
> /reject         reject the current waiting gate
> /cancel         cancel the active goal
> /resume         resume a paused goal
> /doctor         run environment diagnostics
> /clear          clear the visible session
> /help           show all commands
> exit
```

### 4. Monitor and Control

Orca runs in a loop. You can check the state at any time from another terminal.
```bash
# See what's running, what's blocked, and current budget spend
orca status

# Full operational dump (artifact IDs, budget, CI state)
orca status --raw

# Resume a goal after a crash or cancellation
orca resume

# Cancel the active goal and clean up worktrees
orca cancel

# Check environment health (adapters, config, gates)
orca doctor

# Open the desktop UI (requires orca-desktop to be installed)
orca ui
```

---

## How it works

Orca treats agents like contractors: give it a goal and it handles the "how".

*   **Obligations, Not Prompts:** Before any agent runs, Orca defines what "done" looks like — specific, checkable conditions like "tests in `internal/reconciler` must pass." Agents work against a contract, not an open-ended instruction.
*   **Execution Capsules:** Each agent run is isolated. It gets exactly the files it needs, a token budget, and a set of gates it must pass before its patch is accepted.
*   **Context Projections:** Instead of replaying 50kb of chat history, Orca compiles a fresh, role-specific briefing for each step. Faster, cheaper, and it keeps agents on task.
*   **Multi-Provider Wiring:** Orca routes work to the right model automatically. Claude handles the implementation, Codex reviews the risk — no copy-pasting between windows.
*   **Crash Recovery:** Every step is saved to the event log. If your process dies mid-run, `orca resume` picks up from the last checkpoint with no loss.
*   **Lifecycle Hooks:** Configure `pre_capsule` and `post_verify` hooks in `config.yaml` to inject your own checks at capsule boundaries. Hooks return a structured JSON result (`allow`, `deny`, `ask`, `attach_evidence`) that Orca stores as evidence or a gate decision.

---

## How it's built

Orca is a Go-based supervisor that manages a durable artifact graph — not a wrapper around a chat API.

1.  **The Event Log:** Everything that happens is saved to `events.log`. If your computer crashes or the API goes down, Orca picks up where it left off.
2.  **The Store:** All patches, test results, and "claims" (things the agent discovered) are stored as typed JSON artifacts in `.orca/`. Claims stay unverified until evidence backs them up.
3.  **The Reconciler:** It looks at the evidence (test logs, lint output) and checks it against the obligations. If the evidence doesn't match, the patch is rejected — not "flagged for review," rejected.

---

## Integrations

### CI (GitHub Actions)

Set `ci.provider: github_actions` in `config.yaml`. After each patch, Orca polls the Actions run on the capsule's branch and treats a failed run as a blocking gate. Requires `GITHUB_TOKEN` and `intake.repo`.

```bash
# Wait for CI manually (also used internally by the verifier)
orca ci wait --timeout 600
```

### Pull Requests

Set `pr.enabled: true` in `config.yaml`. After a human gate approves a merge, Orca opens a GitHub PR with the goal, obligations addressed, and evidence summary pre-filled in the body. Requires `GITHUB_TOKEN` and `intake.repo`.

### MCP Server

Set `mcp.enabled: true` to start a read-only JSON-RPC MCP server (default `127.0.0.1:7070`). External clients can query goals, obligations, patches, evidence, and events through it.

---

## Supported Drivers

Orca is model-agnostic. Out of the box, it supports:
*   **Claude Code** (via `claude` CLI)
*   **Codex** (via `codex` CLI)
*   **Remote Adapters** (Any MCP-compatible endpoint)

Configure these in `.orca/config.yaml` by pointing to their paths, or ensure they are in your `$PATH`. For GitHub integration, set your `GITHUB_TOKEN` in your environment.

---

[MIT License](LICENSE)

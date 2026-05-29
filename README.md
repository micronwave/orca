# Orca

**Stop babysitting your agents and let them prove their work.**

Orca is a local runtime that turns vague coding goals into checkable work. It wires multiple AI providers (Claude, Codex, etc.) into a single execution loop so you can delegate a massive task and walk away, knowing exactly what was proven when you get back.

No hopping between five different CLI chats or pasting diffs.

---

## The Problem: "Agent Babysitting"

Current agent tools are pretty decent-ish at writing code, but they're terrible at proving it works. You spend half your day:
1. Launching an agent in one terminal.
2. Checking its diff in another.
3. Realizing it hallucinated a test pass.
4. Manually syncing context to a *different* agent because the first one got "confused."

The unit of work should be a **proven patch** and not a "trust me bro". 

---

## The Orca Proof Patches

Orca treats agents like contractors, give it a goal and Orca handles the "how":

*   **Multi-Provider Wiring:** It automatically delegates steps to the right model. Maybe Claude handles the implementation while Codex reviews the risk.
*   **Obligations, Not Prompts:** Orca defines what "done" looks like (e.g., "Tests in `internal/reconciler` must pass") before any code is written.
*   **Execution Capsules:** Each agent run happens in a cage. We give it the exact files it needs (and nothing else), a token budget, and a set of gates it must pass to exit.
*   **Context Projections:** Instead of replaying 50kb of chat history, Orca "compiles" a fresh briefing for each step. It’s faster, cheaper, and keeps the agent from wandering.

---

## What it looks like

When you run Orca, you're watching a state machine advance.

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

### 1. Build and Install

**macOS / Linux:**
```bash
go build -o orca ./cmd/orca
sudo mv orca /usr/local/bin/
```

**Windows (PowerShell):**
```powershell
go build -o orca.exe ./cmd/orca
# To run from anywhere, add the folder to your PATH:
$env:Path += ";$(Get-Location)" 
```
*Note: To make it permanent on Windows, add the directory containing `orca.exe` to your "System Environment Variables".*

### 2. Initialize a Repository (optional)
Running `orca goal` will auto-initialize `.orca/` for you, detecting your project type (`go.mod`, `package.json`, `pom.xml`) and writing sensible gate defaults. You only need to run `orca init` explicitly if you want to inspect or customize `config.yaml` before the first run.

```bash
# If orca is in your PATH:
orca init

# If not (Windows example):
E:\orca\orca.exe init
```
This creates a `.orca/config.yaml` pre-populated for your project type. Open it and adjust the verifier gates if needed.

### 3. Delegate a Goal
You can provide a goal directly, pull from a GitHub issue, or use the interactive REPL.
```bash
# Option A: Direct delegation
orca goal "add a new endpoint to the API that returns the current system load"

# Option B: From an issue (requires GITHUB_TOKEN and intake.repo config)
orca goal --from-issue 42
```

**Option C: Interactive REPL** — run `orca` with no arguments for a prompt-driven session. Auto-initializes `.orca/` on first use.
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

Orca is model-agnostic. Out of the box, we support:
*   **Claude Code** (via `claude` CLI)
*   **Codex** (via `codex` CLI)
*   **Remote Adapters** (Any MCP-compatible endpoint)

Configure these in `.orca/config.yaml` by pointing to their paths, or ensure they are in your `$PATH`. For GitHub integration, set your `GITHUB_TOKEN` in your environment.

---

[MIT License](LICENSE)

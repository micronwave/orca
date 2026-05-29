# Orca

**Stop babysitting your agents. Let them prove their work.**

Orca is a local runtime that turns vague coding goals into checkable work. It wires multiple AI providers (Claude, Codex, etc.) into a single execution loop so you can delegate a massive task and walk away, knowing exactly what was proven when you get back.

No more hopping between five different CLI chats. No more copy-pasting diffs. No more "trust me, I ran the tests" from an LLM.

---

## The Problem: "Agent Babysitting"

Current agent tools are great at writing code, but they're terrible at proving it works. You spend half your day:
1. Launching an agent in one terminal.
2. Checking its diff in another.
3. Realizing it hallucinated a test pass.
4. Manually syncing context to a *different* agent because the first one got "confused."

It’s exhausting. The unit of work shouldn't be a chat transcript; it should be a **proven patch**.

---

## The Orca Way: Proof-Carrying Patches

Orca treats agents like contractors, not chat buddies. You give it a goal, and Orca handles the "how":

*   **Multi-Provider Wiring:** It automatically delegates steps to the right model. Maybe Claude handles the implementation while Codex reviews the risk. You just see the result.
*   **Obligations, Not Prompts:** Orca defines what "done" looks like (e.g., "Tests in `internal/reconciler` must pass") before any code is written.
*   **Execution Capsules:** Each agent run happens in a cage. We give it the exact files it needs (and nothing else), a token budget, and a set of gates it must pass to exit.
*   **Context Projections:** Instead of replaying 50kb of chat history, Orca "compiles" a fresh briefing for each step. It’s faster, cheaper, and keeps the agent from wandering off-track.

---

## What it looks like

When you run Orca, you aren't just watching text stream by. You're watching a state machine advance.

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

## How it's built (The plain English version)

Orca isn't a "wrapper." It's a Go-based supervisor that manages a durable **Artifact Graph**.

1.  **The Event Log:** Everything that happens is saved to `events.log`. If your computer crashes or the API goes down, Orca just picks up where it left off.
2.  **The Store:** All patches, test results, and "claims" (things the agent discovered) are stored as JSON files in `.orca/`.
3.  **The Reconciler:** This is the brain. It looks at the evidence (test logs, lint output) and matches it against the obligations. If the evidence doesn't match, the patch is rejected. Simple as that.

---

## Comparison

| Feature | The "Chat" Way | The Orca Way |
| :--- | :--- | :--- |
| **Workflow** | You chat, you check, you merge. | You set a goal. Orca proves it. |
| **Context** | Replays entire transcripts (expensive). | Compiles minimal "projections" (cheap). |
| **Verification** | "The agent said it passed." | "Here are the 12 signed test logs." |
| **Multi-Agent** | You copy-paste between windows. | Wired together automatically. |

---

## Quickstart

### 1. Install
```bash
go build -o orca ./cmd/orca
# Put it in your path
```

### 2. Init
```bash
orca init
# Creates the .orca/ directory and local config
```

### 3. Delegate
```bash
orca goal "fix the race condition in the event log"
```

---

## Supported Drivers

Orca is model-agnostic. Out of the box, we support:
*   **Claude Code** (via `claude` CLI)
*   **GPT-4 / Codex** (via `codex` CLI)
*   **Remote Adapters** (Any MCP-compatible endpoint)

---

## The "Manual"

We're building this because we want to stop writing "prompt engineering" and start writing **contracts**. 

Orca is currently CLI-first and opinionated. It expects you to have a Go environment (for now) and it expects your code to have some form of automated testing it can hook into. If you don't have tests, Orca will make "identifying a reproduction case" its first obligation.

---

[MIT License](LICENSE)

Deep research required repository write access and was denied, so I analyzed `generalaction/emdash` directly with repository contents, file reads, and code search instead.

Here’s the comprehensive top-to-bottom map of the emdash pipeline as implemented in the repo today.

**Important note:** the code-search tool returns only a limited set of matches, so some search-derived findings may be incomplete. For broader code-search coverage in GitHub UI, use:
- `https://github.com/search?q=repo%3Ageneralaction%2Femdash&type=code`

## Executive summary

Emdash is an **Electron desktop ADE** that orchestrates **multiple agent CLIs in parallel**, each typically bound to its own **git worktree**, with support for **local** and **SSH-hosted** workspaces. The overall flow is:

1. **Electron boot** initializes app services, DB, telemetry, search indexes, dependency probing, updater, and RPC.
2. **Renderer boot** initializes Monaco/editor infrastructure, cached view state, and app stores.
3. **User/project onboarding** establishes account session, imports legacy state, opens/creates projects.
4. **Project provider layer** abstracts local vs SSH repositories/workspaces.
5. **Task creation** chooses a branch/worktree strategy, provisions a workspace, and optionally runs lifecycle scripts.
6. **Terminal/PTy orchestration** launches agent CLIs in local or SSH sessions, often via tmux and provider-specific spawn rules.
7. **Agent runtime integration** uses provider metadata, prompt-delivery modes, event classifiers, and hook servers to translate raw CLI behavior into structured app state.
8. **Workspace services** provide git, filesystem, editor buffers, PR sync, file indexing, and task search.
9. **Renderer workspace UI** presents conversations, terminals, diffs, files, PRs, and integrations through React + MobX + React Query.
10. **Packaging/release pipeline** builds Electron bundles for desktop platforms and Linux/Nix artifacts, then publishes release assets.

So emdash is not one monolithic “pipeline”; it is a layered orchestration system made of:
- **desktop app runtime**
- **project/workspace provisioning**
- **agent execution**
- **event classification + hook ingestion**
- **git/PR/search/indexing support**
- **UI review and workflow tooling**

---

# 1. Product model / what emdash is trying to do

Purpose:
- Run many coding agents in parallel.
- Isolate them by branch/worktree.
- Support local and remote development over SSH.
- Wrap surrounding dev workflow: tickets, diffs, PRs, checks, merge.  

Key description from repo:
- Emdash is a “provider-agnostic desktop app” for multiple coding agents in parallel in isolated git worktrees, locally or over SSH. `README.md` and package metadata describe it as an Electron cross-platform orchestration app.  

Key files:
```markdown name=README.md url=https://github.com/generalaction/emdash/blob/main/README.md#L28-L46
Emdash is a provider-agnostic desktop app that lets you run multiple coding agents in parallel, each isolated in its own git worktree, either locally or over SSH on a remote machine. We call it an Agentic Development Environment (ADE).
```

```json name=package.json url=https://github.com/generalaction/emdash/blob/main/package.json#L2-L8
{
  "name": "emdash",
  "version": "1.1.17",
  "description": "A cross-platform Electron app that orchestrates multiple coding agents in parallel",
  "type": "module"
}
```

Technologies:
- Electron
- React
- TypeScript
- Electron Vite
- SQLite + Drizzle
- PTY orchestration (`node-pty`, `ssh2`)
- Git CLI integration
- Provider integrations: GitHub, GitLab, Jira, Linear, etc.
- Monaco editor
- xterm frontend stack
- Electron Builder
- Nix packaging

Likely gaps:
- The product spans many domains, increasing coupling risk.
- Provider support breadth is high, which likely creates long-tail maintenance overhead.
- CLI-driven orchestration means behavior is dependent on third-party agent UI/CLI stability.

---

# 2. Process architecture and boot sequence

## Stage 2.1: Main process boot

Purpose:
- Start the Electron app and initialize all backend/runtime services.

Input:
- Electron app lifecycle
- environment variables
- packaged or dev runtime context

Output:
- initialized app services
- IPC/RPC router
- main window
- background services started

Key file:
```typescript name=src/main/index.ts url=https://github.com/generalaction/emdash/blob/main/src/main/index.ts#L81-L153
void app.whenReady().then(async () => {
  await resolveUserEnv();

  try {
    await initializeDatabase();
    searchService.initialize();
    workspaceFileIndexService.initialize();
    void editorBufferService.pruneStale();
    ...
  }

  try {
    await telemetryService.initialize({ installSource: app.isPackaged ? 'dmg' : 'dev' });
  } catch (e) {
    ...
  }

  gitWatcherRegistry.initialize();
  projectSettingsService.initialize();
  prSyncScheduler.initialize();
  appService.initialize();
  await appSettingsService.initialize();
  await promptLibraryService.initialize();

  agentHookService.initialize().catch(...);
  emdashAccountService.loadSessionToken().catch(...);

  providerTokenRegistry.register('github', (token) => githubConnectionService.storeToken(token));

  registerRPCRouter(rpcRouter, ipcMain);

  void reconcileResourceSampler();

  localDependencyManager.probeAll().catch(...);

  setupAppProtocol(...);
  setupApplicationMenu();
  createMainWindow();

  try {
    await updateService.initialize();
  } catch (error) {
    ...
  }
});
```

Technologies/techniques:
- Electron lifecycle
- dotenv in dev
- single-instance locking
- service initialization sequencing
- graceful shutdown with disposal hooks

Key services initialized:
- database
- search service
- workspace file index service
- editor buffer cleanup
- telemetry
- git watcher registry
- project settings
- PR sync scheduler
- app settings
- prompt library
- hook server
- dependency probing
- updater

Strengths:
- Clear boot orchestration.
- Explicit disposal path on quit.
- Good separation between initialization concerns.

Likely gaps:
- Boot is broad and centralized; failure isolation may be limited.
- Many background services start eagerly instead of lazily.
- Initialization order is partially implicit; could become fragile over time.

## Stage 2.2: Typed RPC registration

Purpose:
- Expose main-process capabilities to the renderer through a typed namespace router.

Key files:
- `src/main/rpc.ts`
- `src/shared/ipc/rpc.ts`
- `src/preload/index.ts`

Main router:
```typescript name=src/main/rpc.ts url=https://github.com/generalaction/emdash/blob/main/src/main/rpc.ts#L37-L72
export const rpcRouter = createRPCRouter({
  account: accountController,
  ...
  projects: projectController,
  tasks: taskController,
  conversations: conversationController,
  terminals: terminalsController,
  git: gitController,
  ...
  pullRequests: pullRequestController,
  viewState: viewStateController,
  search: searchController,
  workspaces: workspaceController,
});
```

Shared IPC primitive:
```typescript name=src/shared/ipc/rpc.ts url=https://github.com/generalaction/emdash/blob/main/src/shared/ipc/rpc.ts#L13-L20
export function registerRPCRouter(router: RouterMap, ipcMain: IpcMain): void {
  for (const [ns, handlers] of Object.entries(router)) {
    for (const [key, fn] of Object.entries(handlers)) {
      const channel = `${ns}.${key}`;
      ipcMain.handle(channel, (_event, ...args: unknown[]) => fn(...args));
    }
  }
}
```

Preload bridge:
```typescript name=src/preload/index.ts url=https://github.com/generalaction/emdash/blob/main/src/preload/index.ts#L4-L21
contextBridge.exposeInMainWorld('electronAPI', {
  invoke: (channel: string, ...args: unknown[]) => ipcRenderer.invoke(channel, ...args),
  eventSend: (channel: string, data: unknown) => ipcRenderer.send(channel, data),
  eventOn: (channel: string, cb: (data: unknown) => void) => { ... }
});
```

Technologies/techniques:
- Electron `contextBridge`
- namespaced RPC over `ipcMain.handle`
- shared typing via TS generics

Gaps:
- RPC is lightweight and elegant, but no explicit transport-layer auth/permission model inside app boundaries.
- Channel naming is convention-based; scale could make versioning difficult.
- Error normalization appears ad hoc per controller/service.

---

# 3. Renderer boot and top-level UI pipeline

## Stage 3.1: Renderer bootstrap

Purpose:
- Prepare frontend infra before rendering app UI.

Key file:
```tsx name=src/renderer/main.tsx url=https://github.com/generalaction/emdash/blob/main/src/renderer/main.tsx#L22-L64
async function bootstrap() {
  wireModelRegistryInvalidation(modelRegistry);
  wirePrCacheInvalidation();
  wireCommitHistoryInvalidation();

  appState.update.start();
  appState.resourceMonitor.start();
  initSoundPlayer();

  const [, , navResult, sidebarResult, allViewState] = await Promise.all([
    codeEditorPool.init(0).catch(...),
    diffEditorPool.init(0).catch(...),
    rpc.viewState.get('navigation'),
    rpc.viewState.get('sidebar'),
    rpc.viewState.getAll(),
    appState.projects.load(),
  ]);

  viewStateCache.populate(allViewState as Record<string, unknown>);
  ...
  ReactDOM.createRoot(...).render(
    <ErrorBoundary>
      <App />
    </ErrorBoundary>
  );
}
```

Technologies:
- React 19
- Monaco editor pooling
- cached view state restore
- app store startup
- error boundary

Outputs:
- hydrated renderer state
- initialized editor infrastructure
- workspace shell

Likely gaps:
- Monaco init is eagerly awaited, which may increase first-render latency.
- Parallel preload of many concerns suggests some opportunities for incremental rendering.

## Stage 3.2: App composition and onboarding

Purpose:
- Decide whether user sees onboarding, welcome screen, or workspace.

Key file:
```tsx name=src/renderer/App.tsx url=https://github.com/generalaction/emdash/blob/main/src/renderer/App.tsx#L26-L77
function AppContent() {
  const [view, setView] = useState<AppView>(() =>
    localStorage.getItem(HAS_SEEN_ONBOARDING) === 'true' ? 'workspace' : 'onboarding'
  );

  const { data: session, isLoading: sessionLoading } = useAccountSession();
  const { data: legacyStatus, isLoading: legacyLoading } = useLegacyPortStatus();
  ...
  if (view === 'onboarding' && stepsNeeded.length > 0) {
    return <Onboarding steps={stepsNeeded} onComplete={handleOnboardingComplete} />;
  }
  return (
    <>
      <Workspace />
      {view === 'welcome' && <WelcomeScreen ... />}
    </>
  );
}
```

Technologies:
- React Query
- localStorage onboarding state
- layered providers

Top-level providers:
- QueryClientProvider
- feature flags
- workspace layout
- terminal pool
- GitHub context
- integrations provider
- workspace view provider
- theme
- modal renderer
- tooltip/right-sidebar providers

Likely gaps:
- Many top-level providers indicate broad global state, which can become hard to reason about.
- Onboarding and workspace are coupled through app-level state/localStorage conventions.

---

# 4. Core architectural split: main / preload / renderer / shared

The repo’s own internal docs describe the system cleanly:

```markdown name=agents/architecture/overview.md url=https://github.com/generalaction/emdash/blob/main/agents/architecture/overview.md
- `src/main/`: Electron main process — app lifecycle, RPC controllers, domain services, database, PTY orchestration, updater, SSH
- `src/preload/`: Electron preload bridge — exposes typed `invoke`, `eventSend`, `eventOn` to renderer
- `src/renderer/`: React UI — views, components, hooks, contexts, typed RPC client
- `src/shared/`: Provider registry, IPC primitives (RPC + events), MCP types, skills types, shared domain types
```

This is the core backbone of the whole emdash pipeline.

Strength:
- Good clean partitioning.

Gap:
- A lot of business logic still necessarily lives in main-process services; long-term maintainability depends on keeping those service boundaries clean.

---

# 5. Configuration and environment pipeline

## Stage 5.1: Runtime/build env parsing

Purpose:
- Parse build-time and runtime config safely.

Key file:
```typescript name=src/main/lib/env.ts url=https://github.com/generalaction/emdash/blob/main/src/main/lib/env.ts#L1-L41
const buildSchema = z.object({
  VITE_POSTHOG_KEY: z.string().optional(),
  VITE_POSTHOG_HOST: z.string().optional(),
  VITE_BUILD: z.enum(['canary', 'prod']).default('prod'),
});
...
const runtimeSchema = z.object({
  TELEMETRY_ENABLED: z.string().optional(),
  INSTALL_SOURCE: z.string().optional(),
});
```

Technologies:
- Zod schema validation
- split build/dev/runtime env parsing

Gaps:
- Env surface appears modest, but many behavior toggles likely live elsewhere in settings/DB.
- Some runtime toggles documented in `AGENTS.md` suggest hidden operational complexity:
  - `EMDASH_DB_FILE`
  - `EMDASH_DISABLE_NATIVE_DB`
  - `EMDASH_DISABLE_CLONE_CACHE`
  - `EMDASH_DISABLE_PTY`  

## Stage 5.2: Repo/project-level config

Purpose:
- Control shareable task/workspace behavior.

Key file:
```markdown name=agents/workflows/worktrees.md url=https://github.com/generalaction/emdash/blob/main/agents/workflows/worktrees.md#L18-L35
`.emdash.json` stores optional shareable project settings. Supported runtime keys:
- `preservePatterns`
- `scripts.setup`
- `scripts.run`
- `scripts.teardown`
- `shellSetup`

Base project settings are DB-backed Project Settings, not runtime `.emdash.json` keys:
- `worktreeDirectory`
- `defaultBranch`
- `baseRemote`
- `pushRemote`
- `tmux`
- `workspaceProvider`
```

This is a very important stage in the pipeline because it changes how workspaces are provisioned and how shells are bootstrapped.

Strength:
- Good distinction between shareable repo config and local DB-backed settings.

Potential gaps:
- Split config authority between DB and `.emdash.json` can be confusing.
- It may be hard to reason about precedence and provenance of settings.
- Lifecycle scripts are powerful but potentially risky.

---

# 6. Data and persistence pipeline

## Stage 6.1: Local database initialization

Purpose:
- Create/upgrade persistence layer.

Key file:
```typescript name=src/main/db/initialize.ts url=https://github.com/generalaction/emdash/blob/main/src/main/db/initialize.ts#L125-L146
export async function initializeDatabase(
  connection?: BetterSqlite3.Database
): Promise<BetterSqlite3.Database> {
  const conn = connection ?? (await import('./client')).sqlite;
  runBundledMigrations(conn);
  ensureSearchIndex(conn);
  ensureFileIndex(conn);
  return conn;
}
```

Technologies:
- SQLite
- `better-sqlite3`
- Drizzle ORM
- custom FTS5 virtual tables outside Drizzle migrations

Key custom indexes:
- `search_index` for command palette / task/project/conversation search
- `workspace_file_index` for file search

Purpose of FTS tables:
```typescript name=src/main/db/initialize.ts url=https://github.com/generalaction/emdash/blob/main/src/main/db/initialize.ts#L49-L88
CREATE VIRTUAL TABLE search_index USING fts5(... tokenize = 'trigram case_sensitive 0')
...
CREATE VIRTUAL TABLE workspace_file_index USING fts5(... tokenize = 'trigram case_sensitive 0')
```

Strengths:
- Fast local search.
- Smart use of trigram tokenization.
- Explicit versioning for FTS schema.

Gaps:
- FTS DDL outside Drizzle means migration logic is split.
- SQLite is great locally but may become a bottleneck if more cross-entity analytics/state accumulation is added.
- Index freshness relies on eventing and crawl heuristics.

## Stage 6.2: Editor buffer persistence

Purpose:
- Save unsaved editor content per file/workspace.

Key file:
```typescript name=src/main/core/editor/editor-buffer-service.ts url=https://github.com/generalaction/emdash/blob/main/src/main/core/editor/editor-buffer-service.ts#L8-L30
export class EditorBufferService {
  async saveBuffer(projectId: string, workspaceId: string, filePath: string, content: string) {
    const id = `${projectId}:${workspaceId}:${filePath}`;
    await db.insert(editorBuffers).values(...).onConflictDoUpdate(...);
  }
}
```

Technique:
- per-workspace persisted draft buffer
- stale pruning after 7 days

Gap:
- No indication of version/merge awareness between buffer and underlying file changes.

---

# 7. Project ingestion and abstraction pipeline

## Stage 7.1: Project abstraction

Purpose:
- Normalize local and SSH projects behind a provider abstraction.

Repo doc:
```markdown name=agents/architecture/main-process.md url=https://github.com/generalaction/emdash/blob/main/agents/architecture/main-process.md#L21-L29
- **projects** — Project management with provider pattern (`local-project-provider.ts`), worktree service, project settings, CRUD operations
- **terminals** — Terminal lifecycle with provider pattern (`local-terminal-provider.ts`, `ssh-terminal-provider.ts`)
- **fs** — Filesystem operations with provider pattern (`local-fs.ts`, `ssh-fs.ts`)
```

Key manager:
```typescript name=src/main/core/projects/project-manager.ts url=https://github.com/generalaction/emdash/blob/main/src/main/core/projects/project-manager.ts#L41-L66
async openProject(project: LocalProject | SshProject): Promise<Result<ProjectProvider, ...>> {
  return this._lifecycle.provision(project.id, async () => {
    const provider = await withTimeout(
      createProvider(project),
      project.type === 'ssh' ? SSH_PROVIDER_TIMEOUT_MS : LOCAL_PROVIDER_TIMEOUT_MS
    );
    return ok(provider);
  });
}
```

Technologies:
- provider pattern
- lifecycle map
- timeout-wrapped provisioning
- hooks for project opened/closed

Inputs:
- local project path or SSH connection/project info

Outputs:
- `ProjectProvider`

Strength:
- Very strong architectural choice; local and remote can share most higher-level flows.

Gap:
- Timeouts differ by provider but operational diagnostics may still be difficult.
- Provider abstractions often leak over time; worth auditing for branch/workspace/fs/pty differences.

## Stage 7.2: Workspace abstraction

Purpose:
- Define what a provisioned task workspace exposes.

Key file:
```typescript name=src/main/core/workspaces/workspace.ts url=https://github.com/generalaction/emdash/blob/main/src/main/core/workspaces/workspace.ts#L7-L16
export interface Workspace {
  readonly id: string;
  readonly path: string;
  readonly fs: FileSystemProvider;
  readonly git: WorkspaceGitProvider;
  readonly settings: ProjectSettingsProvider;
  readonly lifecycleService: LifecycleScriptService;
  readonly repository: GitRepositoryService;
  readonly fetchService: GitFetchService;
}
```

This is effectively the central internal “unit of work” in the emdash execution pipeline.

Gap:
- Workspace object is rich; if too many consumers use it directly, dependency boundaries may weaken.

---

# 8. Task creation and worktree provisioning pipeline

This is one of the most critical parts of emdash.

## Stage 8.1: Task specification

Purpose:
- Represent a workflow unit with branch/worktree/conversations/PRs.

Key shared type:
```typescript name=src/shared/tasks.ts url=https://github.com/generalaction/emdash/blob/main/src/shared/tasks.ts#L37-L75
export type CreateTaskStrategy =
  | { kind: 'new-branch'; taskBranch: string; pushBranch?: boolean }
  | { kind: 'checkout-existing' }
  | { kind: 'from-pull-request'; ... }
  | { kind: 'no-worktree' };
```

This indicates emdash supports multiple task bootstrap modes:
- new branch
- existing branch
- from PR
- no worktree

## Stage 8.2: Task creation logic

Purpose:
- Create branch if needed, fetch PR branch if needed, prepare workspace.

Key file:
```typescript name=src/main/core/tasks/operations/createTask.ts url=https://github.com/generalaction/emdash/blob/main/src/main/core/tasks/operations/createTask.ts#L40-L138
export async function createTask(params: CreateTaskParams): Promise<Result<CreateTaskSuccess, CreateTaskError>> {
  ...
  switch (strategy.kind) {
    case 'new-branch': {
      ...
      const createResult = await project.repository.createBranch(...)
      ...
      if (strategy.pushBranch) {
        const publishResult = await project.repository.publishBranch(taskBranch, pushRemote);
      }
      break;
    }
    case 'checkout-existing': {
      taskBranch = params.sourceBranch.branch;
      break;
    }
    case 'from-pull-request': {
      const existingWorktree = await project.getWorktreeForBranch(strategy.headBranch);
      if (!existingWorktree) {
        const fetchResult = await project.repository.fetchPrForReview(...)
      }
      ...
    }
```

Techniques:
- branch-name derivation with prefix + optional random suffix
- PR-aware fetch/checkout path
- remote config awareness (`baseRemote`, `pushRemote`)
- warning vs error distinction for push failures

Likely gaps:
- Branch naming randomness may complicate traceability.
- PR fetch workflow likely has edge cases around forks, remotes, detached states.
- Significant branching logic suggests a candidate for formal state-machine modeling.

## Stage 8.3: Worktree management

Purpose:
- Create and manage isolated git worktrees.

Key file:
```typescript name=src/main/core/projects/worktrees/worktree-service.ts url=https://github.com/generalaction/emdash/blob/main/src/main/core/projects/worktrees/worktree-service.ts#L16-L74
export class WorktreeService {
  private gitOpQueue: Promise<unknown> = Promise.resolve();
  ...
  constructor(...) {
    ...
    this.ctx.exec('git', ['worktree', 'prune']).catch(() => {});
  }

  private enqueueGitOp<T>(fn: () => Promise<T>): Promise<T> {
    const result = this.gitOpQueue.then(fn, fn);
    this.gitOpQueue = result.catch(() => {});
    return result as Promise<T>;
  }
```

Techniques:
- serialized git operation queue
- worktree pruning
- local vs SSH path-validation logic
- branch-to-worktree discovery via `git worktree list --porcelain`

Repo-documented behavior:
```markdown name=agents/workflows/worktrees.md url=https://github.com/generalaction/emdash/blob/main/agents/workflows/worktrees.md#L10-L17
- task worktrees are created under the project's DB-backed worktree directory setting
- branch prefix defaults to `emdash` and is configurable in app settings
- generated task branch names use the configured prefix plus a random suffix by default
- selected gitignored files are preserved into worktrees
- worktree creation is managed by the project provider pattern
```

Strengths:
- Using real git worktrees is a solid isolation mechanism.
- Git-op queue is a practical concurrency guard.

Gaps:
- Worktree correctness depends heavily on external git state.
- Preserving selected ignored files is useful but can introduce nondeterminism.
- No obvious transactional envelope across branch creation + worktree provisioning + lifecycle scripts.

---

# 9. Lifecycle script pipeline

Purpose:
- Run repo-specific setup/run/teardown commands inside task workspaces.

## Stage 9.1: Lifecycle script resolution

Key file:
```typescript name=src/main/core/terminals/runLifecycleScript.ts url=https://github.com/generalaction/emdash/blob/main/src/main/core/terminals/runLifecycleScript.ts#L4-L25
const settings = await getEffectiveTaskSettings({
  projectSettings: workspace.settings,
  taskFs: workspace.fs,
});
const script = settings.scripts?.[type];
if (!script) return;
await workspace.lifecycleService.runLifecycleScript(
  { type, script, shellSetup: settings.shellSetup },
  { exit: true }
);
```

## Stage 9.2: Lifecycle execution engine

Key file:
```typescript name=src/main/core/workspaces/workspace-lifecycle-service.ts url=https://github.com/generalaction/emdash/blob/main/src/main/core/workspaces/workspace-lifecycle-service.ts#L76-L122
async runLifecycleScript(script: LifecycleScript, options = {}): Promise<void> {
  ...
  if (!ptySessionRegistry.get(sessionId)) {
    await this.prepareLifecycleScript(script, { initialSize });
  }
  ...
  const command = exit ? `${script.script}; exit` : script.script;
  pty.write(`${command}\n`);
}
```

Techniques:
- lifecycle terminals
- shell injection into PTY
- respawn-after-exit behavior for interactive scripts
- preserved terminal buffer
- special `run` handling with dev-server watching

Strengths:
- Reusable shell session model.
- Supports setup/run/teardown phases cleanly.

Gaps:
- Command execution is shell-string based, so quoting/escaping risk is important.
- Repo-configurable scripts are powerful but can be inconsistent across projects.
- Limited evidence of structured output capture from scripts beyond terminal behavior.

---

# 10. Dependency detection pipeline

Purpose:
- Detect/install/inspect required agent CLIs and related dependencies.

Key file:
```typescript name=src/main/core/dependencies/controller.ts url=https://github.com/generalaction/emdash/blob/main/src/main/core/dependencies/controller.ts#L5-L31
export const dependenciesController = createRPCController({
  getAll: async (connectionId?: string) => {
    const mgr = await getDependencyManager(connectionId);
    return Object.fromEntries(mgr.getAll());
  },
  ...
  probeAll: async (connectionId?: string) => {
    const mgr = await getDependencyManager(connectionId);
    return mgr.probeAll();
  },
  install: async (id: DependencyId, connectionId?: string) => {
    const mgr = await getDependencyManager(connectionId);
    return mgr.install(id);
  },
});
```

And main boot eagerly probes:
```typescript name=src/main/index.ts url=https://github.com/generalaction/emdash/blob/main/src/main/index.ts#L136-L140
localDependencyManager.probeAll().catch((e) => {
  log.error('Failed to probe dependencies:', e);
});
```

Techniques:
- dependency manager abstraction
- local/remote-aware probing

Strength:
- Good UX to know which CLIs are available.

Gap:
- Eager probing at startup may slow load and may produce noisy failures if many CLIs are missing.
- Cross-provider install procedures are likely brittle.

---

# 11. Agent provider integration pipeline

This is the heart of emdash’s execution model.

## Stage 11.1: Provider registry as source of truth

Purpose:
- Define how each supported agent CLI is detected, started, resumed, and driven.

Key file:
```typescript name=src/shared/agent-provider-registry.ts url=https://github.com/generalaction/emdash/blob/main/src/shared/agent-provider-registry.ts#L35-L75
export type AgentProviderDefinition = {
  id: AgentProviderId;
  name: string;
  ...
  commands?: string[];
  versionArgs?: string[];
  detectable?: boolean;
  cli?: string;
  autoApproveFlag?: string;
  initialPromptFlag?: string;
  useKeystrokeInjection?: boolean;
  resumeFlag?: string;
  sessionIdFlag?: string;
  newConversationFlag?: string;
  ...
  supportsHooks?: boolean;
};
```

Examples:
- Codex: CLI-based, supports hooks, auto-approve flag
- Claude: session-id isolation, supports hooks
- OpenCode/Amp/Hermes/etc.: use keystroke injection
- Goose/Kiro/etc.: custom default args or start commands

Repo docs confirm:
```markdown name=agents/integrations/providers.md url=https://github.com/generalaction/emdash/blob/main/agents/integrations/providers.md#L13-L22
Provider Metadata Includes
- CLI and detection commands
- version args
- install command and docs URL
- auto-approve flags
- initial prompt handling
- keystroke injection behavior
- resume and session flags
- optional plan activation and auto-start commands
```

Techniques:
- registry-driven integration
- provider-specific prompt transport
- provider-specific resume/session behavior
- hooks capability flagging

Strength:
- Centralized registry is a strong design choice.

Gaps:
- Registry size is large and heterogeneous.
- Behavior differences are encoded as flags/metadata rather than deeper capabilities modeling.
- Might benefit from capability traits instead of flat optional fields.

## Stage 11.2: Prompt delivery strategy

The provider registry shows two major patterns:
1. **CLI flag prompt injection**
2. **interactive keystroke injection**

Repo docs:
```markdown name=agents/integrations/providers.md url=https://github.com/generalaction/emdash/blob/main/agents/integrations/providers.md#L28-L33
- Claude uses deterministic `--session-id` values for conversation isolation.
- Agents with no CLI prompt flag (e.g., Amp, OpenCode) use keystroke injection — Emdash types the prompt into the TUI after startup.
- ... writes hook config files (`.claude/settings.local.json`, `.codex/config.toml`)
```

This is one of the clearest likely improvement points.

Potential gaps:
- Keystroke injection is inherently fragile.
- TUI timing/race issues likely exist.
- CLI UX changes upstream can break integration.
- Prompt delivery reliability is probably one of the highest-risk zones in the whole pipeline.

---

# 12. PTY and terminal orchestration pipeline

## Stage 12.1: Terminal abstraction

Purpose:
- Normalize session spawning for local and SSH workspaces.

Key file:
```typescript name=src/main/core/terminals/terminal-provider.ts url=https://github.com/generalaction/emdash/blob/main/src/main/core/terminals/terminal-provider.ts#L3-L23
export interface TerminalProvider {
  spawnTerminal(...)
  spawnLifecycleScript(...)
  killTerminal(...)
  destroyAll()
  detachAll()
}
```

## Stage 12.2: Local terminal provider

Key file:
```typescript name=src/main/core/terminals/impl/local-terminal-provider.ts url=https://github.com/generalaction/emdash/blob/main/src/main/core/terminals/impl/local-terminal-provider.ts#L57-L106
async spawnTerminal(...) {
  return this.spawnWithPolicy(..., {
    respawnOnExit: true,
    preserveBufferOnExit: false,
    watchDevServer: true,
  });
}
```

Technologies:
- `node-pty`
- environment construction via `buildTerminalEnv`
- tmux session support
- dev-server watcher
- respawn policy

## Stage 12.3: SSH terminal provider

Key file:
```typescript name=src/main/core/terminals/impl/ssh-terminal-provider.ts url=https://github.com/generalaction/emdash/blob/main/src/main/core/terminals/impl/ssh-terminal-provider.ts#L31-L78
export class SshTerminalProvider implements TerminalProvider {
  ...
  this._handleReconnect = (evt) => {
    if (evt.type === 'reconnected' && evt.connectionId === this.connectionId) {
      this.rehydrate().catch(...)
    }
  };
  sshConnectionManager.on('connection-event', this._handleReconnect);
}
```

Technologies:
- `ssh2`
- remote PTY creation
- reconnect-aware rehydration
- tmux integration
- session tracking

Strengths:
- Strong abstraction symmetry between local and SSH.
- Reconnect/rehydrate support is a very valuable remote-dev feature.

Gaps:
- PTY/session correctness across reconnects is hard.
- tmux/session naming/collision edge cases likely matter.
- Shell setup + command injection + remote state introduces many failure surfaces.

---

# 13. Agent event ingestion pipeline

There are two major event acquisition mechanisms:

## Stage 13.1: Terminal-output classifiers

Repo docs:
```markdown name=agents/integrations/providers.md url=https://github.com/generalaction/emdash/blob/main/agents/integrations/providers.md#L24-L26
Each provider has a terminal output classifier in `src/main/core/conversations/impl/agent-event-classifiers/`. These parse agent terminal output to detect events (task completion, errors, etc.)
```

Technique:
- parse stdout/stderr or rendered terminal text into structured semantic events

Likely gaps:
- output parsing is brittle
- upstream formatting changes break classifiers
- ambiguity in event detection

## Stage 13.2: Hook-based event ingestion

Purpose:
- Let supported providers emit structured events back into emdash.

Key service:
```typescript name=src/main/core/agent-hooks/agent-hook-service.ts url=https://github.com/generalaction/emdash/blob/main/src/main/core/agent-hooks/agent-hook-service.ts#L8-L18
async initialize(): Promise<void> {
  await this.server.start(async (raw) => {
    const event = await enrichEvent(raw);
    event.source = 'hook';
    const appFocused = isAppFocused();
    await maybeShowNotification(event, appFocused);
    events.emit(agentEventChannel, { event, appFocused });
  });
}
```

Provider plugin example:
```javascript name=src/main/core/agent-hooks/opencode-notifications-plugin.js url=https://github.com/generalaction/emdash/blob/main/src/main/core/agent-hooks/opencode-notifications-plugin.js#L1-L27
export const EmdashNotifications = async () => ({
  event: async ({ event }) => {
    const port = process.env.EMDASH_HOOK_PORT;
    const token = process.env.EMDASH_HOOK_TOKEN;
    const ptyId = process.env.EMDASH_PTY_ID;
    if (!port || !token || !ptyId) return;
    ...
    await fetch(`http://127.0.0.1:${port}/hook`, {
      method: 'POST',
      headers: {
        'X-Emdash-Token': token,
        'X-Emdash-Pty-Id': ptyId,
        'X-Emdash-Event-Type': payload.type,
      },
      body: JSON.stringify(payload.body),
    });
  },
});
```

Techniques:
- local loopback HTTP hook server
- shared token auth
- event enrichment
- OS notifications
- event emission to renderer

Strength:
- Hooks are much better than raw terminal parsing when available.

Major gap:
- Best-effort delivery only.
- Mixed event model: some providers use hooks, some rely on parsing.
- This is probably the **single biggest architectural inconsistency** in the pipeline.

**Improvement opportunity:** push toward a standardized event contract across all providers, with terminal parsing only as fallback.

---

# 14. Account/auth pipeline

Purpose:
- Authenticate user to emdash’s auth service and optionally provider token routing.

Key file:
```typescript name=src/main/core/shared/oauth-flow.ts url=https://github.com/generalaction/emdash/blob/main/src/main/core/shared/oauth-flow.ts#L10-L41
export async function executeOAuthFlow(options: OAuthFlowOptions): Promise<Record<string, unknown>> {
  const state = randomBytes(12).toString('base64url');
  const codeVerifier = randomBytes(32).toString('base64url');
  const codeChallenge = createHash('sha256').update(codeVerifier).digest('base64url');
  const { code } = await startLoopbackServer(...);
  return exchangeCode(exchangeUrl, state, code, codeVerifier);
}
```

Account service:
```typescript name=src/main/core/account/services/emdash-account-service.ts url=https://github.com/generalaction/emdash/blob/main/src/main/core/account/services/emdash-account-service.ts#L95-L116
const raw = await executeOAuthFlow({
  authorizeUrl: `${baseUrl}/sign-in`,
  exchangeUrl: `${baseUrl}/api/v1/auth/electron/exchange`,
  successRedirectUrl: `${baseUrl}/auth/success`,
  errorRedirectUrl: `${baseUrl}/auth/error`,
  ...
});
```

Techniques:
- PKCE OAuth
- loopback localhost callback server
- credential store
- provider token dispatch

Strength:
- Good desktop-app OAuth pattern.

Gap:
- Current sign-in comment suggests provider optionality is not fully generalized yet.

---

# 15. Git operations pipeline

Purpose:
- Support status, diffs, branches, worktrees, PR review, and repository manipulation.

Key file:
```typescript name=src/main/core/git/impl/git-service.ts url=https://github.com/generalaction/emdash/blob/main/src/main/core/git/impl/git-service.ts#L97-L145
export class GitService implements GitProvider, IDisposable {
  private _statusInFlight: Promise<FullGitStatus> | null = null;
  ...
  async getFullStatus(): Promise<FullGitStatus> {
    if (this._statusInFlight) return this._statusInFlight;
    this._statusInFlight = this._loadFullStatus()
      .then((status) => {
        this._hooks.callHookBackground('status:updated', status);
        return status;
      })
```

Shared git model includes:
- staged/unstaged changes
- branch/detached/unborn head states
- diff results
- image preview support and unavailable reasons

Key file:
```typescript name=src/shared/git.ts url=https://github.com/generalaction/emdash/blob/main/src/shared/git.ts#L17-L44
export interface FullGitStatus {
  staged: GitChange[];
  unstaged: GitChange[];
  currentBranch: string | null;
  headKind: 'branch' | 'detached' | 'unborn';
  shortHash: string | null;
  totalAdded: number;
  totalDeleted: number;
}
```

Technologies:
- shelling out to git CLI
- cached in-flight status loads
- hashed status fingerprinting
- batched `cat-file` for local optimization

Strength:
- Mature enough git abstraction for the UI they want.

Gaps:
- CLI-shelling remains prone to environment/path differences.
- Local optimizations don’t apply to SSH equally.
- Likely opportunity to formalize backpressure/coalescing around repeated status refreshes.

---

# 16. Search and indexing pipeline

## Stage 16.1: Entity search

Purpose:
- Search tasks, projects, conversations, commands.

Key file:
```typescript name=src/main/core/search/search-service.ts url=https://github.com/generalaction/emdash/blob/main/src/main/core/search/search-service.ts#L36-L58
initialize(): void {
  taskEvents.on('task:created', (task) => this.upsertTask(task));
  ...
  conversationEvents.on('conversation:created', (conversation) =>
    this.upsertConversation(conversation)
  );
  this.backfill();
  this.seedCommands();
}
```

Query path:
```typescript name=src/main/core/search/search-service.ts url=https://github.com/generalaction/emdash/blob/main/src/main/core/search/search-service.ts#L59-L126
const terms = query.trim().split(/[\s\-_]+/).filter((t) => t.length >= 3);
const ftsQuery = terms.map((t) => `"${t}"`).join(' AND ');
...
ORDER BY rank
LIMIT 30
```

## Stage 16.2: Workspace file index

Purpose:
- Search filenames within provisioned workspaces.

Key file:
```typescript name=src/main/core/search/workspace-file-index-service.ts url=https://github.com/generalaction/emdash/blob/main/src/main/core/search/workspace-file-index-service.ts#L47-L111
initialize(): void {
  this.evictStale();
  events.on(fsWatchEventChannel, ({ workspaceId }) => {
    this.scheduleReindex(workspaceId);
  });
}
...
const result = await workspace.fs.list('', {
  recursive: true,
  maxEntries: MAX_FILES,
  timeBudgetMs: CRAWL_TIMEOUT_MS,
});
```

Techniques:
- FTS5 trigram search
- event-driven reindexing
- budgeted recursive crawl
- ignored-dir filtering
- stale eviction after inactivity

Strength:
- Strong local responsiveness for command palette and file discovery.

Gaps:
- Search terms <3 chars are dropped.
- File index is filename/path only, not content-aware.
- Crawl caps/time budgets can miss very large repos.
- Staleness model may lead to subtle “why can’t I find this file?” issues.

---

# 17. PR sync and code review pipeline

Purpose:
- Pull PR metadata/checks into task workflow and keep them fresh.

Key file:
```typescript name=src/main/core/pull-requests/pr-sync-scheduler.ts url=https://github.com/generalaction/emdash/blob/main/src/main/core/pull-requests/pr-sync-scheduler.ts#L27-L55
async onProjectMounted(projectId: string): Promise<void> {
  const remoteUrls = await this._syncAndGetGitHubRemotes(projectId);
  ...
  for (const url of remoteUrls) {
    prSyncEngine.sync(url);

    const handle = setInterval(() => {
      prSyncEngine.sync(url);
    }, INCREMENTAL_SYNC_INTERVAL_MS);
```

Techniques:
- project-open hook
- remote discovery
- periodic incremental sync
- task-provision-triggered single-PR sync
- cancellation on unmount

Strength:
- Good workflow alignment with branch/task lifecycle.

Gaps:
- Polling-based sync may be wasteful.
- GitHub-only remote heuristics might not generalize as neatly.
- PR sync cadence vs UI expectations may cause stale states.

---

# 18. MCP integration pipeline

Purpose:
- Manage MCP server configs across supported agents.

Key file:
```typescript name=src/main/core/mcp/services/McpService.ts url=https://github.com/generalaction/emdash/blob/main/src/main/core/mcp/services/McpService.ts#L8-L62
export class McpService {
  private _writeLock = Promise.resolve();
  ...
  async loadAll(): Promise<McpLoadAllResponse> {
    return this.withWriteLock(async () => {
      const agentIds = getAllMcpAgentIds();
      ...
      const canonical = adaptReverse(meta.adapter, rawServers);
      ...
      const catalog = loadCatalog();
      return { installed, catalog };
    });
  }
}
```

Repo docs:
```markdown name=agents/integrations/mcp.md url=https://github.com/generalaction/emdash/blob/main/agents/integrations/mcp.md#L12-L26
- MCP server configs are read, adapted, merged, and written across supported agent ecosystems
- provider-specific config formats are handled through adapters
- Codex currently supports stdio MCP servers only
- keep canonical MCP data in shared types and adapt at the edges
```

Techniques:
- canonical internal MCP representation
- per-provider adapters
- serialized writes with lock
- installed + catalog merge

Strength:
- Good “canonical in core, adapt at edges” design.

Gap:
- Provider compatibility fragmentation is a recurring problem.
- As with agent providers, MCP capability modeling may need stronger normalization.

---

# 19. Local vs SSH vs BYOI execution pipeline

Purpose:
- Allow execution in different environment types.

Shared workspace type:
```typescript name=src/shared/workspaces.ts url=https://github.com/generalaction/emdash/blob/main/src/shared/workspaces.ts#L1-L7
export type WorkspaceType = 'local' | 'project-ssh' | 'byoi';
```

BYOI docs:
```markdown name=tooling/byoi/README.md url=https://github.com/generalaction/emdash/blob/main/tooling/byoi/README.md#L6-L17
1. When you create a task, emdash runs `provision.sh`
2. The script builds a Docker image (first run only), starts a new container, clones your repo into it, and prints a JSON blob
3. Emdash SSH-connects to the container using password auth and opens the workspace at `/home/devuser/workspace`
4. When you terminate the task, emdash runs `terminate.sh`
```

Interpretation:
- BYOI is effectively a provision-then-SSH model.
- It extends the remote-provider abstraction rather than inventing a separate full stack.

Gap:
- Provisioning via external shell scripts and printed JSON is practical but loose.
- Could benefit from a more strongly typed provider contract and validation layer.

---

# 20. Renderer workflow pipeline

## Stage 20.1: Workspace shell

Key file:
```tsx name=src/renderer/app/workspace.tsx url=https://github.com/generalaction/emdash/blob/main/src/renderer/app/workspace.tsx#L12-L27
export function Workspace() {
  useTheme();
  const { WrapView } = useWorkspaceSlots();
  const { wrapParams } = useWorkspaceWrapParams();

  return (
    <>
      <AppKeyboardShortcuts />
      <CommandShortcutBinder />
      <MonacoKeyboardBridge />
      <WorkspaceLayout
        leftSidebar={<LeftSidebar />}
        mainContent={
          <WrapView {...wrapParams}>
            <WorkspaceViewContent />
          </WrapView>
        }
      />
      <Toaster />
    </>
  );
}
```

Purpose:
- Build the main interactive shell around navigation, sidebar, editor/diff/terminal views.

Technologies:
- React
- MobX
- Monaco
- keyboard-command system
- resizable panel system
- toast notifications

## Stage 20.2: Layout and navigation state

Key files:
- `src/renderer/lib/layout/provider.tsx`
- `src/renderer/lib/layout/layout-provider.tsx`
- `src/renderer/lib/layout/workspace-layout.tsx`

Techniques:
- persisted panel layout in localStorage
- view-driven telemetry scoping
- focus tracking
- panel drag suppression for terminal resize stability

Potential gaps:
- Many interactions between layout, terminals, Monaco, and focus imply UI complexity.
- Could benefit from more explicit “task session” state boundaries.

## Stage 20.3: Task view model

Key file:
```tsx name=src/renderer/features/tasks/stores/workspace-view-model.tsx url=https://github.com/generalaction/emdash/blob/main/src/renderer/features/tasks/stores/workspace-view-model.tsx#L24-L67
export class WorkspaceViewModel implements ILifecycle {
  ...
  readonly tabGroupManager: TabGroupManagerStore;
  readonly terminalTabs: TerminalTabViewStore;
  readonly editorView: FileModelLifecycleStore;
  ...
  diffView: DiffViewStore | null = null;
  prStore: PrStore | null = null;
  devServers: DevServerStore | null = null;
```

Interpretation:
- Task/workspace UI is modeled as a composite of editor, diff, terminal, PR, and dev-server sub-stores.
- Renderer state is sophisticated and session-aware.

Gap:
- There is likely substantial renderer state complexity and lifecycle coupling here.
- Strong candidate for improvement if UI behavior feels inconsistent under frequent task switching.

---

# 21. Observability, events, and telemetry pipeline

Purpose:
- Capture usage/health and synchronize internal state.

Signals from repo:
- `telemetryService.initialize`
- telemetry scope tied to task view
- app close event captured
- agent events emitted through shared event channels
- notification service hooks
- resource sampler reconciliation

Key renderer telemetry scoping:
```tsx name=src/renderer/lib/layout/provider.tsx url=https://github.com/generalaction/emdash/blob/main/src/renderer/lib/layout/provider.tsx#L8-L25
function syncTelemetryScope(currentViewId: ViewId, viewParamsStore: ViewParamsStore): void {
  if (currentViewId !== 'task') {
    clearTelemetryTaskScope();
    return;
  }
  ...
  setTelemetryTaskScope({ projectId, taskId });
}
```

Techniques:
- event channels
- scope-aware telemetry
- logs + warnings + error capture
- OS notifications for agent events

Gaps:
- I didn’t see evidence of rich metrics/tracing across pipeline stages.
- A lot of critical orchestration likely depends on logs rather than structured end-to-end traces.
- This is a strong candidate for improvement: add stage timing, failure taxonomies, provider reliability stats.

---

# 22. Testing pipeline

From package/test config and docs:
- Vitest is used for:
  - node tests
  - main-db tests
  - migrations
  - browser tests
- Playwright-backed browser testing is configured via Vitest browser projects.  
- There are focused tests around account service, workspace lifecycle service, project creation, etc.

Repo architecture doc:
```markdown name=agents/architecture/overview.md url=https://github.com/generalaction/emdash/blob/main/agents/architecture/overview.md#L20-L24
- `vitest.config.ts` — Vitest config with two test projects: `node` (main + renderer unit tests) and `browser` (Playwright-backed renderer tests).
```

Strength:
- Better than average coverage breadth for an Electron app.

Likely gaps:
- Most fragile areas are cross-boundary:
  - provider CLIs
  - PTY orchestration
  - SSH reconnects
  - hook delivery
  - terminal parsing
- Those are the hardest to test, and likely where gaps remain.

---

# 23. Build, packaging, and release pipeline

## Stage 23.1: Local build tooling

Purpose:
- Build Electron main/preload/renderer, package desktop binaries.

From `package.json`:
```json name=package.json url=https://github.com/generalaction/emdash/blob/main/package.json#L24-L33
"build": "electron-vite build",
"package": "pnpm run build && electron-builder --config electron-builder.config.ts",
"package:mac": "pnpm run build && electron-builder --mac --config electron-builder.config.ts",
"package:linux": "pnpm run build && electron-builder --linux --publish never --config electron-builder.config.ts",
"package:win": "pnpm run build && electron-builder --win --publish never --config electron-builder.config.ts"
```

Technologies:
- electron-vite
- electron-builder
- TypeScript
- pnpm
- native rebuilds for `better-sqlite3` and `node-pty`

## Stage 23.2: Nix packaging

Key file excerpt:
```nix name=flake.nix url=https://github.com/generalaction/emdash/blob/main/flake.nix#L84-L121
pnpmDeps = ...
nativeBuildInputs = ...
buildInputs = [
  pkgs.libsecret
  pkgs.sqlite
  pkgs.zlib
  pkgs.libutempter
];
```

And binary wrapper:
```nix name=flake.nix url=https://github.com/generalaction/emdash/blob/main/flake.nix#L153-L176
cat <<EOF > $out/bin/emdash
#!${pkgs.bash}/bin/bash
set -euo pipefail

APP_ROOT="$out/share/emdash/linux-unpacked"
exec "\$APP_ROOT/emdash" "\$@"
EOF
```

Purpose:
- deterministic Linux packaging, likely for distro/Nix users and CI artifacts

## Stage 23.3: GitHub Actions release flow

Repo docs summarize:
```markdown name=CONTRIBUTING.md url=https://github.com/generalaction/emdash/blob/main/CONTRIBUTING.md#L165-L182
**Production Release** (`.github/workflows/release-prod.yml`):
1. Builds Linux, Windows, and macOS packages
2. Signs Windows builds when Azure Trusted Signing secrets are configured
3. Signs, verifies, notarizes, and staples macOS DMGs and ZIPs
4. Uploads release artifacts to Cloudflare R2

**Linux/Nix Build** (`.github/workflows/nix-build.yml`):
1. Computes the correct dependency hash from `pnpm-lock.yaml`
2. Builds the x86_64-linux package via Nix flake
3. Pushes build artifacts to Cachix and uploads the Nix artifact when available

**Canary Release** (`.github/workflows/release-canary.yml`):
1. Builds Linux, Windows, and macOS packages with the canary config
2. Publishes artifacts to the `v1-canary` R2 channel
```

Strength:
- Full cross-platform release pipeline.
- macOS notarization and Windows signing considered.
- Canary/prod separation.

Gaps:
- Release complexity is high.
- Native dependency rebuilds and Electron platform packaging often become a source of intermittent failures.

---

# 24. Comprehensive top-to-bottom emdash flow map

Below is the practical end-to-end flow.

## A. App startup
1. Electron main starts.
2. Dev env loaded if applicable.
3. User environment normalized.
4. SQLite DB initialized, migrations + FTS ensured.
5. Search/indexing services start.
6. Telemetry starts.
7. Git watchers, project settings, PR sync, app settings, prompt library start.
8. Hook server starts.
9. Account session token loads.
10. Dependency probe runs.
11. RPC router registered.
12. Main window created.
13. Updater initialized.

## B. Renderer startup
1. Renderer wires cache invalidation bridges.
2. Monaco editor pools initialize.
3. View state restored from main process.
4. App stores load projects.
5. React app mounts.
6. Onboarding/account/legacy import state determines visible UI.

## C. Project ingestion
1. User adds/opens local or SSH project.
2. `ProjectManager` provisions provider with timeout.
3. Project provider exposes repository, fs, worktrees, terminals, settings.

## D. Task creation
1. User creates task from issue/manual/PR/etc.
2. Strategy chosen:
   - new branch
   - existing branch
   - PR branch
   - no worktree
3. Branch created/fetched/published if needed.
4. Worktree created or resolved.
5. Workspace object becomes available.
6. Search/file indexes and PR sync hooks can begin using workspace/project context.

## E. Workspace bootstrap
1. Effective settings resolved from DB + `.emdash.json`.
2. `scripts.setup` / `scripts.run` / `shellSetup` may be prepared/executed.
3. Terminal provider is selected:
   - local
   - SSH
   - BYOI-over-SSH
4. PTY session created, optionally tmux-backed.

## F. Agent execution
1. User selects provider from registry.
2. Emdash resolves CLI metadata:
   - executable
   - default args
   - prompt mode
   - resume/session flags
   - approval flags
3. Prompt is sent by:
   - CLI flag, or
   - keystroke injection
4. Agent runs inside task workspace terminal.

## G. Event capture
1. Raw terminal output may be parsed by provider classifier.
2. If provider supports hooks, provider-side plugin sends structured hook events.
3. Hook server enriches event, emits app event, maybe shows OS notification.
4. Renderer receives task/conversation/agent updates.

## H. Workflow augmentation
1. Git service tracks working tree status/diffs.
2. Search service indexes tasks/projects/conversations.
3. Workspace file index crawls files for filename search.
4. PR sync scheduler polls remote PR state.
5. Editor buffers persist unsaved content.
6. Integrations/services expose GitHub/Jira/Linear/etc. data.

## I. UI review and control
1. Renderer task workspace shows:
   - conversations
   - terminals
   - diffs
   - editor tabs
   - PR state
   - dev servers
2. User reviews changes, runs commands, checks PRs, merges.

## J. Shutdown
1. Telemetry flushed.
2. Hook server stopped.
3. resource sampler / updater / PR scheduler disposed.
4. project manager closes providers.
5. app exits.

---

# 25. Biggest likely gaps / weak points in methods

Here’s the part most relevant to your “where can I improve workflow methods?” goal.

## 1. Provider integration is heterogeneous
Symptoms:
- flat registry with many optional flags
- prompt-by-arg vs prompt-by-keystroke
- hooks for some providers, terminal parsing for others

Why it matters:
- Reliability varies by provider.
- Maintenance burden grows linearly or worse.

Improvement opportunities:
- Introduce a stronger **provider capability model**:
  - prompt transport capability
  - structured event support
  - session isolation capability
  - resume semantics
  - tool approval semantics
- Prefer **structured adapter interfaces** over flat metadata fields.
- Add provider conformance tests.

## 2. Keystroke injection is fragile
Why it matters:
- Timing-sensitive
- TUI-format dependent
- harder to test

Improvement opportunities:
- Build a deterministic terminal readiness handshake before typing.
- Add per-provider “prompt accepted” confirmation heuristics.
- Prefer stdin/CLI/API prompt transport wherever possible.
- Instrument failures and retries.

## 3. Mixed event ingestion model
Current state:
- hooks for some providers
- parsed terminal output for others

Why it matters:
- inconsistent semantics
- duplicate logic
- brittle parsers

Improvement opportunities:
- Standardize a **normalized agent event envelope** internally.
- Build a provider SDK/bridge for emitting structured events.
- Treat terminal parsing as degraded mode, not first-class mode.

## 4. Worktree + branch + lifecycle flow is complex and not obviously transactional
Why it matters:
- partial failures can leave messy state
- branch exists but worktree not ready
- worktree ready but lifecycle setup failed

Improvement opportunities:
- model task provisioning as an explicit state machine:
  - branch_prepared
  - worktree_created
  - workspace_attached
  - setup_run
  - agent_ready
- add resumable recovery and cleanup logic per state.

## 5. Search/indexing is useful but narrow
Current method:
- trigram FTS on entity titles/keywords and workspace file paths

Gaps:
- not semantic
- not content-aware for workspace files
- crawl/time limits may skip large repos

Improvement opportunities:
- add optional content indexing for active workspace subset
- add git-aware change prioritization
- add semantic/embedding-assisted search for tasks/conversations/prompts if desired

## 6. Observability appears weaker than orchestration complexity demands
Why it matters:
- hardest issues are timing/state-machine failures across PTY, SSH, hooks, providers, git

Improvement opportunities:
- stage timing metrics
- per-provider reliability metrics
- structured failure taxonomy
- end-to-end task provisioning traces
- hook delivery success/failure dashboards

## 7. Config authority is split
Current split:
- DB-backed project settings
- `.emdash.json`
- env vars
- provider config files
- MCP config adapters

Why it matters:
- hard to know “why did this happen?”

Improvement opportunities:
- unified “effective config inspector” UI
- provenance for each effective setting
- dry-run preview of workspace/task provisioning

## 8. Local/SSH/BYOI abstraction is good but likely operationally divergent
Why it matters:
- remote shell envs, auth, tmux, path semantics differ

Improvement opportunities:
- add capability introspection per workspace type
- add preflight validation before task creation
- create reproducibility report for remote environments

---

# 26. Best places to implement new methods

If your goal is to improve emdash’s workflow meaningfully, I’d prioritize these insertion points:

## Highest-impact insertion points

### A. Before agent launch
Where:
- provider runtime / PTY spawn pipeline

What to add:
- launch validation
- prompt transport handshake
- provider capability negotiation
- reproducibility checks

Why:
- This is where many failures originate.

### B. Between task provisioning and interactive execution
Where:
- task creation + lifecycle script pipeline

What to add:
- explicit provisioning state machine
- rollback/recovery
- setup verification checks
- environment diff capture

Why:
- Converts fragile imperative flow into a debuggable workflow.

### C. Event normalization layer
Where:
- after hook server / terminal classifiers, before renderer state updates

What to add:
- canonical event schema
- confidence scores/source metadata
- dedupe and reconciliation

Why:
- Makes downstream UI and analytics much more reliable.

### D. Observability layer
Where:
- across provisioning, spawn, hooks, git refresh, PR sync

What to add:
- structured spans/log events per stage
- provider/session-level health scoring
- “last failed stage” UX

Why:
- Gives you leverage to improve the rest.

### E. Effective-config and environment-inspection layer
Where:
- project/workspace/task settings resolution

What to add:
- inspect final effective settings
- show source of each setting
- validate lifecycle scripts and shell setup

Why:
- Helps users and maintainers debug workflow issues quickly.

---

# 27. Bottom-line assessment

**How emdash works, in one sentence:**  
Emdash is an Electron-based orchestration shell that provisions git-isolated local or remote workspaces, launches provider-specific coding agent CLIs inside managed PTY sessions, converts their behavior into structured task/conversation state via hooks and output parsing, and wraps the surrounding software workflow with search, diffs, PR sync, and UI review tooling.

**What it does well:**
- strong architectural split between main/preload/renderer/shared
- excellent use of provider and project abstractions
- real git worktree isolation
- local + SSH parity ambition
- thoughtful workflow features beyond just “run an agent”

**Where the likely method gaps are:**
- provider heterogeneity
- brittle prompt/event transport
- provisioning complexity without explicit workflow state machine
- limited observability relative to orchestration complexity
- config and environment provenance

If you want, next I can turn this into one of these:

1. **a stage-by-stage improvement matrix**  
   - for each stage: current method, weakness, recommended replacement

2. **a system diagram**
   - textual architecture diagram from app boot to agent output

3. **a prioritized roadmap**
   - “best 10 improvements to implement first”

4. **a file-by-file deep audit plan**
   - exact files to inspect next for each subsystem

If you want maximum usefulness, I’d recommend I do **#1 + #3** next.
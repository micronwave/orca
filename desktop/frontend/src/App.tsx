import { useCallback, useEffect, useReducer, useRef, useState } from 'react'
import type {
  DashboardState, GoalView, ObligationView, CapsuleView,
  PatchView, EvidenceView, FailureView, PendingGate, BudgetSummary,
  TimelineEntry, SetupHealthView,
} from './types'
import {
  GetGoal, ListObligations, ListCapsules, ListPatches,
  ListEvidence, ListFailures, GetMergeReadiness,
  GetBlockedDecisions, GetBudgetSummary, GetTimeline, GetSetupHealth,
} from '../wailsjs/go/main/App'
import { EventsOn } from '../wailsjs/runtime/runtime'

// ── State management ──────────────────────────────────────────────────────────

type Action =
  | { type: 'LOADING' }
  | { type: 'LOADED'; payload: Partial<DashboardState> }
  | { type: 'ERROR'; error: string }
  | { type: 'SELECT_PATCH'; patchID: string | null }
  | { type: 'PATCH_EVIDENCE'; evidence: EvidenceView[] }

function reducer(state: DashboardState, action: Action): DashboardState {
  switch (action.type) {
    case 'LOADING': return { ...state, loading: true, error: null }
    case 'LOADED': return { ...state, loading: false, error: null, ...action.payload }
    case 'ERROR': return { ...state, loading: false, error: action.error }
    case 'SELECT_PATCH': return { ...state, selectedPatch: action.patchID, patchEvidence: [] }
    case 'PATCH_EVIDENCE': return { ...state, patchEvidence: action.evidence }
    default: return state
  }
}

const initial: DashboardState = {
  goal: null, obligations: [], capsules: [], patches: [], failures: [],
  blockedDecisions: [], mergeReadiness: 'unknown', budget: null,
  timeline: [], setupHealth: null,
  loading: true, error: null, selectedPatch: null, patchEvidence: [],
}

// ── Root component ────────────────────────────────────────────────────────────

export default function App() {
  const [state, dispatch] = useReducer(reducer, initial)
  const loadingRef = useRef(false)

  const loadAll = useCallback(async () => {
    if (loadingRef.current) return
    loadingRef.current = true
    dispatch({ type: 'LOADING' })
    try {
      const [goal, obligations, capsules, patches, failures,
             blockedDecisions, mergeReadiness, budget,
             timeline, setupHealth] =
        await Promise.all([
          GetGoal(),
          ListObligations(),
          ListCapsules(),
          ListPatches(),
          ListFailures(),
          GetBlockedDecisions(),
          GetMergeReadiness(),
          GetBudgetSummary(),
          GetTimeline(),
          GetSetupHealth(),
        ])
      dispatch({
        type: 'LOADED',
        payload: {
          goal: goal ?? null,
          obligations: obligations ?? [],
          capsules: capsules ?? [],
          patches: patches ?? [],
          failures: failures ?? [],
          blockedDecisions: blockedDecisions ?? [],
          mergeReadiness: mergeReadiness ?? 'unknown',
          budget: budget ?? null,
          timeline: timeline ?? [],
          setupHealth: setupHealth ?? null,
        },
      })
    } catch (e) {
      dispatch({ type: 'ERROR', error: String(e) })
    } finally {
      loadingRef.current = false
    }
  }, [])

  const selectPatch = useCallback(async (patchID: string | null) => {
    dispatch({ type: 'SELECT_PATCH', patchID })
    if (!patchID) return
    try {
      const ev = await ListEvidence(patchID)
      dispatch({ type: 'PATCH_EVIDENCE', evidence: ev ?? [] })
    } catch {
      dispatch({ type: 'PATCH_EVIDENCE', evidence: [] })
    }
  }, [])

  useEffect(() => {
    loadAll()
    const interval = setInterval(loadAll, 10_000)
    const off = EventsOn('state:refresh', loadAll)
    return () => { clearInterval(interval); off() }
  }, [loadAll])

  return (
    <>
      {state.error && <div className="error-bar">{state.error}</div>}
      <Header goal={state.goal} readiness={state.mergeReadiness} />
      {state.loading && !state.goal ? (
        <div className="loading">Loading…</div>
      ) : (
        <div className="layout">
          <LeftCol
            goal={state.goal}
            mergeReadiness={state.mergeReadiness}
            blockedDecisions={state.blockedDecisions}
            capsules={state.capsules}
            setupHealth={state.setupHealth}
          />
          <TimelineCol timeline={state.timeline} />
          <RightCol
            obligations={state.obligations}
            capsules={state.capsules}
            patches={state.patches}
            failures={state.failures}
            budget={state.budget}
            selectedPatch={state.selectedPatch}
            patchEvidence={state.patchEvidence}
            onSelectPatch={selectPatch}
          />
        </div>
      )}
    </>
  )
}

// ── Header ────────────────────────────────────────────────────────────────────

function readinessBadgeClass(r: string): string {
  switch (r) {
    case 'ready': return 'badge badge-ready'
    case 'blocked': return 'badge badge-blocked'
    case 'needs_human_review': return 'badge badge-review'
    case 'pending_reconciliation': return 'badge badge-pending'
    default: return 'badge badge-unknown'
  }
}

function Header({ goal, readiness }: { goal: GoalView | null; readiness: string }) {
  return (
    <div className="header">
      <h1>Orca</h1>
      {goal && (
        <>
          <span className="badge badge-status">{goal.status}</span>
          <span className="intent" title={goal.original_intent}>{goal.original_intent}</span>
        </>
      )}
      {!goal && <span className="intent">No active goal</span>}
      <span className={readinessBadgeClass(readiness)}>{readiness.replace(/_/g, ' ')}</span>
    </div>
  )
}

// ── Left column — primary screen ──────────────────────────────────────────────

const ACTIVE_CAPSULE_STATES = new Set([
  'pending', 'worktree_created', 'workspace_attached', 'setup_run', 'agent_running',
])

function getActiveCapsule(capsules: CapsuleView[]): CapsuleView | null {
  return [...capsules].reverse().find(c => ACTIVE_CAPSULE_STATES.has(c.state)) ?? null
}

function deriveNextAction(
  readiness: string,
  gates: PendingGate[],
  goal: GoalView | null,
): { label: string; urgent: boolean } {
  if (!goal) return { label: 'Submit a goal to begin', urgent: false }
  if (gates.length > 0) {
    const label = gates[0].gate_type === 'projection_review'
      ? 'Projection review needed'
      : 'Merge review needed'
    return { label, urgent: true }
  }
  switch (readiness) {
    case 'ready': return { label: 'Ready to merge', urgent: false }
    case 'applied': return { label: 'Goal complete — merge applied', urgent: false }
    case 'blocked': return { label: 'Blocked — review failures', urgent: true }
    case 'pending_reconciliation': return { label: 'Reconciling patch…', urgent: false }
    case 'needs_human_review': return { label: 'Awaiting human review', urgent: true }
    default: return { label: goal.status === 'active' ? 'Planning…' : goal.status, urgent: false }
  }
}

function LeftCol({ goal, mergeReadiness, blockedDecisions, capsules, setupHealth }: {
  goal: GoalView | null
  mergeReadiness: string
  blockedDecisions: PendingGate[]
  capsules: CapsuleView[]
  setupHealth: SetupHealthView | null
}) {
  const activeCapsule = getActiveCapsule(capsules)
  const next = deriveNextAction(mergeReadiness, blockedDecisions, goal)
  return (
    <div className="col">
      <GoalCard goal={goal} />
      <NextActionCard next={next} blockedDecisions={blockedDecisions} readiness={mergeReadiness} />
      {activeCapsule && <ActiveCapsuleCard capsule={activeCapsule} />}
      {setupHealth?.warning && <SetupWarningCard warning={setupHealth.warning} />}
    </div>
  )
}

function GoalCard({ goal }: { goal: GoalView | null }) {
  return (
    <div className="card">
      <div className="card-header">
        Goal
        {goal && <span className={`chip chip-${goal.risk_level}`}>{goal.risk_level} risk</span>}
      </div>
      <div className="card-body">
        {!goal ? (
          <div className="empty">No goal found. Run <code>orca init</code> and <code>orca goal</code>.</div>
        ) : (
          <>
            <div style={{ marginBottom: 6, fontWeight: 500 }}>{goal.original_intent}</div>
            <div className="mono detail-id">{goal.goal_id.slice(0, 12)}</div>
            {goal.conditions.length > 0 && (
              <details className="conditions-details">
                <summary className="conditions-summary">
                  Conditions ({goal.conditions.length})
                </summary>
                {goal.conditions.map(c => (
                  <div className="condition-item" key={c.id}>
                    <span className={`chip chip-${conditionStatusClass(c.status)}`}>{c.status}</span>
                    <span>{c.description}</span>
                  </div>
                ))}
              </details>
            )}
          </>
        )}
      </div>
    </div>
  )
}

function conditionStatusClass(status: string): string {
  switch (status) {
    case 'met': return 'satisfied'
    case 'unmet': return 'open'
    case 'blocked': return 'failed'
    default: return 'open'
  }
}

function NextActionCard({ next, blockedDecisions, readiness }: {
  next: { label: string; urgent: boolean }
  blockedDecisions: PendingGate[]
  readiness: string
}) {
  return (
    <div className={`card${next.urgent ? ' card-urgent' : ''}`}>
      <div className="card-header">Next Action</div>
      <div className="card-body">
        <div className={`next-action${next.urgent ? ' next-urgent' : ''}`}>{next.label}</div>
        {blockedDecisions.length > 0 && (
          <div style={{ marginTop: 8 }}>
            {blockedDecisions.map((g, i) => (
              <div className="gate-item" key={i}>
                <div className="gate-type">{g.gate_type.replace(/_/g, ' ')}</div>
                <div className="mono detail-id">{g.related_id.slice(0, 12)}</div>
                {g.reason && (
                  <div style={{ marginTop: 2, color: 'var(--text-muted)', fontSize: 11 }}>{g.reason}</div>
                )}
              </div>
            ))}
          </div>
        )}
        <div className="readiness-row">
          <span className={readinessBadgeClass(readiness)}>{readiness.replace(/_/g, ' ')}</span>
        </div>
      </div>
    </div>
  )
}

function ActiveCapsuleCard({ capsule }: { capsule: CapsuleView }) {
  return (
    <div className="card">
      <div className="card-header">Active Capsule</div>
      <div className="card-body">
        <div style={{ display: 'flex', gap: 6, alignItems: 'center', marginBottom: 4 }}>
          <span className={`chip chip-${capsuleStateClass(capsule.state)}`}>
            {capsule.state.replace(/_/g, ' ')}
          </span>
          <span style={{ fontSize: 11, color: 'var(--text-muted)' }}>{capsule.agent}</span>
          <span style={{ fontSize: 11, color: 'var(--text-muted)' }}>{capsule.role}</span>
        </div>
        <div className="mono detail-id">{capsule.capsule_id.slice(0, 12)}</div>
        {capsule.max_tokens > 0 && (
          <div style={{ fontSize: 11, color: 'var(--text-muted)', marginTop: 4 }}>
            Budget: {capsule.max_tokens.toLocaleString()} tokens
          </div>
        )}
      </div>
    </div>
  )
}

function capsuleStateClass(state: string): string {
  switch (state) {
    case 'completed': return 'completed'
    case 'failed': return 'failed-c'
    case 'agent_running': return 'running'
    default: return 'pending'
  }
}

function SetupWarningCard({ warning }: { warning: string }) {
  return (
    <div className="card card-warning">
      <div className="card-header">Setup Warning</div>
      <div className="card-body">
        <div style={{ color: 'var(--yellow)', fontSize: 12 }}>{warning}</div>
      </div>
    </div>
  )
}

// ── Timeline column ───────────────────────────────────────────────────────────

function TimelineCol({ timeline }: { timeline: TimelineEntry[] }) {
  return (
    <div className="col">
      <TimelineCard timeline={timeline} />
    </div>
  )
}

function formatAt(at: string): string {
  if (!at) return ''
  try {
    return new Date(at).toLocaleTimeString(undefined, {
      hour: '2-digit', minute: '2-digit', second: '2-digit',
    })
  } catch {
    return ''
  }
}

function TimelineCard({ timeline }: { timeline: TimelineEntry[] }) {
  return (
    <div className="card timeline-card">
      <div className="card-header">
        Timeline
        {timeline.length > 0 && <span className="count">{timeline.length}</span>}
      </div>
      <div className="card-body timeline-body">
        {timeline.length === 0 ? (
          <div className="empty">No events yet. Submit a goal to begin.</div>
        ) : (
          <div className="timeline">
            {timeline.map((e, i) => (
              <div key={i} className={`timeline-step step-${e.status || 'default'}`}>
                <div className="step-icon">
                  <div className="step-dot" />
                  {i < timeline.length - 1 && <div className="step-line" />}
                </div>
                <div className="step-content">
                  <div className="step-summary">{e.summary}</div>
                  <div className="step-time">{formatAt(e.at)}</div>
                </div>
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  )
}

// ── Right column — detail tabs ────────────────────────────────────────────────

type TabName = 'obligations' | 'capsules' | 'patches' | 'failures' | 'budget'

function RightCol({ obligations, capsules, patches, failures, budget,
                    selectedPatch, patchEvidence, onSelectPatch }: {
  obligations: ObligationView[]
  capsules: CapsuleView[]
  patches: PatchView[]
  failures: FailureView[]
  budget: BudgetSummary | null
  selectedPatch: string | null
  patchEvidence: EvidenceView[]
  onSelectPatch: (id: string | null) => void
}) {
  const [activeTab, setActiveTab] = useState<TabName>('obligations')
  const tabs: Array<{ name: TabName; count: number }> = [
    { name: 'obligations', count: obligations.length },
    { name: 'capsules', count: capsules.length },
    { name: 'patches', count: patches.length },
    { name: 'failures', count: failures.length },
    { name: 'budget', count: 0 },
  ]
  return (
    <div className="col col-tabs">
      <div className="card detail-tabs">
        <div className="tab-bar">
          {tabs.map(t => (
            <button
              key={t.name}
              className={`tab-btn${activeTab === t.name ? ' active' : ''}`}
              onClick={() => setActiveTab(t.name)}
            >
              {t.name}
              {t.count > 0 && <span className="tab-count">{t.count}</span>}
            </button>
          ))}
        </div>
        <div className="tab-content">
          {activeTab === 'obligations' && <ObligationsTab obligations={obligations} />}
          {activeTab === 'capsules' && <CapsulesTab capsules={capsules} />}
          {activeTab === 'patches' && (
            <PatchesTab
              patches={patches}
              selectedPatch={selectedPatch}
              patchEvidence={patchEvidence}
              onSelectPatch={onSelectPatch}
            />
          )}
          {activeTab === 'failures' && <FailuresTab failures={failures} />}
          {activeTab === 'budget' && <BudgetTab budget={budget} />}
        </div>
      </div>
    </div>
  )
}

// ── Tab content components ────────────────────────────────────────────────────

function ObligationsTab({ obligations }: { obligations: ObligationView[] }) {
  if (obligations.length === 0) return <div className="empty tab-empty">None</div>
  return (
    <table className="tbl">
      <thead>
        <tr>
          <th>ID</th>
          <th>Status</th>
          <th>Risk</th>
          <th>Description</th>
        </tr>
      </thead>
      <tbody>
        {obligations.map(o => (
          <tr key={o.obligation_id}>
            <td className="mono">{o.obligation_id.slice(0, 12)}</td>
            <td><span className={`chip chip-${o.status}`}>{o.status}</span></td>
            <td><span className={`chip chip-${o.risk_level}`}>{o.risk_level}</span></td>
            <td title={o.description}>{truncate(o.description, 60)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

function CapsulesTab({ capsules }: { capsules: CapsuleView[] }) {
  if (capsules.length === 0) return <div className="empty tab-empty">None</div>
  return (
    <table className="tbl">
      <thead>
        <tr>
          <th>ID</th>
          <th>Agent</th>
          <th>Role</th>
          <th>State</th>
        </tr>
      </thead>
      <tbody>
        {capsules.map(c => (
          <tr key={c.capsule_id}>
            <td className="mono">{c.capsule_id.slice(0, 12)}</td>
            <td>{c.agent}</td>
            <td>{c.role}</td>
            <td>
              <span className={`chip chip-${capsuleStateClass(c.state)}`}>
                {c.state.replace(/_/g, ' ')}
              </span>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

function PatchesTab({ patches, selectedPatch, patchEvidence, onSelectPatch }: {
  patches: PatchView[]
  selectedPatch: string | null
  patchEvidence: EvidenceView[]
  onSelectPatch: (id: string | null) => void
}) {
  return (
    <div>
      {patches.length === 0 ? (
        <div className="empty tab-empty">None</div>
      ) : (
        <>
          <table className="tbl">
            <thead>
              <tr>
                <th>ID</th>
                <th>Status</th>
                <th>Tokens</th>
                <th>Files</th>
              </tr>
            </thead>
            <tbody>
              {patches.map(p => (
                <tr
                  key={p.patch_id}
                  className={`patch-row${selectedPatch === p.patch_id ? ' selected' : ''}`}
                  onClick={() => onSelectPatch(selectedPatch === p.patch_id ? null : p.patch_id)}
                  title={p.summary || p.patch_id}
                >
                  <td className="mono">{p.patch_id.slice(0, 12)}</td>
                  <td><span className={`chip chip-${p.status}`}>{p.status}</span></td>
                  <td className="mono">{p.tokens_used > 0 ? p.tokens_used.toLocaleString() : '—'}</td>
                  <td>{p.changed_files?.length ?? 0}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <div className="tab-hint">Click a row to view evidence</div>
        </>
      )}
      {selectedPatch && (
        <div style={{ marginTop: 8, padding: '0 6px' }}>
          <div className="evidence-header">
            Evidence — <span className="mono">{selectedPatch.slice(0, 12)}</span>
            <span className="count">{patchEvidence.length}</span>
          </div>
          {patchEvidence.length === 0 ? (
            <div className="empty tab-empty">No evidence for this patch</div>
          ) : (
            patchEvidence.map(e => (
              <div className="evidence-item" key={e.evidence_id}>
                <div style={{ display: 'flex', gap: 6, alignItems: 'center', marginBottom: 2 }}>
                  <span className={`chip chip-${e.exit_code === 0 ? 'satisfied' : 'failed'}`}>
                    {e.exit_code === 0 ? 'pass' : `exit ${e.exit_code}`}
                  </span>
                  <span style={{ fontSize: 11, color: 'var(--text-muted)' }}>{e.type}</span>
                  {e.reused_from_id && (
                    <span style={{ fontSize: 10, color: 'var(--accent)' }}>reused</span>
                  )}
                </div>
                {e.command && <div className="evidence-command">{e.command}</div>}
                {e.summary && <div className="evidence-summary">{e.summary}</div>}
                {e.raw_log_path && (
                  <details>
                    <summary style={{ fontSize: 11, color: 'var(--text-muted)', cursor: 'pointer' }}>
                      log: {e.raw_log_path}
                    </summary>
                  </details>
                )}
                {e.inline_output && (
                  <details>
                    <summary style={{ fontSize: 11, color: 'var(--text-muted)', cursor: 'pointer' }}>
                      output
                    </summary>
                    <pre className="evidence-output">{e.inline_output}</pre>
                  </details>
                )}
              </div>
            ))
          )}
        </div>
      )}
    </div>
  )
}

function FailuresTab({ failures }: { failures: FailureView[] }) {
  if (failures.length === 0) return <div className="empty tab-empty">None</div>
  return (
    <table className="tbl">
      <thead>
        <tr>
          <th>Type</th>
          <th>Summary</th>
          <th>Attempts</th>
        </tr>
      </thead>
      <tbody>
        {failures.map(f => (
          <tr key={f.failure_id} title={f.error_signature}>
            <td><span className="chip chip-failed">{f.failure_type}</span></td>
            <td>{truncate(f.summary, 60)}</td>
            <td>{f.prior_attempt_count}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

function BudgetTab({ budget }: { budget: BudgetSummary | null }) {
  if (!budget) return <div className="empty tab-empty">No budget records</div>
  const rows: Array<[string, string | number]> = [
    ['Tokens spent', budget.total_tokens_spent.toLocaleString()],
    ['Wall time (s)', budget.total_wall_time_seconds.toFixed(1)],
    ['Tool calls', budget.total_tool_calls],
    ['Retries', budget.total_retries],
    ['Obligations discharged', budget.total_obligations_discharged],
    ['Patches accepted', budget.total_patches_accepted],
    ['Patches rejected', budget.total_patches_rejected],
  ]
  return (
    <div style={{ padding: 10 }}>
      {rows.map(([label, val]) => (
        <div className="budget-row" key={label}>
          <span>{label}</span>
          <span className="budget-val">{val}</span>
        </div>
      ))}
    </div>
  )
}

// ── Utility ───────────────────────────────────────────────────────────────────

function truncate(s: string, max: number): string {
  if (!s) return ''
  return s.length <= max ? s : s.slice(0, max - 1) + '…'
}

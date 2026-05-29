import { useCallback, useEffect, useReducer, useRef } from 'react'
import type {
  DashboardState, GoalView, ObligationView, CapsuleView,
  PatchView, EvidenceView, FailureView, PendingGate, BudgetSummary,
} from './types'
import {
  GetGoal, ListObligations, ListCapsules, ListPatches,
  ListEvidence, ListFailures, GetMergeReadiness,
  GetBlockedDecisions, GetBudgetSummary,
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
      const [goal, obligations, capsules, patches, failures, blockedDecisions, mergeReadiness, budget] =
        await Promise.all([
          GetGoal(),
          ListObligations(),
          ListCapsules(),
          ListPatches(),
          ListFailures(),
          GetBlockedDecisions(),
          GetMergeReadiness(),
          GetBudgetSummary(),
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
        },
      })
    } catch (e) {
      dispatch({ type: 'ERROR', error: String(e) })
    } finally {
      loadingRef.current = false
    }
  }, [])

  // Load evidence when a patch is selected.
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
    // Poll every 10 s as a fallback.
    const interval = setInterval(loadAll, 10_000)
    // Listen for Wails event-log tail signal.
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
          <LeftCol goal={state.goal} budget={state.budget} blockedDecisions={state.blockedDecisions} />
          <MiddleCol obligations={state.obligations} capsules={state.capsules} />
          <RightCol
            patches={state.patches}
            failures={state.failures}
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
          <span className={`badge badge-status`}>{goal.status}</span>
          <span className="intent" title={goal.original_intent}>{goal.original_intent}</span>
        </>
      )}
      {!goal && <span className="intent">No active goal</span>}
      <span className={readinessBadgeClass(readiness)}>{readiness.replace(/_/g, ' ')}</span>
    </div>
  )
}

// ── Left column ───────────────────────────────────────────────────────────────

function LeftCol({ goal, budget, blockedDecisions }: {
  goal: GoalView | null
  budget: BudgetSummary | null
  blockedDecisions: PendingGate[]
}) {
  return (
    <div className="col">
      <GoalCard goal={goal} />
      <MergeReadinessCard blockedDecisions={blockedDecisions} />
      <BudgetCard budget={budget} />
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
            <div className="mono" style={{ marginBottom: 8 }}>{goal.goal_id}</div>
            {goal.conditions.length > 0 && (
              <>
                <div style={{ fontSize: 11, fontWeight: 600, color: 'var(--text-muted)', marginBottom: 4 }}>
                  CONDITIONS
                </div>
                {goal.conditions.map(c => (
                  <div className="condition-item" key={c.id}>
                    <span className={`chip chip-${conditionStatusClass(c.status)}`}>{c.status}</span>
                    <span>{c.description}</span>
                  </div>
                ))}
              </>
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

function MergeReadinessCard({ blockedDecisions }: { blockedDecisions: PendingGate[] }) {
  return (
    <div className="card">
      <div className="card-header">
        Blocked Decisions
        {blockedDecisions.length > 0 && <span className="count">{blockedDecisions.length}</span>}
      </div>
      <div className="card-body">
        {blockedDecisions.length === 0 ? (
          <div className="empty">None</div>
        ) : (
          blockedDecisions.map((g, i) => (
            <div className="gate-item" key={i}>
              <div className="gate-type">{g.gate_type.replace(/_/g, ' ')}</div>
              <div className="mono">{g.related_id}</div>
              {g.reason && <div style={{ marginTop: 2, color: 'var(--text-muted)', fontSize: 11 }}>{g.reason}</div>}
            </div>
          ))
        )}
      </div>
    </div>
  )
}

function BudgetCard({ budget }: { budget: BudgetSummary | null }) {
  if (!budget) {
    return (
      <div className="card">
        <div className="card-header">Cost</div>
        <div className="card-body"><div className="empty">No budget records</div></div>
      </div>
    )
  }
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
    <div className="card">
      <div className="card-header">Cost</div>
      <div className="card-body">
        {rows.map(([label, val]) => (
          <div className="budget-row" key={label}>
            <span>{label}</span>
            <span className="budget-val">{val}</span>
          </div>
        ))}
      </div>
    </div>
  )
}

// ── Middle column ─────────────────────────────────────────────────────────────

function MiddleCol({ obligations, capsules }: {
  obligations: ObligationView[]
  capsules: CapsuleView[]
}) {
  return (
    <div className="col">
      <ObligationsCard obligations={obligations} />
      <CapsulesCard capsules={capsules} />
    </div>
  )
}

function ObligationsCard({ obligations }: { obligations: ObligationView[] }) {
  return (
    <div className="card">
      <div className="card-header">
        Obligations
        <span className="count">{obligations.length}</span>
      </div>
      <div className="card-body" style={{ padding: 0 }}>
        {obligations.length === 0 ? (
          <div className="empty" style={{ padding: 8 }}>None</div>
        ) : (
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
                  <td className="mono">{o.obligation_id}</td>
                  <td><span className={`chip chip-${o.status}`}>{o.status}</span></td>
                  <td><span className={`chip chip-${o.risk_level}`}>{o.risk_level}</span></td>
                  <td title={o.description}>{truncate(o.description, 60)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  )
}

function CapsulesCard({ capsules }: { capsules: CapsuleView[] }) {
  return (
    <div className="card">
      <div className="card-header">
        Capsules
        <span className="count">{capsules.length}</span>
      </div>
      <div className="card-body" style={{ padding: 0 }}>
        {capsules.length === 0 ? (
          <div className="empty" style={{ padding: 8 }}>None</div>
        ) : (
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
                  <td className="mono">{c.capsule_id.slice(0, 14)}</td>
                  <td>{c.agent}</td>
                  <td>{c.role}</td>
                  <td><span className={`chip chip-${capsuleStateClass(c.state)}`}>{c.state.replace(/_/g, ' ')}</span></td>
                </tr>
              ))}
            </tbody>
          </table>
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

// ── Right column ──────────────────────────────────────────────────────────────

function RightCol({ patches, failures, selectedPatch, patchEvidence, onSelectPatch }: {
  patches: PatchView[]
  failures: FailureView[]
  selectedPatch: string | null
  patchEvidence: EvidenceView[]
  onSelectPatch: (id: string | null) => void
}) {
  return (
    <div className="col">
      <PatchesCard patches={patches} selectedPatch={selectedPatch} onSelectPatch={onSelectPatch} />
      {selectedPatch && (
        <EvidenceCard evidence={patchEvidence} patchID={selectedPatch} />
      )}
      <FailuresCard failures={failures} />
    </div>
  )
}

function PatchesCard({ patches, selectedPatch, onSelectPatch }: {
  patches: PatchView[]
  selectedPatch: string | null
  onSelectPatch: (id: string | null) => void
}) {
  return (
    <div className="card">
      <div className="card-header">
        Patches
        <span className="count">{patches.length}</span>
      </div>
      <div className="card-body" style={{ padding: 0 }}>
        {patches.length === 0 ? (
          <div className="empty" style={{ padding: 8 }}>None</div>
        ) : (
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
                  <td className="mono">{p.patch_id.slice(0, 14)}</td>
                  <td><span className={`chip chip-${p.status}`}>{p.status}</span></td>
                  <td className="mono">{p.tokens_used > 0 ? p.tokens_used.toLocaleString() : '—'}</td>
                  <td>{p.changed_files?.length ?? 0}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {patches.length > 0 && (
          <div style={{ padding: '4px 10px', fontSize: 11, color: 'var(--text-muted)' }}>
            Click a row to view evidence
          </div>
        )}
      </div>
    </div>
  )
}

function EvidenceCard({ evidence, patchID }: { evidence: EvidenceView[]; patchID: string }) {
  return (
    <div className="card">
      <div className="card-header">
        Evidence — <span className="mono">{patchID.slice(0, 14)}</span>
        <span className="count">{evidence.length}</span>
      </div>
      <div className="card-body">
        {evidence.length === 0 ? (
          <div className="empty">No evidence for this patch</div>
        ) : (
          evidence.map(e => (
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
                <div className="mono" title={e.raw_log_path} style={{ marginTop: 2 }}>
                  log: {e.raw_log_path}
                </div>
              )}
              {e.inline_output && (
                <details>
                  <summary style={{ fontSize: 11, color: 'var(--text-muted)', cursor: 'pointer' }}>
                    output
                  </summary>
                  <pre style={{ fontSize: 11, marginTop: 4, whiteSpace: 'pre-wrap', color: 'var(--text-muted)' }}>
                    {e.inline_output}
                  </pre>
                </details>
              )}
            </div>
          ))
        )}
      </div>
    </div>
  )
}

function FailuresCard({ failures }: { failures: FailureView[] }) {
  return (
    <div className="card">
      <div className="card-header">
        Failures
        <span className="count">{failures.length}</span>
      </div>
      <div className="card-body" style={{ padding: 0 }}>
        {failures.length === 0 ? (
          <div className="empty" style={{ padding: 8 }}>None</div>
        ) : (
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
                  <td><span className={`chip chip-failed`}>{f.failure_type}</span></td>
                  <td>{truncate(f.summary, 60)}</td>
                  <td>{f.prior_attempt_count}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  )
}

// ── Utility ───────────────────────────────────────────────────────────────────

function truncate(s: string, max: number): string {
  if (!s) return ''
  return s.length <= max ? s : s.slice(0, max - 1) + '…'
}

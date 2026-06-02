export interface ConditionView {
  id: string
  description: string
  status: string
}

export interface GoalView {
  goal_id: string
  original_intent: string
  status: string
  risk_level: string
  conditions: ConditionView[]
  created_at: string
}

export interface ObligationView {
  obligation_id: string
  goal_condition_id: string
  description: string
  status: string
  blocking: boolean
  risk_level: string
  satisfied_by: string[]
}

export interface CapsuleView {
  capsule_id: string
  agent: string
  role: string
  state: string
  worktree_path: string
  max_tokens: number
  max_wall_time_seconds: number
  topology_decision_id: string
}

export interface PatchView {
  patch_id: string
  capsule_id: string
  status: string
  summary: string
  changed_files: string[]
  obligation_ids_claimed: string[]
  tokens_used: number
  wall_time_seconds: number
  base_commit: string
  diff_path: string
}

export interface EvidenceView {
  evidence_id: string
  type: string
  source: string
  command: string
  exit_code: number
  summary: string
  raw_log_path: string
  inline_output: string
  supports: string[]
  reused_from_id: string
  created_at: string
}

export interface FailureView {
  failure_id: string
  source_capsule_id: string
  failure_type: string
  summary: string
  affected_files: string[]
  error_signature: string
  prior_attempt_count: number
  recommended_next_action: string
}

export interface BudgetView {
  budget_id: string
  goal_id: string
  capsule_id: string
  obligation_id: string
  tokens_spent: number
  wall_time_seconds: number
  tool_calls: number
  retries: number
  obligations_discharged: number
  patches_accepted: number
  patches_rejected: number
}

export interface BudgetSummary {
  total_tokens_spent: number
  total_wall_time_seconds: number
  total_tool_calls: number
  total_retries: number
  total_obligations_discharged: number
  total_patches_accepted: number
  total_patches_rejected: number
}

export interface DecisionView {
  decision_id: string
  context: string
  decision: string
  rationale: string
  made_by: string
  related_ids: string[]
  created_at: string
}

export interface PendingGate {
  gate_type: string
  related_id: string
  reason: string
}

export interface TimelineEntry {
  at: string
  type: string
  summary: string
  status: string // "ok" | "error" | "warning" | ""
}

export interface SetupHealthView {
  config_exists: boolean
  event_log_exists: boolean
  warning?: string
}

export interface DashboardState {
  goal: GoalView | null
  obligations: ObligationView[]
  capsules: CapsuleView[]
  patches: PatchView[]
  failures: FailureView[]
  blockedDecisions: PendingGate[]
  mergeReadiness: string
  budget: BudgetSummary | null
  timeline: TimelineEntry[]
  setupHealth: SetupHealthView | null
  loading: boolean
  error: string | null
  selectedPatch: string | null
  patchEvidence: EvidenceView[]
}

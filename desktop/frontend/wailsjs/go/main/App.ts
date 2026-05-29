// Stub bindings for standalone npm build.
// Wails replaces these with App.js + App.d.ts during wails build/dev.

import type {
  GoalView, ObligationView, CapsuleView, PatchView,
  EvidenceView, FailureView, BudgetView, BudgetSummary,
  DecisionView, PendingGate
} from '../../../src/types'

// Returns a never-resolving promise when called outside a Wails context.
function stub<T>(name: string): Promise<T> {
  console.warn(`[orca-stub] ${name} called outside Wails runtime`)
  return new Promise(() => {})
}

export function GetGoal(): Promise<GoalView | null> {
  return stub('GetGoal')
}
export function ListObligations(): Promise<ObligationView[]> {
  return stub('ListObligations')
}
export function ListCapsules(): Promise<CapsuleView[]> {
  return stub('ListCapsules')
}
export function ListPatches(): Promise<PatchView[]> {
  return stub('ListPatches')
}
export function ListEvidence(patchID: string): Promise<EvidenceView[]> {
  void patchID
  return stub('ListEvidence')
}
export function ListFailures(): Promise<FailureView[]> {
  return stub('ListFailures')
}
export function GetBudget(capsuleID: string): Promise<BudgetView | null> {
  void capsuleID
  return stub('GetBudget')
}
export function GetBudgetSummary(): Promise<BudgetSummary | null> {
  return stub('GetBudgetSummary')
}
export function GetMergeReadiness(): Promise<string> {
  return stub('GetMergeReadiness')
}
export function GetBlockedDecisions(): Promise<PendingGate[]> {
  return stub('GetBlockedDecisions')
}
export function ListDecisions(): Promise<DecisionView[]> {
  return stub('ListDecisions')
}
export function SetOrcaDir(dir: string): Promise<void> {
  void dir
  return stub('SetOrcaDir')
}
export function GetOrcaDir(): Promise<string> {
  return stub('GetOrcaDir')
}

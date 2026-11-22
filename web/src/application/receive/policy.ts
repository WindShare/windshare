import type { ReceiveLifecycleState } from '../../output/workspace'
import { isTerminalLifecycleState } from '../../output/workspace'
import type { MaterializationPlan } from '../../transfer/intent'
import type {
  RecoverySummary,
} from '../../transfer/lifecycle/recovery-summary'
import type { LifecycleUserAction, ActiveReceiveControl, ReceiveSettlementWait } from './actions'

export interface ReceiveActionFacts {
  readonly state: ReceiveLifecycleState
  readonly plan: MaterializationPlan
  readonly activeControls?: readonly ActiveReceiveControl[]
  readonly interruption?: ReceiveSettlementWait | null
  readonly recoverySummary?: RecoverySummary | null
}

export function receiveLifecycleCategory(state: ReceiveLifecycleState): 'active' | 'retained' | 'terminal' {
  if (state.kind === 'resumable-start' || state.kind === 'resumable-receive' || state.kind === 'resumable-package' ||
      state.kind === 'waiting-to-save' ||
      (state.kind === 'download-started' && state.attemptKind === 'workspace')) return 'retained'
  if (state.kind === 'published' || state.kind === 'partial-directory' ||
      state.kind === 'restart-required' || state.kind === 'source-invalidated' || state.kind === 'discarded' ||
      state.kind === 'needs-attention' || state.kind === 'download-started') return 'terminal'
  return 'active'
}

export function availableReceiveActions(input: ReceiveActionFacts): readonly LifecycleUserAction[] {
  if (input.interruption != null) return Object.freeze([])
  const controls = input.activeControls ?? []
  if (controls.length > 0) {
    // A durable pause/completion can be observed before the writer has drained.
    if (receiveLifecycleCategory(input.state) !== 'active') return Object.freeze([])
    if (new Set(controls).size !== controls.length) throw new TypeError('active receive controls contain a duplicate action')
    return Object.freeze([...controls])
  }
  if (input.state.kind === 'resumable-receive' && input.state.payloadKind === 'file-set' &&
      input.plan.kind === 'direct-tree' && input.recoverySummary == null) return Object.freeze([])
  return Object.freeze(stableReceiveActions(input.state, input.plan.kind))
}

function stableReceiveActions(
  state: ReceiveLifecycleState,
  plan: MaterializationPlan['kind'],
): readonly LifecycleUserAction[] {
  switch (state.kind) {
    case 'resumable-start': return ['continue', 'discard']
    case 'resumable-receive':
      return plan === 'workspace-then-publish' ? ['continue', 'discard'] : ['continue', 'redownload']
    case 'resumable-package': return ['continue', 'discard']
    case 'waiting-to-save': return ['save', 'delete']
    case 'download-started': return state.attemptKind === 'workspace' ? ['redownload', 'delete'] : []
    case 'source-invalidated': return ['discard']
    default: return []
  }
}

export type NewReceiveOperationKind = 'direct-tree-to-zip'

export function newReceiveOperationKind(
  lifecycle: ReceiveLifecycleState | null,
  plan: MaterializationPlan | null,
): NewReceiveOperationKind | null {
  if (lifecycle === null || plan === null) return null
  if (plan.kind === 'direct-tree' && isTerminalLifecycleState(lifecycle)) return 'direct-tree-to-zip'
  return null
}

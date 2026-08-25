import type { FSATerminalMutationKind } from '../browser/mutation-coordination/model'
import type { FSASettlementTraceEvent } from './settlement'

export function terminalMutationKind(
  kind: 'pause' | 'stop' | 'settle',
): FSATerminalMutationKind {
  switch (kind) {
    case 'pause': return 'pause-operation'
    case 'stop': return 'stop-operation'
    case 'settle': return 'settle-operation'
  }
}

export function normalizedSettlementOutcome(
  outcome: Extract<
    FSASettlementTraceEvent,
    { name: 'receive.fsa.settlement.completed' }
  >['outcome'],
): 'published' | 'partial_directory' | 'resumable_receive' | 'discarded' | 'needs_attention' {
  return outcome === 'partial-directory' || outcome === 'resumable-receive' ||
      outcome === 'needs-attention'
    ? outcome.replace('-', '_') as
      | 'partial_directory'
      | 'resumable_receive'
      | 'needs_attention'
    : outcome
}

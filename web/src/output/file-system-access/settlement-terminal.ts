import type { FSATerminalMutationKind } from '../browser/mutation-coordination/model'
import type { CreateFileSystemAccessSettlementAuthorityOptions, FSASettlementTraceEvent } from './settlement'
import type { ReceiveLifecycleState } from '../workspace/state'
import { emitOutputTrace, outputTraceEvent, recordOutputException } from '../diagnostics'
import { completeFileSystemAccessPublishedCleanup } from './published-cleanup'
import type { FSAFinalSettlementObservation } from './session'

export async function cleanupTerminalFSAMetadata(
  input: Pick<CreateFileSystemAccessSettlementAuthorityOptions,
    'intent' | 'repository' | 'lifecycleLeaseId' | 'diagnostics'> & {
    readonly lifecycle: ReceiveLifecycleState
    readonly observation: Pick<FSAFinalSettlementObservation,
      'retireRecoveryMetadata' | 'clearCompatibleNamePendingOutcome'>
  },
): Promise<ReceiveLifecycleState> {
  const { lifecycle, observation, diagnostics } = input
  const cleanup = async () => {
    await observation.retireRecoveryMetadata()
    await observation.clearCompatibleNamePendingOutcome()
  }
  try {
    if (lifecycle.kind === 'published') {
      return (await completeFileSystemAccessPublishedCleanup({
        intent: input.intent, lifecycle, repository: input.repository,
        leaseId: input.lifecycleLeaseId,
        ...(diagnostics?.trace === undefined ? {} : { trace: diagnostics.trace }),
        cleanup,
      })).lifecycle
    }
    await cleanup()
  } catch (cleanupFailure) {
    // The publication receipt is durable; only metadata retirement remains retryable.
    recordOutputException(diagnostics?.failures?.cleanup, cleanupFailure)
    emitOutputTrace(diagnostics?.trace, () => outputTraceEvent('cleanup', {
      backend: 'file_system_access',
      transition: 'failed',
      operation_id: input.intent.operationId,
      receive_intent_digest: input.intent.digest,
      lifecycle_generation: lifecycle.generation.toString(),
    }))
  }
  return lifecycle
}

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

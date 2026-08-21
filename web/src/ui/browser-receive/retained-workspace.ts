import { openOriginPrivateRetainedArtifactBackend } from '../../output/origin-private/session'
import { TargetOwnershipUnknownError } from '../../output/persistent-tree/errors'
import type {
  AuthorityOwnedReceiveOperationContinuation,
  ReopenedWorkspaceOperation,
} from '../../output/resume/reopen-authority'
import type { WorkspaceStageTraceListener } from '../../output/workspace/stages'
import type {
  OutputDiagnosticsPorts,
  OutputFailureBinding,
  OutputTraceSource,
} from '../../output/diagnostics'
import { recordOutputException, emitOutputTrace, outputTraceEvent } from '../../output/diagnostics'
import { classificationForTransferFailure } from '../../transfer/job/failures'
import { V2TransferFailureSettlementError } from '../../transfer/settlement/v2-output'
import type { V2BoundReceiveOperation, V2RetainedReceiveAction } from '../v2-receive-runtime'
import type { BrowserReceiveWindow } from './contracts'
import { WorkspaceReceiveOperation } from './workspace-operation'
import { handoffRetainedWorkspacePackage } from './workspace-publication'
import { diagnosticsOption } from './retained-diagnostics'
import { bindRuntimeOutputFailures } from './retained'

type RetainedContinuationAction = Extract<V2RetainedReceiveAction, 'save' | 'redownload'>
type WorkspaceReceiveContinuation = Extract<
  AuthorityOwnedReceiveOperationContinuation,
  { readonly kind: 'workspace-receive' }
>

export async function resumeWorkspaceReceive(
  windowPort: BrowserReceiveWindow,
  continuation: WorkspaceReceiveContinuation,
  signal: AbortSignal,
  trace: WorkspaceStageTraceListener | undefined,
  outputTrace: OutputTraceSource | undefined,
  binding: OutputFailureBinding,
): Promise<V2BoundReceiveOperation> {
  const fallback = continuation.operation.receiveAdmissionFallback
  if (fallback === undefined) {
    throw new TypeError('Workspace continuation omitted its admission fallback')
  }
  try {
    const backend = await continuation.operation.receiveContinuation.openBackend({
      ...(trace === undefined ? {} : { onTrace: trace }),
      ...diagnosticsOption('origin_private', outputTrace, binding.sinks),
    })
    try {
      signal.throwIfAborted()
      const runtime = await WorkspaceReceiveOperation.reopen({
        windowPort,
        operation: continuation.operation,
        backend,
        ...(trace === undefined ? {} : { trace }),
        ...diagnosticsOption('origin_private', outputTrace, binding.sinks),
      })
      return bindRuntimeOutputFailures(runtime, binding)
    } catch (error) {
      let cleanupFailed = false
      let cleanupFailure: unknown
      try {
        await backend.close()
      } catch (caughtCleanupFailure) {
        cleanupFailed = true
        cleanupFailure = caughtCleanupFailure
      }
      if (cleanupFailed) {
        throw new AggregateError(
          [error, cleanupFailure],
          'Workspace continuation adoption and output cleanup both failed',
          { cause: error },
        )
      }
      throw error
    }
  } catch (error) {
    try {
      await settleWorkspaceReceiveAdmissionFailure(continuation, fallback, error)
    } catch (settlementError) {
      const consequence = classificationForTransferFailure(settlementError, {
        stage: 'settlement',
        relation: 'consequence',
      })
      if (consequence === undefined) throw error
      throw new V2TransferFailureSettlementError(
        classificationForTransferFailure(error, {
          stage: 'reopen',
          relation: 'contributor',
        }),
        [consequence],
      )
    }
    throw error
  }
}

async function settleWorkspaceReceiveAdmissionFailure(
  continuation: WorkspaceReceiveContinuation,
  fallback: NonNullable<WorkspaceReceiveContinuation['operation']['receiveAdmissionFallback']>,
  error: unknown,
): Promise<void> {
  if (!(error instanceof TargetOwnershipUnknownError)) {
    await continuation.operation.stages.restoreReceiveContinuation(fallback)
    return
  }
  if (error.operationId !== null &&
      error.operationId !== continuation.operation.intent.operationId) {
    throw new TypeError('Workspace recovery evidence belongs to another operation', {
      cause: error,
    })
  }
  await continuation.operation.stages.recordTargetOwnershipUnknown(
    continuation.operation.intent.digest,
  )
}

export async function continueRetainedWorkspaceOperation(
  windowPort: BrowserReceiveWindow,
  operation: ReopenedWorkspaceOperation,
  action: RetainedContinuationAction,
  signal: AbortSignal,
  diagnostics?: OutputDiagnosticsPorts,
): Promise<void> {
  signal.throwIfAborted()
  if ((action === 'save' && operation.lifecycle.kind !== 'waiting-to-save') ||
      (action === 'redownload' && (operation.lifecycle.kind !== 'download-started' ||
        operation.lifecycle.attemptKind !== 'workspace'))) {
    throw continuationMismatch()
  }
  const backend = await openOriginPrivateRetainedArtifactBackend({
    receiveIntent: operation.intent,
    operationRepository: operation.repository,
    namespace: operation.namespace,
    ...(diagnostics === undefined ? {} : { diagnostics }),
  }).catch((error: unknown) => {
    recordOutputException(diagnostics?.failures?.continuation, error)
    emitOutputTrace(diagnostics?.trace, () =>
      outputTraceEvent('continuation', {
        backend: 'origin_private',
        transition: 'admission_failed',
      }))
    throw error
  })
  let handoffFailed = false
  let handoffFailure: unknown
  try {
    signal.throwIfAborted()
    await handoffRetainedWorkspacePackage(windowPort, operation, backend, diagnostics)
  } catch (error) {
    handoffFailed = true
    handoffFailure = error
    if (!signal.aborted) {
      recordOutputException(diagnostics?.failures?.publication, error)
      emitOutputTrace(diagnostics?.trace, () =>
        outputTraceEvent('publication', {
          backend: 'origin_private',
          transition: 'unknown',
        }))
    }
  }

  let cleanupFailed = false
  let cleanupFailure: unknown
  try {
    await backend.close()
  } catch (error) {
    // The retained backend owns checkpoint-close classification and trace emission.
    cleanupFailed = true
    cleanupFailure = error
  }

  if (handoffFailed && cleanupFailed) {
    throw new AggregateError(
      [handoffFailure, cleanupFailure],
      'Retained handoff and output cleanup both failed',
    )
  }
  if (handoffFailed) throw handoffFailure
  if (cleanupFailed) throw cleanupFailure
}

export function continuationMismatch(): DOMException {
  return new DOMException(
    'Retained continuation does not match its reopened authority',
    'InvalidStateError',
  )
}

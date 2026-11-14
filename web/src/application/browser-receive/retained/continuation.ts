import { catchUpFileSystemAccessCompatibleNames } from '../../../output/file-system-access/settlement'
import { FSAReceiveOperation } from '../fsa/operation'
import { reopenFolderDeliveryContext } from '../fsa/folder-delivery'
import { WorkspaceReceiveOperation } from '../workspace/operation'
import { continueRetainedWorkspaceOperation, resumeWorkspaceReceive } from './workspace'
import { createOutputFailureBinding, recordOutputException, type OutputFailureSinks, type OutputTraceSource, type LocalOutputOperationFailureDiagnosticsPort } from '../../../output/diagnostics'
import { bindRuntimeOutputFailures, diagnosticsFor, diagnosticsOption } from './diagnostics'
import type { BrowserReceiveWindow } from '../../../output/browser/environment'
import type { WorkspaceStageTraceListener } from '../../../output/workspace/stages'
import type { BoundReceiveOperation, RetainedReceiveAction } from '../../receive/contracts'
import type { BrowserDirectZipCompositionPort } from '../direct-zip/contracts'
import type { AuthorityOwnedReceiveOperationContinuation } from '../../../output/resume/reopen-authority'
import type { BrowserRetainedContinuationExecutor } from './inventory'
import { unavailableRoute } from '../route-contract'
type RetainedContinuationAction = Extract<RetainedReceiveAction, 'save' | 'redownload'>
type DirectTreeCatchUpContinuation = Extract<AuthorityOwnedReceiveOperationContinuation, { kind: 'direct-tree-catch-up' }>
type ReceiveContinuation = Extract<AuthorityOwnedReceiveOperationContinuation, { kind: 'direct-tree-receive' | 'workspace-receive' | 'workspace-start' | 'direct-zip' }>
type WorkspacePackageContinuation = Extract<AuthorityOwnedReceiveOperationContinuation, { kind: 'workspace-package' }>
type WorkspaceRetainedContinuation = Extract<AuthorityOwnedReceiveOperationContinuation, { kind: 'workspace-retained' }>

export function browserRetainedContinuationExecutor(
  windowPort: BrowserReceiveWindow,
  trace: WorkspaceStageTraceListener | undefined,
  outputTrace: OutputTraceSource | undefined,
  localOutputFailures: LocalOutputOperationFailureDiagnosticsPort | undefined,
  directZip?: BrowserDirectZipCompositionPort,
): BrowserRetainedContinuationExecutor {
  return Object.freeze({
    catchUpCompatibleNames: async (
      continuation: DirectTreeCatchUpContinuation,
      signal: AbortSignal,
      failures?: OutputFailureSinks,
    ) => {
      try {
        await catchUpFileSystemAccessCompatibleNames({
          operation: continuation.operation,
          signal,
        })
      } catch (error) {
        if (!signal.aborted) recordOutputException(failures?.settlement, error)
        throw error
      }
    },
    resumeReceive: async (
      continuation: ReceiveContinuation,
      signal: AbortSignal,
      failures?: OutputFailureSinks,
    ): Promise<BoundReceiveOperation> => {
      signal.throwIfAborted()
      const binding = createOutputFailureBinding(failures)
      switch (continuation.kind) {
        case 'direct-tree-receive':
          return bindRuntimeOutputFailures(
            await FSAReceiveOperation.reopen(
              continuation.operation,
              diagnosticsFor('file_system_access', outputTrace, binding.sinks),
              localOutputFailures,
              undefined,
              await reopenFolderDeliveryContext(windowPort, continuation.operation.intent.operationId),
            ),
            binding,
          )
        case 'workspace-receive':
          return resumeWorkspaceReceive(
            windowPort,
            continuation,
            signal,
            trace,
            outputTrace,
            binding,
          )
        case 'workspace-start':
          return bindRuntimeOutputFailures(await WorkspaceReceiveOperation.reopenStart({
            windowPort, operation: continuation.operation,
            ...(trace === undefined ? {} : { trace }),
            ...diagnosticsOption('origin_private', outputTrace, binding.sinks),
          }), binding)
        case 'direct-zip': {
          if (directZip === undefined) throw unavailableRoute()
          const runtime = await (failures === undefined
            ? directZip.runtime.resume(continuation.operation, signal)
            : directZip.runtime.resume(continuation.operation, signal, failures))
          return bindRuntimeOutputFailures(runtime, binding)
        }
      }
    },
    resumePackage: async (
      continuation: WorkspacePackageContinuation,
      signal: AbortSignal,
      failures?: OutputFailureSinks,
    ) => {
      try {
        const result = await continuation.operation.packageContinuation.execute(signal)
        if (result.kind === 'sealed') {
          await continueRetainedWorkspaceOperation(
            windowPort, { ...continuation.operation, lifecycle: result.state }, 'save', signal,
            diagnosticsFor('origin_private', outputTrace, failures),
          )
        }
      } catch (error) {
        if (!signal.aborted) recordOutputException(failures?.continuation, error)
        throw error
      }
    },
    continueRetained: (
      continuation: WorkspaceRetainedContinuation,
      action: RetainedContinuationAction,
      signal: AbortSignal,
      failures?: OutputFailureSinks,
    ) => continueRetainedWorkspaceOperation(
      windowPort,
      continuation.operation,
      action,
      signal,
      diagnosticsFor('origin_private', outputTrace, failures),
    ),
  })
}

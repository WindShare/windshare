import type { OutputDiagnosticsPorts } from '../../output/diagnostics'
import type { AuthorityOwnedReceiveOperationContinuation } from '../../output/resume/reopen-authority'
import type { V2RetainedReceiveActionResult } from '../v2-receive-runtime'
import type { BrowserReceiveWindow } from './contracts'
import { WorkspaceReceiveOperation } from './workspace-operation'
import { handoffRetainedWorkspacePackage } from './workspace-publication'

type ProgressiveOperation = Extract<AuthorityOwnedReceiveOperationContinuation,
  { kind: 'workspace-progressive-zip' }>['operation']

export async function continueProgressiveZip(
  windowPort: BrowserReceiveWindow, operation: ProgressiveOperation, signal: AbortSignal,
  diagnostics?: OutputDiagnosticsPorts,
): Promise<V2RetainedReceiveActionResult> {
  const { backend, requirement } = operation.progressiveContinuation
  if (requirement === 'remote-content-needed') {
    try {
      signal.throwIfAborted()
      const runtime = await WorkspaceReceiveOperation.reopenProgressive({
        windowPort, operation, ...(diagnostics === undefined ? {} : { diagnostics }),
      })
      return Object.freeze({ kind: 'receive-continuation', runtime })
    } catch (error) {
      await operation.close()
      throw error
    }
  }
  try {
    signal.throwIfAborted()
    const checkpoint = await backend.archive.finalize(signal)
    const lifecycle = await operation.stages.progressive.seal(backend.store, checkpoint)
    signal.throwIfAborted()
    await handoffRetainedWorkspacePackage(windowPort, { ...operation, lifecycle }, backend, diagnostics, signal)
    return Object.freeze({ kind: 'completed' })
  } finally { await operation.close() }
}

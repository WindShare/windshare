import { equalBytes } from '../../../crypto/bytes'
import { verifyBrowserReceiveOperationLease, type BrowserReceiveOperationLease } from '../../../output/browser/session-lease'
import { isBrowserDeliveryLocalLifecycle, type BrowserDeliveryLocalAction } from '../../../output/browser-delivery/recovery/local-actions'
import { beginBrowserDeliveryLocalMutation, reconcileBrowserDeliveryLifecycle } from '../../../output/browser-delivery/recovery/local-lifecycle'
import type { OutputDiagnosticsPorts } from '../../../output/diagnostics'
import { reopenFileSystemAccessOutput, type FileSystemAccessOutputSession } from '../../../output/file-system-access/session'
import type { ReceiveOperationRepository } from '../../../output/workspace/repository'
import { isTerminalLifecycleState, type ReceiveLifecycleState } from '../../../output/workspace/state'
import { canonicalReceiveLifecycleStateBytes } from '../../../output/workspace/state-codec'
import type { ReceiveIntent } from '../../../transfer/intent'
import type { V2LifecycleActionOutcome, V2LifecycleMutation } from '../../v2-receive-runtime'
import type { FSAResourceOwner } from '../fsa-resource-owner'
import { readLifecycle, unavailableRoute } from '../shared'
import { openFolderCleanupAttempt, openFolderDeliveryAttempt, readFolderRecoverySummary, type BrowserFolderDeliveryContext } from './folder-delivery'

interface ActiveBrowserFolderLocalAuthority {
  readonly intent: ReceiveIntent
  readonly repository: ReceiveOperationRepository
  readonly lease: BrowserReceiveOperationLease
  readonly context: BrowserFolderDeliveryContext | undefined
  readonly diagnostics: OutputDiagnosticsPorts | undefined
  readonly output: Pick<FSAResourceOwner, 'replaceOutputSession'>
  readonly checkpointAuthorities: {
    close(): void | Promise<void>
    replace(close: () => void | Promise<void>): void
  }
}

/** Local actions keep the active operation's lease while replacing only drained output attempts. */
export async function runActiveBrowserFolderAction(
  input: ActiveBrowserFolderLocalAuthority,
  action: BrowserDeliveryLocalAction,
  lifecycle: ReceiveLifecycleState,
): Promise<V2LifecycleMutation> {
  const context = input.context
  if (context === undefined || !isBrowserDeliveryLocalLifecycle(lifecycle) ||
      (action === 'discard-incomplete-staging' && !isTerminalLifecycleState(lifecycle)) ||
      lifecycle.operationId !== input.intent.operationId || lifecycle.receiveIntentDigest !== input.intent.digest) {
    throw unavailableRoute()
  }
  await verifyBrowserReceiveOperationLease(input.repository, input.lease)
  const current = await readLifecycle(input.repository, input.intent.operationId)
  if (!equalBytes(canonicalReceiveLifecycleStateBytes(current), canonicalReceiveLifecycleStateBytes(lifecycle))) {
    throw new DOMException('Folder lifecycle changed before local action', 'InvalidStateError')
  }
  await input.checkpointAuthorities.close()
  return action === 'save-staged-files'
    ? saveStagedFiles(input, context, lifecycle) : disposeStaging(input, context, action, lifecycle)
}

async function disposeStaging(
  input: ActiveBrowserFolderLocalAuthority,
  context: BrowserFolderDeliveryContext,
  action: Exclude<BrowserDeliveryLocalAction, 'save-staged-files'>,
  lifecycle: ReceiveLifecycleState,
): Promise<V2LifecycleMutation> {
  const cleanup = await openFolderCleanupAttempt(context, {
    intent: input.intent, operationLease: input.lease,
    ...(input.diagnostics === undefined ? {} : { diagnostics: input.diagnostics }),
  })
  input.checkpointAuthorities.replace(cleanup.close)
  const failures: unknown[] = []
  try {
    if (action === 'cleanup-staging') await cleanup.delivery.cleanupStaging()
    else await cleanup.delivery.discardIncompleteStaging()
  } catch (error) { failures.push(error) }
  try { await cleanup.close() } catch (error) { failures.push(error) }
  const latest = await reconcileBrowserDeliveryLifecycle({ repository: input.repository, lease: input.lease })
  const retainedLifecycle = equalBytes(canonicalReceiveLifecycleStateBytes(latest), canonicalReceiveLifecycleStateBytes(lifecycle))
    ? lifecycle : latest
  return Object.freeze({ lifecycle: retainedLifecycle,
    workspaceUsage: null, actionOutcome: localDeliveryOutcome(failures) })
}

async function saveStagedFiles(
  input: ActiveBrowserFolderLocalAuthority,
  context: BrowserFolderDeliveryContext,
  lifecycle: ReceiveLifecycleState,
): Promise<V2LifecycleMutation> {
  const localAuthority = { repository: input.repository, lease: input.lease }
  await beginBrowserDeliveryLocalMutation(localAuthority)
  let session: FileSystemAccessOutputSession | undefined
  let attempt: Awaited<ReturnType<typeof openFolderDeliveryAttempt>> | undefined
  const failures: unknown[] = []
  try {
    session = await reopenFileSystemAccessOutput({
      intent: input.intent, operationRepository: input.repository,
      ...(input.diagnostics === undefined ? {} : { diagnostics: input.diagnostics }),
    })
    input.output.replaceOutputSession(session)
    await session.activate()
    attempt = await openFolderDeliveryAttempt(context, {
      target: session, intent: input.intent, operationLease: input.lease,
      ...(input.diagnostics === undefined ? {} : { diagnostics: input.diagnostics }),
    })
    input.checkpointAuthorities.replace(attempt.close)
    await attempt.delivery.saveStagedFiles()
  } catch (error) { failures.push(error) }
  for (const close of [() => attempt?.close(), () => session?.close()]) {
    try { await close() } catch (error) { failures.push(error) }
  }
  const reconciled = await reconcileBrowserDeliveryLifecycle(localAuthority).catch(error => {
    if (failures.length === 0) throw error
    throw new AggregateError([...failures, error], 'Local folder saving could not reconcile its recovery authority', { cause: failures[0] })
  })
  const retainedLifecycle = equalBytes(canonicalReceiveLifecycleStateBytes(reconciled), canonicalReceiveLifecycleStateBytes(lifecycle))
    ? lifecycle : reconciled
  let recoverySummary: Awaited<ReturnType<typeof readFolderRecoverySummary>> | undefined
  try {
    if (retainedLifecycle.kind === 'resumable-receive' && retainedLifecycle.payloadKind === 'file-set') {
      recoverySummary = await readFolderRecoverySummary(input.intent, retainedLifecycle)
    }
  }
  catch (error) { failures.push(error) }
  const actionOutcome = localDeliveryOutcome(failures)
  // Local copying proves individual target files; remaining discovery still owns task completion.
  return Object.freeze({ lifecycle: retainedLifecycle, workspaceUsage: null, actionOutcome,
    ...(recoverySummary === undefined ? {} : { recoverySummary }) })
}

function localDeliveryOutcome(failures: readonly unknown[]): V2LifecycleActionOutcome {
  if (failures.length === 0) return Object.freeze({ kind: 'completed' })
  const error = failures.length === 1 ? failures[0] : new AggregateError(failures,
    'Local folder saving could not release output authority', { cause: failures[0] })
  return Object.freeze({ kind: 'failed', error })
}

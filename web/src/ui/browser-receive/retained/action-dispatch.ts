import type { OutputFailureSinks } from '../../../output/diagnostics'
import type { ReceiveOperationResumeAuthority, ReceiveOperationResumeRef } from '../../../output/resume/authority'
import type {
  AuthorityOwnedReceiveOperationContinuation,
  AuthorityOwnedReceiveOperationMutationResult,
} from '../../../output/resume/reopen-authority'
import type { V2RetainedReceiveAction, V2RetainedReceiveOperation } from '../../v2-receive-runtime'

type RetainedActionAuthority = Pick<
  ReceiveOperationResumeAuthority<AuthorityOwnedReceiveOperationMutationResult>,
  'forget' | 'discard' | 'cleanup' | 'catchUp' | 'resume'
>

type RetainedAuthorityDispatch =
  | Readonly<{ kind: 'completed' }>
  | Readonly<{
      kind: 'continuation'
      continuation: AuthorityOwnedReceiveOperationContinuation
      directZipAction: boolean
    }>

/** Keeps action intent separate from the continuation runtime that takes over its leased output. */
export async function dispatchRetainedAuthorityAction(
  authority: RetainedActionAuthority,
  reference: ReceiveOperationResumeRef,
  operation: V2RetainedReceiveOperation,
  action: V2RetainedReceiveAction,
  signal: AbortSignal,
  failures?: OutputFailureSinks,
): Promise<RetainedAuthorityDispatch> {
  if (action === 'forget') {
    await authority.forget(reference)
    signal.throwIfAborted()
    return Object.freeze({ kind: 'completed' })
  }
  const directZipAction = isDirectZipContinuation(operation.continuation)
  if (!directZipAction && (action === 'discard' || (action === 'delete' &&
      operation.continuation !== 'retry-cleanup'))) {
    await authority.discard(reference, failures)
    signal.throwIfAborted()
    return Object.freeze({ kind: 'completed' })
  }
  let result: AuthorityOwnedReceiveOperationMutationResult
  if (action === 'delete' &&
      operation.continuation === 'retry-cleanup') {
    result = await authority.cleanup(reference, failures)
  } else if (action === 'catch-up') {
    result = await authority.catchUp(reference, failures)
  } else {
    const retainedFileRecovery = retainedFileRecoveryFor(operation, action)
    result = await authority.resume(reference, {
      ...(action === 'save-partial' ? { purpose: 'partial-export' as const } : {}),
      ...(retainedFileRecovery === undefined ? {} : { retainedFileRecovery }),
      ...(failures === undefined ? {} : { failures }),
    })
  }
  if (result.kind === 'cleanup') {
    signal.throwIfAborted()
    return Object.freeze({ kind: 'completed' })
  }
  return Object.freeze({ kind: 'continuation', continuation: result.continuation, directZipAction })
}

function retainedFileRecoveryFor(
  operation: V2RetainedReceiveOperation,
  action: V2RetainedReceiveAction,
): 'preserve' | 'restart-owned-file' | undefined {
  if (operation.recoverySummary === undefined) return undefined
  return action === 'redownload' ? 'restart-owned-file' : 'preserve'
}

function isDirectZipContinuation(
  continuation: V2RetainedReceiveOperation['continuation'],
): boolean {
  return continuation === 'resume-direct-zip' || continuation === 'reauthorize-direct-zip' ||
    continuation === 'verify-direct-zip-target' || continuation === 'verify-direct-zip-completion' ||
    continuation === 'retry-direct-zip-space'
}

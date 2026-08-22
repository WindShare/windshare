import type { DirectZipCanonicalBytes } from '../format/canonical'
import {
  directZipTraceIdentity,
  type DirectZipLifecycleDecision,
  type DirectZipTargetStage,
  type DirectZipTargetTrace,
  type DirectZipTargetTraceEvent,
} from './model'
import type {
  DirectZipFileSystemPort,
  DirectZipOperationLease,
  DirectZipOperationLeasePort,
  DirectZipParentLock,
  DirectZipParentLockPort,
} from './ports'

export interface DirectZipHeldTargetLocks {
  readonly operation: DirectZipOperationLease
  readonly parent: DirectZipParentLock
  release(): Promise<void>
}

export async function acquireDirectZipTargetLocks<ParentHandle>(
  operationLeases: DirectZipOperationLeasePort,
  parentLocks: DirectZipParentLockPort<ParentHandle>,
  operationId: DirectZipCanonicalBytes,
  parent: ParentHandle,
  stage: DirectZipTargetStage,
  trace: DirectZipTargetTrace,
): Promise<DirectZipHeldTargetLocks> {
  const operation = await operationLeases.acquire(operationId)
  emitDirectZipTargetTrace(trace, {
    name: 'direct_zip.target.lock',
    operation_id: directZipTraceIdentity(operationId),
    stage,
    outcome: 'operation-lease-acquired',
  })
  const parentLock = await parentLocks.acquire(parent).catch(
    error => releaseAfterFailure(operation, error),
  )
  emitDirectZipTargetTrace(trace, {
    name: 'direct_zip.target.lock',
    operation_id: directZipTraceIdentity(operationId),
    stage,
    outcome: 'parent-lock-acquired',
  })

  let releasePromise: Promise<void> | undefined
  return Object.freeze({
    operation,
    parent: parentLock,
    release: () => {
      releasePromise ??= releaseBoth(parentLock, operation, operationId, stage, trace)
      return releasePromise
    },
  })
}

export async function authorizeDirectZipParent<ParentHandle, FileHandle>(
  fileSystem: DirectZipFileSystemPort<ParentHandle, FileHandle>,
  parent: ParentHandle,
  trustedAction: boolean,
  operationId: DirectZipCanonicalBytes,
  trace: DirectZipTargetTrace,
): Promise<undefined | DirectZipLifecycleDecision> {
  let permission
  try {
    permission = await fileSystem.queryPermission(parent)
  } catch (error) {
    const decision = directZipLifecycleDecisionForError(error, 'permission-query')
    emitPermission(trace, operationId, 'permission-query', decision, error)
    return decision
  }
  if (permission === 'granted') {
    emitPermission(trace, operationId, 'permission-query', undefined)
    return undefined
  }
  if (permission === 'unsupported') {
    const decision = authorization('permission-query', 'permission-api-unavailable')
    emitPermission(trace, operationId, 'permission-query', decision)
    return decision
  }
  if (!trustedAction) {
    const decision = authorization(
      'permission-query',
      permission === 'prompt' ? 'permission-prompt' : 'permission-denied',
    )
    emitPermission(trace, operationId, 'permission-query', decision)
    return decision
  }

  try {
    permission = await fileSystem.requestPermission(parent)
  } catch (error) {
    const decision = directZipLifecycleDecisionForError(error, 'permission-request')
    emitPermission(trace, operationId, 'permission-request', decision, error)
    return decision
  }
  if (permission === 'granted') {
    emitPermission(trace, operationId, 'permission-request', undefined)
    return undefined
  }
  const decision = authorization(
    'permission-request',
    permission === 'unsupported' ? 'permission-api-unavailable' : 'permission-denied',
  )
  emitPermission(trace, operationId, 'permission-request', decision)
  return decision
}

export function directZipLifecycleDecisionForError(
  error: unknown,
  stage: DirectZipTargetStage,
): DirectZipLifecycleDecision {
  const name = nativeErrorName(error)
  if (name === 'QuotaExceededError') {
    return Object.freeze({ kind: 'destination-space-required', stage })
  }
  if (name === 'NotAllowedError' || name === 'SecurityError') {
    return authorization(stage, 'permission-denied')
  }
  if (name === 'NotFoundError') {
    return Object.freeze({ kind: 'restart-required', stage, reason: 'target-deleted' })
  }
  return Object.freeze({
    kind: 'target-verification-required',
    stage,
    reason: 'native-effect-ambiguous',
    proof: 'fresh-observation',
  })
}

export function nativeErrorName(error: unknown): string | undefined {
  if (error === null || typeof error !== 'object' || !('name' in error)) return undefined
  const name = (error as { readonly name?: unknown }).name
  return typeof name === 'string' && name.length > 0 ? name : undefined
}

export function emitDirectZipTargetTrace(
  trace: DirectZipTargetTrace,
  event: DirectZipTargetTraceEvent,
): void {
  try {
    trace(Object.freeze(event))
  } catch {
    // Diagnostics cannot change local-file authority or turn a refusal into a mutation.
  }
}

function authorization(
  stage: DirectZipTargetStage,
  reason: Extract<DirectZipLifecycleDecision, { readonly kind: 'authorization-required' }>['reason'],
): DirectZipLifecycleDecision {
  return Object.freeze({ kind: 'authorization-required', stage, reason })
}

function emitPermission(
  trace: DirectZipTargetTrace,
  operationId: DirectZipCanonicalBytes,
  stage: 'permission-query' | 'permission-request',
  decision?: DirectZipLifecycleDecision,
  error?: unknown,
): void {
  const errorName = nativeErrorName(error)
  emitDirectZipTargetTrace(trace, {
    name: 'direct_zip.target.permission',
    operation_id: directZipTraceIdentity(operationId),
    stage,
    outcome: decision === undefined ? 'granted' : 'gated',
    ...(decision === undefined ? {} : { decision: decision.kind }),
    ...(errorName === undefined ? {} : { native_error_name: errorName }),
  })
}

async function releaseBoth(
  parent: DirectZipParentLock,
  operation: DirectZipOperationLease,
  operationId: DirectZipCanonicalBytes,
  stage: DirectZipTargetStage,
  trace: DirectZipTargetTrace,
): Promise<void> {
  let parentFailure: unknown
  try {
    await parent.release()
  } catch (error) {
    parentFailure = error
  }
  try {
    await operation.release()
  } catch (operationFailure) {
    if (parentFailure !== undefined) {
      throw new AggregateError(
        [parentFailure, operationFailure],
        'Direct ZIP target locks could not be released',
        { cause: operationFailure },
      )
    }
    throw operationFailure
  }
  if (parentFailure !== undefined) throw parentFailure
  emitDirectZipTargetTrace(trace, {
    name: 'direct_zip.target.lock',
    operation_id: directZipTraceIdentity(operationId),
    stage,
    outcome: 'released',
  })
}

async function releaseAfterFailure(
  operation: DirectZipOperationLease,
  acquisitionFailure: unknown,
): Promise<never> {
  try {
    await operation.release()
  } catch (releaseFailure) {
    throw new AggregateError(
      [acquisitionFailure, releaseFailure],
      'Direct ZIP parent lock failed and the operation lease could not be released',
      { cause: releaseFailure },
    )
  }
  throw acquisitionFailure
}

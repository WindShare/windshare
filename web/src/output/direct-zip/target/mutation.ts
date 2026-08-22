import {
  acquireDirectZipTargetLocks,
  authorizeDirectZipParent,
  directZipLifecycleDecisionForError,
  emitDirectZipTargetTrace,
  nativeErrorName,
  type DirectZipHeldTargetLocks,
} from './authority'
import {
  directZipCandidateTraceIdentity,
  directZipTraceIdentity,
  type DirectZipLifecycleDecision,
  type DirectZipTargetObservationV1,
  type DirectZipTargetOutcome,
} from './model'
import { equalDirectZipBytes } from '../format/canonical'
import {
  observeDirectZipTarget,
  verifyDirectZipCommittedEpochChain,
  verifyDirectZipEpochProof,
} from './observation'
import type { DirectZipFileSnapshotPort, DirectZipWritablePort } from './ports'
import { inspectDirectZipTargetLocked } from './reopen'
import {
  gateDirectZipTarget,
  readyDirectZipTarget,
  type DirectZipCleanupRequest,
  type DirectZipCleanupResult,
  type DirectZipEpochCloseResult,
  type DirectZipEpochWritable,
  type DirectZipReopenRequest,
  type DirectZipTargetRuntime,
  type DirectZipTruncateResult,
} from './target'

export async function openDirectZipEpoch<ParentHandle, FileHandle>(
  runtime: DirectZipTargetRuntime<ParentHandle, FileHandle>,
  request: DirectZipReopenRequest<ParentHandle, FileHandle>,
): Promise<DirectZipTargetOutcome<DirectZipEpochWritable>> {
  const locks = await acquireDirectZipTargetLocks(
    runtime.operationLeases,
    runtime.parentLocks,
    request.binding.operationId,
    request.currentParent,
    'epoch-open',
    runtime.trace,
  )
  let handedOff = false
  try {
    const inspected = await inspectDirectZipTargetLocked(runtime, request)
    if (inspected.kind === 'gated') return inspected
    if (inspected.value.resolution.kind !== 'replay-predecessor') {
      return gateDirectZipTarget(resolutionMustSettle(inspected.value.resolution.kind))
    }
    let writable: DirectZipWritablePort
    try {
      writable = await runtime.fileSystem.createWritable(inspected.value.currentFile, true)
    } catch (error) {
      return gateDirectZipTarget(directZipLifecycleDecisionForError(error, 'epoch-open'))
    }
    handedOff = true
    emitDirectZipTargetTrace(runtime.trace, {
      name: 'direct_zip.target.writable',
      operation_id: directZipTraceIdentity(request.binding.operationId),
      candidate_id: directZipCandidateTraceIdentity(request.binding.candidateId),
      stable_name: request.binding.stableName,
      stage: 'epoch-open',
      outcome: 'opened-keep-existing-data',
      target_length: inspected.value.observation.size.toString(),
    })
    return readyDirectZipTarget(createLockedEpochWritable(
      runtime,
      request,
      inspected.value.currentFile,
      writable,
      locks,
    ))
  } finally {
    if (!handedOff) await locks.release()
  }
}

export async function truncateDirectZipTarget<ParentHandle, FileHandle>(
  runtime: DirectZipTargetRuntime<ParentHandle, FileHandle>,
  request: DirectZipReopenRequest<ParentHandle, FileHandle>,
): Promise<DirectZipTargetOutcome<DirectZipTruncateResult>> {
  const locks = await acquireDirectZipTargetLocks(
    runtime.operationLeases,
    runtime.parentLocks,
    request.binding.operationId,
    request.currentParent,
    'epoch-truncate',
    runtime.trace,
  )
  try {
    const inspected = await inspectDirectZipTargetLocked(runtime, request)
    if (inspected.kind === 'gated') return inspected
    if (inspected.value.resolution.kind === 'replay-predecessor') {
      return readyDirectZipTarget(Object.freeze({
        disposition: 'already-at-predecessor',
        observation: inspected.value.observation,
      }))
    }
    if (inspected.value.resolution.kind === 'promote-candidate') {
      return gateDirectZipTarget(resolutionMustSettle('promote-candidate'))
    }
    return performVerifiedTruncation(runtime, request, inspected.value.currentFile)
  } finally {
    await locks.release()
  }
}

async function performVerifiedTruncation<ParentHandle, FileHandle>(
  runtime: DirectZipTargetRuntime<ParentHandle, FileHandle>,
  request: DirectZipReopenRequest<ParentHandle, FileHandle>,
  currentFile: FileHandle,
): Promise<DirectZipTargetOutcome<DirectZipTruncateResult>> {
  let writable: DirectZipWritablePort
  try {
    writable = await runtime.fileSystem.createWritable(currentFile, true)
  } catch (error) {
    return gateDirectZipTarget(directZipLifecycleDecisionForError(error, 'epoch-open'))
  }
  const effect = await truncateAndClose(writable, request.predecessor.committedLength)
  const resolved = await inspectDirectZipTargetLocked(runtime, request)
  if (resolved.kind === 'ready' && resolved.value.resolution.kind === 'replay-predecessor') {
    traceTruncation(runtime, request, resolved.value.observation, effect.closeError)
    return readyDirectZipTarget(Object.freeze({
      disposition: 'truncated',
      observation: resolved.value.observation,
      ...(effect.closeError === undefined ? {} : { nativeCloseError: effect.closeError }),
    }))
  }
  if (effect.mutationError !== undefined) {
    return gateDirectZipTarget(directZipLifecycleDecisionForError(
      effect.mutationError,
      'epoch-truncate',
    ))
  }
  if (resolved.kind === 'gated') return resolved
  if (effect.closeError !== undefined) {
    return gateDirectZipTarget(directZipLifecycleDecisionForError(effect.closeError, 'epoch-close'))
  }
  return gateDirectZipTarget(ambiguousTruncation())
}

async function truncateAndClose(
  writable: DirectZipWritablePort,
  committedLength: bigint,
): Promise<Readonly<{ readonly mutationError?: unknown; readonly closeError?: unknown }>> {
  try {
    await writable.truncate(committedLength)
  } catch (mutationError) {
    try {
      await writable.abort(mutationError)
    } catch {
      // The exact-name observation, not abort completion, resolves the target.
    }
    return Object.freeze({ mutationError })
  }
  try {
    await writable.close()
    return Object.freeze({})
  } catch (closeError) {
    return Object.freeze({ closeError })
  }
}

function traceTruncation<ParentHandle, FileHandle>(
  runtime: DirectZipTargetRuntime<ParentHandle, FileHandle>,
  request: DirectZipReopenRequest<ParentHandle, FileHandle>,
  observation: DirectZipTargetObservationV1,
  closeError: unknown,
): void {
  const errorName = nativeErrorName(closeError)
  emitDirectZipTargetTrace(runtime.trace, {
    name: 'direct_zip.target.writable',
    operation_id: directZipTraceIdentity(request.binding.operationId),
    candidate_id: directZipCandidateTraceIdentity(request.binding.candidateId),
    stable_name: request.binding.stableName,
    stage: 'epoch-truncate',
    outcome: closeError === undefined ? 'truncated-and-proven' : 'throw-after-truncate-proven',
    target_length: observation.size.toString(),
    ...(errorName === undefined ? {} : { native_error_name: errorName }),
  })
}

function ambiguousTruncation(): DirectZipLifecycleDecision {
  return Object.freeze({
    kind: 'target-verification-required',
    stage: 'epoch-truncate',
    reason: 'native-effect-ambiguous',
    proof: 'fresh-observation',
  })
}

export async function deleteDirectZipTarget<ParentHandle, FileHandle>(
  runtime: DirectZipTargetRuntime<ParentHandle, FileHandle>,
  request: DirectZipCleanupRequest<ParentHandle, FileHandle>,
): Promise<DirectZipTargetOutcome<DirectZipCleanupResult>> {
  const binding = request.binding
  const locks = await acquireDirectZipTargetLocks(
    runtime.operationLeases,
    runtime.parentLocks,
    binding.operationId,
    request.currentParent,
    'cleanup-delete',
    runtime.trace,
  )
  try {
    const permission = await authorizeDirectZipParent(
      runtime.fileSystem,
      request.currentParent,
      request.trustedAction,
      binding.operationId,
      runtime.trace,
    )
    if (permission !== undefined) return gateDirectZipTarget(permission)
    const parentLocator = await runtime.handleBindings.compareParent(
      binding.parentBinding,
      request.currentParent,
    )
    if (parentLocator !== 'same') {
      return gateDirectZipTarget(Object.freeze({
        kind: 'target-verification-required',
        stage: 'cleanup-delete',
        reason: 'parent-binding-changed',
        proof: 'fresh-observation',
      }))
    }
    const lookup = await runtime.fileSystem.lookupExactName(
      request.currentParent,
      binding.stableName,
    )
    if (lookup.kind === 'absent') {
      return readyDirectZipTarget(Object.freeze({ disposition: 'already-absent' }))
    }
    if (lookup.kind === 'occupied-non-file') return gateDirectZipTarget(foreignCleanupTarget())

    const fileLocator = await runtime.handleBindings.compareFile(binding.fileBinding, lookup.handle)
    const snapshot = await runtime.fileSystem.snapshot(lookup.handle)
    const observation = await observeDirectZipTarget(snapshot, {
      resultRootComponent: binding.resultRootComponent,
      marker: binding.marker,
      parentLocator,
      fileLocator,
    })
    const ownershipGate = cleanupOwnershipGate(
      observation,
      request.predecessor.committedLength,
      request.candidate?.stagedEnd,
    )
    if (ownershipGate !== undefined) return gateDirectZipTarget(ownershipGate)

    const rangeGate = await cleanupRangeProofGate(snapshot, request, observation)
    if (rangeGate !== undefined) return gateDirectZipTarget(rangeGate)

    const finalLookup = await runtime.fileSystem.lookupExactName(
      request.currentParent,
      binding.stableName,
    )
    if (finalLookup.kind !== 'file' ||
        await runtime.handleBindings.compareCurrentFiles(lookup.handle, finalLookup.handle) !== 'same') {
      return gateDirectZipTarget(foreignCleanupTarget())
    }
    try {
      await runtime.fileSystem.removeExactName(request.currentParent, binding.stableName)
    } catch (error) {
      const classified = directZipLifecycleDecisionForError(error, 'cleanup-delete')
      if (classified.kind === 'authorization-required') return gateDirectZipTarget(classified)
      return gateDirectZipTarget(cleanupRefused())
    }
    let after
    try {
      after = await runtime.fileSystem.lookupExactName(request.currentParent, binding.stableName)
    } catch {
      return gateDirectZipTarget(cleanupRefused())
    }
    if (after.kind !== 'absent') return gateDirectZipTarget(cleanupRefused())
    emitDirectZipTargetTrace(runtime.trace, {
      name: 'direct_zip.target.cleanup',
      operation_id: directZipTraceIdentity(binding.operationId),
      candidate_id: directZipCandidateTraceIdentity(binding.candidateId),
      stable_name: binding.stableName,
      stage: 'cleanup-observe',
      outcome: 'deleted-and-absence-proven',
      target_length: observation.size.toString(),
    })
    return readyDirectZipTarget(Object.freeze({ disposition: 'deleted' }))
  } catch (error) {
    return gateDirectZipTarget(directZipLifecycleDecisionForError(error, 'cleanup-delete'))
  } finally {
    await locks.release()
  }
}

async function cleanupRangeProofGate<ParentHandle, FileHandle>(
  snapshot: DirectZipFileSnapshotPort,
  request: DirectZipCleanupRequest<ParentHandle, FileHandle>,
  observation: DirectZipTargetObservationV1,
): Promise<DirectZipLifecycleDecision | undefined> {
  let epochsMatch: boolean
  try {
    epochsMatch = await verifyDirectZipCommittedEpochChain(
      snapshot,
      request.predecessor.committedEpochs,
      request.predecessor.committedLength,
    )
  } catch {
    return Object.freeze({
      kind: 'target-verification-required',
      stage: 'range-proof',
      reason: 'native-effect-ambiguous',
      proof: 'predecessor-epochs',
    })
  }
  if (!epochsMatch) {
    return Object.freeze({
      kind: 'needs-attention',
      stage: 'range-proof',
      reason: 'committed-prefix-mismatch',
    })
  }
  if (request.candidate === undefined || observation.size !== request.candidate.stagedEnd) {
    return undefined
  }
  try {
    return candidateFollowsPredecessor(request) &&
      await verifyDirectZipEpochProof(snapshot, request.candidate.epoch)
      ? undefined
      : foreignCleanupTarget()
  } catch {
    return Object.freeze({
      kind: 'target-verification-required',
      stage: 'range-proof',
      reason: 'native-effect-ambiguous',
      proof: 'candidate-range',
    })
  }
}

function candidateFollowsPredecessor<ParentHandle, FileHandle>(
  request: DirectZipCleanupRequest<ParentHandle, FileHandle>,
): boolean {
  const candidate = request.candidate
  if (candidate === undefined || candidate.epoch.start !== request.predecessor.committedLength ||
      candidate.epoch.end !== candidate.stagedEnd) return false
  const predecessor = request.predecessor.committedEpochs.at(-1)
  return predecessor !== undefined &&
    equalDirectZipBytes(candidate.epoch.predecessorRoot, predecessor.epochRoot)
}

function createLockedEpochWritable<ParentHandle, FileHandle>(
  runtime: DirectZipTargetRuntime<ParentHandle, FileHandle>,
  request: DirectZipReopenRequest<ParentHandle, FileHandle>,
  openedFile: FileHandle,
  writable: DirectZipWritablePort,
  locks: DirectZipHeldTargetLocks,
): DirectZipEpochWritable {
  let state: 'open' | 'write-failed' | 'settled' = 'open'
  let settlePromise: Promise<DirectZipTargetOutcome<DirectZipEpochCloseResult>> | undefined

  const write = async (
    position: bigint,
    bytes: Uint8Array,
  ): Promise<DirectZipTargetOutcome<undefined>> => {
    requireWritableState(state, 'open')
    try {
      await writable.write(position, bytes)
      return readyDirectZipTarget(undefined)
    } catch (error) {
      state = 'write-failed'
      return gateDirectZipTarget(directZipLifecycleDecisionForError(error, 'epoch-write'))
    }
  }
  const truncate = async (size: bigint): Promise<DirectZipTargetOutcome<undefined>> => {
    requireWritableState(state, 'open')
    try {
      await writable.truncate(size)
      return readyDirectZipTarget(undefined)
    } catch (error) {
      state = 'write-failed'
      return gateDirectZipTarget(directZipLifecycleDecisionForError(error, 'epoch-truncate'))
    }
  }
  const closeAndObserve = (): Promise<DirectZipTargetOutcome<DirectZipEpochCloseResult>> => {
    requireWritableState(state, 'open')
    state = 'settled'
    settlePromise = settleWritable(runtime, request, openedFile, writable, locks, 'close')
    return settlePromise
  }
  const abortAndObserve = async (
    reason?: unknown,
  ): Promise<DirectZipTargetOutcome<DirectZipTargetObservationV1>> => {
    if (state === 'settled') throw new DOMException('Direct ZIP epoch writable is settled', 'InvalidStateError')
    state = 'settled'
    const result = await settleWritable(runtime, request, openedFile, writable, locks, 'abort', reason)
    if (result.kind === 'gated') return result
    return readyDirectZipTarget(result.value.observation)
  }
  return Object.freeze({ write, truncate, closeAndObserve, abortAndObserve })
}

async function settleWritable<ParentHandle, FileHandle>(
  runtime: DirectZipTargetRuntime<ParentHandle, FileHandle>,
  request: DirectZipReopenRequest<ParentHandle, FileHandle>,
  openedFile: FileHandle,
  writable: DirectZipWritablePort,
  locks: DirectZipHeldTargetLocks,
  kind: 'close' | 'abort',
  reason?: unknown,
): Promise<DirectZipTargetOutcome<DirectZipEpochCloseResult>> {
  let nativeError: unknown
  try {
    if (kind === 'close') await writable.close()
    else await writable.abort(reason)
  } catch (error) {
    nativeError = error
  }
  try {
    const observed = await observeFreshTargetLocked(runtime, request, openedFile)
    if (observed.kind === 'gated') return observed
    const errorName = nativeErrorName(nativeError)
    emitDirectZipTargetTrace(runtime.trace, {
      name: 'direct_zip.target.writable',
      operation_id: directZipTraceIdentity(request.binding.operationId),
      candidate_id: directZipCandidateTraceIdentity(request.binding.candidateId),
      stable_name: request.binding.stableName,
      stage: kind === 'close' ? 'epoch-close' : 'epoch-abort',
      outcome: nativeError === undefined ? `${kind}-observed` : `${kind}-threw-observed`,
      target_length: observed.value.size.toString(),
      ...(errorName === undefined ? {} : { native_error_name: errorName }),
    })
    return readyDirectZipTarget(Object.freeze({
      observation: observed.value,
      ...(nativeError === undefined ? {} : { nativeCloseError: nativeError }),
    }))
  } finally {
    await locks.release()
  }
}

async function observeFreshTargetLocked<ParentHandle, FileHandle>(
  runtime: DirectZipTargetRuntime<ParentHandle, FileHandle>,
  request: DirectZipReopenRequest<ParentHandle, FileHandle>,
  openedFile: FileHandle,
): Promise<DirectZipTargetOutcome<DirectZipTargetObservationV1>> {
  try {
    const lookup = await runtime.fileSystem.lookupExactName(
      request.currentParent,
      request.binding.stableName,
    )
    if (lookup.kind === 'absent') {
      return gateDirectZipTarget(Object.freeze({
        kind: 'restart-required',
        stage: 'snapshot',
        reason: 'target-deleted',
      }))
    }
    if (lookup.kind === 'occupied-non-file') return gateDirectZipTarget(foreignCleanupTarget())
    const currentComparison = await runtime.handleBindings.compareCurrentFiles(openedFile, lookup.handle)
    const fileLocator = await runtime.handleBindings.compareFile(request.binding.fileBinding, lookup.handle)
    const snapshot = await runtime.fileSystem.snapshot(lookup.handle)
    const observation = await observeDirectZipTarget(snapshot, {
      resultRootComponent: request.binding.resultRootComponent,
      marker: request.binding.marker,
      parentLocator: 'same',
      fileLocator: currentComparison === 'same' ? fileLocator : 'different',
    })
    return readyDirectZipTarget(observation)
  } catch (error) {
    return gateDirectZipTarget(directZipLifecycleDecisionForError(error, 'snapshot'))
  }
}

function cleanupOwnershipGate(
  observation: DirectZipTargetObservationV1,
  committedLength: bigint,
  candidateLength?: bigint,
): DirectZipLifecycleDecision | undefined {
  if (observation.marker.kind === 'foreign') return foreignCleanupTarget()
  if (observation.marker.kind === 'partial') {
    return Object.freeze({
      kind: 'needs-attention',
      stage: 'cleanup-delete',
      reason: 'ownership-unknown',
    })
  }
  if (observation.marker.kind === 'malformed') {
    return Object.freeze({
      kind: 'needs-attention',
      stage: 'cleanup-delete',
      reason: 'ownership-unknown',
    })
  }
  if (observation.size !== committedLength && observation.size !== candidateLength) {
    return Object.freeze({
      kind: 'needs-attention',
      stage: 'cleanup-delete',
      reason: 'ownership-unknown',
    })
  }
  return undefined
}

function resolutionMustSettle(
  resolution: 'promote-candidate' | 'truncate-to-predecessor',
): DirectZipLifecycleDecision {
  return Object.freeze({
    kind: 'target-verification-required',
    stage: 'epoch-open',
    reason: resolution === 'promote-candidate' ? 'candidate-ambiguous' : 'unknown-tail',
    proof: resolution === 'promote-candidate' ? 'candidate-range' : 'predecessor-epochs',
  })
}

function foreignCleanupTarget(): DirectZipLifecycleDecision {
  return Object.freeze({
    kind: 'needs-attention',
    stage: 'cleanup-delete',
    reason: 'foreign-replacement',
  })
}

function cleanupRefused(): DirectZipLifecycleDecision {
  return Object.freeze({
    kind: 'needs-attention',
    stage: 'cleanup-delete',
    reason: 'cleanup-refused',
  })
}

function requireWritableState(actual: string, expected: 'open'): void {
  if (actual !== expected) {
    throw new DOMException('Direct ZIP epoch writable cannot be used after a failed or settled effect', 'InvalidStateError')
  }
}

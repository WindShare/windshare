import {
  DIRECT_ZIP_BINDING_DIGEST_BYTES,
  DIRECT_ZIP_CANDIDATE_ID_BYTES,
  DIRECT_ZIP_OPERATION_ID_BYTES,
  DIRECT_ZIP_OWNERSHIP_NONCE_BYTES,
  encodeDirectZipBootstrapPrefixV1,
} from '../format'
import { snapshotDirectZipFixedBytes } from '../format/canonical'
import {
  acquireDirectZipTargetLocks,
  authorizeDirectZipParent,
  directZipLifecycleDecisionForError,
  emitDirectZipTargetTrace,
  nativeErrorName,
} from './authority'
import {
  directZipCandidateTraceIdentity,
  directZipStableTargetName,
  directZipTraceIdentity,
  snapshotDirectZipReservationCandidate,
  type DirectZipLifecycleDecision,
  type DirectZipOwnedTargetBinding,
  type DirectZipReservationCandidate,
  type DirectZipTargetObservationV1,
  type DirectZipTargetOutcome,
} from './model'
import { observeDirectZipTarget } from './observation'
import type { DirectZipFileSnapshotPort, DirectZipOperationLeaseEvidence } from './ports'
import {
  gateDirectZipTarget,
  readyDirectZipTarget,
  type DirectZipBootstrapCandidateResult,
  type DirectZipReserveBootstrapRequest,
  type DirectZipReservedBootstrap,
  type DirectZipResumeBootstrapRequest,
  type DirectZipTargetRuntime,
} from './target'

type CandidateAttempt<ParentHandle, FileHandle> = DirectZipTargetOutcome<
  DirectZipBootstrapCandidateResult<ParentHandle, FileHandle>,
  DirectZipReservationCandidate<ParentHandle>
>

export async function reserveDirectZipBootstrap<ParentHandle, FileHandle>(
  runtime: DirectZipTargetRuntime<ParentHandle, FileHandle>,
  request: DirectZipReserveBootstrapRequest<ParentHandle>,
): Promise<DirectZipTargetOutcome<
  DirectZipReservedBootstrap<ParentHandle, FileHandle>,
  DirectZipReservationCandidate<ParentHandle>
>> {
  const operationId = snapshotDirectZipFixedBytes(
    request.operationId,
    DIRECT_ZIP_OPERATION_ID_BYTES,
    'direct ZIP operation ID',
  )
  const locks = await acquireDirectZipTargetLocks(
    runtime.operationLeases,
    runtime.parentLocks,
    operationId,
    request.currentParent,
    'candidate-persist',
    runtime.trace,
  )
  try {
    const permission = await authorizeDirectZipParent(
      runtime.fileSystem,
      request.currentParent,
      request.trustedAction,
      operationId,
      runtime.trace,
    )
    if (permission !== undefined) return gateDirectZipTarget(permission)
    const parentLocator = await runtime.handleBindings.compareParent(
      request.parentBinding,
      request.currentParent,
    )
    if (parentLocator !== 'same') {
      return gateDirectZipTarget(parentBindingDecision())
    }

    for (let attempt = 0; attempt < runtime.maximumReservationCandidates; attempt += 1) {
      const candidateId = snapshotDirectZipFixedBytes(
        runtime.random.bytes(DIRECT_ZIP_CANDIDATE_ID_BYTES),
        DIRECT_ZIP_CANDIDATE_ID_BYTES,
        'direct ZIP reservation candidate ID',
      )
      const ownershipNonce = snapshotDirectZipFixedBytes(
        runtime.random.bytes(DIRECT_ZIP_OWNERSHIP_NONCE_BYTES),
        DIRECT_ZIP_OWNERSHIP_NONCE_BYTES,
        'direct ZIP ownership nonce',
      )
      const stableName = directZipStableTargetName(request.resultRootComponent, candidateId)
      const draft = Object.freeze({
        operationId,
        candidateId,
        resultRootComponent: request.resultRootComponent,
        stableName,
        ownershipNonce,
        parentBinding: request.parentBinding,
      })
      const persisted = await runtime.reservations.persistCandidate(draft, locks.operation)
      const candidate = snapshotDirectZipReservationCandidate(draft, persisted)
      traceCandidate(runtime, candidate, 'persisted')
      const result = await claimCandidateLocked(
        runtime,
        candidate,
        request.currentParent,
        locks.operation,
        false,
      )
      if (result.kind === 'gated') return result
      if ('disposition' in result.value) continue
      return readyDirectZipTarget(result.value)
    }
    return gateDirectZipTarget(Object.freeze({
      kind: 'needs-attention',
      stage: 'candidate-persist',
      reason: 'reservation-exhausted',
    }))
  } finally {
    await locks.release()
  }
}

export async function resumeDirectZipBootstrap<ParentHandle, FileHandle>(
  runtime: DirectZipTargetRuntime<ParentHandle, FileHandle>,
  request: DirectZipResumeBootstrapRequest<ParentHandle>,
): Promise<CandidateAttempt<ParentHandle, FileHandle>> {
  const candidate = request.candidate
  const locks = await acquireDirectZipTargetLocks(
    runtime.operationLeases,
    runtime.parentLocks,
    candidate.operationId,
    request.currentParent,
    'exact-name-lookup',
    runtime.trace,
  )
  try {
    const permission = await authorizeDirectZipParent(
      runtime.fileSystem,
      request.currentParent,
      request.trustedAction,
      candidate.operationId,
      runtime.trace,
    )
    if (permission !== undefined) return gateDirectZipTarget(permission, candidate)
    const parentLocator = await runtime.handleBindings.compareParent(
      candidate.parentBinding,
      request.currentParent,
    )
    if (parentLocator !== 'same') {
      return gateDirectZipTarget(parentBindingDecision(), candidate)
    }
    return claimCandidateLocked(runtime, candidate, request.currentParent, locks.operation, true)
  } finally {
    await locks.release()
  }
}

async function claimCandidateLocked<ParentHandle, FileHandle>(
  runtime: DirectZipTargetRuntime<ParentHandle, FileHandle>,
  candidate: DirectZipReservationCandidate<ParentHandle>,
  parent: ParentHandle,
  lease: DirectZipOperationLeaseEvidence,
  mayOwnExistingEffect: boolean,
): Promise<CandidateAttempt<ParentHandle, FileHandle>> {
  let lookup
  try {
    lookup = await runtime.fileSystem.lookupExactName(parent, candidate.stableName)
  } catch (error) {
    return gateDirectZipTarget(
      directZipLifecycleDecisionForError(error, 'exact-name-lookup'),
      candidate,
    )
  }
  if (lookup.kind === 'occupied-non-file') {
    if (mayOwnExistingEffect) {
      return gateDirectZipTarget(bootstrapForeignReplacementDecision('exact-name-lookup'), candidate)
    }
    await retireCandidate(runtime, candidate, 'occupied-name', lease)
    return retired(candidate)
  }
  if (lookup.kind === 'file') {
    return inspectExistingCandidate(
      runtime,
      candidate,
      lookup.handle,
      lease,
      mayOwnExistingEffect,
    )
  }

  let file: FileHandle
  try {
    file = await runtime.fileSystem.createFile(parent, candidate.stableName)
  } catch (error) {
    const reconciliation = await reconcileCreateFailure(runtime, candidate, parent, lease, error)
    if (reconciliation !== undefined) return reconciliation
    return gateDirectZipTarget(
      directZipLifecycleDecisionForError(error, 'exact-name-create'),
      candidate,
    )
  }
  emitDirectZipTargetTrace(runtime.trace, {
    name: 'direct_zip.target.bootstrap',
    operation_id: directZipTraceIdentity(candidate.operationId),
    candidate_id: directZipCandidateTraceIdentity(candidate.candidateId),
    stable_name: candidate.stableName,
    stage: 'exact-name-create',
    outcome: 'file-handle-returned',
  })
  const initial = await snapshotCandidate(runtime, candidate, file)
  if (initial.kind === 'gated') return initial
  if (initial.value.size > 0n) {
    return resolveOccupiedObservation(runtime, candidate, file, initial.value, lease, true)
  }
  return writeBootstrapPrefix(runtime, candidate, file)
}

async function inspectExistingCandidate<ParentHandle, FileHandle>(
  runtime: DirectZipTargetRuntime<ParentHandle, FileHandle>,
  candidate: DirectZipReservationCandidate<ParentHandle>,
  file: FileHandle,
  lease: DirectZipOperationLeaseEvidence,
  mayOwnExistingEffect: boolean,
): Promise<CandidateAttempt<ParentHandle, FileHandle>> {
  const observed = await snapshotCandidate(runtime, candidate, file)
  if (observed.kind === 'gated') return observed
  return resolveOccupiedObservation(
    runtime,
    candidate,
    file,
    observed.value,
    lease,
    mayOwnExistingEffect,
  )
}

async function resolveOccupiedObservation<ParentHandle, FileHandle>(
  runtime: DirectZipTargetRuntime<ParentHandle, FileHandle>,
  candidate: DirectZipReservationCandidate<ParentHandle>,
  file: FileHandle,
  observation: DirectZipTargetObservationV1,
  lease: DirectZipOperationLeaseEvidence,
  mayOwnExistingEffect: boolean,
): Promise<CandidateAttempt<ParentHandle, FileHandle>> {
  if (observation.marker.kind === 'matching') {
    if (observation.size === observation.marker.prefixLength) {
      return bindReservedBootstrap(runtime, candidate, file, observation, true)
    }
    return gateDirectZipTarget(Object.freeze({
      kind: 'target-verification-required',
      stage: 'snapshot',
      reason: 'unknown-tail',
      proof: 'predecessor-epochs',
    }), candidate)
  }
  if (!mayOwnExistingEffect) {
    await retireCandidate(
      runtime,
      candidate,
      observation.marker.kind === 'foreign' ? 'bootstrap-marker-mismatch' : 'occupied-name',
      lease,
    )
    return retired(candidate)
  }
  return gateDirectZipTarget(bootstrapObservationDecision(observation), candidate)
}

async function writeBootstrapPrefix<ParentHandle, FileHandle>(
  runtime: DirectZipTargetRuntime<ParentHandle, FileHandle>,
  candidate: DirectZipReservationCandidate<ParentHandle>,
  file: FileHandle,
): Promise<CandidateAttempt<ParentHandle, FileHandle>> {
  const prefix = encodeDirectZipBootstrapPrefixV1(candidate.resultRootComponent, candidate.marker)
  let writable
  try {
    writable = await runtime.fileSystem.createWritable(file, false)
  } catch (error) {
    return resolveBootstrapFailure(runtime, candidate, file, error, 'bootstrap-write')
  }
  try {
    await writable.write(0n, prefix)
  } catch (error) {
    try {
      await writable.abort(error)
    } catch {
      // Fresh observation, not abort success, resolves whether the target changed.
    }
    return resolveBootstrapFailure(runtime, candidate, file, error, 'bootstrap-write')
  }
  emitDirectZipTargetTrace(runtime.trace, {
    name: 'direct_zip.target.bootstrap',
    operation_id: directZipTraceIdentity(candidate.operationId),
    candidate_id: directZipCandidateTraceIdentity(candidate.candidateId),
    stable_name: candidate.stableName,
    stage: 'bootstrap-write',
    outcome: 'prefix-staged',
  })

  let closeError: unknown
  try {
    await writable.close()
  } catch (error) {
    closeError = error
  }
  const observed = await snapshotCandidate(runtime, candidate, file)
  if (observed.kind === 'ready' && observed.value.marker.kind === 'matching' &&
      observed.value.size === observed.value.marker.prefixLength) {
    const errorName = nativeErrorName(closeError)
    emitDirectZipTargetTrace(runtime.trace, {
      name: 'direct_zip.target.bootstrap',
      operation_id: directZipTraceIdentity(candidate.operationId),
      candidate_id: directZipCandidateTraceIdentity(candidate.candidateId),
      stable_name: candidate.stableName,
      stage: 'bootstrap-close',
      outcome: closeError === undefined ? 'published-and-proven' : 'throw-after-publication-proven',
      target_length: observed.value.size.toString(),
      ...(errorName === undefined ? {} : { native_error_name: errorName }),
    })
    return bindReservedBootstrap(runtime, candidate, file, observed.value, false)
  }
  if (observed.kind === 'gated') return observed
  if (closeError !== undefined) {
    const classified = directZipLifecycleDecisionForError(closeError, 'bootstrap-close')
    if (classified.kind === 'destination-space-required' || classified.kind === 'authorization-required') {
      return gateDirectZipTarget(classified, candidate)
    }
  }
  return gateDirectZipTarget(bootstrapObservationDecision(observed.value), candidate)
}

async function resolveBootstrapFailure<ParentHandle, FileHandle>(
  runtime: DirectZipTargetRuntime<ParentHandle, FileHandle>,
  candidate: DirectZipReservationCandidate<ParentHandle>,
  file: FileHandle,
  error: unknown,
  stage: 'bootstrap-write',
): Promise<CandidateAttempt<ParentHandle, FileHandle>> {
  const observed = await snapshotCandidate(runtime, candidate, file)
  if (observed.kind === 'ready' && observed.value.marker.kind === 'matching' &&
      observed.value.size === observed.value.marker.prefixLength) {
    return bindReservedBootstrap(runtime, candidate, file, observed.value, false)
  }
  if (observed.kind === 'gated') return observed
  const decision = directZipLifecycleDecisionForError(error, stage)
  return decision.kind === 'destination-space-required' || decision.kind === 'authorization-required'
    ? gateDirectZipTarget(decision, candidate)
    : gateDirectZipTarget(bootstrapObservationDecision(observed.value), candidate)
}

async function reconcileCreateFailure<ParentHandle, FileHandle>(
  runtime: DirectZipTargetRuntime<ParentHandle, FileHandle>,
  candidate: DirectZipReservationCandidate<ParentHandle>,
  parent: ParentHandle,
  lease: DirectZipOperationLeaseEvidence,
  error: unknown,
): Promise<CandidateAttempt<ParentHandle, FileHandle> | undefined> {
  let lookup
  try {
    lookup = await runtime.fileSystem.lookupExactName(parent, candidate.stableName)
  } catch {
    return undefined
  }
  if (lookup.kind === 'file') {
    const inspected = await inspectExistingCandidate(runtime, candidate, lookup.handle, lease, true)
    const classified = directZipLifecycleDecisionForError(error, 'exact-name-create')
    if (inspected.kind === 'ready' && 'disposition' in inspected.value &&
        (classified.kind === 'destination-space-required' ||
          classified.kind === 'authorization-required')) {
      return gateDirectZipTarget(classified)
    }
    return inspected
  }
  if (lookup.kind === 'occupied-non-file') {
    return gateDirectZipTarget(bootstrapForeignReplacementDecision('exact-name-create'), candidate)
  }
  const decision = directZipLifecycleDecisionForError(error, 'exact-name-create')
  return decision.kind === 'destination-space-required' || decision.kind === 'authorization-required'
    ? gateDirectZipTarget(decision, candidate)
    : undefined
}

function bootstrapObservationDecision(
  observation: DirectZipTargetObservationV1,
): DirectZipLifecycleDecision {
  if (observation.marker.kind === 'foreign') {
    return Object.freeze({
      kind: 'needs-attention',
      stage: 'snapshot',
      reason: 'foreign-replacement',
    })
  }
  if (observation.marker.kind === 'matching') {
    return Object.freeze({
      kind: 'target-verification-required',
      stage: 'snapshot',
      reason: 'unknown-tail',
      proof: 'predecessor-epochs',
    })
  }
  return Object.freeze({
    kind: 'target-verification-required',
    stage: 'snapshot',
    reason: observation.marker.kind === 'partial'
      ? 'ownership-marker-incomplete'
      : 'ownership-marker-malformed',
    proof: 'ownership-marker',
  })
}

function bootstrapForeignReplacementDecision(
  stage: 'exact-name-lookup' | 'exact-name-create',
): DirectZipLifecycleDecision {
  return Object.freeze({ kind: 'needs-attention', stage, reason: 'foreign-replacement' })
}

async function snapshotCandidate<ParentHandle, FileHandle>(
  runtime: DirectZipTargetRuntime<ParentHandle, FileHandle>,
  candidate: DirectZipReservationCandidate<ParentHandle>,
  file: FileHandle,
): Promise<DirectZipTargetOutcome<DirectZipTargetObservationV1, DirectZipReservationCandidate<ParentHandle>>> {
  let snapshot: DirectZipFileSnapshotPort
  try {
    snapshot = await runtime.fileSystem.snapshot(file)
    const observation = await observeDirectZipTarget(snapshot, {
      resultRootComponent: candidate.resultRootComponent,
      marker: candidate.marker,
      parentLocator: 'same',
      fileLocator: 'unknown',
    })
    emitDirectZipTargetTrace(runtime.trace, {
      name: 'direct_zip.target.observation',
      operation_id: directZipTraceIdentity(candidate.operationId),
      candidate_id: directZipCandidateTraceIdentity(candidate.candidateId),
      stable_name: candidate.stableName,
      stage: 'snapshot',
      outcome: observation.marker.kind,
      target_length: observation.size.toString(),
    })
    return readyDirectZipTarget(observation)
  } catch (error) {
    return gateDirectZipTarget(
      directZipLifecycleDecisionForError(error, 'snapshot'),
      candidate,
    )
  }
}

async function bindReservedBootstrap<ParentHandle, FileHandle>(
  runtime: DirectZipTargetRuntime<ParentHandle, FileHandle>,
  candidate: DirectZipReservationCandidate<ParentHandle>,
  file: FileHandle,
  observation: DirectZipTargetObservationV1,
  recoveredExistingPrefix: boolean,
): Promise<CandidateAttempt<ParentHandle, FileHandle>> {
  if (observation.marker.kind !== 'matching') throw new Error('bootstrap binding lacks marker proof')
  try {
    const fileBinding = await runtime.handleBindings.bindFile({
      operationId: candidate.operationId,
      targetRef: candidate.targetRef,
      stableName: candidate.stableName,
      parentBinding: candidate.parentBinding,
      file,
    })
    const binding: DirectZipOwnedTargetBinding<ParentHandle, FileHandle> = Object.freeze({
      ...candidate,
      fileBinding: Object.freeze({
        handleRef: fileBinding.handleRef,
        bindingDigest: snapshotDirectZipFixedBytes(
          fileBinding.bindingDigest,
          DIRECT_ZIP_BINDING_DIGEST_BYTES,
          'direct ZIP file binding digest',
        ),
        persistedHandle: fileBinding.persistedHandle,
      }),
      bootstrapPrefixLength: observation.marker.prefixLength,
    })
    const boundObservation = Object.freeze({ ...observation, fileLocator: 'same' as const })
    return readyDirectZipTarget(Object.freeze({
      binding,
      observation: boundObservation,
      recoveredExistingPrefix,
    }))
  } catch (error) {
    emitDirectZipTargetTrace(runtime.trace, {
      name: 'direct_zip.target.bootstrap',
      operation_id: directZipTraceIdentity(candidate.operationId),
      candidate_id: directZipCandidateTraceIdentity(candidate.candidateId),
      stable_name: candidate.stableName,
      stage: 'candidate-persist',
      outcome: 'file-binding-refused',
      native_error_name: nativeErrorName(error) ?? 'Error',
    })
    return gateDirectZipTarget(Object.freeze({
      kind: 'target-verification-required',
      stage: 'candidate-persist',
      reason: 'native-effect-ambiguous',
      proof: 'fresh-observation',
    }), candidate)
  }
}

async function retireCandidate<ParentHandle, FileHandle>(
  runtime: DirectZipTargetRuntime<ParentHandle, FileHandle>,
  candidate: DirectZipReservationCandidate<ParentHandle>,
  reason: Parameters<typeof runtime.reservations.retireCandidate>[1],
  lease: DirectZipOperationLeaseEvidence,
): Promise<void> {
  await runtime.reservations.retireCandidate(candidate, reason, lease)
  traceCandidate(runtime, candidate, `retired:${reason}`)
}

function retired<ParentHandle>(candidate: DirectZipReservationCandidate<ParentHandle>) {
  return readyDirectZipTarget(Object.freeze({ disposition: 'retired' as const, candidate }))
}

function parentBindingDecision(): DirectZipLifecycleDecision {
  return Object.freeze({
    kind: 'target-verification-required',
    stage: 'snapshot',
    reason: 'parent-binding-changed',
    proof: 'fresh-observation',
  })
}

function traceCandidate<ParentHandle, FileHandle>(
  runtime: DirectZipTargetRuntime<ParentHandle, FileHandle>,
  candidate: DirectZipReservationCandidate<ParentHandle>,
  outcome: string,
): void {
  emitDirectZipTargetTrace(runtime.trace, {
    name: 'direct_zip.target.candidate',
    operation_id: directZipTraceIdentity(candidate.operationId),
    candidate_id: directZipCandidateTraceIdentity(candidate.candidateId),
    stable_name: candidate.stableName,
    stage: 'candidate-persist',
    outcome,
  })
}

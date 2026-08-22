import {
  acquireDirectZipTargetLocks,
  authorizeDirectZipParent,
  directZipLifecycleDecisionForError,
  emitDirectZipTargetTrace,
} from './authority'
import {
  directZipCandidateTraceIdentity,
  directZipTraceIdentity,
  type DirectZipProofStatus,
  type DirectZipLifecycleDecision,
  type DirectZipRecoveryResolution,
  type DirectZipTargetObservationV1,
  type DirectZipTargetOutcome,
} from './model'
import {
  observeDirectZipTarget,
  verifyDirectZipCommittedEpochChain,
  verifyDirectZipEpochProof,
} from './observation'
import type { DirectZipFileSnapshotPort } from './ports'
import { decideDirectZipRecovery } from './recovery'
import {
  gateDirectZipTarget,
  readyDirectZipTarget,
  type DirectZipReopenRequest,
  type DirectZipReopenResult,
  type DirectZipTargetRuntime,
} from './target'

export interface DirectZipLockedInspection<FileHandle> extends DirectZipReopenResult<FileHandle> {
  readonly snapshot: DirectZipFileSnapshotPort
}

interface LocatedDirectZipTarget<FileHandle> {
  readonly currentFile: FileHandle
  readonly snapshot: DirectZipFileSnapshotPort
  readonly observation: DirectZipTargetObservationV1
}

export async function reopenDirectZipTarget<ParentHandle, FileHandle>(
  runtime: DirectZipTargetRuntime<ParentHandle, FileHandle>,
  request: DirectZipReopenRequest<ParentHandle, FileHandle>,
): Promise<DirectZipTargetOutcome<DirectZipReopenResult<FileHandle>>> {
  const locks = await acquireDirectZipTargetLocks(
    runtime.operationLeases,
    runtime.parentLocks,
    request.binding.operationId,
    request.currentParent,
    'exact-name-lookup',
    runtime.trace,
  )
  try {
    const result = await inspectDirectZipTargetLocked(runtime, request)
    if (result.kind === 'gated') return result
    return readyDirectZipTarget(Object.freeze({
      resolution: result.value.resolution,
      observation: result.value.observation,
      currentFile: result.value.currentFile,
    }))
  } finally {
    await locks.release()
  }
}

export async function inspectDirectZipTargetLocked<ParentHandle, FileHandle>(
  runtime: DirectZipTargetRuntime<ParentHandle, FileHandle>,
  request: DirectZipReopenRequest<ParentHandle, FileHandle>,
): Promise<DirectZipTargetOutcome<DirectZipLockedInspection<FileHandle>>> {
  const binding = request.binding
  const permission = await authorizeDirectZipParent(
    runtime.fileSystem,
    request.currentParent,
    request.trustedAction,
    binding.operationId,
    runtime.trace,
  )
  if (permission !== undefined) return gateDirectZipTarget(permission)
  const located = await locateDirectZipTarget(runtime, request)
  if (located.kind === 'gated') return located
  const { currentFile, snapshot, observation } = located.value
  emitDirectZipTargetTrace(runtime.trace, {
    name: 'direct_zip.target.observation',
    operation_id: directZipTraceIdentity(binding.operationId),
    candidate_id: directZipCandidateTraceIdentity(binding.candidateId),
    stable_name: binding.stableName,
    stage: 'snapshot',
    outcome: `${observation.marker.kind}:${observation.fileLocator}`,
    target_length: observation.size.toString(),
  })
  const proofResolution = await resolveRecoveryProofs(request, snapshot, observation)
  const { predecessorProof, candidateProof, decision } = proofResolution
  emitDirectZipTargetTrace(runtime.trace, {
    name: 'direct_zip.target.recovery',
    operation_id: directZipTraceIdentity(binding.operationId),
    candidate_id: directZipCandidateTraceIdentity(binding.candidateId),
    stable_name: binding.stableName,
    stage: 'range-proof',
    outcome: `${predecessorProof}:${candidateProof}`,
    target_length: observation.size.toString(),
    decision: decision.kind,
  })
  if (isRecoveryResolution(decision)) {
    return readyDirectZipTarget(Object.freeze({
      resolution: decision,
      observation,
      currentFile,
      snapshot,
    }))
  }
  return gateDirectZipTarget(decision)
}

async function locateDirectZipTarget<ParentHandle, FileHandle>(
  runtime: DirectZipTargetRuntime<ParentHandle, FileHandle>,
  request: DirectZipReopenRequest<ParentHandle, FileHandle>,
): Promise<DirectZipTargetOutcome<LocatedDirectZipTarget<FileHandle>>> {
  let parentLocator
  try {
    parentLocator = await runtime.handleBindings.compareParent(
      request.binding.parentBinding,
      request.currentParent,
    )
  } catch (error) {
    return gateDirectZipTarget(directZipLifecycleDecisionForError(error, 'snapshot'))
  }
  if (parentLocator !== 'same') return gateDirectZipTarget(parentBindingChanged())

  let lookup
  try {
    lookup = await runtime.fileSystem.lookupExactName(
      request.currentParent,
      request.binding.stableName,
    )
  } catch (error) {
    return gateDirectZipTarget(directZipLifecycleDecisionForError(error, 'exact-name-lookup'))
  }
  if (lookup.kind === 'absent') return gateDirectZipTarget(targetDeleted())
  if (lookup.kind === 'occupied-non-file') return gateDirectZipTarget(foreignTarget())

  try {
    const fileLocator = await runtime.handleBindings.compareFile(
      request.binding.fileBinding,
      lookup.handle,
    )
    const snapshot = await runtime.fileSystem.snapshot(lookup.handle)
    const observation = await observeDirectZipTarget(snapshot, {
      resultRootComponent: request.binding.resultRootComponent,
      marker: request.binding.marker,
      parentLocator,
      fileLocator,
    })
    return readyDirectZipTarget(Object.freeze({
      currentFile: lookup.handle,
      snapshot,
      observation,
    }))
  } catch (error) {
    return gateDirectZipTarget(directZipLifecycleDecisionForError(error, 'snapshot'))
  }
}

async function resolveRecoveryProofs<ParentHandle, FileHandle>(
  request: DirectZipReopenRequest<ParentHandle, FileHandle>,
  snapshot: DirectZipFileSnapshotPort,
  observation: DirectZipTargetObservationV1,
): Promise<Readonly<{
  readonly predecessorProof: DirectZipProofStatus
  readonly candidateProof: DirectZipProofStatus
  readonly decision: ReturnType<typeof decideDirectZipRecovery>
}>> {
  let predecessorProof: DirectZipProofStatus = 'unchecked'
  let candidateProof: DirectZipProofStatus = 'unchecked'
  let decision = recoveryDecision(request, observation, predecessorProof, candidateProof)
  if (request.verifyChangedEvidence === false) {
    return Object.freeze({ predecessorProof, candidateProof, decision })
  }
  if (decision.kind === 'target-verification-required' &&
      decision.proof === 'ownership-marker') {
    predecessorProof = await proofStatus(() => verifyDirectZipCommittedEpochChain(
      snapshot,
      request.predecessor.committedEpochs,
      request.predecessor.committedLength,
    ))
    decision = finalizeMarkerAmbiguity(request, observation, predecessorProof)
    return Object.freeze({ predecessorProof, candidateProof, decision })
  }
  if (decision.kind === 'target-verification-required' && decision.proof === 'candidate-range' &&
      request.candidate !== undefined) {
    candidateProof = await proofStatus(() => verifyDirectZipEpochProof(snapshot, request.candidate!.epoch))
    decision = recoveryDecision(request, observation, predecessorProof, candidateProof)
  }
  if (decision.kind === 'target-verification-required' &&
      decision.proof === 'predecessor-epochs') {
    predecessorProof = await proofStatus(() => verifyDirectZipCommittedEpochChain(
      snapshot,
      request.predecessor.committedEpochs,
      request.predecessor.committedLength,
    ))
    decision = recoveryDecision(request, observation, predecessorProof, candidateProof)
  }
  return Object.freeze({ predecessorProof, candidateProof, decision })
}

function finalizeMarkerAmbiguity<ParentHandle, FileHandle>(
  request: DirectZipReopenRequest<ParentHandle, FileHandle>,
  observation: DirectZipTargetObservationV1,
  proof: DirectZipProofStatus,
): ReturnType<typeof decideDirectZipRecovery> {
  if (proof === 'unchecked') {
    return Object.freeze({
      kind: 'target-verification-required',
      stage: 'range-proof',
      reason: 'native-effect-ambiguous',
      proof: 'predecessor-epochs',
    })
  }
  let reason: Extract<DirectZipLifecycleDecision, { readonly kind: 'needs-attention' }>['reason']
  if (proof === 'verified') reason = 'ownership-unknown'
  else if (observation.size < request.predecessor.committedLength) {
    reason = 'committed-prefix-lost'
  } else reason = 'committed-prefix-mismatch'
  return Object.freeze({ kind: 'needs-attention', stage: 'range-proof', reason })
}

function recoveryDecision<ParentHandle, FileHandle>(
  request: DirectZipReopenRequest<ParentHandle, FileHandle>,
  observation: DirectZipTargetObservationV1,
  predecessorProof: DirectZipProofStatus,
  candidateProof: DirectZipProofStatus,
): ReturnType<typeof decideDirectZipRecovery> {
  return decideDirectZipRecovery({
    target: observation,
    predecessor: request.predecessor,
    ...(request.candidate === undefined ? {} : { candidate: request.candidate }),
    predecessorProof,
    candidateProof,
  })
}

function isRecoveryResolution(
  decision: ReturnType<typeof decideDirectZipRecovery>,
): decision is DirectZipRecoveryResolution {
  return decision.kind === 'replay-predecessor' || decision.kind === 'promote-candidate' ||
    decision.kind === 'truncate-to-predecessor'
}

function parentBindingChanged() {
  return Object.freeze({
    kind: 'target-verification-required' as const,
    stage: 'snapshot' as const,
    reason: 'parent-binding-changed' as const,
    proof: 'fresh-observation' as const,
  })
}

function targetDeleted() {
  return Object.freeze({
    kind: 'restart-required' as const,
    stage: 'exact-name-lookup' as const,
    reason: 'target-deleted' as const,
  })
}

function foreignTarget() {
  return Object.freeze({
    kind: 'needs-attention' as const,
    stage: 'exact-name-lookup' as const,
    reason: 'foreign-replacement' as const,
  })
}

async function proofStatus(proof: () => Promise<boolean>): Promise<DirectZipProofStatus> {
  try {
    return await proof() ? 'verified' : 'mismatch'
  } catch {
    return 'unchecked'
  }
}

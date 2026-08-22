import type { DirectZipCanonicalBytes } from '../format/canonical'
import { DIRECT_ZIP_RESERVATION_MAXIMUM_CANDIDATES } from './model'
import type {
  DirectZipCandidateTargetExpectation,
  DirectZipCheckpointTargetExpectation,
  DirectZipLifecycleDecision,
  DirectZipOwnedTargetBinding,
  DirectZipParentBinding,
  DirectZipRecoveryResolution,
  DirectZipReservationCandidate,
  DirectZipTargetObservationV1,
  DirectZipTargetOutcome,
  DirectZipTargetTrace,
} from './model'
import { reserveDirectZipBootstrap, resumeDirectZipBootstrap } from './reservation'
import { reopenDirectZipTarget } from './reopen'
import { deleteDirectZipTarget, openDirectZipEpoch, truncateDirectZipTarget } from './mutation'
import type {
  DirectZipFileSystemPort,
  DirectZipHandleBindingPort,
  DirectZipOperationLeasePort,
  DirectZipParentLockPort,
  DirectZipReservationCandidatePort,
  DirectZipTargetRandomPort,
} from './ports'

export interface DirectZipTargetDependencies<ParentHandle, FileHandle> {
  readonly fileSystem: DirectZipFileSystemPort<ParentHandle, FileHandle>
  readonly handleBindings: DirectZipHandleBindingPort<ParentHandle, FileHandle>
  readonly reservations: DirectZipReservationCandidatePort<ParentHandle>
  readonly operationLeases: DirectZipOperationLeasePort
  readonly parentLocks: DirectZipParentLockPort<ParentHandle>
  readonly random: DirectZipTargetRandomPort
  readonly trace?: DirectZipTargetTrace
  readonly maximumReservationCandidates?: number
}

export interface DirectZipReserveBootstrapRequest<ParentHandle> {
  readonly operationId: Uint8Array
  readonly resultRootComponent: string
  readonly parentBinding: DirectZipParentBinding<ParentHandle>
  readonly currentParent: ParentHandle
  readonly trustedAction: boolean
}

export interface DirectZipReservedBootstrap<ParentHandle, FileHandle> {
  readonly binding: DirectZipOwnedTargetBinding<ParentHandle, FileHandle>
  readonly observation: DirectZipTargetObservationV1
  readonly recoveredExistingPrefix: boolean
}

export interface DirectZipResumeBootstrapRequest<ParentHandle> {
  readonly candidate: DirectZipReservationCandidate<ParentHandle>
  readonly currentParent: ParentHandle
  readonly trustedAction: boolean
}

export type DirectZipBootstrapCandidateResult<ParentHandle, FileHandle> =
  | DirectZipReservedBootstrap<ParentHandle, FileHandle>
  | Readonly<{
    readonly disposition: 'retired'
    readonly candidate: DirectZipReservationCandidate<ParentHandle>
  }>

export interface DirectZipReopenRequest<ParentHandle, FileHandle> {
  readonly binding: DirectZipOwnedTargetBinding<ParentHandle, FileHandle>
  readonly currentParent: ParentHandle
  readonly predecessor: DirectZipCheckpointTargetExpectation
  readonly candidate?: DirectZipCandidateTargetExpectation
  readonly trustedAction: boolean
  readonly verifyChangedEvidence?: boolean
}

export interface DirectZipReopenResult<FileHandle> {
  readonly resolution: DirectZipRecoveryResolution
  readonly observation: DirectZipTargetObservationV1
  readonly currentFile: FileHandle
}

export interface DirectZipEpochCloseResult {
  readonly observation: DirectZipTargetObservationV1
  readonly nativeCloseError?: unknown
}

export interface DirectZipEpochWritable {
  write(position: bigint, bytes: Uint8Array): Promise<DirectZipTargetOutcome<undefined>>
  truncate(size: bigint): Promise<DirectZipTargetOutcome<undefined>>
  closeAndObserve(): Promise<DirectZipTargetOutcome<DirectZipEpochCloseResult>>
  abortAndObserve(reason?: unknown): Promise<DirectZipTargetOutcome<DirectZipTargetObservationV1>>
}

export interface DirectZipTruncateResult {
  readonly disposition: 'already-at-predecessor' | 'truncated'
  readonly observation: DirectZipTargetObservationV1
  readonly nativeCloseError?: unknown
}

export interface DirectZipCleanupRequest<ParentHandle, FileHandle> {
  readonly binding: DirectZipOwnedTargetBinding<ParentHandle, FileHandle>
  readonly currentParent: ParentHandle
  readonly predecessor: DirectZipCheckpointTargetExpectation
  readonly candidate?: DirectZipCandidateTargetExpectation
  readonly trustedAction: boolean
}

export interface DirectZipCleanupResult {
  readonly disposition: 'deleted' | 'already-absent'
}

export interface DirectZipTargetPort<ParentHandle, FileHandle> {
  reserveBootstrap(
    request: DirectZipReserveBootstrapRequest<ParentHandle>,
  ): Promise<DirectZipTargetOutcome<
    DirectZipReservedBootstrap<ParentHandle, FileHandle>,
    DirectZipReservationCandidate<ParentHandle>
  >>
  resumeBootstrap(
    request: DirectZipResumeBootstrapRequest<ParentHandle>,
  ): Promise<DirectZipTargetOutcome<
    DirectZipBootstrapCandidateResult<ParentHandle, FileHandle>,
    DirectZipReservationCandidate<ParentHandle>
  >>
  reopen(
    request: DirectZipReopenRequest<ParentHandle, FileHandle>,
  ): Promise<DirectZipTargetOutcome<DirectZipReopenResult<FileHandle>>>
  openEpoch(
    request: DirectZipReopenRequest<ParentHandle, FileHandle>,
  ): Promise<DirectZipTargetOutcome<DirectZipEpochWritable>>
  truncateToPredecessor(
    request: DirectZipReopenRequest<ParentHandle, FileHandle>,
  ): Promise<DirectZipTargetOutcome<DirectZipTruncateResult>>
  deleteProvenTarget(
    request: DirectZipCleanupRequest<ParentHandle, FileHandle>,
  ): Promise<DirectZipTargetOutcome<DirectZipCleanupResult>>
}

export interface DirectZipTargetRuntime<ParentHandle, FileHandle>
  extends DirectZipTargetDependencies<ParentHandle, FileHandle> {
  readonly trace: DirectZipTargetTrace
  readonly maximumReservationCandidates: number
}

export function createDirectZipTarget<ParentHandle, FileHandle>(
  dependencies: DirectZipTargetDependencies<ParentHandle, FileHandle>,
): DirectZipTargetPort<ParentHandle, FileHandle> {
  const runtime = directZipTargetRuntime(dependencies)
  const target: DirectZipTargetPort<ParentHandle, FileHandle> = {
    reserveBootstrap: (request: DirectZipReserveBootstrapRequest<ParentHandle>) =>
      reserveDirectZipBootstrap(runtime, request),
    resumeBootstrap: (request: DirectZipResumeBootstrapRequest<ParentHandle>) =>
      resumeDirectZipBootstrap(runtime, request),
    reopen: (request: DirectZipReopenRequest<ParentHandle, FileHandle>) =>
      reopenDirectZipTarget(runtime, request),
    openEpoch: (request: DirectZipReopenRequest<ParentHandle, FileHandle>) =>
      openDirectZipEpoch(runtime, request),
    truncateToPredecessor: (request: DirectZipReopenRequest<ParentHandle, FileHandle>) =>
      truncateDirectZipTarget(runtime, request),
    deleteProvenTarget: (request: DirectZipCleanupRequest<ParentHandle, FileHandle>) =>
      deleteDirectZipTarget(runtime, request),
  }
  return Object.freeze(target)
}

function directZipTargetRuntime<ParentHandle, FileHandle>(
  dependencies: DirectZipTargetDependencies<ParentHandle, FileHandle>,
): DirectZipTargetRuntime<ParentHandle, FileHandle> {
  if (dependencies === null || typeof dependencies !== 'object') {
    throw new TypeError('direct ZIP target dependencies are required')
  }
  const maximumReservationCandidates = dependencies.maximumReservationCandidates ??
    DIRECT_ZIP_RESERVATION_MAXIMUM_CANDIDATES
  if (!Number.isInteger(maximumReservationCandidates) || maximumReservationCandidates <= 0) {
    throw new TypeError('direct ZIP reservation candidate bound must be a positive integer')
  }
  return Object.freeze({
    ...dependencies,
    trace: dependencies.trace ?? (() => undefined),
    maximumReservationCandidates,
  })
}

export function gateDirectZipTarget<Value, Effect = never>(
  decision: DirectZipLifecycleDecision,
  retainedEffect?: Effect,
): DirectZipTargetOutcome<Value, Effect> {
  return Object.freeze({
    kind: 'gated',
    decision,
    ...(retainedEffect === undefined ? {} : { retainedEffect }),
  })
}

export function readyDirectZipTarget<Value>(value: Value): DirectZipTargetOutcome<Value> {
  return Object.freeze({ kind: 'ready', value })
}

export function directZipOperationBytes(value: Uint8Array): DirectZipCanonicalBytes {
  return Uint8Array.from(value)
}

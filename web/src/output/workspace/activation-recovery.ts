import type { ReceiveIntent } from '../../transfer/intent'
import {
  inspectOriginPrivateWorkspaceActivationCandidate,
  reopenOriginPrivateWorkspaceNamespace,
  type OriginPrivateWorkspaceNamespace,
} from '../origin-private/namespace'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import type {
  WorkspaceCheckpointCleanupObservation,
  WorkspaceOwnedCleanupPort,
  WorkspaceOwnedObjectCleanupObservation,
} from './cleanup'
import { equalCanonicalBytes } from './canonical'
import {
  decodeStoredReceiveOperation,
  operationRecordId,
  RECEIVE_RECORD_LIFECYCLE_STATE,
  RECEIVE_RECORD_OPERATION,
  RECEIVE_RECORD_WORKSPACE_ACTIVATION,
  RECEIVE_RECORD_WORKSPACE_BINDING,
  storedWorkspaceActivationCandidate,
  type PersistedReceiveRecord,
  type WorkspaceActivationCandidateV1,
} from './records'
import type {
  ReceiveOperationRepository,
  WorkspaceActivationJournalRepository,
} from './repository'
import { createCleanupReceipt } from './receipts'
import {
  initialReceiveLifecycleState,
  nextReceiveLifecycleState,
  type ReceiveLifecycleState,
} from './state'
import { decodeStoredReceiveLifecycleState } from './state-codec'
import {
  WorkspaceOperationStages,
  promoteWorkspaceActivation,
  type WorkspaceContentRequestCounter,
  type WorkspaceStageTraceListener,
} from './stages'
import type { OutputDiagnosticsPorts } from '../diagnostics'

const ZERO_CONTENT_REQUESTS: WorkspaceContentRequestCounter = Object.freeze({ count: () => 0n })
const WORKSPACE_ACTIVATION_LOCK_DOMAIN = 'windshare/workspace-activation/v1'

export type WorkspaceActivationCandidateRecovery =
  | Readonly<{ readonly kind: 'absent' }>
  | Readonly<{
      readonly kind: 'promoted'
      readonly namespace: OriginPrivateWorkspaceNamespace
    }>
  | Readonly<{
      readonly kind: 'needs-attention'
      readonly lifecycle: Extract<ReceiveLifecycleState, { kind: 'needs-attention' }>
    }>

/** Inventory never observes an unclassified activation candidate. */
export async function recoverWorkspaceActivationCandidates(input: {
  readonly repository: WorkspaceActivationJournalRepository
  readonly storage?: StorageManager & { getDirectory(): Promise<FileSystemDirectoryHandle> }
  readonly locks?: LockManager
}): Promise<void> {
  const candidates = await input.repository.listWorkspaceActivationCandidates()
  for (const candidate of candidates) {
    await withWorkspaceActivationLock(candidate.operationId, () =>
      recoverWorkspaceActivationCandidate({
        repository: input.repository,
        candidate,
        ...(input.storage === undefined ? {} : { storage: input.storage }),
      }), input.locks)
  }
  const initialOperationIds = await input.repository.listInitialWorkspaceActivationOperationIds()
  for (const operationId of initialOperationIds) {
    await withWorkspaceActivationLock(operationId, () =>
      retainInterruptedPromotedActivation({
        repository: input.repository,
        operationId,
        ...(input.storage === undefined ? {} : { storage: input.storage }),
      }), input.locks)
  }
}

export function withWorkspaceActivationLock<Result>(
  operationId: string,
  execute: () => Promise<Result>,
  manager: LockManager = navigator.locks,
): Promise<Result> {
  if (typeof manager?.request !== 'function') {
    throw new DOMException('Workspace activation lock authority is unavailable', 'NotSupportedError')
  }
  return manager.request(`${WORKSPACE_ACTIVATION_LOCK_DOMAIN}/${operationId}`, execute)
}

export async function recoverWorkspaceActivationCandidate(input: {
  readonly repository: WorkspaceActivationJournalRepository
  readonly candidate: WorkspaceActivationCandidateV1
  readonly storage?: StorageManager & { getDirectory(): Promise<FileSystemDirectoryHandle> }
}): Promise<WorkspaceActivationCandidateRecovery> {
  const candidateRecord = await storedWorkspaceActivationCandidate(input.candidate)
  if (await input.repository.readRecord(candidateRecord.id) === undefined) {
    return recoverPromotedWorkspaceActivation(input)
  }
  const authority = await readWorkspaceActivationAuthority(input.repository, input.candidate)
  let observation
  try {
    observation = await inspectOriginPrivateWorkspaceActivationCandidate({
      candidate: input.candidate,
      ...(input.storage === undefined ? {} : { storage: input.storage }),
    })
  } catch {
    return recordActivationNeedsAttention(input.repository, input.candidate, authority.lifecycle)
  }
  if (observation.kind === 'ownership-unknown') {
    return recordActivationNeedsAttention(input.repository, input.candidate, authority.lifecycle)
  }
  const [pages, handles, lease] = await Promise.all([
    input.repository.listManifestPages(input.candidate.operationId),
    input.repository.listHandles(input.candidate.operationId),
    input.repository.readLease(input.candidate.operationId),
  ])
  if (pages.length !== 0 || handles.length !== 0 || lease !== undefined) {
    return recordActivationNeedsAttention(input.repository, input.candidate, authority.lifecycle)
  }
  if (observation.kind === 'verified') {
    await promoteWorkspaceActivation({
      repository: input.repository,
      candidate: input.candidate,
      workspaceRootHandle: observation.namespace.root,
      expectedLifecycleGeneration: authority.lifecycle.generation,
    })
    return Object.freeze({ kind: 'promoted', namespace: observation.namespace })
  }

  await input.repository.commitTransition({
    operationId: input.candidate.operationId,
    expectedLifecycleGeneration: authority.lifecycle.generation,
    deleteRecordIds: authority.records.map(record => record.id),
  })
  const persistence = await observeWorkspaceActivationPersistence({
    repository: input.repository,
    operationId: input.candidate.operationId,
    rootHandleId: input.candidate.rootHandleId,
  })
  const candidates = await input.repository.listWorkspaceActivationCandidates()
  if (persistence.kind !== 'absent' ||
      candidates.some(candidate => candidate.operationId === input.candidate.operationId)) {
    throw new DOMException('Workspace activation absence could not be durably verified', 'InvalidStateError')
  }
  return Object.freeze({ kind: 'absent' })
}

async function recoverPromotedWorkspaceActivation(input: {
  readonly repository: WorkspaceActivationJournalRepository
  readonly candidate: WorkspaceActivationCandidateV1
  readonly storage?: StorageManager & { getDirectory(): Promise<FileSystemDirectoryHandle> }
}): Promise<WorkspaceActivationCandidateRecovery> {
  const [root, observation] = await Promise.all([
    input.repository.readHandle<FileSystemDirectoryHandle>(input.candidate.rootHandleId),
    inspectOriginPrivateWorkspaceActivationCandidate({
      candidate: input.candidate,
      ...(input.storage === undefined ? {} : { storage: input.storage }),
    }),
  ])
  if (root === undefined && observation.kind === 'absent') {
    const persistence = await observeWorkspaceActivationPersistence({
      repository: input.repository,
      operationId: input.candidate.operationId,
      rootHandleId: input.candidate.rootHandleId,
    })
    if (persistence.kind === 'absent') return Object.freeze({ kind: 'absent' })
  }
  if (root === undefined || root.operationId !== input.candidate.operationId ||
      root.authorityRef !== input.candidate.repositoryAuthority ||
      root.ownedObjectId !== input.candidate.rootOwnedObjectId ||
      root.handle.kind !== 'directory' || observation.kind !== 'verified' ||
      !await root.handle.isSameEntry(observation.namespace.root)) {
    throw new TargetOwnershipUnknownError('parent-authority', input.candidate.operationId)
  }
  return Object.freeze({ kind: 'promoted', namespace: observation.namespace })
}

async function retainInterruptedPromotedActivation(input: {
  readonly repository: WorkspaceActivationJournalRepository
  readonly operationId: string
  readonly storage?: StorageManager & { getDirectory(): Promise<FileSystemDirectoryHandle> }
}): Promise<void> {
  const [operationRecord, lifecycleRecord, lease] = await Promise.all([
    input.repository.readRecord(operationRecordId(input.operationId, RECEIVE_RECORD_OPERATION)),
    input.repository.readLifecycle(input.operationId),
    input.repository.readLease(input.operationId),
  ])
  if (operationRecord === undefined || lifecycleRecord === undefined || lease !== undefined) return
  const operation = await decodeStoredReceiveOperation(operationRecord)
  if (operation.receiveIntent.plan.kind !== 'workspace-then-publish') return
  const lifecycle = decodeStoredReceiveLifecycleState(lifecycleRecord)
  if (lifecycle.kind !== 'intent-frozen') return

  let exactOwnership = false
  let lastVerifiedRecordDigest = operation.digest
  try {
    const namespace = await reopenOriginPrivateWorkspaceNamespace({
      receiveIntent: operation.receiveIntent,
      repository: input.repository,
      ...(input.storage === undefined ? {} : { storage: input.storage }),
    })
    exactOwnership = true
    lastVerifiedRecordDigest = namespace.rootOwnedObjectId
  } catch {
    // The durable lifecycle must remain visible even when the exact OPFS owner is inconclusive.
  }
  await input.repository.commitTransition({
    operationId: input.operationId,
    expectedLifecycleGeneration: lifecycle.generation,
    lifecycle: nextReceiveLifecycleState(lifecycle, {
      kind: 'needs-attention',
      reason: exactOwnership ? 'cleanup-unknown' : 'target-ownership-unknown',
      lastVerifiedRecordDigest,
    }),
  })
}

export type WorkspaceActivationPersistenceObservation =
  | Readonly<{ kind: 'absent' }>
  | Readonly<{ kind: 'owned-effects' }>

export interface WorkspaceActivationSettlement {
  readonly lifecycle: ReceiveLifecycleState
  readonly workspaceUsage: null
}

/** Produces terminal evidence only after both durable stores proved absence. */
export async function discardedWorkspaceActivationSettlement(
  intent: ReceiveIntent,
  removedOwnedObjectIds: readonly string[],
): Promise<WorkspaceActivationSettlement> {
  const initial = initialReceiveLifecycleState({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
  })
  const receipt = await createCleanupReceipt({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    removedObjectIds: removedOwnedObjectIds,
    removedRecordDigests: [],
    cleanupGeneration: initial.generation + 1n,
  })
  return mutation(nextReceiveLifecycleState(initial, {
    kind: 'discarded',
    cleanupReceiptDigest: receipt.digest,
  }))
}

/**
 * A rejected route commit may claim harmless invalidation only after every
 * operation-confined store has proved absence. A partial inventory is ownership.
 */
export async function observeWorkspaceActivationPersistence(input: {
  readonly repository: ReceiveOperationRepository
  readonly operationId: string
  readonly rootHandleId: string
}): Promise<WorkspaceActivationPersistenceObservation> {
  const handleInventory = 'listHandles' in input.repository &&
      typeof input.repository.listHandles === 'function'
    ? input.repository.listHandles(input.operationId)
    : input.repository.readHandle(input.rootHandleId).then(root =>
        root === undefined ? Object.freeze([]) : Object.freeze([root]))
  const [records, pages, handles, lease] = await Promise.all([
    input.repository.listRecords(input.operationId),
    input.repository.listManifestPages(input.operationId),
    handleInventory,
    input.repository.readLease(input.operationId),
  ])
  return records.length === 0 && pages.length === 0 && handles.length === 0 && lease === undefined
    ? Object.freeze({ kind: 'absent' })
    : Object.freeze({ kind: 'owned-effects' })
}

async function readWorkspaceActivationAuthority(
  repository: WorkspaceActivationJournalRepository,
  candidate: WorkspaceActivationCandidateV1,
): Promise<Readonly<{
  records: readonly PersistedReceiveRecord[]
  lifecycle: ReceiveLifecycleState
}>> {
  const records = await repository.listRecords(candidate.operationId)
  const candidateRecord = await storedWorkspaceActivationCandidate(candidate)
  const operationRecord = records.find(record => record.kind === RECEIVE_RECORD_OPERATION)
  const lifecycleRecord = records.find(record => record.kind === RECEIVE_RECORD_LIFECYCLE_STATE)
  const workspaceRecord = records.find(record => record.kind === RECEIVE_RECORD_WORKSPACE_BINDING)
  const journalRecord = records.find(record => record.kind === RECEIVE_RECORD_WORKSPACE_ACTIVATION)
  const expectedIds = new Set([
    operationRecord?.id,
    lifecycleRecord?.id,
    workspaceRecord?.id,
    candidateRecord.id,
  ])
  if (operationRecord === undefined || lifecycleRecord === undefined || workspaceRecord === undefined ||
      journalRecord === undefined || journalRecord.id !== candidateRecord.id ||
      records.length !== 4 || expectedIds.has(undefined) || expectedIds.size !== 4) {
    throw new TypeError('workspace activation journal lacks its exact operation authority')
  }
  const operation = await decodeStoredReceiveOperation(operationRecord)
  if (operation.operationId !== candidate.operationId ||
      operation.receiveIntent.plan.kind !== 'workspace-then-publish' ||
      operation.receiveIntent.artifact.kind === 'directory-tree' ||
      operation.receiveIntent.plan.workspace.repositoryRef !== candidate.repositoryAuthority ||
      !equalCanonicalBytes(
        workspaceRecord.canonicalBytes,
        operation.receiveIntent.plan.workspace.canonicalBytes,
      )) {
    throw new TypeError('workspace activation journal changed its workspace binding')
  }
  const lifecycle = decodeStoredReceiveLifecycleState(lifecycleRecord)
  if (lifecycle.receiveIntentDigest !== operation.receiveIntent.digest) {
    throw new TypeError('workspace activation lifecycle escaped its receive intent')
  }
  return Object.freeze({ records, lifecycle })
}

async function recordActivationNeedsAttention(
  repository: WorkspaceActivationJournalRepository,
  candidate: WorkspaceActivationCandidateV1,
  current: ReceiveLifecycleState,
): Promise<Extract<WorkspaceActivationCandidateRecovery, { kind: 'needs-attention' }>> {
  const lifecycle = (current.kind === 'needs-attention'
    ? current
    : nextReceiveLifecycleState(current, {
        kind: 'needs-attention',
        reason: 'target-ownership-unknown',
        lastVerifiedRecordDigest: candidate.digest,
      })) as Extract<ReceiveLifecycleState, { kind: 'needs-attention' }>
  if (current.kind !== 'needs-attention') {
    await repository.commitTransition({
      operationId: candidate.operationId,
      expectedLifecycleGeneration: current.generation,
      lifecycle,
    })
  }
  const [persistedLifecycle, candidates] = await Promise.all([
    repository.readLifecycle(candidate.operationId),
    repository.listWorkspaceActivationCandidates(),
  ])
  if (persistedLifecycle === undefined ||
      decodeStoredReceiveLifecycleState(persistedLifecycle).kind !== 'needs-attention' ||
      !candidates.some(entry => entry.operationId === candidate.operationId &&
        entry.digest === candidate.digest)) {
    throw new DOMException(
      'Workspace activation NeedsAttention disposition was not durably verified',
      'InvalidStateError',
    )
  }
  return Object.freeze({ kind: 'needs-attention', lifecycle })
}

/**
 * Converts the activation's minimal persisted workspace into a terminal durable
 * disposition. Cleanup uncertainty is retained as NeedsAttention instead of
 * escaping as an ownerless exception.
 */
export class WorkspaceActivationRecovery {
  readonly lifecycle: ReceiveLifecycleState
  readonly #stages: WorkspaceOperationStages
  readonly #cleanupRequest: Parameters<WorkspaceOperationStages['discard']>[0] | undefined

  private constructor(input: {
    readonly lifecycle: ReceiveLifecycleState
    readonly stages: WorkspaceOperationStages
    readonly cleanupRequest?: Parameters<WorkspaceOperationStages['discard']>[0]
  }) {
    this.lifecycle = input.lifecycle
    this.#stages = input.stages
    this.#cleanupRequest = input.cleanupRequest
  }

  static async open(input: {
    readonly repository: ReceiveOperationRepository
    readonly receiveIntent: ReceiveIntent
    readonly leaseId: string
    readonly rootHandleId?: string
    readonly removeNamespace?: () => Promise<'removed' | 'already-absent'>
    readonly clock?: () => number
    readonly onTrace?: WorkspaceStageTraceListener
    readonly diagnostics?: OutputDiagnosticsPorts
  }): Promise<WorkspaceActivationRecovery> {
    const lifecycleRecord = await input.repository.readLifecycle(input.receiveIntent.operationId)
    if (lifecycleRecord === undefined) {
      throw new TypeError('workspace activation recovery lacks durable lifecycle authority')
    }
    const lifecycle = decodeStoredReceiveLifecycleState(lifecycleRecord)
    if (lifecycle.receiveIntentDigest !== input.receiveIntent.digest) {
      throw new TypeError('workspace activation recovery escaped its receive intent')
    }
    const stages = await WorkspaceOperationStages.open({
      repository: input.repository,
      receiveIntent: input.receiveIntent,
      leaseId: input.leaseId,
      clock: input.clock ?? Date.now,
      contentRequests: ZERO_CONTENT_REQUESTS,
      ...(input.onTrace === undefined ? {} : { onTrace: input.onTrace }),
      ...(input.diagnostics === undefined ? {} : { diagnostics: input.diagnostics }),
    })
    const cleanupRequest = input.rootHandleId === undefined || input.removeNamespace === undefined
      ? undefined
      : activationCleanupRequest(
          input.receiveIntent,
          input.rootHandleId,
          input.removeNamespace,
        )
    return new WorkspaceActivationRecovery({
      lifecycle,
      stages,
      ...(cleanupRequest === undefined ? {} : { cleanupRequest }),
    })
  }

  async settle(reason: unknown): Promise<WorkspaceActivationSettlement> {
    if (this.#cleanupRequest === undefined) return this.#recordOwnershipUnknown(reason)
    try {
      const result = await this.#stages.discard(this.#cleanupRequest)
      if (result.kind === 'clean' || result.kind === 'needs-attention') {
        return mutation(result.state)
      }
      return this.#recordOwnershipUnknown(reason)
    } catch (cleanupFailure) {
      return this.#recordOwnershipUnknown(new AggregateError(
        [reason, cleanupFailure],
        'Workspace activation cleanup could not prove a clean disposition',
        { cause: cleanupFailure },
      ))
    }
  }

  async #recordOwnershipUnknown(reason: unknown): Promise<WorkspaceActivationSettlement> {
    try {
      return mutation(await this.#stages.recordTargetOwnershipUnknown(this.lifecycle.receiveIntentDigest))
    } catch (recordFailure) {
      throw new AggregateError(
        [reason, recordFailure],
        'Workspace activation failure could not record its retained ownership state',
        { cause: recordFailure },
      )
    }
  }
}

function activationCleanupRequest(
  intent: ReceiveIntent,
  rootHandleId: string,
  removeNamespace: () => Promise<'removed' | 'already-absent'>,
): Parameters<WorkspaceOperationStages['discard']>[0] {
  const port: WorkspaceOwnedCleanupPort = Object.freeze({
    removeOwnedObject: (): Promise<WorkspaceOwnedObjectCleanupObservation> =>
      Promise.resolve(Object.freeze({ kind: 'ownership-unknown' })),
    removeFileCheckpoints: async (input: {
      readonly operationId: string
      readonly receiveIntentDigest: string
    }): Promise<WorkspaceCheckpointCleanupObservation> => {
      if (input.operationId !== intent.operationId ||
          input.receiveIntentDigest !== intent.digest) {
        return Object.freeze({ kind: 'ownership-unknown' })
      }
      try {
        await removeNamespace()
        return Object.freeze({ kind: 'clean', removedRecordDigests: Object.freeze([]) })
      } catch (error) {
        return Object.freeze({
          kind: error instanceof TargetOwnershipUnknownError
            ? 'ownership-unknown'
            : 'retryable-failure',
        })
      }
    },
  })
  return Object.freeze({
    targets: Object.freeze([]),
    metadataHandleIds: Object.freeze([rootHandleId]),
    port,
  })
}

function mutation(lifecycle: ReceiveLifecycleState): WorkspaceActivationSettlement {
  return Object.freeze({ lifecycle, workspaceUsage: null })
}

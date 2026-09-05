import type { ReceiveIntent } from '../../../transfer/intent'
import type { BrowserReceiveOperationLease } from '../../browser/session-lease'
import {
  OriginPrivateWorkspaceBudgetAuthority,
  type OriginPrivateStorageEstimate,
  type OriginPrivateWorkspaceBudgetClaim,
} from '../../origin-private/admission'
import { OriginPrivateWorkspaceBudgetOwnershipError } from '../../origin-private/admission-authority'
import { OriginPrivateWorkspaceRoot } from '../../origin-private/workspace-root'
import { TargetOwnershipUnknownError } from '../../persistent-tree/errors'
import { reduceReceiveLifecycle } from '../../workspace/lifecycle'
import {
  RECEIVE_RECORD_RECEIPT,
  RECEIVE_RECORD_RESERVATION,
  RECEIVE_RECORD_WORKSPACE_BINDING,
  createPersistedReceiveRecord,
  type PersistedReceiveRecord,
} from '../../workspace/records'
import {
  createExpiryReceipt,
  decodePreparationAdmissionAuthority,
  persistedReceiptRecord,
  type ExpiryReceiptV1,
  type PreparationAdmissionReceiptV1,
} from '../../workspace/receipts'
import type { ReceiveOperationRepository } from '../../workspace/repository'
import { storedReceiveLifecycleState } from '../../workspace/state-codec'
import { lifecycleDeadline, type PlanKind, type ReceiveLifecycleState } from '../../workspace/state'
import type { AdmittedWorkspaceContent, WorkspaceBudgetClaim } from '../../workspace/stages'
import { receiveOperationResumeDescriptor, type ReceiveOperationResumeDescriptor } from '../descriptor'
import type {
  PersistedReceiveOperationReopenPurpose,
  PersistedReopenSnapshot,
  PersistedWorkspaceBudgetReclaimInput,
  ReducerContext,
  ReopenResources,
  StableLifecycleKind,
} from './model'

export async function expectedBindingRecord(
  intent: ReceiveIntent,
): Promise<PersistedReceiveRecord | undefined> {
  if (intent.plan.kind === 'direct-tree') {
    return createPersistedReceiveRecord({
      operationId: intent.operationId,
      kind: RECEIVE_RECORD_RESERVATION,
      canonicalBytes: intent.plan.reservation.canonicalBytes,
    })
  }
  if (intent.plan.kind === 'workspace-then-publish') {
    return createPersistedReceiveRecord({
      operationId: intent.operationId,
      kind: RECEIVE_RECORD_WORKSPACE_BINDING,
      canonicalBytes: intent.plan.workspace.canonicalBytes,
    })
  }
  if (intent.plan.kind === 'direct-resumable-zip') return undefined
  throw new TypeError('persisted reopen does not support this materialization plan')
}

export async function assertDescriptorAuthority(
  descriptor: ReceiveOperationResumeDescriptor,
  snapshot: PersistedReopenSnapshot,
  nowMilliseconds: number,
  purpose: PersistedReceiveOperationReopenPurpose,
): Promise<void> {
  const lifecycleProjection = await storedReceiveLifecycleState(descriptor.lifecycle)
  if (descriptor.operationId !== snapshot.operation.operationId ||
      descriptor.receiveIntentDigest !== snapshot.operation.receiveIntentDigest ||
      descriptor.lifecycleGeneration !== snapshot.lifecycle.generation ||
      !samePersistedRecord(snapshot.lifecycleRecord, lifecycleProjection)) {
    throw new DOMException('Receive resume descriptor is stale or foreign', 'InvalidStateError')
  }
  const current = receiveOperationResumeDescriptor(snapshot.lifecycle, nowMilliseconds)
  if (current === undefined) {
    throw new DOMException('Receive lifecycle has no reopen authority', 'InvalidStateError')
  }
  if (current.continuation === 'needs-attention') {
    throw new DOMException('Receive continuation requires explicit owner recovery', 'InvalidStateError')
  }
  if (purpose === 'continue') {
    const crossedDeadline = current.continuation === 'cleanup-expired' &&
      descriptor.expiresAt !== undefined && nowMilliseconds >= descriptor.expiresAt
    if ((!crossedDeadline && current.continuation !== descriptor.continuation) ||
        current.continuation === 'retry-cleanup') {
      throw new DOMException('Receive continuation is stale or inert', 'InvalidStateError')
    }
  }
}

export async function persistReceiveResume(
  repository: ReceiveOperationRepository,
  snapshot: PersistedReopenSnapshot,
  lease: BrowserReceiveOperationLease,
  nowMilliseconds: number,
): Promise<Extract<ReceiveLifecycleState, { kind: 'receiving' }>> {
  const reduction = reduceReceiveLifecycle(snapshot.lifecycle, Object.freeze({
    kind: snapshot.lifecycle.kind === 'receiving' ? 'receive-authority-reacquired' : 'resume-started',
    expectedGeneration: snapshot.lifecycle.generation,
    leaseId: lease.leaseId,
  }), reducerContext(snapshot.operation.receiveIntent, lease, nowMilliseconds))
  if (reduction.status !== 'applied' || reduction.state.kind !== 'receiving') {
    throw new TypeError('receive reopen did not enter Receiving')
  }
  await repository.commitTransition({
    operationId: snapshot.operation.operationId,
    expectedLifecycleGeneration: snapshot.lifecycle.generation,
    expectedLeaseId: lease.leaseId,
    lifecycle: reduction.state,
  })
  return reduction.state
}

export async function persistOwnershipAttention(
  repository: ReceiveOperationRepository,
  snapshot: PersistedReopenSnapshot,
  lease: BrowserReceiveOperationLease,
  nowMilliseconds: number,
): Promise<Extract<ReceiveLifecycleState, { kind: 'needs-attention' }>> {
  const reduction = reduceReceiveLifecycle(snapshot.lifecycle, Object.freeze({
    kind: 'ownership-unknown',
    expectedGeneration: snapshot.lifecycle.generation,
    leaseId: lease.leaseId,
    lastVerifiedRecordDigest: snapshot.operationRecord.digest,
  }), reducerContext(snapshot.operation.receiveIntent, lease, nowMilliseconds))
  if (reduction.status !== 'applied' || reduction.state.kind !== 'needs-attention') {
    throw new TypeError('unknown reopen ownership did not become NeedsAttention')
  }
  await repository.commitTransition({
    operationId: snapshot.operation.operationId,
    expectedLifecycleGeneration: snapshot.lifecycle.generation,
    expectedLeaseId: lease.leaseId,
    lifecycle: reduction.state,
  })
  return reduction.state
}

export async function persistExpiry(
  repository: ReceiveOperationRepository,
  snapshot: PersistedReopenSnapshot,
  lease: BrowserReceiveOperationLease,
  nowMilliseconds: number,
): Promise<Readonly<{
  state: Extract<ReceiveLifecycleState, { kind: 'expired' }>
  receipt: ExpiryReceiptV1
}>> {
  const deadline = lifecycleDeadline(snapshot.lifecycle)
  if (deadline === undefined || nowMilliseconds < deadline) {
    throw new TypeError('receive expiry was requested before its stable deadline')
  }
  const receipt = await createExpiryReceipt({
    operationId: snapshot.operation.operationId,
    receiveIntentDigest: snapshot.operation.receiveIntentDigest,
    priorStableState: stableLifecycleKind(snapshot.lifecycle),
    expiresAt: deadline,
    cleanupState: 'cleanup-pending',
  })
  const reduction = reduceReceiveLifecycle(snapshot.lifecycle, Object.freeze({
    kind: 'expiry-observed',
    expectedGeneration: snapshot.lifecycle.generation,
    leaseId: lease.leaseId,
    expiryReceiptDigest: receipt.digest,
    cleanupState: 'cleanup-pending',
  }), reducerContext(snapshot.operation.receiveIntent, lease, nowMilliseconds))
  if (reduction.status !== 'applied' || reduction.state.kind !== 'expired') {
    throw new TypeError('elapsed receive operation did not become Expired')
  }
  await repository.commitTransition({
    operationId: snapshot.operation.operationId,
    expectedLifecycleGeneration: snapshot.lifecycle.generation,
    expectedLeaseId: lease.leaseId,
    records: [await persistedReceiptRecord(receipt)],
    lifecycle: reduction.state,
  })
  return Object.freeze({ state: reduction.state, receipt })
}

export async function readPersistedWorkspaceAdmission(
  repository: ReceiveOperationRepository,
  intent: ReceiveIntent,
): Promise<Readonly<{
  budget: AdmittedWorkspaceContent['budget']
  receipt: PreparationAdmissionReceiptV1
}>> {
  try {
    const records = await repository.listRecords(intent.operationId, RECEIVE_RECORD_RECEIPT)
    const authorities: Array<Readonly<{
      budget: AdmittedWorkspaceContent['budget']
      receipt: PreparationAdmissionReceiptV1
    }>> = []
    for (const record of records) {
      const authority = await decodePreparationAdmissionAuthority(record, intent)
      if (authority !== undefined) authorities.push(authority)
    }
    if (authorities.length !== 1) {
      throw new TypeError('workspace admission authority is missing or ambiguous')
    }
    return authorities[0]!
  } catch (error) {
    throw new TargetOwnershipUnknownError('reservation', intent.operationId, { cause: error })
  }
}

export async function reclaimOriginPrivateWorkspaceBudget(
  input: PersistedWorkspaceBudgetReclaimInput,
): Promise<import('../../workspace/stages').WorkspaceBudgetClaimResult> {
  if (input.intent.plan.kind !== 'workspace-then-publish') {
    throw new TypeError('workspace budget reclaim requires a workspace receive intent')
  }
  const durableLease = await input.repository.readLease(input.intent.operationId)
  if (durableLease?.leaseId !== input.operationLease.leaseId ||
      input.operationLease.operationId !== input.intent.operationId ||
      input.receipt.operationId !== input.intent.operationId ||
      input.receipt.receiveIntentDigest !== input.intent.digest ||
      input.receipt.workspaceBudgetDigest !== input.budget.digest) {
    throw new OriginPrivateWorkspaceBudgetOwnershipError()
  }
  const root = new OriginPrivateWorkspaceRoot({
    operationId: input.intent.operationId,
    receiveIntentDigest: input.intent.digest,
    workspaceBindingDigest: input.intent.plan.workspace.digest,
    authorityRef: input.intent.plan.workspace.repositoryRef,
    workspaceRootHandleId: input.namespace.rootHandleId,
    workspaceRootHandle: input.namespace.root,
    repository: input.repository,
  })
  const authority = await OriginPrivateWorkspaceBudgetAuthority.open(input.intent.operationId, {
    estimate: input.estimate,
    verifiedAlreadyOwnedBytes: () => root.verifiedAlreadyOwnedBytes(),
    jobLimitBytes: input.receipt.jobLimitBytes,
    processLimitBytes: input.receipt.processLimitBytes,
    minimumReserveBytes: input.receipt.minimumReserveBytes,
    now: input.now,
    ...(input.databaseName === undefined ? {} : { databaseName: input.databaseName }),
  })
  return authority.reclaim(input.budget, input.operationLease)
}

export function samePersistedRecord(
  expected: PersistedReceiveRecord,
  actual: PersistedReceiveRecord | undefined,
): boolean {
  if (actual === undefined || expected.id !== actual.id || expected.kind !== actual.kind ||
      expected.operationId !== actual.operationId || expected.digest !== actual.digest ||
      expected.reopenKey !== actual.reopenKey || expected.state !== actual.state ||
      expected.expiresAt !== actual.expiresAt ||
      expected.lifecycleGeneration !== actual.lifecycleGeneration ||
      expected.canonicalBytes.byteLength !== actual.canonicalBytes.byteLength) return false
  return expected.canonicalBytes.every((byte, index) => byte === actual.canonicalBytes[index])
}

export function requireOriginPrivateBudgetClaim(
  claim: WorkspaceBudgetClaim,
  operationId: string,
): OriginPrivateWorkspaceBudgetClaim {
  if (!('readmit' in claim) || typeof claim.readmit !== 'function') {
    throw new TargetOwnershipUnknownError('reservation', operationId)
  }
  return claim as OriginPrivateWorkspaceBudgetClaim
}

export function closeAuthority(
  repository: ReceiveOperationRepository,
  lease: BrowserReceiveOperationLease,
  resources: ReopenResources,
  observeReleaseFailure?: (failure: unknown) => void,
): () => Promise<void> {
  let closed: Promise<void> | undefined
  return () => {
    closed ??= (async () => {
      resources.closed = true
      await resources.receiveBackendOpening?.catch(() => undefined)
      const releases = await Promise.allSettled([
        ...(resources.receiveBackend === undefined ? [] : [resources.receiveBackend.close()]),
        ...(resources.packageBackend === undefined ? [] : [resources.packageBackend.close()]),
        ...(resources.reclaimedClaim === undefined ? [] : [resources.reclaimedClaim.release()]),
        lease.release(),
      ])
      resources.directZipJournal?.close()
      repository.close()
      const failures = releases.filter(
        (result): result is PromiseRejectedResult => result.status === 'rejected',
      )
      if (failures.length > 0) {
        for (const failure of failures) observeReleaseFailure?.(failure.reason)
        throw new AggregateError(
          failures.map((result) => result.reason),
          'Receive reopen authority did not close cleanly',
        )
      }
    })()
    return closed
  }
}

export async function closeAfterFailure(
  repository: ReceiveOperationRepository,
  resources: ReopenResources,
  failure: unknown,
  observeReleaseFailure?: (failure: unknown) => void,
): Promise<never> {
  const releases = await Promise.allSettled([
    ...(resources.packageBackend === undefined ? [] : [resources.packageBackend.close()]),
    ...(resources.reclaimedClaim === undefined ? [] : [resources.reclaimedClaim.release()]),
    ...(resources.lease === undefined ? [] : [resources.lease.release()]),
  ])
  resources.directZipJournal?.close()
  repository.close()
  const releaseFailures = releases
    .filter((result): result is PromiseRejectedResult => result.status === 'rejected')
    .map((result) => result.reason)
  if (releaseFailures.length > 0) {
    for (const releaseFailure of releaseFailures) {
      try {
        observeReleaseFailure?.(releaseFailure)
      } catch {
        // Authority release remains independent from an observational callback.
      }
    }
    throw new AggregateError(
      [failure, ...releaseFailures],
      'Receive reopen failure could not release its authority',
      { cause: releaseFailures[0] },
    )
  }
  throw failure
}

export async function estimateOriginPrivateStorage(): Promise<OriginPrivateStorageEstimate> {
  const storage = globalThis.navigator?.storage
  if (storage === undefined || typeof storage.estimate !== 'function') {
    throw new DOMException('Origin-private storage estimate is unavailable', 'NotSupportedError')
  }
  return storage.estimate()
}

export const SYSTEM_CLOCK = Object.freeze({ now: () => Date.now() })
export const ZERO_CONTENT_REQUESTS = Object.freeze({ count: () => 0n })

function reducerContext(
  intent: ReceiveIntent,
  lease: BrowserReceiveOperationLease,
  nowMilliseconds: number,
): ReducerContext {
  return Object.freeze({
    planKind: intent.plan.kind as PlanKind,
    preparationRequired: intent.plan.kind === 'workspace-then-publish' &&
      intent.plan.preparation === 'exact-zip',
    activeLeaseId: lease.leaseId,
    nowMilliseconds,
  })
}

function stableLifecycleKind(state: ReceiveLifecycleState): StableLifecycleKind {
  switch (state.kind) {
    case 'resumable-receive':
    case 'resumable-package':
    case 'waiting-to-save':
    case 'download-started':
    case 'authorization-required':
    case 'target-verification-required':
    case 'destination-space-required': return state.kind
    default: throw new TypeError('receive expiry requires a stable lifecycle')
  }
}

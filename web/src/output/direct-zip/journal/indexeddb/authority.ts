import {
  INDEXEDDB_DIRECT_ZIP_CENTRAL_PAGE_STORE,
  INDEXEDDB_DIRECT_ZIP_EPOCH_PAGE_STORE,
  INDEXEDDB_DIRECT_ZIP_LAYOUT_PAGE_STORE,
} from '../../../browser/indexeddb-database'
import { equalCanonicalBytes, snapshotIdentity } from '../../../workspace/canonical'
import type { PersistedReceiveRecord } from '../../../workspace/records'
import type { ReceiveLifecycleState } from '../../../workspace/state'
import type {
  DirectZipBootstrapCandidateV1,
  DirectZipCandidateRetirementV1,
  DirectZipCandidateV1,
  DirectZipCheckpointProposalV1,
  DirectZipCommitCandidateV1,
  DirectZipImmutablePageV1,
  DirectZipJournalFenceV1,
  DirectZipPageKind,
  DirectZipPolicyDigestsV1,
  DirectZipRecoveryGateV1,
  DirectZipStateRowV1,
} from '../model'
import {
  validateDirectZipBootstrapCandidateV1,
  validateDirectZipCommitCandidateV1,
} from '../records'

const DEFAULT_CURSOR_BATCH = 64
export const MAXIMUM_CURSOR_BATCH = 256
export const PAGE_ID_SUFFIX = '\uffff'

export class DirectZipJournalConcurrencyError extends DOMException {
  constructor(message = 'Direct ZIP journal fence no longer owns the operation') {
    super(message, 'InvalidStateError')
  }
}

export async function validateCandidate(input: unknown): Promise<DirectZipCandidateV1> {
  if (typeof input !== 'object' || input === null || !('kind' in input)) {
    throw new TypeError('Direct ZIP candidate row is invalid')
  }
  return input.kind === 'bootstrap'
    ? validateDirectZipBootstrapCandidateV1(input as DirectZipBootstrapCandidateV1)
    : validateDirectZipCommitCandidateV1(input as DirectZipCommitCandidateV1)
}

export function snapshotFence(input: DirectZipJournalFenceV1): DirectZipJournalFenceV1 {
  if (typeof input.checkpointGeneration !== 'bigint' || input.checkpointGeneration < 1n) {
    throw new TypeError('Direct ZIP journal checkpoint generation is invalid')
  }
  return Object.freeze({
    operationId: snapshotIdentity(input.operationId, 16, 'operation ID'),
    leaseId: snapshotIdentity(input.leaseId, 16, 'lease ID'),
    checkpointGeneration: input.checkpointGeneration,
  })
}

export function assertCandidateFence(
  candidate: DirectZipCommitCandidateV1,
  fence: DirectZipJournalFenceV1,
): void {
  if (candidate.operationId !== fence.operationId || candidate.leaseId !== fence.leaseId ||
      candidate.predecessorCheckpointGeneration !== fence.checkpointGeneration) {
    throw new TypeError('Direct ZIP candidate escaped its journal fence')
  }
}

export function previousPageBudgetUsage(page: DirectZipImmutablePageV1): Readonly<{
  memberCount: bigint
  canonicalMetadataBytes: bigint
}> {
  const metadataDelta = BigInt(page.canonicalBytes.byteLength)
  if (page.budgetUsage.memberCount < page.memberCountDelta ||
      page.budgetUsage.canonicalMetadataBytes < metadataDelta) {
    throw new TypeError('Direct ZIP page budget projection is invalid')
  }
  return Object.freeze({
    memberCount: page.budgetUsage.memberCount - page.memberCountDelta,
    canonicalMetadataBytes: page.budgetUsage.canonicalMetadataBytes - metadataDelta,
  })
}

export function sameUsage(
  page: DirectZipImmutablePageV1,
  usage: Readonly<{ memberCount: bigint; canonicalMetadataBytes: bigint }>,
): boolean {
  const previous = previousPageBudgetUsage(page)
  return previous.memberCount === usage.memberCount &&
    previous.canonicalMetadataBytes === usage.canonicalMetadataBytes
}

export function addCheckpointReachability(
  reachable: Map<string, bigint>,
  checkpoint: DirectZipStateRowV1['checkpoint'] | DirectZipCheckpointProposalV1,
): void {
  for (const [kind, chain] of [
    ['layout', checkpoint.layoutPages],
    ['central', checkpoint.centralPages],
    ['epoch', checkpoint.epochPages],
  ] as const) {
    const key = pageReachabilityKey(kind, chain.chainId)
    const current = reachable.get(key) ?? 0n
    if (chain.pageCount > current) reachable.set(key, chain.pageCount)
  }
}

export function pageReachabilityKey(kind: DirectZipPageKind, chainId: string): string {
  return `${kind}\u0000${chainId}`
}

export function directZipPageStore(kind: DirectZipPageKind): string {
  switch (kind) {
    case 'layout': return INDEXEDDB_DIRECT_ZIP_LAYOUT_PAGE_STORE
    case 'central': return INDEXEDDB_DIRECT_ZIP_CENTRAL_PAGE_STORE
    case 'epoch': return INDEXEDDB_DIRECT_ZIP_EPOCH_PAGE_STORE
  }
}

export function pageKindFromId(id: string): DirectZipPageKind {
  for (const kind of ['layout', 'central', 'epoch'] as const) {
    if (id.includes(`/${kind}/`)) return kind
  }
  throw new TypeError('Direct ZIP page ID has no canonical kind')
}

export function requirePageKind(kind: DirectZipPageKind): DirectZipPageKind {
  if (kind === 'layout' || kind === 'central' || kind === 'epoch') return kind
  throw new TypeError('Direct ZIP page kind is invalid')
}

export function cursorLimit(input: number | undefined): number {
  const limit = input ?? DEFAULT_CURSOR_BATCH
  if (!Number.isInteger(limit) || limit < 1 || limit > MAXIMUM_CURSOR_BATCH) {
    throw new TypeError('Direct ZIP cursor batch limit is invalid')
  }
  return limit
}

export function requireBootstrapCandidateCursor(input: string): string {
  if (typeof input !== 'string' ||
      !input.startsWith('windshare/direct-zip-candidate/v1/') ||
      input.length > 512) {
    throw new TypeError('Direct ZIP bootstrap candidate cursor is invalid')
  }
  return input
}

export function sameRanking(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index])
}

export function sameDirectZipPolicies(
  binding: Readonly<{
    zipEncoding: string
    layout: string
    checkpoint: string
    journalBudget: string
    epoch: string
  }>,
  journal: DirectZipPolicyDigestsV1,
): boolean {
  return binding.zipEncoding === journal.encodingPolicyDigest &&
    binding.layout === journal.layoutPolicyDigest &&
    binding.checkpoint === journal.checkpointPolicyDigest &&
    binding.journalBudget === journal.journalBudgetDigest &&
    binding.epoch === journal.epochPolicyDigest
}

export function sameJournalPolicies(
  left: DirectZipPolicyDigestsV1,
  right: DirectZipPolicyDigestsV1,
): boolean {
  return left.encodingPolicyDigest === right.encodingPolicyDigest &&
    left.layoutPolicyDigest === right.layoutPolicyDigest &&
    left.checkpointPolicyDigest === right.checkpointPolicyDigest &&
    left.journalBudgetDigest === right.journalBudgetDigest &&
    left.epochPolicyDigest === right.epochPolicyDigest
}

export function assertLifecycleCheckpointCut(
  lifecycle: ReceiveLifecycleState,
  checkpoint: DirectZipStateRowV1['checkpoint'],
  recoveryGate: DirectZipRecoveryGateV1 | undefined,
): void {
  if (lifecycle.kind === 'resumable-receive' && lifecycle.payloadKind === 'direct-zip' &&
      (lifecycle.directZipCheckpointDigest !== checkpoint.digest ||
       lifecycle.safeSelectedPayloadBytes !== checkpoint.committedSelectedPayloadBytes ||
       lifecycle.committedArchiveLength !== checkpoint.committedArchiveLength ||
       lifecycle.checkpointPhase !== checkpoint.phase)) {
    throw new TypeError('Direct ZIP resumable lifecycle disagrees with its checkpoint')
  }
  const gated = lifecycle.kind === 'authorization-required' ||
    lifecycle.kind === 'target-verification-required' ||
    lifecycle.kind === 'destination-space-required'
  if (gated !== (recoveryGate !== undefined)) {
    throw new TypeError('Direct ZIP lifecycle and recovery gate must advance together')
  }
  if (recoveryGate !== undefined &&
      (recoveryGate.operationId !== checkpoint.operationId ||
       recoveryGate.receiveIntentDigest !== checkpoint.receiveIntentDigest ||
       recoveryGate.checkpointDigest !== checkpoint.digest ||
       recoveryGate.kind !== lifecycle.kind ||
       recoveryGate.digest !== lifecycle.recoveryGateDigest)) {
    throw new TypeError('Direct ZIP recovery gate disagrees with its lifecycle cut')
  }
}

export function assertPromotedCheckpointCut(
  checkpoint: DirectZipStateRowV1['checkpoint'],
  candidate: DirectZipCommitCandidateV1,
): void {
  const proposed = candidate.proposedCheckpoint
  if (checkpoint.generation !== proposed.generation ||
      checkpoint.predecessorCheckpointDigest !== candidate.predecessorCheckpointDigest ||
      checkpoint.candidateLineageDigest !== candidate.digest ||
      checkpoint.targetObservation.digest === candidate.predecessorTargetObservation.digest ||
      checkpoint.targetObservation.ownershipMarkerDigest !==
        candidate.predecessorTargetObservation.ownershipMarkerDigest ||
      ((candidate.kind === 'closing') !==
        (checkpoint.closingReplay?.completion !== undefined)) ||
      (candidate.kind === 'closing' &&
        checkpoint.closingReplay?.completion?.preClosingEpochRootDigest !==
          candidate.predecessorTargetObservation.epochRootDigest) ||
      !sameCheckpointResumeAuthority(proposed, checkpoint)) {
    throw new TypeError('Direct ZIP promotion did not bind its fresh observed checkpoint')
  }
}

export function assertRecoveryLifecycleCut(
  lifecycle: ReceiveLifecycleState,
  state: DirectZipStateRowV1,
  fence: DirectZipJournalFenceV1,
  recoveryGate: DirectZipRecoveryGateV1 | undefined,
  candidate: DirectZipCommitCandidateV1 | undefined,
): void {
  const gated = lifecycle.kind === 'authorization-required' ||
    lifecycle.kind === 'target-verification-required' ||
    lifecycle.kind === 'destination-space-required'
  if (gated !== (recoveryGate !== undefined)) {
    throw new TypeError('Direct ZIP recovery lifecycle and gate must advance together')
  }
  if (recoveryGate !== undefined && gated) {
    if (recoveryGate.operationId !== fence.operationId ||
        recoveryGate.receiveIntentDigest !== state.checkpoint.receiveIntentDigest ||
        recoveryGate.checkpointDigest !== state.checkpointDigest ||
        recoveryGate.kind !== lifecycle.kind ||
        recoveryGate.digest !== lifecycle.recoveryGateDigest ||
        ((candidate === undefined) !== (recoveryGate.candidateDigest === undefined)) ||
        (candidate !== undefined && recoveryGate.candidateDigest !== candidate.digest)) {
      throw new TypeError('Direct ZIP recovery gate disagrees with its retained authority')
    }
    return
  }
  if (candidate !== undefined) {
    throw new TypeError('Direct ZIP candidate recovery cannot clear its gate before resolution')
  }
  if (lifecycle.kind === 'receiving') {
    if (lifecycle.activeLeaseId !== fence.leaseId) {
      throw new TypeError('Direct ZIP recovery resumed under a foreign lease')
    }
    return
  }
  if (lifecycle.kind !== 'resumable-receive' || lifecycle.payloadKind !== 'direct-zip' ||
      lifecycle.directZipCheckpointDigest !== state.checkpointDigest ||
      lifecycle.safeSelectedPayloadBytes !== state.checkpoint.committedSelectedPayloadBytes ||
      lifecycle.committedArchiveLength !== state.checkpoint.committedArchiveLength ||
      lifecycle.checkpointPhase !== state.checkpoint.phase) {
    throw new TypeError('Direct ZIP recovery lifecycle does not retain its checkpoint')
  }
}

export function assertRetirementCheckpointCut(
  disposition: DirectZipCandidateRetirementV1['disposition'],
  state: DirectZipStateRowV1,
  checkpoint: DirectZipStateRowV1['checkpoint'],
  candidate: DirectZipCommitCandidateV1,
): void {
  if (disposition === 'replay-predecessor') {
    if (checkpoint.digest !== state.checkpointDigest) {
      throw new TypeError('Direct ZIP predecessor replay must retain its exact checkpoint')
    }
    return
  }
  if (disposition !== 'truncate-and-replay' ||
      checkpoint.generation !== state.checkpoint.generation + 1n ||
      checkpoint.predecessorCheckpointDigest !== state.checkpointDigest ||
      !sameCheckpointResumeAuthority(state.checkpoint, checkpoint) ||
      checkpoint.candidateLineageDigest !== candidate.digest) {
    throw new TypeError('Direct ZIP truncate retirement did not persist one fresh observation')
  }
}

export function assertLifecycleForCheckpoint(
  lifecycle: ReceiveLifecycleState,
  checkpoint: DirectZipStateRowV1['checkpoint'],
  fence: DirectZipJournalFenceV1,
): void {
  if (lifecycle.operationId !== fence.operationId ||
      lifecycle.receiveIntentDigest !== checkpoint.receiveIntentDigest) {
    throw new TypeError('Direct ZIP retirement lifecycle escaped its operation')
  }
  if (lifecycle.kind === 'receiving') {
    if (lifecycle.activeLeaseId !== fence.leaseId) {
      throw new TypeError('Direct ZIP retirement resumed under a foreign lease')
    }
    return
  }
  if (lifecycle.kind !== 'resumable-receive' || lifecycle.payloadKind !== 'direct-zip' ||
      lifecycle.directZipCheckpointDigest !== checkpoint.digest ||
      lifecycle.safeSelectedPayloadBytes !== checkpoint.committedSelectedPayloadBytes ||
      lifecycle.committedArchiveLength !== checkpoint.committedArchiveLength ||
      lifecycle.checkpointPhase !== checkpoint.phase) {
    throw new TypeError('Direct ZIP retirement lifecycle disagrees with its checkpoint')
  }
}

export function sameCheckpointResumeAuthority(
  left: DirectZipStateRowV1['checkpoint'] | DirectZipCheckpointProposalV1,
  right: DirectZipStateRowV1['checkpoint'],
): boolean {
  return left.operationId === right.operationId &&
    left.receiveIntentDigest === right.receiveIntentDigest &&
    left.targetBindingDigest === right.targetBindingDigest &&
    sameJournalPolicies(left.policies, right.policies) &&
    left.phase === right.phase && left.entryOrdinal === right.entryOrdinal &&
    sameCurrentMember(left.currentMember, right.currentMember) &&
    equalCanonicalBytes(left.discovery.cursorCanonicalBytes, right.discovery.cursorCanonicalBytes) &&
    left.discovery.directoryAdmissionDigest === right.discovery.directoryAdmissionDigest &&
    left.discovery.discoveryRootDigest === right.discovery.discoveryRootDigest &&
    left.archiveOffset === right.archiveOffset &&
    left.committedArchiveLength === right.committedArchiveLength &&
    left.committedSelectedPayloadBytes === right.committedSelectedPayloadBytes &&
    left.parentBindingDigest === right.parentBindingDigest &&
    left.fileBindingDigest === right.fileBindingDigest &&
    left.epochRootDigest === right.epochRootDigest &&
    samePageChain(left.layoutPages, right.layoutPages) &&
    samePageChain(left.centralPages, right.centralPages) &&
    samePageChain(left.epochPages, right.epochPages) &&
    left.journalUsage.memberCount === right.journalUsage.memberCount &&
    left.journalUsage.canonicalMetadataBytes === right.journalUsage.canonicalMetadataBytes &&
    left.accountingTailPageId === right.accountingTailPageId &&
    sameClosingReplay(left.closingReplay, right.closingReplay)
}

export function sameCurrentMember(
  left: DirectZipStateRowV1['checkpoint']['currentMember'],
  right: DirectZipStateRowV1['checkpoint']['currentMember'],
): boolean {
  if (left === undefined || right === undefined) return left === right
  return left.fileId === right.fileId && left.fileRevision === right.fileRevision &&
    left.exactSize === right.exactSize &&
    left.sourceRangeAuthorityDigest === right.sourceRangeAuthorityDigest &&
    left.entryPlan.ordinal === right.entryPlan.ordinal &&
    left.entryPlanDigest === right.entryPlanDigest &&
    equalCanonicalBytes(left.entryPlanCanonicalBytes, right.entryPlanCanonicalBytes) &&
    left.memberPayloadOffset === right.memberPayloadOffset &&
    left.crc32Accumulator === right.crc32Accumulator &&
    sameMemberRollback(left.rollback, right.rollback)
}

export function sameMemberRollback(
  left: NonNullable<DirectZipStateRowV1['checkpoint']['currentMember']>['rollback'],
  right: NonNullable<DirectZipStateRowV1['checkpoint']['currentMember']>['rollback'],
): boolean {
  return left.archiveOffset === right.archiveOffset &&
    left.safeSelectedPayloadBytes === right.safeSelectedPayloadBytes &&
    left.entryOrdinal === right.entryOrdinal && left.epochStart === right.epochStart &&
    left.predecessorEpochRootDigest === right.predecessorEpochRootDigest &&
    left.epochContentDigest === right.epochContentDigest &&
    left.epochRootDigest === right.epochRootDigest &&
    samePageChain(left.layoutPages, right.layoutPages) &&
    samePageChain(left.centralPages, right.centralPages) &&
    samePageChain(left.epochPages, right.epochPages) &&
    left.journalUsage.memberCount === right.journalUsage.memberCount &&
    left.journalUsage.canonicalMetadataBytes === right.journalUsage.canonicalMetadataBytes &&
    left.accountingTailPageId === right.accountingTailPageId
}

export function samePageChain(
  left: DirectZipStateRowV1['checkpoint']['layoutPages'],
  right: DirectZipStateRowV1['checkpoint']['layoutPages'],
): boolean {
  return left.chainId === right.chainId && left.rootDigest === right.rootDigest &&
    left.pageCount === right.pageCount && left.recordCount === right.recordCount &&
    left.canonicalMetadataBytes === right.canonicalMetadataBytes
}

export function sameClosingReplay(
  left: DirectZipStateRowV1['checkpoint']['closingReplay'],
  right: DirectZipStateRowV1['checkpoint']['closingReplay'],
): boolean {
  if (left === undefined || right === undefined) return left === right
  return left.archiveOffset === right.archiveOffset &&
    left.centralRecordRootDigest === right.centralRecordRootDigest
}

export function sameStateRow(input: unknown, expected: DirectZipStateRowV1): boolean {
  if (!isObjectRow(input)) return false
  const checkpoint = input.checkpoint
  if (!isObjectRow(checkpoint) || !(checkpoint.canonicalBytes instanceof Uint8Array)) return false
  const gate = input.recoveryGate
  const sameGate = expected.recoveryGate === undefined
    ? gate === undefined
    : isObjectRow(gate) && gate.digest === expected.recoveryGate.digest &&
      gate.canonicalBytes instanceof Uint8Array &&
      equalCanonicalBytes(gate.canonicalBytes, expected.recoveryGate.canonicalBytes)
  return input.id === expected.id && input.operationId === expected.operationId &&
    input.leaseId === expected.leaseId &&
    input.checkpointGeneration === expected.checkpointGeneration &&
    input.checkpointDigest === expected.checkpointDigest && sameGate &&
    equalCanonicalBytes(checkpoint.canonicalBytes, expected.checkpoint.canonicalBytes)
}

export function sameCandidateRow(input: unknown, expected: DirectZipCandidateV1): boolean {
  return isObjectRow(input) && input.id === expected.id && input.kind === expected.kind &&
    input.operationId === expected.operationId && input.candidateId === expected.candidateId &&
    input.digest === expected.digest && input.canonicalBytes instanceof Uint8Array &&
    equalCanonicalBytes(input.canonicalBytes, expected.canonicalBytes)
}

export function sameBootstrapReservationAuthority(
  left: DirectZipBootstrapCandidateV1,
  right: DirectZipBootstrapCandidateV1,
): boolean {
  return left.id === right.id && left.operationId === right.operationId &&
    left.candidateId === right.candidateId &&
    equalCanonicalBytes(left.selectionCanonicalBytes, right.selectionCanonicalBytes) &&
    equalCanonicalBytes(left.artifactCanonicalBytes, right.artifactCanonicalBytes) &&
    equalCanonicalBytes(left.choiceIdentityCanonicalBytes, right.choiceIdentityCanonicalBytes) &&
    left.choiceId === right.choiceId && sameRanking(left.preClickRanking, right.preClickRanking) &&
    left.stablePhysicalName === right.stablePhysicalName &&
    left.ownershipNonce === right.ownershipNonce &&
    left.targetBindingDigest === right.targetBindingDigest &&
    sameJournalPolicies(left.policies, right.policies) &&
    left.parentHandleId === right.parentHandleId
}

export function samePageRow(input: unknown, expected: DirectZipImmutablePageV1): boolean {
  return isObjectRow(input) && input.id === expected.id && input.pageKind === expected.pageKind &&
    input.operationId === expected.operationId && input.chainId === expected.chainId &&
    input.pageOrdinal === expected.pageOrdinal && input.digest === expected.digest &&
    input.canonicalBytes instanceof Uint8Array &&
    equalCanonicalBytes(input.canonicalBytes, expected.canonicalBytes)
}

export function samePersistedRecordRow(
  input: unknown,
  expected: PersistedReceiveRecord,
): boolean {
  return isObjectRow(input) && input.id === expected.id && input.kind === expected.kind &&
    input.operationId === expected.operationId && input.digest === expected.digest &&
    input.state === expected.state && input.expiresAt === expected.expiresAt &&
    input.lifecycleGeneration === expected.lifecycleGeneration &&
    input.canonicalBytes instanceof Uint8Array &&
    equalCanonicalBytes(input.canonicalBytes, expected.canonicalBytes)
}

export function isHandleForOperation(input: unknown, operationId: string, handleId: string): boolean {
  return isObjectRow(input) && input.id === handleId && input.operationId === operationId &&
    typeof input.kind === 'number' && typeof input.authorityRef === 'string' && 'handle' in input
}

export function isObjectRow(input: unknown): input is Record<string, unknown> {
  return typeof input === 'object' && input !== null && !Array.isArray(input)
}

export function collectCursorValues(request: IDBRequest<IDBCursorWithValue | null>, limit: number): Promise<unknown[]> {
  return collectCursorRows(request, limit).then(rows => rows.map(row => row.value))
}

export function collectCursorRows(
  request: IDBRequest<IDBCursorWithValue | null>,
  limit: number,
): Promise<readonly Readonly<{ primaryKey: IDBValidKey; value: unknown }>[]> {
  return new Promise((resolve, reject) => {
    const rows: Readonly<{ primaryKey: IDBValidKey; value: unknown }>[] = []
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('success', () => {
      const cursor = request.result
      if (cursor === null || rows.length >= limit) {
        resolve(Object.freeze(rows))
        return
      }
      rows.push(Object.freeze({ primaryKey: cursor.primaryKey, value: cursor.value }))
      if (rows.length >= limit) resolve(Object.freeze(rows))
      else cursor.continue()
    })
  })
}

export function abortQuietly(transaction: IDBTransaction): void {
  try {
    transaction.abort()
  } catch {
    // Completion or prior abort already preserved the transaction boundary.
  }
}

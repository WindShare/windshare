import type { BrowserReceiveOperationLease } from '../../browser/session-lease'
import { verifyBrowserReceiveOperationLease } from '../../browser/session-lease'
import { openFSAFileCheckpointRepository, scanAllFSAFileCheckpoints } from '../../file-system-access/checkpoint-repository'
import { requireDirectTreeIntent } from '../../file-system-access/settlement-proof'
import { retireFSAMaterializationRecoveryMetadata } from '../../file-system-access/recovery-metadata-retirement'
import { createMaterializationLedgerBinding } from '../../materialization-ledger/codec'
import { decodeStoredReceiveOperation, operationRecordId, RECEIVE_RECORD_OPERATION } from '../../workspace/records'
import { decodeStoredReceiveLifecycleState } from '../../workspace/state-codec'
import type { ReceiveOperationRepository } from '../../workspace/repository'
import type { ReceiveLifecycleState } from '../../workspace/state'
import { IndexedDbBrowserDeliveryRepository } from '../indexeddb'
import type { BrowserDeliveryRecordV1 } from '../model'
import { deriveBrowserDeliveryLifecycle } from './authority'
import { persistBrowserDeliveryLocalMutation } from './mutation-journal'

export interface BrowserDeliveryLifecycleAuthority {
  readonly repository: ReceiveOperationRepository
  readonly lease: BrowserReceiveOperationLease
  readonly databaseName?: string
}

/** Persist the old target cut before opening any writer; a crash can then reconstruct the frozen lifecycle exactly. */
export async function beginBrowserDeliveryLocalMutation(input: BrowserDeliveryLifecycleAuthority): Promise<ReceiveLifecycleState> {
  const result = await reconcileBrowserDeliveryLifecycle(input)
  await withLocalAuthority(input, async authority => {
    const { lifecycle, records, checkpoints, lifecycleRecord } = authority
    if (lifecycle.kind !== 'resumable-receive' || lifecycle.payloadKind !== 'file-set') return
    for (const previous of records) {
      if (!['receiving', 'restart-authorized', 'staged-complete', 'copying'].includes(previous.state.kind)) continue
      const priorTargetCheckpoint = checkpoints.find(checkpoint => checkpoint.fileId === previous.fileId)
      await persistBrowserDeliveryLocalMutation({
        ...(input.databaseName === undefined ? {} : { databaseName: input.databaseName }),
        previous, lifecycleRecord, leaseId: input.lease.leaseId,
        mutation: {
          lifecycleGeneration: lifecycle.generation, checkpointSetDigest: lifecycle.checkpointSetDigest,
          ...(priorTargetCheckpoint === undefined ? {} : { priorTargetCheckpoint }),
        },
      })
    }
  })
  return result
}

/** Call after all target writers drain, including failed copies. Exact baselines also make this safe after a crash. */
export async function reconcileBrowserDeliveryLifecycle(input: BrowserDeliveryLifecycleAuthority): Promise<ReceiveLifecycleState> {
  return withLocalAuthority(input, async authority => {
    const { lifecycle, intent, policy, records, checkpoints } = authority
    if (lifecycle.kind === 'partial-directory' || lifecycle.kind === 'published') {
      await retireTerminalDeliveryMetadata(input, intent)
      return lifecycle
    }
    if (lifecycle.kind !== 'resumable-receive' || lifecycle.payloadKind !== 'file-set' || policy === undefined) return lifecycle
    const next = await deriveBrowserDeliveryLifecycle({ intent, lifecycle, policy, records, checkpoints })
    if (next !== lifecycle) await input.repository.commitTransition({
      operationId: lifecycle.operationId, expectedLeaseId: input.lease.leaseId,
      expectedLifecycleGeneration: lifecycle.generation, lifecycle: next,
    })
    return next
  })
}

async function retireTerminalDeliveryMetadata(
  input: BrowserDeliveryLifecycleAuthority,
  intent: Awaited<ReturnType<typeof requireDirectTreeIntent>>,
): Promise<void> {
  const reservation = intent.plan.reservation
  if (reservation.kind !== 'named-container-entry' || reservation.authorityKind !== 'fsa-container') {
    throw new TypeError('Terminal folder cleanup requires the original reserved target')
  }
  const checkpoints = await openFSAFileCheckpointRepository(
    input.databaseName === undefined ? {} : { databaseName: input.databaseName }, intent, reservation)
  try {
    const binding = await createMaterializationLedgerBinding({
      operationId: intent.operationId, receiveIntentDigest: intent.digest,
      materializationBindingDigest: reservation.digest, authorityRef: reservation.authorityRef,
    })
    // The repository retains target ownership for unsettled child deliveries atomically.
    await retireFSAMaterializationRecoveryMetadata(checkpoints, binding)
  } finally { checkpoints.close() }
}

async function withLocalAuthority<T>(
  input: BrowserDeliveryLifecycleAuthority,
  action: (authority: Awaited<ReturnType<typeof readLocalAuthority>>) => Promise<T>,
): Promise<T> {
  await verifyBrowserReceiveOperationLease(input.repository, input.lease)
  return action(await readLocalAuthority(input))
}

async function readLocalAuthority(input: BrowserDeliveryLifecycleAuthority) {
  const operationId = input.lease.operationId
  const [lifecycleRecord, operationRecord] = await Promise.all([
    input.repository.readLifecycle(operationId),
    input.repository.readRecord(operationRecordId(operationId, RECEIVE_RECORD_OPERATION)),
  ])
  if (lifecycleRecord === undefined || operationRecord === undefined) throw new TypeError('Local delivery lost receive authority')
  const lifecycle = decodeStoredReceiveLifecycleState(lifecycleRecord)
  const operation = await decodeStoredReceiveOperation(operationRecord)
  const intent = await requireDirectTreeIntent(operation.receiveIntent)
  if (lifecycle.receiveIntentDigest !== intent.digest) throw new TypeError('Local delivery lifecycle intent changed')
  if (lifecycle.kind !== 'resumable-receive' || lifecycle.payloadKind !== 'file-set') {
    return { lifecycleRecord, lifecycle, intent, policy: undefined, records: [], checkpoints: [] }
  }
  const journal = await IndexedDbBrowserDeliveryRepository.open(
    input.databaseName === undefined ? {} : { databaseName: input.databaseName })
  try {
    const policy = await journal.readPolicy(operationId)
    const records: BrowserDeliveryRecordV1[] = []
    if (policy !== undefined) {
      let afterFileId: string | undefined
      do {
        const page = await journal.scanFiles({ operationId, ...(afterFileId === undefined ? {} : { afterFileId }) })
        records.push(...page.records)
        afterFileId = page.nextFileId
      } while (afterFileId !== undefined)
    }
    if (intent.plan.reservation.kind !== 'named-container-entry' || intent.plan.reservation.authorityKind !== 'fsa-container') {
      throw new TypeError('Local delivery requires a reserved FSA target')
    }
    const checkpoints = await openFSAFileCheckpointRepository(
      input.databaseName === undefined ? {} : { databaseName: input.databaseName }, intent, intent.plan.reservation)
    try {
      return { lifecycleRecord, lifecycle, intent, policy, records,
        checkpoints: await scanAllFSAFileCheckpoints(checkpoints, 'committed') }
    } finally { checkpoints.close() }
  } finally { journal.close() }
}

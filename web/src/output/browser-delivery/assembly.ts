import { encodeBase64Url } from '../../crypto/bytes'
import { createOperationID, validateReceiveIntent, type ReceiveIntent } from '../../transfer/intent'
import type { OutputDiagnosticsPorts } from '../diagnostics'
import { FILE_CHECKPOINT_MATERIALIZER_FSA_TREE, FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE } from '../persistence/checkpoint'
import { chooseFileReceivingPlacement } from '../planning/file-receiving-placement'
import { RecoveryCostObserver } from '../planning/recovery-cost'
import type { BrowserStagingStorageFacts } from '../planning/staging-storage'
import { StagingBudgetCoordinator } from '../staging-budget/coordinator'
import { IndexedDbStagingBudgetStore } from '../staging-budget/indexeddb-store'
import type { StagingBudgetPolicy } from '../staging-budget/contracts'
import type { BrowserDeliveryRecordV1, BrowserRecoveryPreference } from './model'
import { IndexedDbBrowserDeliveryRepository } from './indexeddb'
import { createBrowserSavePolicy } from './policy'
import { BrowserDeliveryMaterialization, type BrowserDeliveryMaterializationOptions, type BrowserDeliveryCleanup } from './session'
import { BrowserDeliveryStaging } from './staging'
import type { BrowserDeliveryRuntimeTrace, BrowserDeliveryStagePort, BrowserDeliveryTargetPort } from './ports'
import { checkpointBytes } from './engine'
import { stageCheckpoint } from './lifecycle'

export interface OpenBrowserFolderDeliveryOptions {
  readonly target: BrowserDeliveryTargetPort
  readonly intent: ReceiveIntent
  readonly storage: Pick<StorageManager, 'getDirectory'>
  readonly storageFacts: () => Promise<BrowserStagingStorageFacts>
  readonly preference: BrowserRecoveryPreference
  readonly operationLease: Readonly<{ operationId: string; leaseId: string }>
  readonly diagnostics?: OutputDiagnosticsPorts
  readonly databaseName?: string
  readonly capacityDatabaseName?: string
  readonly budgetPolicy?: StagingBudgetPolicy
  readonly costs?: RecoveryCostObserver
  readonly now?: () => number
  readonly trace?: (event: BrowserDeliveryRuntimeTrace) => void
}

/** The final FSA lease owns both target mutation and the policy-bound child staging namespace. */
export async function openBrowserFolderDelivery(input: OpenBrowserFolderDeliveryOptions): Promise<BrowserDeliveryMaterialization> {
  return openAssembly(input, options => BrowserDeliveryMaterialization.open({ ...options, target: input.target }))
}

export type OpenBrowserFolderDeliveryCleanupOptions = Omit<OpenBrowserFolderDeliveryOptions, 'target' | 'costs'>

export async function openBrowserFolderDeliveryCleanup(input: OpenBrowserFolderDeliveryCleanupOptions): Promise<BrowserDeliveryCleanup> {
  return openAssembly(input, options => BrowserDeliveryMaterialization.openForCleanup(options))
}

async function openAssembly<T>(
  input: OpenBrowserFolderDeliveryOptions | OpenBrowserFolderDeliveryCleanupOptions,
  open: (options: Omit<BrowserDeliveryMaterializationOptions, 'target'>) => Promise<T>,
): Promise<T> {
  const intent = await validateReceiveIntent(input.intent)
  if (intent.plan.kind !== 'direct-tree' || intent.plan.reservation.kind !== 'named-container-entry' ||
      intent.plan.reservation.authorityKind !== 'fsa-container' || input.operationLease.operationId !== intent.operationId ||
      input.operationLease.leaseId.length === 0) throw new TypeError('Folder delivery requires the original leased FSA target')
  const repository = await IndexedDbBrowserDeliveryRepository.open(input.databaseName === undefined ? {} : { databaseName: input.databaseName })
  let capacity: IndexedDbStagingBudgetStore | undefined
  let stage: BrowserDeliveryStaging | undefined
  try {
    const existing = await repository.readPolicy(intent.operationId)
    if (!('target' in input) && existing === undefined) throw new TypeError('Cleanup requires an existing browser delivery policy')
    const storage = existing === undefined ? await input.storageFacts() : undefined
    const stagingAvailable = storage?.opfs === 'usable' && typeof input.storage.getDirectory === 'function' &&
      typeof globalThis.navigator?.locks?.request === 'function'
    const policy = existing ?? createBrowserSavePolicy({ operationId: intent.operationId, receiveIntentDigest: intent.digest,
      preference: input.preference,
      target: { operationId: intent.operationId, receiveIntentDigest: intent.digest,
        materializationBindingDigest: intent.plan.reservation.digest, materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
        authorityRef: intent.plan.reservation.authorityRef },
      ...(input.preference === 'direct' || !stagingAvailable ? {} : { staging: {
        operationId: createOperationID(), receiveIntentDigest: intent.digest,
        materializationBindingDigest: randomDigest(), materializerKind: FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE,
        authorityRef: randomDigest(),
      } }) })
    if (policy.receiveIntentDigest !== intent.digest || policy.preference !== input.preference) throw new TypeError('Browser folder policy changed after task creation')
    await repository.installPolicy(policy)
    capacity = await IndexedDbStagingBudgetStore.open(input.capacityDatabaseName)
    const budget = new StagingBudgetCoordinator({ store: capacity, storage: input.storageFacts,
      ...(input.budgetPolicy === undefined ? {} : { policy: input.budgetPolicy }) })
    const costs = ('costs' in input ? input.costs : undefined) ?? new RecoveryCostObserver()
    const now = input.now ?? (() => performance.now())
    let stageOpening: Promise<BrowserDeliveryStaging> | undefined
    const openStage = () => {
      stageOpening ??= (async () => {
        stage = await BrowserDeliveryStaging.open({ policy, parent: await input.storage.getDirectory(),
          ...(input.databaseName === undefined ? {} : { databaseName: input.databaseName }),
          ...(input.diagnostics === undefined ? {} : { diagnostics: input.diagnostics }) })
        return stage
      })()
      return stageOpening
    }
    // Most folder members stay direct; merely offering automatic recovery must not
    // allocate an OPFS namespace or open a Worker for every small-file task.
    const staging: BrowserDeliveryStagePort = {
      beginFile: async request => (await openStage()).beginFile(request),
      ensureDirectory: async path => (await openStage()).ensureDirectory(path),
      readCheckpoint: async fileId => (await openStage()).readCheckpoint(fileId),
      readComplete: async checkpoint => (await openStage()).readComplete(checkpoint),
      removeComplete: async checkpoint => (await openStage()).removeComplete(checkpoint),
      discard: async (source, checkpoint) => (await openStage()).discard(source, checkpoint),
      bindCapacity: async (source, objectCapacity) => (await openStage()).bindCapacity(source, objectCapacity),
      close: async () => { await stage?.close() },
    }
    return await open({ policy, repository,
      ...(policy.staging === undefined ? {} : { staging }),
      choosePlacement: source => chooseFileReceivingPlacement({ exactSize: source.exactSize,
        preference: policy.preference, storage: async () => policy.staging === undefined
          ? { ...await input.storageFacts(), opfs: 'unavailable' } : await input.storageFacts(),
        costs: costs.snapshot(Math.floor(now())) }),
      reserveStage: async (source, retained) => {
        if (retained === undefined) {
          const decision = await budget.tryReserve({ operationId: policy.operationId, fileId: source.fileId, exactSize: source.exactSize })
          return decision.kind === 'admitted' ? decision.reservation : undefined
        }
        const state = retained.state
        const checkpoint = state.kind === 'receiving' || state.kind === 'discarding' ? state.checkpoint : stageCheckpoint(state)
        return budget.restore({ operationId: policy.operationId, fileId: source.fileId, exactSize: source.exactSize,
          verifiedStagedBytes: checkpointBytes(checkpoint), operationLease: input.operationLease,
          phase: retainedBudgetPhase(retained) })
      },
      observeCopy: (bytes, durationMilliseconds) => costs.observeCopy({ bytes, durationMilliseconds }),
      observeReceipt: newReceivedBytes => costs.observeReceipt({ atMilliseconds: Math.floor(now()), newReceivedBytes }),
      observeFlush: (bytes, durationMilliseconds) => costs.observeFlush({ bytes, durationMilliseconds }),
      now,
      closeResources: () => { repository.close(); capacity?.close() },
      reconcileReservations: async deliveryFileIds => { await budget.reconcileUnused({ operationId: policy.operationId,
        deliveryFileIds, operationLease: input.operationLease }) },
      ...(input.trace === undefined ? {} : { trace: input.trace }),
    })
  } catch (error) {
    await stage?.close().catch(() => undefined)
    if ('target' in input) await input.target.close().catch(() => undefined)
    capacity?.close()
    repository.close()
    throw error
  }
}

function retainedBudgetPhase(record: BrowserDeliveryRecordV1): 'receiving' | 'target-saved' | 'queued' {
  if (record.state.kind === 'receiving' || record.state.kind === 'discarding') return 'receiving'
  if (record.state.kind === 'target-saved' || record.state.kind === 'cleanup-pending') return 'target-saved'
  return 'queued'
}

function randomDigest(): string { return encodeBase64Url(crypto.getRandomValues(new Uint8Array(32))) }

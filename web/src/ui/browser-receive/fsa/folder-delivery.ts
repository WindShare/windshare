import { openBrowserFolderDelivery, openBrowserFolderDeliveryCleanup } from '../../../output/browser-delivery/assembly'
import type { BrowserRecoveryPreference } from '../../../output/browser-delivery/model'
import type { BrowserDeliveryResumeSummary } from '../../../output/browser-delivery/retained'
import type { BrowserStagingStorageFacts } from '../../../output/planning/staging-storage'
import type { OriginPrivateStorageManager } from '../contracts'
import type { V2BrowserFolderProgressSnapshot, V2BrowserFolderProgressSource } from '../../v2-receive-runtime'
import { IndexedDbBrowserDeliveryRepository } from '../../../output/browser-delivery/indexeddb'
import type { BrowserReceiveWindow } from '../contracts'
import { inspectBrowserStagingStorage } from './staging-storage'
import { probeNativeObjectSupport, type NativeSupportRuntime } from '../../../output/origin-private/native-object/support'
import { browserFolderDeliveryTrace } from './delivery-trace'
import { openFSAFileCheckpointRepository } from '../../../output/file-system-access/checkpoint-repository'
import { readFSARecoverySummary } from '../../../output/file-system-access/recovery-summary'
import { requireDirectTreeIntent } from '../../../output/file-system-access/settlement-proof'
import type { ReceiveIntent } from '../../../transfer/intent'
import type { ReceiveLifecycleState } from '../../../output/workspace/state'

export interface BrowserFolderDeliveryContext {
  readonly storage: OriginPrivateStorageManager
  readonly storageFacts: () => Promise<BrowserStagingStorageFacts>
  readonly preference: BrowserRecoveryPreference
  readonly progress: BrowserFolderDeliveryProgress
  readonly open?: typeof openBrowserFolderDelivery
  readonly openCleanup?: typeof openBrowserFolderDeliveryCleanup
}

export async function readFolderRecoverySummary(intent: ReceiveIntent, lifecycle: ReceiveLifecycleState) {
  if (lifecycle.kind !== 'resumable-receive' || lifecycle.payloadKind !== 'file-set') {
    throw new TypeError('Local folder saving lost its resumable lifecycle')
  }
  const directIntent = await requireDirectTreeIntent(intent)
  if (directIntent.plan.reservation.kind !== 'named-container-entry' ||
      directIntent.plan.reservation.authorityKind !== 'fsa-container') {
    throw new TypeError('Local folder saving requires its reserved destination')
  }
  const checkpoints = await openFSAFileCheckpointRepository({}, directIntent, directIntent.plan.reservation)
  try { return await readFSARecoverySummary({ intent: directIntent, lifecycle, checkpoints }) }
  finally { checkpoints.close() }
}

export async function reopenFolderDeliveryContext(windowPort: BrowserReceiveWindow, operationId: string): Promise<BrowserFolderDeliveryContext | undefined> {
  const repository = await IndexedDbBrowserDeliveryRepository.open()
  const policy = await repository.readPolicy(operationId).finally(() => repository.close())
  if (policy === undefined) return undefined
  const support = probeNativeObjectSupport(windowPort as unknown as NativeSupportRuntime)
  return {
    storage: windowPort.navigator.storage,
    storageFacts: async () => inspectBrowserStagingStorage(windowPort.navigator.storage, await support),
    preference: policy.preference, progress: new BrowserFolderDeliveryProgress(),
  }
}

/** One operation-owned stream survives receive attempts; journal cuts supply its facts. */
export class BrowserFolderDeliveryProgress implements V2BrowserFolderProgressSource {
  readonly #listeners = new Set<(snapshot: V2BrowserFolderProgressSnapshot) => void>()
  #snapshot: V2BrowserFolderProgressSnapshot | undefined
  #generation = 0n

  getSnapshot(): V2BrowserFolderProgressSnapshot {
    if (this.#snapshot === undefined) throw new DOMException('Folder delivery is not initialized', 'InvalidStateError')
    return this.#snapshot
  }

  publish(summary: BrowserDeliveryResumeSummary): void {
    this.#snapshot = Object.freeze({
      kind: 'browser-folder', operationId: summary.policy.operationId,
      generation: ++this.#generation, summary,
    })
    for (const listener of this.#listeners) {
      try { listener(this.#snapshot) } catch { /* Observation cannot reject a durable delivery cut. */ }
    }
  }

  subscribe(listener: (snapshot: V2BrowserFolderProgressSnapshot) => void): () => void {
    this.#listeners.add(listener)
    return () => { this.#listeners.delete(listener) }
  }
}

export async function openFolderDeliveryAttempt(
  context: BrowserFolderDeliveryContext,
  input: Pick<Parameters<typeof openBrowserFolderDelivery>[0], 'target' | 'intent' | 'operationLease' | 'diagnostics'>,
) {
  const delivery = await (context.open ?? openBrowserFolderDelivery)({
    ...input, storage: context.storage, storageFacts: context.storageFacts, preference: context.preference,
    trace: browserFolderDeliveryTrace(input.diagnostics),
  })
  return observeDeliveryAttempt(context, delivery)
}

export async function openFolderCleanupAttempt(
  context: BrowserFolderDeliveryContext,
  input: Pick<Parameters<typeof openBrowserFolderDeliveryCleanup>[0], 'intent' | 'operationLease' | 'diagnostics'>,
) {
  const delivery = await (context.openCleanup ?? openBrowserFolderDeliveryCleanup)({
    ...input, storage: context.storage, storageFacts: context.storageFacts, preference: context.preference,
    trace: browserFolderDeliveryTrace(input.diagnostics),
  })
  return observeDeliveryAttempt(context, delivery)
}

function observeDeliveryAttempt<T extends Pick<Awaited<ReturnType<typeof openBrowserFolderDelivery>>,
  'getSummary' | 'subscribe' | 'close'>>(context: BrowserFolderDeliveryContext, delivery: T) {
  context.progress.publish(delivery.getSummary())
  const unsubscribe = delivery.subscribe(summary => context.progress.publish(summary))
  return {
    delivery,
    close: async () => {
      try { await delivery.close() } finally { unsubscribe() }
    },
  }
}

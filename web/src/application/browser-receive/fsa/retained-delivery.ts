import { IndexedDbBrowserDeliveryRepository } from '../../../output/browser-delivery/indexeddb'
import type { BrowserDeliveryRepository } from '../../../output/browser-delivery/repository'
import { readBrowserDeliverySummary } from '../../../output/browser-delivery/retained-authority'
import {
  hasBrowserDeliveryStaging,
  type BrowserDeliverySummary,
} from '../../../output/model/browser-delivery'
import { browserDeliveryLocalActions } from '../../../output/model/browser-delivery-actions'
import type { ReceiveLifecycleState } from '../../../transfer/lifecycle/state'
import type { RetainedReceiveAction } from '../../receive/contracts'

export type BrowserDeliveryRepositoryFactory = () => Promise<BrowserDeliveryRepository>

export function retainedBrowserDeliveryActions(lifecycle: ReceiveLifecycleState, summary: BrowserDeliverySummary | undefined,
  ordinary: readonly RetainedReceiveAction[]): readonly RetainedReceiveAction[] {
  if (summary === undefined) return ordinary
  const actions = ordinary.filter(action => !hasBrowserDeliveryStaging(summary) || action !== 'forget')
  return Object.freeze([...browserDeliveryLocalActions(lifecycle, summary), ...actions])
}

export async function readRetainedBrowserDeliveries(
  operations: readonly Readonly<{ operationId: string; receiveIntentDigest: string }>[],
  signal: AbortSignal,
  open: BrowserDeliveryRepositoryFactory = () => IndexedDbBrowserDeliveryRepository.open(),
): Promise<ReadonlyMap<string, BrowserDeliverySummary>> {
  const repository = await open()
  try {
    const summaries = new Map<string, BrowserDeliverySummary>()
    for (const operation of operations) {
      signal.throwIfAborted()
      const summary = await readBrowserDeliverySummary({ repository, operationId: operation.operationId })
      if (summary === undefined) continue
      if (summary.operationId !== operation.operationId ||
          summary.receiveIntentDigest !== operation.receiveIntentDigest) {
        throw new TypeError('Retained browser delivery escaped its receive intent')
      }
      summaries.set(operation.operationId, summary)
    }
    signal.throwIfAborted()
    return summaries
  } finally {
    repository.close()
  }
}

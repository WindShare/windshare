import { IndexedDbBrowserDeliveryRepository } from '../../../output/browser-delivery/indexeddb'
import type { BrowserDeliveryRepository } from '../../../output/browser-delivery/repository'
import { readBrowserDeliveryResumeSummary } from '../../../output/browser-delivery/retained-authority'
import { hasBrowserDeliveryStaging, type BrowserDeliveryResumeSummary } from '../../../output/browser-delivery/retained'
import { browserDeliveryLocalActions } from '../../../output/browser-delivery/recovery/local-actions'
import type { ReceiveLifecycleState } from '../../../output/workspace/state'
import type { V2RetainedReceiveAction } from '../../v2-receive-runtime'

export type BrowserDeliveryRepositoryFactory = () => Promise<BrowserDeliveryRepository>

export function retainedBrowserDeliveryActions(lifecycle: ReceiveLifecycleState, summary: BrowserDeliveryResumeSummary | undefined,
  ordinary: readonly V2RetainedReceiveAction[]): readonly V2RetainedReceiveAction[] {
  if (summary === undefined) return ordinary
  const actions = ordinary.filter(action => !hasBrowserDeliveryStaging(summary) || action !== 'forget')
  return Object.freeze([...browserDeliveryLocalActions(lifecycle, summary), ...actions])
}

export async function readRetainedBrowserDeliveries(
  operations: readonly Readonly<{ operationId: string; receiveIntentDigest: string }>[],
  signal: AbortSignal,
  open: BrowserDeliveryRepositoryFactory = () => IndexedDbBrowserDeliveryRepository.open(),
): Promise<ReadonlyMap<string, BrowserDeliveryResumeSummary>> {
  const repository = await open()
  try {
    const summaries = new Map<string, BrowserDeliveryResumeSummary>()
    for (const operation of operations) {
      signal.throwIfAborted()
      const summary = await readBrowserDeliveryResumeSummary({ repository, operationId: operation.operationId })
      if (summary === undefined) continue
      if (summary.policy.receiveIntentDigest !== operation.receiveIntentDigest) {
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

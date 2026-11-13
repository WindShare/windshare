import { describe, expect, it, vi } from 'vitest'
import { ReceiveRuntimeState } from '../../src/application/receive/state'
import { BrowserFolderDeliveryProgress } from '../../src/application/browser-receive/fsa/folder-delivery'
import { readRetainedBrowserDeliveries } from '../../src/application/browser-receive/fsa/retained-delivery'
import { summarizeBrowserDeliveries } from '../../src/output/browser-delivery/retained'
import { advanceBrowserDeliveryRecord } from '../../src/output/browser-delivery/lifecycle'
import { localDeliveryFixture } from '../output/browser-delivery-local-fixture'
import { deliveryIdentity, deliveryPolicy } from '../output/browser-delivery-fixture'
import { DeliveryMemoryRepository } from '../output/browser-delivery-engine-fixture'

describe('browser folder observation identity', () => {
  it('preserves operation, intent and progress-generation fences at the live projection boundary', async () => {
    const f = await localDeliveryFixture()
    const state = new ReceiveRuntimeState()
    const source = new BrowserFolderDeliveryProgress()
    const summary = summarizeBrowserDeliveries(f.policy, [f.initial])
    state.adoptRetainedReceiveIntent(f.intent, f.lifecycle)
    const accept = vi.fn(snapshot => state.updateBrowserFolderProgress(snapshot))
    const unsubscribe = source.subscribe(accept)
    try {
      source.publish(summary)
      const first = source.getSnapshot()
      expect(accept).toHaveLastReturnedWith(true)
      expect(first.operationId).toBe(f.intent.operationId)
      expect(first.summary).toBe(summary)
      expect(state.getSnapshot().browserFolderProgress).toEqual(first)
      for (const stale of [
        first,
        { ...first, generation: first.generation - 1n },
        { ...first, generation: first.generation + 1n, operationId: deliveryIdentity(60, 16) },
        { ...first, generation: first.generation + 1n, summary: { ...summary, operationId: deliveryIdentity(61, 16) } },
        { ...first, generation: first.generation + 1n, summary: { ...summary, receiveIntentDigest: deliveryIdentity(62) } },
      ]) {
        expect(state.updateBrowserFolderProgress(stale)).toBe(false)
        expect(state.getSnapshot().browserFolderProgress).toEqual(first)
      }
      const complete = advanceBrowserDeliveryRecord(f.policy, f.initial, { kind: 'staged-complete', stage: f.stage })
      source.publish(summarizeBrowserDeliveries(f.policy, [complete]))
      expect(accept).toHaveLastReturnedWith(true)
      expect(state.getSnapshot().browserFolderProgress).toMatchObject({
        generation: first.generation + 1n,
        summary: { operationId: f.intent.operationId, receiveIntentDigest: f.intent.digest, stagedCompleteFiles: 1 },
      })
    } finally { unsubscribe(); state.close() }
  })

  it.each(['operationId', 'receiveIntentDigest'] as const)('rejects retained summaries for a different %s', async field => {
    const policy = deliveryPolicy()
    const repository = new DeliveryMemoryRepository(policy)
    const close = vi.spyOn(repository, 'close')
    const requested = {
      operationId: policy.operationId, receiveIntentDigest: policy.receiveIntentDigest,
      [field]: deliveryIdentity(63, field === 'operationId' ? 16 : 32),
    }
    await expect(readRetainedBrowserDeliveries([requested], new AbortController().signal, async () => repository))
      .rejects.toThrow('Retained browser delivery escaped its receive intent')
    expect(close).toHaveBeenCalledOnce()
  })
})

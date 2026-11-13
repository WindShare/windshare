import { describe, expect, it } from 'vitest'
import { browserDeliveryLocalActions } from '../../src/output/model/browser-delivery-actions'
import { hasBrowserDeliveryStaging } from '../../src/output/model/browser-delivery'
import { summarizeBrowserDeliveries } from '../../src/output/browser-delivery/retained'
import type { ReceiveLifecycleState } from '../../src/transfer/lifecycle/state'
import { deliveryFixture, deliveryIdentity, stoppedDeliveryLifecycle } from './browser-delivery-fixture'

describe('browser delivery observations', () => {
  it('offers local actions only for the matching operation and intent after receiving settles', () => {
    const f = deliveryFixture()
    const summary = summarizeBrowserDeliveries(f.policy, [
      f.advance(f.initial, { kind: 'staged-complete', stage: f.stage! }),
    ])
    const lifecycle = stoppedDeliveryLifecycle(f.policy)
    expect(browserDeliveryLocalActions(lifecycle, summary)).toEqual(['save-staged-files'])
    for (const foreign of [
      { ...summary, operationId: deliveryIdentity(40, 16) },
      { ...summary, receiveIntentDigest: deliveryIdentity(41) },
      undefined,
      null,
    ]) {
      expect(browserDeliveryLocalActions(lifecycle, foreign)).toEqual([])
    }
    const receiving: ReceiveLifecycleState = {
      operationId: lifecycle.operationId, receiveIntentDigest: lifecycle.receiveIntentDigest,
      generation: 1n, kind: 'receiving', activeLeaseId: deliveryIdentity(42, 16),
    }
    expect(browserDeliveryLocalActions(receiving, summary)).toEqual([])
  })

  it('retains incomplete stages for resume and offers disposal only after terminal settlement', () => {
    const f = deliveryFixture()
    const summary = summarizeBrowserDeliveries(f.policy, [f.initial])
    const stopped = stoppedDeliveryLifecycle(f.policy)
    const paused: ReceiveLifecycleState = {
      operationId: stopped.operationId, receiveIntentDigest: stopped.receiveIntentDigest, generation: 2n,
      kind: 'resumable-receive', payloadKind: 'file-set', checkpointSetDigest: deliveryIdentity(43),
      completedFileCount: 0n, completedBytes: 0n, retainedBytes: 0n,
      selectionFacts: { discoveredFileCount: 1n, discoveredBytes: f.source.exactSize, discovery: 'complete' },
    }
    expect(hasBrowserDeliveryStaging(summary)).toBe(true)
    expect(browserDeliveryLocalActions(paused, summary)).toEqual([])
    const actions = browserDeliveryLocalActions(stopped, summary)
    expect(actions).toEqual(['discard-incomplete-staging'])
    expect(Object.isFrozen(actions)).toBe(true)
  })

  it('keeps empty owned files and outstanding reservations visible to cleanup', () => {
    const f = deliveryFixture()
    const empty = summarizeBrowserDeliveries(f.policy, [])
    expect(hasBrowserDeliveryStaging(empty)).toBe(false)
    expect(hasBrowserDeliveryStaging({ ...empty, stagedCompleteFiles: 1 })).toBe(true)
    expect(hasBrowserDeliveryStaging({ ...empty, reservedStagingBytes: 1n })).toBe(true)
  })
})

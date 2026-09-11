import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { V2BlockRangeReader } from '../../src/content/v2-broker'
import type { CheckpointObservation } from '../../src/transfer/checkpoint/controller'
import { byteRange } from '../../src/content/geometry'
import {
  catalogFixture, fileEntry, identity, planAuthorityFixture, readerFixture,
  receiveIntentFixture, selectOnlyFile, testOutput, transferJobFixture,
} from './v2-job-fixture'

function deferred() {
  let resolve!: () => void
  const promise = new Promise<void>(complete => { resolve = complete })
  return { promise, resolve }
}

describe('file transfer checkpoint wakeups', () => {
  beforeEach(() => { vi.useFakeTimers(); vi.setSystemTime(0) })
  afterEach(() => vi.useRealTimers())

  it.each([false, true])('checkpoints during a stalled read with initial live coverage = %s', async live => {
    const file = fileEntry(identity(11), 'stalled.bin', 6n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: identity(2), entries: [file] }])
    const readers = readerFixture([file])
    const stalled = deferred()
    const release = deferred()
    const requested: bigint[] = []
    const observations: CheckpointObservation[] = []
    let receivedBytes = 0n
    const broker: V2BlockRangeReader = { readRange: async function* (_descriptor, _lease, range) {
      requested.push(range.start)
      if (!live) yield { offset: 0n, data: new Uint8Array([1, 2]) }
      stalled.resolve()
      await release.promise
      yield { offset: 2n, data: new Uint8Array([3, 4, 5, 6]) }
    } }
    const output = testOutput([], { durability: 'ProcessRestart',
      ...(live ? { acceptedRanges: [byteRange(0n, 2n)] } : {}),
      checkpointPolicy: { kind: 'incremental', pendingBytes: 100n, pendingMilliseconds: 1_000 } })
    const intent = await receiveIntentFixture({
      planKind: 'workspace-then-publish', artifactKind: 'original-file', selection, file,
    })
    const job = transferJobFixture({ catalog: catalog.catalog, selection, intent,
      plans: planAuthorityFixture({ output }), revisions: readers.revisions, broker,
      onCheckpointObservation: event => observations.push(event),
      onProgress: event => { receivedBytes = event.writtenBytes } })
    const running = job.run()
    await stalled.promise
    expect(output.checkpointAdvances).toHaveLength(0)
    await vi.advanceTimersByTimeAsync(1_000)
    expect(output.checkpointAdvances).toEqual([file.idText])
    expect(observations).toContainEqual(expect.objectContaining({
      stage: 'advanced', pendingBytes: 0n, durableBytes: 2n, checkpointBytes: 2n,
      lastCheckpointMilliseconds: 1_000,
    }))
    release.resolve()
    await expect(running).resolves.toMatchObject({ worker: { status: 'Succeeded' } })
    expect(requested).toEqual([live ? 2n : 0n])
    expect(receivedBytes).toBe(live ? 4n : 6n)
    await vi.advanceTimersByTimeAsync(60_000)
    expect(output.checkpointAdvances).toHaveLength(1)
  })

  it('wakes a stalled reader on checkpoint failure without misclassifying it as user cancellation', async () => {
    const file = fileEntry(identity(11), 'stalled.bin', 6n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: identity(2), entries: [file] }])
    const readers = readerFixture([file])
    const stalled = deferred()
    const broker: V2BlockRangeReader = { readRange: async function* (_descriptor, _lease, _range, request) {
      yield { offset: 0n, data: new Uint8Array([1, 2]) }
      stalled.resolve()
      await new Promise<void>((_resolve, reject) => {
        const signal = request?.signal
        if (signal?.aborted === true) reject(signal.reason)
        else signal?.addEventListener('abort', () => reject(signal.reason), { once: true })
      })
    } }
    const output = testOutput([], { durability: 'ProcessRestart',
      checkpointPolicy: { kind: 'incremental', pendingBytes: 100n, pendingMilliseconds: 1_000 },
      beforeAutomaticCheckpoint: async () => { throw new Error('metadata rejected') } })
    const intent = await receiveIntentFixture({
      planKind: 'workspace-then-publish', artifactKind: 'original-file', selection, file,
    })
    const running = transferJobFixture({ catalog: catalog.catalog, selection, intent,
      plans: planAuthorityFixture({ output }), revisions: readers.revisions, broker }).run()
    await stalled.promise
    await vi.advanceTimersByTimeAsync(1_000)
    const result = await running
    expect(result.worker.status).toBe('Paused')
    expect(result.failureTrigger).toMatchObject({ materializationFailureReason: 'output-write-failed' })
    expect(output.commits).toEqual([])
    expect(readers.releases).toEqual([file.idText])
    await vi.advanceTimersByTimeAsync(60_000)
    expect(output.automaticCheckpointAttempts).toHaveLength(1)
  })
})

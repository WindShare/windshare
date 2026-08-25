import { describe, expect, it } from 'vitest'

import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import type { V2BlockRangeReader } from '../../src/content/v2-broker'
import {
  catalogFixture,
  directoryEntry,
  fileEntry,
  identity,
  planAuthorityFixture,
  readerFixture,
  receiveIntentFixture,
  selectOnlyFile,
  testOutput,
  transferJobFixture,
} from './v2-job-fixture'

describe('v2 cancellation settlement', () => {
  it.each([
    { planKind: 'direct-atomic', lifecycle: 'restart-required' },
    { planKind: 'workspace-then-publish', lifecycle: 'resumable-receive' },
    { planKind: 'portable-handoff', lifecycle: 'restart-required' },
  ] as const)('pauses $planKind at its own stable cut during an authenticated range read', async ({
    planKind,
    lifecycle,
  }) => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readStarted = deferred()
    const releaseRead = deferred()
    const readers = readerFixture([file], [], {
      beforeRead: async () => {
        readStarted.resolve()
        await releaseRead.promise
      },
    })
    const output = testOutput([], { durability: 'ProcessRestart' })
    const plans = planAuthorityFixture({ output })
    const intent = await receiveIntentFixture({
      planKind,
      artifactKind: 'original-file',
      selection,
      file,
    })
    const controller = new AbortController()
    const running = transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
    }).run(controller.signal)

    await readStarted.promise
    controller.abort(new DOMException('receiver paused transfer', 'AbortError'))
    releaseRead.resolve()
    const result = await running

    expect(result.worker.status).toBe('Paused')
    expect(result.lifecycle.kind).toBe(lifecycle)
    expect(plans.settlements).toEqual([])
    expect(plans.pauses).toEqual([planKind])
    expect(readers.revisionRequests).toEqual([file.idText])
    expect(readers.blockRequests).toEqual([file.idText])
    expect(readers.releases).toEqual([file.idText])
    expect(output.commits).toEqual([])
  })

  it('does not request a revision or block when cancellation precedes execution', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 2n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file])
    const plans = planAuthorityFixture()
    const intent = await receiveIntentFixture({
      planKind: 'direct-atomic',
      artifactKind: 'original-file',
      selection,
      file,
    })
    const controller = new AbortController()
    controller.abort(new DOMException('canceled before run', 'AbortError'))

    const result = await transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
    }).run(controller.signal)

    expect(result.worker.status).toBe('Paused')
    expect(result.lifecycle.kind).toBe('restart-required')
    expect(plans.settlements).toEqual([])
    expect(plans.pauses).toEqual(['direct-atomic'])
    expect(readers.revisionRequests).toEqual([])
    expect(readers.blockRequests).toEqual([])
  })

  it('does not report DirectTree Paused until an admitted file has returned exact pause evidence', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = new V2SelectionPolicy(true)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const secondBlockStarted = deferred()
    const releaseSecondBlock = deferred()
    const pauseStarted = deferred()
    const releasePause = deferred()
    const readers = readerFixture([file])
    const broker: V2BlockRangeReader = {
      readRange: async function* (descriptor, leaseId, range, request) {
        for await (const slice of readers.broker.readRange(descriptor, leaseId, range, request)) {
          const firstBlockBytes = Number(descriptor.geometry.blockSize)
          yield Object.freeze({ offset: slice.offset, data: slice.data.subarray(0, firstBlockBytes) })
          secondBlockStarted.resolve()
          await releaseSecondBlock.promise
          yield Object.freeze({
            offset: slice.offset + descriptor.geometry.blockSize,
            data: slice.data.subarray(firstBlockBytes),
          })
        }
      },
    }
    const output = testOutput([], {
      durability: 'ProcessRestart',
      beforePause: async () => {
        pauseStarted.resolve()
        await releasePause.promise
      },
    })
    const plans = planAuthorityFixture({ output })
    const intent = await receiveIntentFixture({
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      selection,
    })
    const controller = new AbortController()
    const running = transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker,
    }).run(controller.signal)

    await secondBlockStarted.promise
    controller.abort(new DOMException('receiver paused transfer', 'AbortError'))
    releaseSecondBlock.resolve()
    await pauseStarted.promise
    let settled = false
    const observedRunning = running.then(
      result => {
        settled = true
        return result
      },
      error => {
        settled = true
        throw error
      },
    )
    await Promise.resolve()

    expect(settled).toBe(false)
    expect(output.events).not.toContain('pause-completed')
    expect(output.pauseEvidence).toEqual([])
    expect(plans.pauses).toEqual([])

    releasePause.resolve()
    const result = await observedRunning

    expect(result.worker.status).toBe('Paused')
    expect(result.lifecycle.kind).toBe('partial-directory')
    expect(output.pauseEvidence).toHaveLength(1)
    expect(output.pauseEvidence[0]?.ranges).toEqual([{ start: 0n, end: 2n }])
    expect(plans.pauses).toEqual(['direct-tree'])
  })

  it('cancels prepared-plan discovery before preparation can acquire output authority', async () => {
    const root = identity(2)
    const child = identity(3)
    const selection = new V2SelectionPolicy(true)
    const childReplayStarted = deferred()
    const releaseChildReplay = deferred()
    const catalog = catalogFixture([
      { id: root, entries: [directoryEntry(child, 'child')] },
      {
        id: child,
        entries: [],
        beforePages: async () => {
          childReplayStarted.resolve()
          await releaseChildReplay.promise
        },
      },
    ])
    const readers = readerFixture([])
    const plans = planAuthorityFixture()
    const intent = await receiveIntentFixture({
      planKind: 'workspace-then-publish',
      artifactKind: 'zip-archive',
      selection,
    })
    const controller = new AbortController()
    const running = transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
    }).run(controller.signal)

    await childReplayStarted.promise
    controller.abort(new DOMException('cancel exact discovery', 'AbortError'))
    releaseChildReplay.resolve()
    const result = await running

    expect(result.worker.status).toBe('Paused')
    expect(result.lifecycle.kind).toBe('discarded')
    expect(plans.routes).toEqual([])
    expect(plans.preparations).toEqual([])
    expect(plans.settlements).toEqual([])
    expect(plans.admissionFailures).toEqual([expect.anything()])
    expect(plans.output.requests).toEqual([])
    expect(readers.revisionRequests).toEqual([])
    expect(readers.blockRequests).toEqual([])
  })
})

function deferred(): {
  readonly promise: Promise<void>
  readonly resolve: () => void
} {
  let resolve!: () => void
  const promise = new Promise<void>((complete) => {
    resolve = complete
  })
  return { promise, resolve }
}

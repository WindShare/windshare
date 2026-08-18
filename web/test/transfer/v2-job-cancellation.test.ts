import { describe, expect, it } from 'vitest'

import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
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

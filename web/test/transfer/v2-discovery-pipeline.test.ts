import { describe, expect, it } from 'vitest'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import type { TransferProgress } from '../../src/transfer/v2-job'
import {
  catalogFixture, directoryEntry, fileEntry, identity, planAuthorityFixture,
  readerFixture, receiveIntentFixture, transferJobFixture,
} from './v2-job-fixture'

describe('independent selection discovery', () => {
  it.each(['direct-tree', 'workspace-then-publish'] as const)(
    'establishes the %s total through ten levels while file transfer and its queue are blocked',
    async planKind => {
      const files = Array.from({ length: 12 }, (_, index) =>
        fileEntry(identity(50 + index), `file-${index.toString().padStart(2, '0')}.bin`, 2n))
      const directories = Array.from({ length: 11 }, (_, level) => {
        let entries = [directoryEntry(identity(3 + level), 'child')] as Array<ReturnType<typeof fileEntry> | ReturnType<typeof directoryEntry>>
        if (level === 0) entries = [...files.slice(0, 11), directoryEntry(identity(3), 'z-child')]
        if (level === 10) entries = [files[11]!]
        return { id: identity(2 + level), entries }
      })
      const catalog = catalogFixture(directories)
      const selection = new V2SelectionPolicy(true)
      const intent = await receiveIntentFixture({
        planKind, artifactKind: planKind === 'direct-tree' ? 'directory-tree' : 'zip-archive', selection,
      })
      const firstRead = deferred<void>()
      const releaseRead = deferred<void>()
      const discovered = deferred<TransferProgress>()
      const readers = readerFixture(files, [], {
        beforeRead: async () => { firstRead.resolve(); await releaseRead.promise },
      })
      const plans = planAuthorityFixture()
      const snapshots: TransferProgress[] = []
      const running = transferJobFixture({
        catalog: catalog.catalog, selection, intent, plans,
        revisions: readers.revisions, broker: readers.broker,
        maximumConcurrentFiles: 1, maximumPendingFiles: 1,
        onProgress: progress => {
          snapshots.push(progress)
          if (progress.discovery === 'complete') discovered.resolve(progress)
        },
      }).run()
      try {
        const [, progress] = await Promise.all([firstRead.promise, discovered.promise])
        expect(progress).toMatchObject({
          discovery: 'complete', discoveredFiles: 12, discoveredBytes: 24n,
          completedFiles: 0, phase: 'receiving',
        })
        expect(readers.blockRequests).toHaveLength(1)
        expect(catalog.loads).toHaveLength(11)
        expect(new Set(catalog.loads).size).toBe(11)
      } finally {
        releaseRead.resolve()
      }
      expect((await running).worker.status).toBe('Succeeded')
      expect(snapshots.at(-1)?.discoveredBytes).toBe(24n)
      expect(plans.output.commits).toHaveLength(12)
    },
  )

  it('keeps a completed denominator when content is cancelled afterwards', async () => {
    const file = fileEntry(identity(20), 'file.bin', 2n)
    const catalog = catalogFixture([{ id: identity(2), entries: [file] }])
    const selection = new V2SelectionPolicy(true)
    const intent = await receiveIntentFixture({ planKind: 'direct-tree', artifactKind: 'directory-tree', selection })
    const firstRead = deferred<void>()
    const releaseRead = deferred<void>()
    const discovered = deferred<void>()
    const readers = readerFixture([file], [], {
      beforeRead: async () => { firstRead.resolve(); await releaseRead.promise },
    })
    const controller = new AbortController()
    const plans = planAuthorityFixture()
    const running = transferJobFixture({
      catalog: catalog.catalog, selection, intent, plans,
      revisions: readers.revisions, broker: readers.broker,
      onProgress: progress => { if (progress.discovery === 'complete') discovered.resolve() },
    }).run(controller.signal)
    await Promise.all([firstRead.promise, discovered.promise])
    controller.abort(new DOMException('cancel after discovery', 'AbortError'))
    releaseRead.resolve()
    const result = await running
    expect(result.measure.discovery).toBe('complete')
    expect(result.worker.status).toBe('Paused')
    expect(plans.pauseRequests[0]?.selectionFacts.discovery).toBe('complete')
  })
})
function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve!: (value: T) => void
  const promise = new Promise<T>(complete => { resolve = complete })
  return { promise, resolve }
}

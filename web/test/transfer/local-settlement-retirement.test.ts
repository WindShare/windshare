import { expect, it, vi } from 'vitest'
import { wireFixture } from './lease-retirement-wire-fixture'
import { identity, catalogFixture, selectOnlyFile, planAuthorityFixture, receiveIntentFixture, testOutput, transferJobFixture } from './v2-job-fixture'
import type { V2BlockRangeReader } from '../../src/content/v2-broker'

it.each(['complete', 'pause'] as const)('settles local %s before a remote release response arrives', async action => {
  const wire = await wireFixture()
  const controller = new AbortController()
  const pause = new DOMException('User paused', 'AbortError')
  const file = wire.files[0]!
  const selection = selectOnlyFile(file)
  const catalog = catalogFixture([{ id: identity(2), entries: [file] }])
  const output = testOutput([], { durability: 'ProcessRestart' })
  const plans = planAuthorityFixture({ output })
  const intent = await receiveIntentFixture({
    planKind: 'workspace-then-publish', artifactKind: 'original-file', selection, file,
  })
  const broker: V2BlockRangeReader = {
    readRange: async function* (_descriptor, _lease, range) {
      const bytes = action === 'pause' ? 2 : Number(range.end - range.start)
      yield { offset: range.start, data: new Uint8Array(bytes).fill(7), authenticatedRoute: 'direct' }
      if (action === 'pause') {
        controller.abort(pause)
        throw pause
      }
    },
  }
  const running = transferJobFixture({
    catalog: catalog.catalog, selection, intent, plans, revisions: wire.scoped.revisions, broker,
    chunkSize: wire.share.chunkSize,
  }).run(controller.signal)
  try {
    await wire.releaseReached.promise
    const result = await running
    expect(result.worker.status).toBe(action === 'pause' ? 'Paused' : 'Succeeded')
    if (action === 'pause') {
      expect(result.lifecycle.kind).toBe('resumable-receive')
      expect(plans.pauses).toEqual(['workspace-then-publish'])
      expect(output.commits).toEqual([])
    } else {
      expect(output.commits).toEqual([file.idText])
      expect(output.writes.reduce((sum, write) => sum + write.bytes, 0)).toBe(4)
    }
    expect(wire.retirements.some(event => event.transition === 'released')).toBe(false)
    expect(wire.runtime.isClosed).toBe(false)
    await wire.completeRelease()
    await vi.waitFor(() => expect(wire.retirements).toContainEqual(
      expect.objectContaining({ transition: 'released', attempt: 1 })))
  } finally {
    controller.abort(pause)
    await wire.close()
    await running.catch(() => undefined)
  }
})

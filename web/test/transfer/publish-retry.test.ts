import { describe, expect, it, vi } from 'vitest'
import { SelectionPolicy } from '../../src/catalog/selection'
import { byteRange } from '../../src/content/geometry'
import { RevisionLeaseExpiredError } from '../../src/content/session-services'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { RetryableFilePublishError } from '../../src/transfer/fault'
import { TransferJob } from '../../src/transfer/job'
import { gate, id, makeShareFixture, outputFile } from './share-fixture'

const file = { seed: 45, name: 'photo.bin', bytes: Uint8Array.of(1, 2, 3, 4, 5, 6, 7, 8) }
const selection = new SelectionPolicy(true).snapshot()
const capabilities = { resumable: true, partialOutput: true, maxConcurrentFiles: 1 }
const metadata = (seed: number) => encodeBase64Url(new Uint8Array(32).fill(seed))
const discardedPublish = () => new RetryableFilePublishError(new DOMException('Swap close failed', 'InvalidStateError'))

describe('replay of a discarded initial publication', () => {
  it('reacquires the same source and reports unique coverage alongside actual network traffic', async () => {
    const fixture = makeShareFixture([file])
    const release = vi.fn(async () => undefined)
    const coverage: bigint[] = []
    let published: Uint8Array | undefined
    let publications = 0
    const job = new TransferJob({
      ...fixture, selection,
      revisions: { open: async (...args) => {
        const opened = await fixture.revisions.open(...args)
        const revision = id(fixture.openedFileIds.length)
        return { ...opened, release, descriptor: { ...opened.descriptor, sourceMetadata: metadata(1),
          fileRevision: revision, fileRevisionText: encodeBase64Url(revision) } }
      } },
      broker: { readRange: async function* (...args) {
        for await (const slice of fixture.broker.readRange(...args)) {
          args[3]?.onReceive?.(slice.data.byteLength)
          yield slice
        }
      } },
      output: { sessionId: 'retry', capabilities, openFile: async () => {
        const swap = new Uint8Array(file.bytes.length)
        return outputFile({
          writeAt: async (offset, data) => { swap.set(data, Number(offset)) },
          commit: async () => {
            if (++publications === 1) throw discardedPublish()
            published = swap
          },
        })
      } },
      onProgress: progress => { coverage.push(progress.materializedBytes) },
    })
    expect(await job.run()).toMatchObject({ status: 'completed', completedFiles: 1n, completedBytes: 8n })
    expect(published).toEqual(file.bytes)
    expect(release).toHaveBeenCalledTimes(2)
    expect(fixture.readRanges).toEqual([byteRange(0n, 8n), byteRange(0n, 8n)])
    expect(coverage.every(value => value >= 0n && value <= 8n)).toBe(true)
    expect(coverage.some((value, index) => index > 0 && coverage[index - 1] === 8n && value === 0n)).toBe(true)
    expect(job.progress()).toMatchObject({ materializedBytes: 8n, writtenBytes: 8n, receivedObjectBytes: 16n })
  })

  it.each(['changed', 'unverified'] as const)('rejects a %s source before opening the replacement writer', async mode => {
    const fixture = makeShareFixture([file])
    const release = vi.fn(async () => undefined)
    const openFile = vi.fn(async () => outputFile({ commit: async () => { throw discardedPublish() } }))
    const job = new TransferJob({
      ...fixture, selection,
      revisions: { open: async (...args) => {
        const opened = await fixture.revisions.open(...args)
        const attempt = fixture.openedFileIds.length
        const revision = id(attempt)
        return { ...opened, release, descriptor: { ...opened.descriptor,
          fileRevision: revision, fileRevisionText: encodeBase64Url(revision),
          ...(mode === 'changed' ? { sourceMetadata: metadata(attempt) } : {}),
        } }
      } },
      output: { sessionId: 'source', capabilities, openFile },
    })
    expect((await job.run()).status).toBe(mode === 'changed' ? 'source-changed' : 'failed')
    expect(openFile).toHaveBeenCalledOnce()
    expect(release).toHaveBeenCalledTimes(2)
    expect(job.progress()).toMatchObject({ materializedBytes: 0n, writtenBytes: 0n })
  })

  it('keeps the one-replay limit across an intervening expired lease', async () => {
    const fixture = makeShareFixture([file])
    const release = vi.fn(async () => undefined)
    const failure = discardedPublish()
    const job = new TransferJob({
      ...fixture, selection,
      revisions: { open: async (...args) => {
        if (fixture.openedFileIds.length === 3) throw new Error('Unexpected fourth admission')
        return { ...await fixture.revisions.open(...args), release }
      } },
      broker: { readRange: async function* (...args) {
        if (fixture.openedFileIds.length === 2) throw new RevisionLeaseExpiredError()
        yield* fixture.broker.readRange(...args)
      } },
      output: { sessionId: 'limit', capabilities, openFile: async () => outputFile({
        commit: async () => { throw failure },
      }) },
    })
    expect(await job.run()).toMatchObject({ status: 'failed', failedFiles: 1n, error: failure })
    expect(release).toHaveBeenCalledTimes(3)
    expect(job.progress()).toMatchObject({ materializedBytes: 0n, writtenBytes: 0n })
  })

  it.each([['pause', 'paused'], ['stop', 'stopped'], ['detach', 'detached']] as const)(
    'honors %s during retirement of the discarded file', async (action, expectedStatus) => {
      const fixture = makeShareFixture([file])
      const release = gate(), discarded = gate()
      let accepted = false
      const job = new TransferJob({
        ...fixture, selection,
        revisions: { open: async (...args) => ({ ...await fixture.revisions.open(...args), release: () => release.promise }) },
        onProgress: progress => {
          if (progress.materializedBytes > 0n) accepted = true
          else if (accepted) discarded.resolve()
        },
        output: { sessionId: 'exit', capabilities, openFile: async () => outputFile({
          commit: async () => { throw discardedPublish() },
        }) },
      })
      const run = job.run()
      await discarded.promise
      expect(job.allReceivedSaved).toBe(false)
      job[action]()
      release.resolve()
      expect((await run).status).toBe(expectedStatus)
      expect(fixture.openedFileIds).toHaveLength(1)
      expect(job.progress()).toMatchObject({ materializedBytes: 0n, writtenBytes: 0n })
    },
  )
})

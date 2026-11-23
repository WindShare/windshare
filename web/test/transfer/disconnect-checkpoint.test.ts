import { describe, expect, it, vi } from 'vitest'
import { SelectionPolicy } from '../../src/catalog/selection'
import type { BlockRangeReader } from '../../src/content/broker'
import { byteRange } from '../../src/content/geometry'
import { FaultScope, OutputFaultCode, outputFailure } from '../../src/transfer/fault'
import { TransferJob } from '../../src/transfer/job'
import type { TransferOutputFile } from '../../src/transfer/output-port'
import { makeShareFixture } from './share-fixture'

function gate() {
  let resolve!: () => void
  const promise = new Promise<void>(done => { resolve = done })
  return { promise, resolve }
}

function outputFile(overrides: Partial<TransferOutputFile>): TransferOutputFile {
  return {
    completedRanges: [], writeAt: async () => undefined, checkpoint: async () => 'saved',
    pause: async () => undefined, commit: async () => undefined, abort: async () => undefined,
    ...overrides,
  }
}

const selection = new SelectionPolicy(true).snapshot()
const movie = { seed: 10, name: 'movie.bin', bytes: new Uint8Array(8) }
const song = { seed: 11, name: 'song.bin', bytes: new Uint8Array(8) }
const resumable = { maxConcurrentFiles: 2, resumable: true }
const live = () => new AbortController().signal

function waitForAbort(signal: AbortSignal): Promise<never> {
  return new Promise((_, reject) => {
    const rejectRead = () => reject(new DOMException('Reader closed', 'AbortError'))
    if (signal.aborted) rejectRead()
    else signal.addEventListener('abort', rejectRead, { once: true })
  })
}

describe('active file checkpoints', () => {
  it('confirms only received durable bytes and does not repeat a clean cut', async () => {
    const fixture = makeShareFixture([movie])
    const receiving = gate(), deliver = gate(), waiting = gate()
    const checkpoint = vi.fn<TransferOutputFile['checkpoint']>(async () => 'saved')
    const job = new TransferJob({
      ...fixture, selection,
      broker: { readRange: async function* (_descriptor, _lease, _span, options) {
        receiving.resolve()
        await deliver.promise
        yield { offset: 0n, data: Uint8Array.of(1, 2) }
        waiting.resolve()
        await waitForAbort(options!.signal!)
      } },
      output: { sessionId: 'out', capabilities: resumable, openFile: async () => outputFile({ checkpoint }) },
    })
    const run = job.run()
    await receiving.promise
    expect(await job.checkpointActive(live())).toMatchObject({ allReceivedSaved: false, savedFiles: 0 })
    deliver.resolve()
    await waiting.promise
    expect(await job.checkpointActive(live())).toMatchObject({ allReceivedSaved: true, savedFiles: 1, savedBytes: 2n })
    expect(await job.checkpointActive(live())).toMatchObject({ allReceivedSaved: true, savedFiles: 0, savedBytes: 0n })
    expect(checkpoint).toHaveBeenCalledOnce()
    job.detach()
    expect((await run).status).toBe('detached')
  })

  it('takes the latest ranges under the writer lock and serializes subsequent writes and final commit', async () => {
    const fixture = makeShareFixture([movie])
    const firstWrite = gate(), finishWrite = gate(), checkpointStarted = gate(), finishCut = gate()
    const events: string[] = []
    const job = new TransferJob({
      ...fixture, selection,
      output: { sessionId: 'out', capabilities: resumable, openFile: async () => outputFile({
        writeAt: async offset => {
          events.push('write-' + offset)
          if (offset === 0n) { firstWrite.resolve(); await finishWrite.promise }
        },
        checkpoint: async (ranges, reason) => {
          expect(ranges).toEqual([byteRange(0n, 4n)])
          expect(reason).toBe('disconnect')
          events.push('cut')
          checkpointStarted.resolve()
          await finishCut.promise
          return 'saved'
        },
        commit: async ranges => { expect(ranges).toEqual([byteRange(0n, 8n)]); events.push('commit') },
      }) },
    })
    const run = job.run()
    await firstWrite.promise
    const result = job.checkpointActive(live())
    expect(events).toEqual(['write-0'])
    finishWrite.resolve()
    await checkpointStarted.promise
    expect(events).toEqual(['write-0', 'cut'])
    finishCut.resolve()
    expect(await result).toMatchObject({ savedFiles: 1, savedBytes: 4n })
    expect((await run).status).toBe('completed')
    expect(events).toEqual(['write-0', 'cut', 'write-4', 'commit'])
  })

  it('releases a revision once before local commit and tracks the file through a slow commit', async () => {
    const fixture = makeShareFixture([movie])
    const committing = gate(), finishCommit = gate(), releaseRemote = gate()
    const release = vi.fn(async () => { await releaseRemote.promise; throw new Error('sender offline') })
    const checkpoint = vi.fn<TransferOutputFile['checkpoint']>(async () => 'saved')
    const job = new TransferJob({
      ...fixture, selection,
      revisions: { open: async (...args) => ({ ...await fixture.revisions.open(...args), release }) },
      output: { sessionId: 'out', capabilities: resumable, openFile: async () => outputFile({
        checkpoint, commit: async () => { committing.resolve(); await finishCommit.promise },
      }) },
    })
    const run = job.run()
    await committing.promise
    expect(release).toHaveBeenCalledTimes(1)
    expect(await job.checkpointActive(live())).toMatchObject({ allReceivedSaved: false, skippedFiles: 1 })
    expect(checkpoint).not.toHaveBeenCalled()
    releaseRemote.resolve()
    finishCommit.resolve()
    expect((await run).status).toBe('completed')
    expect(release).toHaveBeenCalledTimes(1)
  })

  it.each([
    [FaultScope.FileLocal, 'partial'], [FaultScope.Job, 'failed'],
  ] as const)('retires the correct readers for a %s output failure', async (scope, expectedStatus) => {
    const fixture = makeShareFixture([movie, song])
    const blocked = gate(), allowSibling = gate()
    let readers = 0
    const broker: BlockRangeReader = {
      readRange: async function* (desc, _lease, _span, options) {
        yield { offset: 0n, data: new Uint8Array(4) }
        if (++readers === 2) blocked.resolve()
        const signal = options!.signal!
        if (desc.fileId[0] === movie.seed) await waitForAbort(signal)
        await Promise.race([allowSibling.promise, waitForAbort(signal)])
        yield { offset: 4n, data: new Uint8Array(4) }
      },
    }
    const cause = outputFailure(scope, new Error('cannot flush this output'), OutputFaultCode.CommitFailed)
    const checkpoint = vi.fn<TransferOutputFile['checkpoint']>(async () => { throw cause })
    const job = new TransferJob({
      ...fixture, broker, selection,
      output: { sessionId: 'out', capabilities: resumable, openFile: async req =>
        outputFile(req.path[0] === movie.name ? { checkpoint } : {}) },
    })
    const run = job.run()
    await blocked.promise
    expect(await job.checkpointActive(live())).toMatchObject({ allReceivedSaved: false, failedFiles: 1 })
    if (scope === FaultScope.FileLocal) allowSibling.resolve()
    const result = await run
    expect(result.status).toBe(expectedStatus)
    expect(result.error).toBe(cause)
    expect(result.completedFiles).toBe(scope === FaultScope.FileLocal ? 1n : 0n)
  })

  it('keeps skipped bytes dirty and lets an already-started cut finish on detach', async () => {
    const fixture = makeShareFixture([movie])
    const waiting = gate(), saving = gate(), finishCut = gate()
    let attempts = 0
    const abort = vi.fn(async () => undefined)
    const job = new TransferJob({
      ...fixture, selection,
      broker: { readRange: async function* (_desc, _lease, _span, options) {
        yield { offset: 0n, data: new Uint8Array(4) }
        waiting.resolve()
        await waitForAbort(options!.signal!)
      } },
      output: { sessionId: 'out', capabilities: resumable, openFile: async () => outputFile({
        abort, checkpoint: async () => {
          if (++attempts === 1) return 'skipped'
          saving.resolve()
          await finishCut.promise
          return 'saved'
        },
      }) },
    })
    const run = job.run()
    await waiting.promise
    expect(await job.checkpointActive(live())).toMatchObject({ skippedFiles: 1, savedBytes: 0n, allReceivedSaved: false })
    const savingResult = job.checkpointActive(live())
    await saving.promise
    job.detach()
    expect(abort).not.toHaveBeenCalled()
    finishCut.resolve()
    expect(await savingResult).toMatchObject({ savedFiles: 1, savedBytes: 4n, allReceivedSaved: false })
    expect((await run).status).toBe('detached')
    expect(abort).toHaveBeenCalledOnce()
  })
})

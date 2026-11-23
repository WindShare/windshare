import { describe, expect, it, vi } from 'vitest'
import type { CatalogClient } from '../../src/catalog/client'
import { SelectionPolicy } from '../../src/catalog/selection'
import type { BlockRangeReader } from '../../src/content/broker'
import { RevisionLeaseExpiredError } from '../../src/content/session-services'
import { byteRange, type ByteRange } from '../../src/content/geometry'
import { FaultScope, FileDestinationConflictError, OutputFaultCode, SourceFaultCode, TransferFaultError, outputFailure, sourceFault } from '../../src/transfer/fault'
import { TransferJob } from '../../src/transfer/job'
import type { TransferOutputFile } from '../../src/transfer/output-port'
import { gate, id, makeShareFixture, outputFile } from './share-fixture'

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

describe('transfer output settlement', () => {
  it.each(['disconnect', 'retry'] as const)('keeps a %s checkpoint durable through slow lease retirement', async reason => {
    const fixture = makeShareFixture([movie])
    const waiting = gate(), expire = gate(), retiring = gate(), release = gate()
    const savedBy: string[] = []
    let ranges: readonly ByteRange[] = []
    const retire = vi.fn(async () => { retiring.resolve(); await release.promise })
    const job = new TransferJob({
      ...fixture, selection,
      revisions: { open: async (...args) => {
        const opened = await fixture.revisions.open(...args)
        return fixture.openedFileIds.length === 1 ? { ...opened, release: retire } : opened
      } },
      broker: { readRange: async function* (...args) {
        if (args[2].start > 0n) { yield* fixture.broker.readRange(...args); return }
        yield { offset: 0n, data: new Uint8Array(4) }
        waiting.resolve()
        await expire.promise
        throw new RevisionLeaseExpiredError()
      } },
      output: { sessionId: 'retry', capabilities: resumable, openFile: async () => outputFile({
        completedRanges: ranges,
        checkpoint: async (next, trigger) => { ranges = next; savedBy.push(trigger); return 'saved' },
      }) },
    })
    const run = job.run()
    await waiting.promise
    if (reason === 'disconnect') expect((await job.checkpointActive(live())).savedBytes).toBe(4n)
    expire.resolve()
    await retiring.promise
    expect(await job.checkpointActive(live())).toMatchObject({ allReceivedSaved: true, skippedFiles: 0, savedFiles: 0 })
    expect(savedBy).toEqual([reason])
    release.resolve()
    expect(await run).toMatchObject({ status: 'completed', completedBytes: 8n })
    expect(retire).toHaveBeenCalledOnce()
    expect(fixture.readRanges).toEqual([byteRange(4n, 8n)])
  })

  it.each([
    ['stop', 'write', 'stopped'], ['detach', 'write', 'detached'],
    ['stop', 'retry', 'stopped'], ['detach', 'retry', 'detached'],
  ] as const)('preserves %s while a %s failure is in flight', async (exit, stage, status) => {
    const fixture = makeShareFixture([movie])
    const attempt = gate(), fail = gate()
    const error = new DOMException('Disk full', 'QuotaExceededError')
    const failWrite = async (): Promise<never> => { attempt.resolve(); await fail.promise; throw error }
    const pause = vi.fn<TransferOutputFile['pause']>(async () => undefined)
    const abort = vi.fn<TransferOutputFile['abort']>(async () => undefined)
    const job = new TransferJob({
      ...fixture, selection,
      broker: stage === 'write' ? fixture.broker : { readRange: async function* () {
        yield { offset: 0n, data: new Uint8Array(4) }
        throw new RevisionLeaseExpiredError()
      } },
      output: { sessionId: 'exit', capabilities: resumable, openFile: async () => outputFile({
        pause, abort, writeAt: async offset => { if (offset === 4n) await failWrite() }, checkpoint: failWrite,
      }) },
    })
    const run = job.run()
    await attempt.promise
    job[exit]()
    fail.resolve()
    expect(await run).toMatchObject({ status, failedFiles: 0n })
    expect(pause).not.toHaveBeenCalled()
    expect(abort).toHaveBeenCalledOnce()
    expect(fixture.openedFileIds).toHaveLength(1)
  })

  it.each([
    ['user', 'QuotaExceededError', 'storage-full'], ['interrupted', 'NotAllowedError', 'permission-needed'],
  ] as const)('preserves the storage cause when a %s pause cannot flush', async (requested, name, reason) => {
    const waiting = gate()
    const cause = new DOMException('Cannot save prefix', name)
    const pause = vi.fn<TransferOutputFile['pause']>(async () => { throw cause })
    const abort = vi.fn<TransferOutputFile['abort']>(async () => undefined)
    const job = new TransferJob({
      ...makeShareFixture([movie]), selection,
      broker: { readRange: async function* (_desc, _lease, _span, options) {
        yield { offset: 0n, data: new Uint8Array(4) }
        waiting.resolve()
        await waitForAbort(options!.signal!)
      } },
      output: { sessionId: 'pause', capabilities: resumable, openFile: async () => outputFile({ pause, abort }) },
    })
    const run = job.run()
    await waiting.promise
    job.pause(requested)
    job.detach()
    expect(await run).toMatchObject({ status: 'paused', pauseReason: reason, error: cause, failedFiles: 0n })
    expect(pause).toHaveBeenCalledExactlyOnceWith([byteRange(0n, 4n)])
    expect(abort).toHaveBeenCalledExactlyOnceWith(cause)
  })

  it.each(['writer', 'source', 'cancelled-write'] as const)('settles a %s error racing an accepted pause', async stage => {
    const fixture = makeShareFixture([movie])
    const pending = gate(), finish = gate()
    const sourceChanged = new TransferFaultError(sourceFault(FaultScope.FileLocal, SourceFaultCode.RevisionChanged), 'Edited')
    const damaged = new Error('Cannot write this file')
    const pause = vi.fn<TransferOutputFile['pause']>(async () => undefined)
    const abort = vi.fn<TransferOutputFile['abort']>(async () => undefined)
    const job = new TransferJob({
      ...fixture, selection,
      broker: stage === 'source' ? { readRange: async function* () {
        yield { offset: 0n, data: new Uint8Array(4) }
        pending.resolve()
        await finish.promise
        throw sourceChanged
      } } : fixture.broker,
      output: { sessionId: 'pause-race', capabilities: resumable, openFile: async (_request, signal) => outputFile({
        pause, abort,
        writeAt: async offset => {
          if (offset === 0n) return
          pending.resolve()
          await finish.promise
          if (stage === 'cancelled-write') signal.throwIfAborted()
          throw damaged
        },
      }) },
    })
    const run = job.run()
    await pending.promise
    job.pause()
    finish.resolve()
    const result = await run
    expect(result).toMatchObject({ status: 'paused', pauseReason: 'user' })
    if (stage === 'cancelled-write') {
      expect(pause).toHaveBeenCalledExactlyOnceWith([byteRange(0n, 4n)])
      expect(abort).not.toHaveBeenCalled()
      expect(result.failedFiles).toBe(0n)
    } else {
      expect(pause).not.toHaveBeenCalled()
      expect(abort).toHaveBeenCalledExactlyOnceWith(stage === 'source' ? sourceChanged : damaged)
      expect(result.failedFiles).toBe(1n)
      if (stage === 'source') expect(result.sourceRevisionFailures?.count).toBe(1n)
      else expect(result.error).toBe(damaged)
    }
  })

  it.each([FaultScope.FileLocal, FaultScope.Job])('saves healthy siblings according to the %s quota fault', async scope => {
    const bothDirty = gate()
    let readers = 0
    const cause = outputFailure(scope, new DOMException('No space available', 'QuotaExceededError'), OutputFaultCode.WriteFailed)
    const pauseSibling = vi.fn<TransferOutputFile['pause']>(async () => undefined)
    const abortSibling = vi.fn<TransferOutputFile['abort']>(async () => undefined)
    const job = new TransferJob({
      ...makeShareFixture([movie, song]), selection,
      broker: { readRange: async function* (descriptor, _lease, _span, options) {
        yield { offset: 0n, data: new Uint8Array(4) }
        if (++readers === 2) bothDirty.resolve()
        if (descriptor.fileId[0] === movie.seed) {
          await bothDirty.promise
          yield { offset: 4n, data: new Uint8Array(4) }
        } else {
          await waitForAbort(options!.signal!)
        }
      } },
      output: { sessionId: 'quota-scope', capabilities: resumable, openFile: async request =>
        outputFile(request.path[0] === movie.name
          ? { writeAt: async offset => { if (offset === 4n) throw cause } }
          : { pause: pauseSibling, abort: abortSibling }),
      },
    })
    expect(await job.run()).toMatchObject({ status: 'paused', pauseReason: 'storage-full', failedFiles: 0n, error: cause })
    if (scope === FaultScope.FileLocal) {
      expect(pauseSibling).toHaveBeenCalledExactlyOnceWith([byteRange(0n, 4n)])
      expect(abortSibling).not.toHaveBeenCalled()
    } else {
      expect(pauseSibling).not.toHaveBeenCalled()
      expect(abortSibling).toHaveBeenCalledExactlyOnceWith(cause)
    }
  })

  it.each(['failed', 'partial'] as const)('identifies the actual destination of a conflict in a %s download', async status => {
    const destinationPath = 'Downloads/Travel (2)/safe-movie.bin'
    const cause = new FileDestinationConflictError(destinationPath)
    const result = await new TransferJob({
      ...makeShareFixture(status === 'failed' ? [movie] : [movie, song]), selection,
      output: { sessionId: 'fsa-conflict', capabilities: resumable, openFile: async request =>
        outputFile(request.path[0] === movie.name ? {
          commit: async () => { throw outputFailure(FaultScope.FileLocal, cause, OutputFaultCode.CommitFailed) },
        } : {}),
      },
    }).run()
    expect(result.status).toBe(status)
    expect(result.error).toBe(cause)
    expect(cause.message).toContain(destinationPath)
    expect(result.contentWarning?.missingFiles).toEqual([
      { path: [movie.name], reason: 'destination-conflict', destinationPath },
    ])
  })

  it('retains the error on a non-resumable output even after an earlier member committed', async () => {
    const cause = new DOMException('Browser spool full', 'QuotaExceededError')
    const pause = vi.fn<TransferOutputFile['pause']>(async () => undefined)
    const abort = vi.fn<TransferOutputFile['abort']>(async () => undefined)
    const job = new TransferJob({
      ...makeShareFixture([movie, song]), selection,
      output: { sessionId: 'portable', capabilities: { resumable: false, maxConcurrentFiles: 1 },
        openFile: async req => outputFile(req.path[0] === song.name
          ? { pause, abort, commit: async () => { throw cause } } : {}),
      },
    })
    expect(await job.run()).toMatchObject({ status: 'failed', completedFiles: 1n, error: cause })
    expect(abort).toHaveBeenCalledExactlyOnceWith(cause)
    expect(pause).not.toHaveBeenCalled()
  })

  it('pauses on a directory storage failure without counting the directory as failed', async () => {
    const fixture = makeShareFixture([])
    const cause = new DOMException('Permission revoked', 'NotAllowedError')
    const catalog = { ...fixture.catalog, page: async (...args: Parameters<CatalogClient['page']>) => ({
      ...await fixture.catalog.page(...args), entries: [{ kind: 'directory', id: id(12), idText: 'child', name: 'Photos' }],
    }) } as unknown as CatalogClient
    const markDiscoveryComplete = vi.fn()
    const job = new TransferJob({
      ...fixture, catalog, selection,
      output: { sessionId: 'directory', capabilities: resumable, openFile: vi.fn(), markDiscoveryComplete,
        ensureDirectory: async () => { throw cause },
      },
    })
    expect(await job.run()).toMatchObject({
      status: 'paused', pauseReason: 'permission-needed', discoveryComplete: false, failedDirectories: 0n,
    })
    expect(job.progress()).toMatchObject({ discovery: 'open', failedDirectories: 0 })
    expect(markDiscoveryComplete).not.toHaveBeenCalled()
  })

  it.each(['quota', 'database', 'database-after-pause'] as const)('retires active workers when discovery persistence fails with %s', async mode => {
    const waiting = gate()
    const cause = mode === 'quota' ? new DOMException('No storage', 'QuotaExceededError') : new Error('Database unavailable')
    const pause = vi.fn<TransferOutputFile['pause']>(async () => undefined)
    const abort = vi.fn<TransferOutputFile['abort']>(async () => undefined)
    const job = new TransferJob({
      ...makeShareFixture([movie]), selection,
      broker: { readRange: async function* (_desc, _lease, _span, options) {
        yield { offset: 0n, data: new Uint8Array(4) }
        waiting.resolve()
        await waitForAbort(options!.signal!)
      } },
      output: { sessionId: 'catalog', capabilities: resumable, openFile: async () => outputFile({ pause, abort }),
        markDiscoveryComplete: async () => {
          await waiting.promise
          if (mode === 'database-after-pause') job.pause()
          throw cause
        },
      },
    })
    const result = await job.run()
    expect(result).toMatchObject({ discoveryComplete: false, failedDirectories: 0n, error: cause })
    if (mode === 'quota') {
      expect(result).toMatchObject({ status: 'paused', pauseReason: 'storage-full' })
      expect(pause).toHaveBeenCalledExactlyOnceWith([byteRange(0n, 4n)])
      expect(job.progress().discovery).toBe('open')
    } else {
      expect(result.status).toBe('failed')
      if (mode === 'database') expect(abort).toHaveBeenCalledOnce()
      expect(job.progress().discovery).toBe('failed')
    }
  })
})

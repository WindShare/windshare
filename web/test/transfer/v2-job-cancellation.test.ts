import { describe, expect, it, vi } from 'vitest'

import type { V2CatalogClient } from '../../src/catalog/v2-client'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import { byteRange } from '../../src/content/geometry'
import type { V2BlockRangeReader } from '../../src/content/v2-broker'
import type { V2RevisionReader } from '../../src/content/v2-session-services'
import {
  COMPLETED_JOB_SETTLEMENT,
  pausedJobSettlement,
  VerifiedDurableRanges,
  type OutputDirectoryAdmission,
  type OutputFile,
  type OutputFileOwnership,
  type OutputSession,
} from '../../src/transfer/output-session'
import { TransferJob, type TransferProgress } from '../../src/transfer/v2-job'
import {
  committedDirectoryFor,
  BIND_TEST_DIRECTORY_ADMISSION_SCOPE,
  directoryEntry,
  fileEntry,
  identity,
  identityText,
  openedRevision,
  outputAuthority,
  testDirectoryAdmissionLedgerBinding,
  traversalPage,
  type ScopeBoundTestOutputSession,
  withTimeout,
} from './v2-job-fixture'

type BlockingStage = 'root-admission' | 'child-admission' | 'begin' | 'write' | 'checkpoint' | 'commit'

describe('v2 cancellation-aware output mutations', () => {
  it.each([
    'root-admission',
    'child-admission',
    'begin',
    'write',
    'checkpoint',
    'commit',
  ] as const)('settles a cancellation during %s without starting a sibling mutation', async (stage) => {
    const fixture = cancellationOutput(stage)
    const controller = new AbortController()
    const progress: TransferProgress[] = []
    const running = cancellationJob(stage, fixture.session, (value) => progress.push(value)).run(controller.signal)

    await withTimeout(fixture.stageStarted, 1_000, `${stage} did not receive output authority`)
    controller.abort(new DOMException(`cancel ${stage}`, 'AbortError'))
    const result = await withTimeout(running, 1_000, `${stage} did not settle after cancellation`)

    expect(result.outcome.status).toBe('Paused')
    expect(result.settlement.kind).toBe('Paused')
    expect(fixture.perFileRetire).not.toHaveBeenCalled()
    expect(fixture.active()).toBe(false)
    expect(fixture.begunPaths).toHaveLength(stage === 'root-admission' || stage === 'child-admission' ? 0 : 1)
    expect(progress.at(-1)?.discovery).toBe(
      stage === 'write' || stage === 'checkpoint' || stage === 'commit' ? 'complete' : 'failed',
    )
  })

  it('retains a durable prefix on external cancellation instead of aborting the file transaction', async () => {
    const fixture = cancellationOutput(undefined)
    const controller = new AbortController()
    const secondRead = deferred<void>()
    const broker = {
      readRange: async function* (_descriptor: unknown, _lease: Uint8Array, range: { start: bigint }) {
        if (range.start === 0n) {
          yield { offset: 0n, data: Uint8Array.of(1) }
          return
        }
        secondRead.resolve()
        await rejectOnAbort(controller.signal)
      },
    } as unknown as V2BlockRangeReader
    const running = fileJob(fixture.session, 2n, broker).run(controller.signal)

    await secondRead.promise
    controller.abort(new DOMException('retain checkpoint', 'AbortError'))
    const result = await running

    expect(result.outcome.status).toBe('Paused')
    expect(result.settlement.kind).toBe('Paused')
    expect(fixture.durableRanges()).toEqual([byteRange(0n, 1n)])
    expect(fixture.perFileRetire).not.toHaveBeenCalled()
  })

  it('reports attention without acquiring destructive discard authority when pause fails', async () => {
    const fixture = cancellationOutput('write', true)
    const controller = new AbortController()
    const running = cancellationJob('write', fixture.session).run(controller.signal)
    await fixture.stageStarted
    controller.abort(new DOMException('discard after retention failure', 'AbortError'))

    const result = await running

    expect(result.outcome.status).toBe('Paused')
    expect(result.settlement.kind).toBe('NeedsAttention')
    expect(fixture.active()).toBe(true)
    expect(fixture.perFileRetire).not.toHaveBeenCalled()
  })
})

function cancellationJob(
  stage: BlockingStage,
  output: OutputSession,
  onProgress?: (progress: TransferProgress) => void,
): TransferJob {
  if (stage === 'child-admission') {
    const root = identity(2)
    const child = identity(3)
    const catalog = {
      loadDirectory: async (id: Uint8Array<ArrayBuffer>) => committedDirectoryFor(
        id,
        identityText(id[0] ?? 0),
        1,
      ),
      pages: async function* (directory: { directoryId: Uint8Array<ArrayBuffer> }) {
        yield traversalPage(
          directory.directoryId,
          directory.directoryId[0] === root[0]
            ? [directoryEntry(child, identityText(3), 'child')]
            : [fileEntry(identity(11), 'a.bin', 1n)],
        )
      },
    } as unknown as V2CatalogClient
    return newJob(root, catalog, output, 1n, undefined, onProgress)
  }
  return fileJob(output, 1n, undefined, onProgress)
}

function fileJob(
  output: OutputSession,
  exactSize: bigint,
  broker?: V2BlockRangeReader,
  onProgress?: (progress: TransferProgress) => void,
): TransferJob {
  const root = identity(2)
  const entries = [
    fileEntry(identity(11), 'a.bin', exactSize),
    fileEntry(identity(12), 'b.bin', exactSize),
  ]
  const catalog = {
    loadDirectory: async () => committedDirectoryFor(root, identityText(2), entries.length),
    pages: async function* () { yield traversalPage(root, entries) },
  } as unknown as V2CatalogClient
  return newJob(root, catalog, output, exactSize, broker, onProgress)
}

function newJob(
  root: Uint8Array<ArrayBuffer>,
  catalog: V2CatalogClient,
  output: OutputSession,
  exactSize: bigint,
  broker?: V2BlockRangeReader,
  onProgress?: (progress: TransferProgress) => void,
): TransferJob {
  return new TransferJob({
    descriptor: {
      shareInstance: identity(1), syntheticRoot: root, syntheticRootId: identityText(root[0] ?? 0), chunkSize: 1,
    } as never,
    catalog,
    selection: new V2SelectionPolicy(true),
    revisions: {
      open: async (id: Uint8Array<ArrayBuffer>) => openedRevision(id, exactSize, 1n),
    } as V2RevisionReader,
    broker: broker ?? {
      readRange: async function* (_descriptor, _lease, range) {
        yield { offset: range.start, data: new Uint8Array(Number(range.end - range.start)) }
      },
    },
    lanes: { size: 1 },
    output: outputAuthority(output),
    maximumConcurrentDirectories: 1,
    maximumConcurrentFiles: 1,
    outputSettlementTimeoutMilliseconds: 100,
    ...(onProgress === undefined ? {} : { onProgress }),
  })
}

function cancellationOutput(stage?: BlockingStage, failRetention = false): {
  readonly session: OutputSession
  readonly stageStarted: Promise<void>
  readonly begunPaths: string[]
  readonly perFileRetire: ReturnType<typeof vi.fn>
  readonly active: () => boolean
  readonly durableRanges: () => readonly { readonly start: bigint; readonly end: bigint }[]
} {
  const started = deferred<void>()
  const admissions = testDirectoryAdmissionLedgerBinding()
  const identity = Object.freeze({ backend: 'cancel-output', outputSessionId: identityText(41) })
  const begunPaths: string[] = []
  const perFileRetire = vi.fn(async (...reasons: [unknown]) => {
    if (reasons.length !== 1) throw new Error('file retirement requires one reason')
    return 'FileIsolated' as const
  })
  let active = false
  let durable = [] as readonly { readonly start: bigint; readonly end: bigint }[]
  const block = async (candidate: BlockingStage, signal: AbortSignal) => {
    if (stage !== candidate) return
    started.resolve()
    await rejectOnAbort(signal)
  }
  const session: ScopeBoundTestOutputSession = {
    [BIND_TEST_DIRECTORY_ADMISSION_SCOPE]: admissions.bind,
    identity,
    format: 'directory',
    capabilities: {
      durability: 'ProcessRestart', randomWrite: true, fileFailureIsolation: true, modificationTime: false,
    },
    admitDirectory: (request: OutputDirectoryAdmission, signal: AbortSignal) =>
      admissions.get().admitDirectory(request, signal, async (directory, operationSignal) => {
        await block(directory.path.length === 0 ? 'root-admission' : 'child-admission', operationSignal)
      }),
    finalizeDirectory: (admission, signal) => admissions.get().finalizeDirectory(admission, signal),
    beginFile: async (input: OutputFile, signal: AbortSignal) => {
      const file = admissions.get().validateFileParent(input)
      begunPaths.push(file.path.join('/'))
      active = true
      await block('begin', signal)
      const ownership: OutputFileOwnership = Object.freeze({
        ...identity,
        canonicalPath: file.path,
        ownedFileIdentity: `${identity.outputSessionId}:${file.path.join('/')}`,
      })
      return Object.freeze({
        durableRanges: new VerifiedDurableRanges(ownership, file.source, file.exactSize, durable),
        transaction: Object.freeze({
          writeRange: async (offset: bigint, data: Uint8Array, operationSignal: AbortSignal) => {
            await block('write', operationSignal)
            durable = [byteRange(offset, offset + BigInt(data.byteLength))]
          },
          checkpoint: async (operationSignal: AbortSignal) => {
            await block('checkpoint', operationSignal)
            return new VerifiedDurableRanges(ownership, file.source, file.exactSize, durable)
          },
          commit: async (operationSignal: AbortSignal) => {
            await block('commit', operationSignal)
            active = false
          },
          retire: async (reason: unknown) => {
            active = false
            return perFileRetire(reason)
          },
          pause: async () => { active = false },
        }),
      })
    },
    completeJob: async () => COMPLETED_JOB_SETTLEMENT,
    pauseJob: async () => {
      if (failRetention) throw new Error('retention unavailable')
      active = false
      return pausedJobSettlement('ProcessRestart')
    },
  }
  if (stage === undefined) started.resolve()
  return {
    session,
    stageStarted: started.promise,
    begunPaths,
    perFileRetire,
    active: () => active,
    durableRanges: () => durable,
  }
}

async function rejectOnAbort(signal: AbortSignal): Promise<never> {
  return new Promise<never>((_resolve, reject) => {
    const abort = () => reject(signal.reason)
    if (signal.aborted) abort()
    else signal.addEventListener('abort', abort, { once: true })
  })
}

function deferred<T>(): {
  readonly promise: Promise<T>
  readonly resolve: (value: T) => void
} {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((complete) => { resolve = complete })
  return { promise, resolve }
}

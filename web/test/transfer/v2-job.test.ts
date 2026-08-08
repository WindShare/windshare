import { describe, expect, it, vi } from 'vitest'

import type { V2CatalogClient } from '../../src/catalog/v2-client'
import type { V2CommittedDirectory } from '../../src/catalog/v2-page-store'
import type { V2CatalogEntry } from '../../src/catalog/v2-records'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import { FileGeometry, byteRange } from '../../src/content/geometry'
import type { V2BlockRangeReader } from '../../src/content/v2-broker'
import type { V2OpenedRevision, V2RevisionReader } from '../../src/content/v2-session-services'
import { createBoundedPortableDownloadStream } from '../../src/output/portable/browser-download'
import {
  SINGLE_FILE_STREAM_BACKEND,
  SingleFileStreamOutputSession,
} from '../../src/output/streams/single-file'
import { OutputCheckpointContractError } from '../../src/transfer/output-file-transaction'
import {
  OutputBudgetExceededError,
  directoryAdmissionScope,
  pausedJobSettlement,
  type OutputFile,
  type OutputSession,
  type V2OutputAuthority,
} from '../../src/transfer/output-session'
import { createTransferIntentDraft, freezeTransferIntent } from '../../src/transfer/intent'
import type { TransferTraceEvent } from '../../src/transfer/intent'
import { SenderObjectError } from '../../src/crypto/sender-object'
import { V2CborError, encodeCanonicalCbor } from '../../src/protocol/cbor'
import {
  V2BlockOperationError,
  V2RemoteOperationError,
  V2RemoteRevisionError,
  V2RevisionChangedDuringRecoveryError,
} from '../../src/content/v2-session-services'
import {
  TransferJob,
  isV2FileScopedTransferFailure,
} from '../../src/transfer/v2-job'
import {
  normalizeV2FileTransferFailure,
} from '../../src/transfer/job/failures'
import {
  FaultDomain,
  FaultScope,
  OutputFaultCode,
  SessionFaultCode,
} from '../../src/transfer/fault'
import { V2SessionRuntimeError } from '../../src/session/v2-runtime-types'
import {
  BIND_TEST_DIRECTORY_ADMISSION_SCOPE,
  catalogCommitment,
  committedDirectory,
  committedDirectoryFor,
  directoryEntry,
  fileEntry,
  identity,
  identityText,
  opaqueOutputIdentityText,
  openedRevision,
  outputAuthority,
  terminalBoundaryOutput,
  traversalJob,
  traversalOutput,
  traversalPage,
  wideIdentity,
  wideIdentityNumber,
  withTimeout,
  type ScopeBoundTestOutputSession,
  type TestOutputSessionFactory,
} from './v2-job-fixture'

describe('v2 transfer failure domains', () => {
  it('recognizes file scope only after one closed boundary normalization', () => {
    const revisionError = new V2RemoteOperationError(encodeCanonicalCbor(
      new Map<number, unknown>([
        [0, 1], [1, 3], [2, 0x3001], [3, false], [4, null], [5, 'changed'],
      ]),
    ))
    const blockError = new V2RemoteOperationError(encodeCanonicalCbor(
      new Map<number, unknown>([
        [0, 1], [1, 4], [2, 0x4001], [3, false], [4, null], [5, 'missing'],
      ]),
    ))

    const remoteRevision = new V2RemoteRevisionError({
      code: 0x3002,
      retryable: false,
    })
    const authenticatedBlockFailure = new V2BlockOperationError(
      'object-auth',
      'block sender object failed authentication',
      { cause: new Error('invalid signature') },
    )

    for (const raw of [revisionError, blockError, remoteRevision, authenticatedBlockFailure]) {
      expect(isV2FileScopedTransferFailure(raw)).toBe(false)
    }
    expect(isV2FileScopedTransferFailure(
      normalizeV2FileTransferFailure(revisionError).diagnostic,
    )).toBe(true)
    expect(isV2FileScopedTransferFailure(
      normalizeV2FileTransferFailure(blockError).diagnostic,
    )).toBe(true)
    expect(isV2FileScopedTransferFailure(
      normalizeV2FileTransferFailure(remoteRevision).diagnostic,
    )).toBe(true)
    expect(isV2FileScopedTransferFailure(
      normalizeV2FileTransferFailure(authenticatedBlockFailure).diagnostic,
    )).toBe(false)
  })

  it('promotes sender-object, wire, and session failures out of the file domain', () => {
    expect(isV2FileScopedTransferFailure(
      new SenderObjectError('authentication', 'ciphertext failed authentication'),
    )).toBe(false)
    expect(isV2FileScopedTransferFailure(new V2CborError('malformed signed result'))).toBe(false)
    expect(isV2FileScopedTransferFailure(
      new V2SessionRuntimeError('session', 'share identity changed'),
    )).toBe(false)
  })
})

describe('v2 portable output failure domain', () => {
  it('aborts the job when the portable final sink overflows', async () => {
    const publish = vi.fn()
    const stream = createBoundedPortableDownloadStream('unknown.zip', {
      createBlob: (parts) => new Blob([...parts]),
      publish,
    }, 3)
    let output: SingleFileStreamOutputSession | undefined
    const outputFactory: TestOutputSessionFactory = {
      backend: SINGLE_FILE_STREAM_BACKEND,
      format: 'single-file',
      open: (scope) => {
        output = new SingleFileStreamOutputSession('portable-overflow', scope, stream)
        return output
      },
    }
    const fileId = identity(11)
    const revision = {
      shareInstance: identity(1),
      shareInstanceId: 'share',
      fileId,
      fileIdText: 'file',
      fileRevision: identity(12),
      fileRevisionText: 'revision',
      exactSize: 4n,
      geometry: new FileGeometry(4n, 4n),
    }
    const committed = {
      directoryIdText: 'root',
      generationText: 'generation',
      directoryId: identity(2),
      generation: identity(3),
      pageCount: 1,
      entryCount: 1,
      omittedCount: 0n,
      terminalCommitment: catalogCommitment(),
    }
    const entry = {
      kind: 'file' as const,
      id: fileId,
      idText: 'file',
      name: 'overflow.bin',
      expectedSize: 4n,
    }
    const catalog = {
      loadDirectory: async () => committed,
      pages: async function* () {
        yield traversalPage(identity(2), [entry])
      },
    } as unknown as V2CatalogClient
    const revisions = {
      open: async () => ({
        descriptor: revision,
        leaseId: identity(13),
        release: async () => undefined,
      }),
    } as unknown as V2RevisionReader
    const broker = {
      readRange: async function* () {
        yield { offset: 0n, data: Uint8Array.of(1, 2, 3, 4) }
      },
    } as unknown as V2BlockRangeReader

    const result = await new TransferJob({
      descriptor: {
        shareInstance: identity(1),
        syntheticRoot: identity(2),
        syntheticRootId: 'root',
        chunkSize: 4,
      } as never,
      catalog,
      selection: new V2SelectionPolicy(),
      revisions,
      broker,
      lanes: { size: 1 },
      output: outputAuthority(outputFactory),
      maximumConcurrentFiles: 1,
    }).run()

    expect(result.outcome).toEqual({
      status: 'Paused',
      failures: [],
      failureCount: 0,
      omittedFailureCount: 0,
    })
    expect(output?.capabilities.durability).toBe('None')
    expect(publish).not.toHaveBeenCalled()
  })
})

describe('v2 incremental discovery boundary', () => {
  it('writes the first committed generation while a sibling directory is delayed', async () => {
    const root = identity(2)
    const sibling = identity(3)
    const selectedFile = identity(4)
    let outputOpened = false
    let revisionOpens = 0
    let releaseSibling: () => void = () => undefined
    let signalSiblingStarted: () => void = () => undefined
    let resolveFirstWrite: () => void = () => undefined
    const siblingStarted = new Promise<void>((resolve) => { signalSiblingStarted = resolve })
    const siblingGate = new Promise<void>((resolve) => { releaseSibling = resolve })
    const firstWrite = new Promise<void>((resolve) => { resolveFirstWrite = resolve })
    const admittedPaths: string[][] = []
    const catalog = {
      loadDirectory: async (id: Uint8Array) => {
        if (id[0] === sibling[0]) {
          signalSiblingStarted()
          await siblingGate
          return committedDirectory('child', 0)
        }
        return committedDirectory('root', 2)
      },
      pages: async function* (directory: V2CommittedDirectory) {
        if (directory.directoryIdText === 'root') {
          yield traversalPage(root, [{
            kind: 'file',
            id: selectedFile,
            idText: 'selected-file',
            name: 'selected.bin',
            expectedSize: 1n,
          }, directoryEntry(sibling, 'child', 'delayed')])
          return
        }
        yield traversalPage(sibling, [])
      },
    } as unknown as V2CatalogClient
    const output = terminalBoundaryOutput()
    const outputWithAdmission: OutputSession = {
      ...output,
      admitDirectory: async (directory, signal) => {
        const admission = await output.admitDirectory(directory, signal)
        if (directory.path.length > 0) admittedPaths.push([...directory.path])
        return admission
      },
      beginFile: async (file, signal) => {
        const begun = await output.beginFile(file, signal)
        const transaction = begun.transaction
        return {
          durableRanges: begun.durableRanges,
          transaction: {
            writeRange: async (offset, data, operationSignal) => {
              await transaction.writeRange(offset, data, operationSignal)
              resolveFirstWrite()
            },
            checkpoint: (operationSignal) => transaction.checkpoint(operationSignal),
            commit: (operationSignal) => transaction.commit(operationSignal),
            retire: (reason: unknown) => transaction.retire(reason),
            pause: (reason: unknown) => transaction.pause(reason),
          },
        }
      },
    }
    const authority: V2OutputAuthority = {
      confirmOutput: async (draft) => {
        outputOpened = true
        const intent = await freezeTransferIntent(draft, {
          target: opaqueOutputIdentityText(200),
          targetKind: 2,
          backend: 'test-terminal',
          format: 'directory',
        })
        ;(outputWithAdmission as ScopeBoundTestOutputSession)[
          BIND_TEST_DIRECTORY_ADMISSION_SCOPE
        ]?.(directoryAdmissionScope(intent))
        return { intent, session: outputWithAdmission }
      },
      openOutput: async (intent) => {
        outputOpened = true
        ;(outputWithAdmission as ScopeBoundTestOutputSession)[
          BIND_TEST_DIRECTORY_ADMISSION_SCOPE
        ]?.(directoryAdmissionScope(intent))
        return outputWithAdmission
      },
      abort: async (reason: unknown) => {
        await outputWithAdmission.pauseJob(reason)
      },
    }
    const revisions = {
      open: async () => {
        revisionOpens += 1
        return {
          descriptor: {
            shareInstance: identity(1),
            shareInstanceId: 'share',
            fileId: selectedFile,
            fileIdText: 'selected-file',
            fileRevision: identity(5),
            fileRevisionText: 'revision',
            exactSize: 1n,
            geometry: new FileGeometry(1n, 1n),
          },
          leaseId: identity(6),
          release: async () => undefined,
        }
      },
    } as unknown as V2RevisionReader
    const broker = {
      readRange: async function* () {
        yield { offset: 0n, data: Uint8Array.of(7) }
      },
    } as unknown as V2BlockRangeReader

    const resultPromise = new TransferJob({
      descriptor: { shareInstance: identity(1), syntheticRoot: root, syntheticRootId: 'root', chunkSize: 1 } as never,
      catalog,
      selection: new V2SelectionPolicy(),
      revisions,
      broker,
      lanes: { size: 1 },
      output: authority,
      maximumConcurrentFiles: 1,
      maximumConcurrentDirectories: 2,
    }).run()

    await siblingStarted
    await Promise.race([
      firstWrite,
      new Promise<never>((_, reject) => setTimeout(() => reject(new Error('first file write was delayed by sibling discovery')), 1_000)),
    ])
    expect(outputOpened).toBe(true)
    expect(revisionOpens).toBe(1)
    expect(admittedPaths).toEqual([])

    releaseSibling()
    const result = await resultPromise
    expect(result.outcome.status).toBe('Succeeded')
    expect(admittedPaths).toEqual([['delayed']])
  })

  it('applies pending-file item and byte backpressure while a worker is busy', async () => {
    const root = identity(2)
    const files = [11, 12, 13].map((first) => ({
      kind: 'file' as const,
      id: identity(first),
      idText: `file-${first}`,
      name: `file-${first}.bin`,
      expectedSize: 1n,
    }))
    let pageResumed = false
    let openedFiles = 0
    let signalFirstStarted: () => void = () => undefined
    const firstStartedSignal = new Promise<void>((resolve) => { signalFirstStarted = resolve })
    const catalog = {
      loadDirectory: async () => committedDirectory('root', files.length),
      pages: async function* () {
        yield traversalPage(root, files)
        pageResumed = true
      },
    } as unknown as V2CatalogClient
    let firstGateResolve: () => void = () => undefined
    const firstGate = new Promise<void>((resolve) => { firstGateResolve = resolve })
    const revisions = {
      open: async (fileId: Uint8Array) => {
        openedFiles += 1
        if (openedFiles === 1) signalFirstStarted()
        if (openedFiles === 1) await firstGate
        const fileNumber = fileId[0] ?? 0
        const descriptor = {
          shareInstance: identity(1),
          shareInstanceId: 'share',
          fileId,
          fileIdText: `file-${fileNumber}`,
          fileRevision: identity(40 + fileNumber),
          fileRevisionText: `revision-${fileNumber}`,
          exactSize: 1n,
          geometry: new FileGeometry(1n, 1n),
        }
        return {
          descriptor,
          leaseId: identity(80 + fileNumber),
          release: async () => undefined,
        }
      },
    } as unknown as V2RevisionReader
    const broker = {
      readRange: async function* () {
        yield { offset: 0n, data: Uint8Array.of(1) }
      },
    } as unknown as V2BlockRangeReader
    const resultPromise = new TransferJob({
      descriptor: { shareInstance: identity(1), syntheticRoot: root, syntheticRootId: 'root', chunkSize: 1 } as never,
      catalog,
      selection: new V2SelectionPolicy(),
      revisions,
      broker,
      lanes: { size: 1 },
      output: outputAuthority(terminalBoundaryOutput()),
      maximumConcurrentFiles: 1,
      maximumPendingFiles: 1,
      maximumPendingFileMetadataBytes: 400n,
    }).run()

    await firstStartedSignal
    await new Promise((resolve) => setTimeout(resolve, 10))
    expect(pageResumed).toBe(false)
    expect(openedFiles).toBe(1)
    firstGateResolve()
    const result = await resultPromise
    expect(result.outcome.status).toBe('Succeeded')
    expect(pageResumed).toBe(true)
    expect(openedFiles).toBe(3)
  })

  it('publishes terminal discovery while a selected file is still transferring', async () => {
    const root = identity(2)
    const selectedFile = identity(11)
    let releaseRevision: () => void = () => undefined
    let signalRevisionStarted: () => void = () => undefined
    let signalDiscoveryComplete: () => void = () => undefined
    const revisionGate = new Promise<void>((resolve) => { releaseRevision = resolve })
    const revisionStarted = new Promise<void>((resolve) => { signalRevisionStarted = resolve })
    const discoveryComplete = new Promise<void>((resolve) => { signalDiscoveryComplete = resolve })
    const progress: Array<{ readonly discovery: string; readonly partial: boolean }> = []
    const catalog = {
      loadDirectory: async () => committedDirectory('root', 1),
      pages: async function* () {
        yield traversalPage(root, [{
          kind: 'file',
          id: selectedFile,
          idText: 'selected-file',
          name: 'selected.bin',
          expectedSize: 1n,
        }])
      },
    } as unknown as V2CatalogClient
    const revisions = {
      open: async () => {
        signalRevisionStarted()
        await revisionGate
        return {
          descriptor: {
            shareInstance: identity(1),
            shareInstanceId: 'share',
            fileId: selectedFile,
            fileIdText: 'selected-file',
            fileRevision: identity(12),
            fileRevisionText: 'revision',
            exactSize: 1n,
            geometry: new FileGeometry(1n, 1n),
          },
          leaseId: identity(13),
          release: async () => undefined,
        }
      },
    } as unknown as V2RevisionReader
    const broker = {
      readRange: async function* () { yield { offset: 0n, data: Uint8Array.of(1) } },
    } as unknown as V2BlockRangeReader
    let settled = false
    const resultPromise = new TransferJob({
      descriptor: { shareInstance: identity(1), syntheticRoot: root, syntheticRootId: 'root', chunkSize: 1 } as never,
      catalog,
      selection: new V2SelectionPolicy(),
      revisions,
      broker,
      lanes: { size: 1 },
      output: outputAuthority(terminalBoundaryOutput()),
      maximumConcurrentFiles: 1,
      onMeasure: (measure) => {
        if (measure.discovery === 'complete') signalDiscoveryComplete()
      },
      onProgress: (snapshot) => { progress.push(snapshot) },
    }).run().finally(() => { settled = true })

    await revisionStarted
    await discoveryComplete
    expect(settled).toBe(false)
    expect(progress.at(-1)).toMatchObject({ discovery: 'complete', partial: false })

    releaseRevision()
    expect((await resultPromise).outcome.status).toBe('Succeeded')
  })
})

describe('v2 incremental failure and observability boundaries', () => {
  it('cancels in-flight workers before draining a fatal sibling failure', async () => {
    const root = identity(2)
    const blockedFile = identity(11)
    const fatalFile = identity(12)
    let signalBlockedStarted: () => void = () => undefined
    const blockedStarted = new Promise<void>((resolve) => { signalBlockedStarted = resolve })
    let blockedCancelled = false
    let outputWrites = 0
    const fatal = new SenderObjectError('authentication', 'fatal sibling authentication failure')
    const entries: readonly V2CatalogEntry[] = [{
      kind: 'file',
      id: blockedFile,
      idText: 'blocked-file',
      name: 'blocked.bin',
      expectedSize: 1n,
    }, {
      kind: 'file',
      id: fatalFile,
      idText: 'fatal-file',
      name: 'fatal.bin',
      expectedSize: 1n,
    }]
    const catalog = {
      loadDirectory: async () => committedDirectory('root', entries.length),
      pages: async function* () { yield traversalPage(root, entries) },
    } as unknown as V2CatalogClient
    const revisions = {
      open: async (fileId: Uint8Array, signal?: AbortSignal) => {
        if (fileId[0] === fatalFile[0]) {
          await blockedStarted
          throw fatal
        }
        signalBlockedStarted()
        if (signal === undefined) throw new Error('Transfer worker omitted its lifetime signal')
        await new Promise<never>((_resolve, reject) => {
          const cancel = () => {
            blockedCancelled = true
            reject(signal.reason)
          }
          if (signal.aborted) {
            cancel()
            return
          }
          signal.addEventListener('abort', cancel, { once: true })
        })
        throw new Error('Cancelled revision unexpectedly resumed')
      },
    } as unknown as V2RevisionReader
    const baseOutput = terminalBoundaryOutput()
    const output: OutputSession = {
      ...baseOutput,
      beginFile: async (file, signal) => {
        const begun = await baseOutput.beginFile(file, signal)
        return {
          ...begun,
          transaction: {
            ...begun.transaction,
            writeRange: async (offset: bigint, data: Uint8Array<ArrayBuffer>, operationSignal: AbortSignal) => {
              outputWrites += 1
              await begun.transaction.writeRange(offset, data, operationSignal)
            },
          },
        }
      },
    }

    const result = await withTimeout(new TransferJob({
      descriptor: { shareInstance: identity(1), syntheticRoot: root, syntheticRootId: 'root', chunkSize: 1 } as never,
      catalog,
      selection: new V2SelectionPolicy(),
      revisions,
      broker: {} as V2BlockRangeReader,
      lanes: { size: 1 },
      output: outputAuthority(output),
      maximumConcurrentFiles: 2,
    }).run(), 1_000, 'Fatal worker failure did not cancel its blocked sibling')

    expect(result.outcome.status).toBe('Paused')
    expect(result.abortReason).toMatchObject({
      fault: {
        domain: FaultDomain.Session,
        scope: FaultScope.OutputPause,
        code: SessionFaultCode.DependencyContract,
      },
      cause: fatal,
    })
    expect(blockedCancelled).toBe(true)
    expect(outputWrites).toBe(0)
  })

  it('keeps diagnostic observers isolated and emits causally ordered correlated traces', async () => {
    const root = identity(2)
    const selectedFile = identity(11)
    const traces: TransferTraceEvent[] = []
    const catalog = {
      loadDirectory: async () => committedDirectory('root', 1),
      pages: async function* () {
        yield traversalPage(root, [{
          kind: 'file',
          id: selectedFile,
          idText: identityText(11),
          name: 'selected.bin',
          expectedSize: 1n,
        }])
      },
    } as unknown as V2CatalogClient
    const revisions = {
      open: async () => openedRevision(selectedFile, 1n, 1n),
    } as unknown as V2RevisionReader
    const broker = {
      readRange: async function* () { yield { offset: 0n, data: Uint8Array.of(1) } },
    } as unknown as V2BlockRangeReader

    const result = await new TransferJob({
      descriptor: {
        shareInstance: identity(1),
        syntheticRoot: root,
        syntheticRootId: identityText(2),
        chunkSize: 1,
      } as never,
      catalog,
      selection: new V2SelectionPolicy(),
      revisions,
      broker,
      lanes: { size: 1 },
      output: outputAuthority(terminalBoundaryOutput()),
      maximumConcurrentFiles: 1,
      protocolSessionId: () => identityText(31),
      onTrace: (event) => { traces.push(event) },
      onMeasure: () => { throw new Error('diagnostic measure observer failed') },
      onProgress: () => { throw new Error('diagnostic progress observer failed') },
    }).run()

    expect(
      result.outcome.status,
      result.abortReason instanceof Error ? result.abortReason.stack : String(result.abortReason),
    ).toBe('Succeeded')
    const index = (name: TransferTraceEvent['name']) => traces.findIndex((trace) => trace.name === name)
    expect(index('directory-generation-committed')).toBeLessThan(index('directory-admitted'))
    expect(index('file-enqueued')).toBeLessThan(index('file-started'))
    expect(index('file-written')).toBeLessThan(index('file-completed'))
    expect(traces.filter(({ name }) => name === 'file-written')).toHaveLength(1)
    expect(traces.find(({ name }) => name === 'file-enqueued')?.context.selectionDecision)
      .toBe('default-rule')
    for (const trace of traces) {
      expect(trace.context).toMatchObject({
        shareInstance: identityText(1),
        transferJobId: result.transferJobId,
        protocolSessionId: identityText(31),
      })
    }
    expect(traces.find(({ name }) => name === 'file-completed')?.context.outputSessionId)
      .toBe('selection-bound')
  })
})

describe('v2 opened-revision authority', () => {
  it.each([
    ['share identity', (opened: V2OpenedRevision) => ({
      ...opened,
      descriptor: { ...opened.descriptor, shareInstance: identity(9) },
    })],
    ['file identity', (opened: V2OpenedRevision) => ({
      ...opened,
      descriptor: { ...opened.descriptor, fileId: identity(9) },
    })],
    ['exact size', (opened: V2OpenedRevision) => ({
      ...opened,
      descriptor: { ...opened.descriptor, exactSize: 2n, geometry: new FileGeometry(2n, 1n) },
    })],
    ['block geometry', (opened: V2OpenedRevision) => ({
      ...opened,
      descriptor: { ...opened.descriptor, geometry: new FileGeometry(1n, 2n) },
    })],
    ['lease identity', (opened: V2OpenedRevision) => ({
      ...opened,
      leaseId: new Uint8Array(16),
    })],
  ] as const)('rejects mismatched %s before output or content I/O', async (_name, mutate) => {
    const root = identity(2)
    const selectedFile = identity(11)
    const beginFile = vi.fn()
    const baseOutput = terminalBoundaryOutput()
    const output = { ...baseOutput, beginFile } as OutputSession
    const readRange = vi.fn(async function* () { yield { offset: 0n, data: Uint8Array.of(1) } })
    const catalog = {
      loadDirectory: async () => committedDirectory('root', 1),
      pages: async function* () {
        yield traversalPage(root, [{
          kind: 'file', id: selectedFile, idText: identityText(11), name: 'file.bin', expectedSize: 1n,
        }])
      },
    } as unknown as V2CatalogClient

    const result = await new TransferJob({
      descriptor: {
        shareInstance: identity(1), syntheticRoot: root, syntheticRootId: identityText(2), chunkSize: 1,
      } as never,
      catalog,
      selection: new V2SelectionPolicy(),
      revisions: {
        open: async () => mutate(openedRevision(selectedFile, 1n, 1n)),
      } as unknown as V2RevisionReader,
      broker: { readRange } as unknown as V2BlockRangeReader,
      lanes: { size: 1 },
      output: outputAuthority(output),
      maximumConcurrentFiles: 1,
    }).run()

    expect(result.outcome.status).toBe('CompletedWithErrors')
    expect(result.outcome.failureCount).toBe(1)
    expect(beginFile).not.toHaveBeenCalled()
    expect(readRange).not.toHaveBeenCalled()
  })
})

describe('v2 bounded scheduler regressions', () => {
  it('pauses before starting a sibling when a checkpoint invents future durability', async () => {
    const root = identity(2)
    const files = [fileEntry(identity(11), 'a.bin', 2n), fileEntry(identity(12), 'b.bin', 2n)]
    const base = terminalBoundaryOutput()
    const beginFile = vi.fn((file: OutputFile, signal: AbortSignal) => base.beginFile(file, signal))
    const output = { ...base, beginFile }
    const catalog = {
      loadDirectory: async () => committedDirectory('root', files.length),
      pages: async function* () { yield traversalPage(root, files) },
    } as unknown as V2CatalogClient
    const revisions = {
      open: async (id: Uint8Array<ArrayBuffer>) => openedRevision(id, 2n, 1n),
    } as unknown as V2RevisionReader
    const broker = {
      readRange: async function* () { yield { offset: 0n, data: Uint8Array.of(1) } },
    } as unknown as V2BlockRangeReader

    const result = await new TransferJob({
      descriptor: {
        shareInstance: identity(1), syntheticRoot: root, syntheticRootId: identityText(2), chunkSize: 1,
      } as never,
      catalog,
      selection: new V2SelectionPolicy(),
      revisions,
      broker,
      lanes: { size: 1 },
      output: outputAuthority(output),
      maximumConcurrentFiles: 1,
    }).run()

    expect(result.outcome.status).toBe('Paused')
    expect(result.abortReason).toMatchObject({
      fault: {
        domain: FaultDomain.Output,
        scope: FaultScope.OutputPause,
        code: OutputFaultCode.Contract,
      },
      cause: expect.any(OutputCheckpointContractError),
    })
    expect(beginFile).toHaveBeenCalledOnce()
  })

  it('treats a malformed adapter retirement disposition as a job-wide contract failure', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'file.bin', 1n)
    const base = terminalBoundaryOutput()
    const output: OutputSession = {
      ...base,
      beginFile: async (input, signal) => {
        const begun = await base.beginFile(input, signal)
        return {
          ...begun,
          transaction: {
            ...begun.transaction,
            retire: async () => 'forged' as never,
          },
        }
      },
    }
    const brokerFailure = new V2RevisionChangedDuringRecoveryError()
    const result = await new TransferJob({
      descriptor: {
        shareInstance: identity(1), syntheticRoot: root, syntheticRootId: identityText(2), chunkSize: 1,
      } as never,
      catalog: {
        loadDirectory: async () => committedDirectory('root', 1),
        pages: async function* () { yield traversalPage(root, [file]) },
      } as unknown as V2CatalogClient,
      selection: new V2SelectionPolicy(),
      revisions: { open: async () => openedRevision(file.id, 1n, 1n) } as unknown as V2RevisionReader,
      broker: {
        readRange: async function* () {
          await Promise.reject(brokerFailure)
          yield { offset: 0n, data: Uint8Array.of(0) }
        },
      } as unknown as V2BlockRangeReader,
      lanes: { size: 1 },
      output: outputAuthority(output),
      maximumConcurrentFiles: 1,
    }).run()

    expect(result.outcome.status).toBe('Paused')
    expect(result.abortReason).toMatchObject({
      fault: {
        domain: FaultDomain.Output,
        scope: FaultScope.OutputPause,
        code: OutputFaultCode.MutationAmbiguous,
      },
    })
  })

  it('transfers a file larger than the pending metadata budget without treating its content as queued memory', async () => {
    const root = identity(2)
    const largeFile = identity(11)
    const exactSize = 65n * 1024n * 1024n
    const requestedRanges: Array<{ readonly start: bigint; readonly end: bigint }> = []
    const catalog = {
      loadDirectory: async () => committedDirectory('root', 1),
      pages: async function* () {
        yield traversalPage(root, [{
          kind: 'file',
          id: largeFile,
          idText: 'large-file',
          name: 'large.bin',
          expectedSize: exactSize,
        }])
      },
    } as unknown as V2CatalogClient
    const revisions = {
      open: async () => ({
        descriptor: {
          shareInstance: identity(1),
          shareInstanceId: 'share',
          fileId: largeFile,
          fileIdText: 'large-file',
          fileRevision: identity(12),
          fileRevisionText: 'revision',
          exactSize,
          geometry: new FileGeometry(exactSize, 1024n * 1024n),
        },
        leaseId: identity(13),
        release: async () => undefined,
      }),
    } as unknown as V2RevisionReader
    const broker = {
      readRange: async function* (
        _revision: unknown,
        _leaseId: unknown,
        range: { readonly start: bigint; readonly end: bigint },
      ) {
        requestedRanges.push({ ...range })
        yield { offset: range.start, data: Uint8Array.of(7) }
      },
    } as unknown as V2BlockRangeReader

    const result = await new TransferJob({
      descriptor: { shareInstance: identity(1), syntheticRoot: root, syntheticRootId: 'root', chunkSize: 1024 * 1024 } as never,
      catalog,
      selection: new V2SelectionPolicy(),
      revisions,
      broker,
      lanes: { size: 1 },
      output: outputAuthority(terminalBoundaryOutput(1n)),
      maximumConcurrentFiles: 1,
    }).run()

    expect(result.outcome.status).toBe('Succeeded')
    expect(requestedRanges).toEqual([byteRange(0n, 1n)])
  })

  it('cannot deadlock when every directory worker produces beyond the bounded breadth queue', async () => {
    const parentCount = 4
    const childrenPerParent = 65
    const root = wideIdentity(1)
    let loads = 0
    let readyParents = 0
    let releaseParents: () => void = () => undefined
    const parentsReady = new Promise<void>((resolve) => { releaseParents = resolve })
    const catalog = {
      loadDirectory: async (id: Uint8Array<ArrayBuffer>) => {
        loads += 1
        const number = wideIdentityNumber(id)
        let entryCount = 0
        if (number === 1) entryCount = parentCount
        else if (number <= parentCount + 1) entryCount = childrenPerParent
        return committedDirectoryFor(id, `wide-${number}`, entryCount)
      },
      pages: async function* (directory: V2CommittedDirectory) {
        const number = wideIdentityNumber(directory.directoryId)
        if (number === 1) {
          yield traversalPage(root, Array.from({ length: parentCount }, (_, index) => {
            const childNumber = index + 2
            return directoryEntry(wideIdentity(childNumber), `wide-${childNumber}`, `parent-${index}`)
          }))
          return
        }
        if (number <= parentCount + 1) {
          readyParents += 1
          if (readyParents === parentCount) releaseParents()
          await parentsReady
          const parentIndex = number - 2
          yield traversalPage(directory.directoryId, Array.from(
            { length: childrenPerParent },
            (_, childIndex) => {
              const childNumber = parentCount + 2 + parentIndex * childrenPerParent + childIndex
              return directoryEntry(
                wideIdentity(childNumber),
                `wide-${childNumber}`,
                `leaf-${parentIndex}-${childIndex}`,
              )
            },
          ))
          return
        }
        yield traversalPage(directory.directoryId, [])
      },
    } as unknown as V2CatalogClient
    const output = traversalOutput()

    const result = await withTimeout(
      traversalJob(catalog, output.session, root, 'wide-1').run(),
      2_000,
      'directory workers stalled while their breadth queue was saturated',
    )

    expect(result.outcome.status).toBe('Succeeded')
    expect(loads).toBe(1 + parentCount + parentCount * childrenPerParent)
  })
})

describe('v2 output opening boundary', () => {
  it('does not discover a catalog when picker-confirmed OpenOutput is rejected', async () => {
    const loadDirectory = vi.fn(async () => committedDirectory('root', 0))
    const catalog = {
      loadDirectory,
      pages: async function* () {
        yield traversalPage(identity(2), [])
      },
    } as unknown as V2CatalogClient
    const pickerError = new DOMException('Output selection was cancelled.', 'AbortError')
    const authority: V2OutputAuthority = {
      confirmOutput: vi.fn(async () => { throw pickerError }),
      openOutput: vi.fn(async () => { throw pickerError }),
      abort: vi.fn(async () => undefined),
    }

    const result = await new TransferJob({
      descriptor: { shareInstance: identity(1), syntheticRoot: identity(2), syntheticRootId: 'root' } as never,
      catalog,
      selection: new V2SelectionPolicy(),
      revisions: {} as V2RevisionReader,
      broker: {} as V2BlockRangeReader,
      lanes: { size: 1 },
      output: authority,
    }).run()

    expect(authority.confirmOutput).toHaveBeenCalledOnce()
    expect(authority.abort).toHaveBeenCalledWith(pickerError)
    expect(loadDirectory).not.toHaveBeenCalled()
    expect(result.abortReason).toBe(pickerError)
    expect(result.outcome.status).toBe('Paused')
  })

  it('maps finite output-policy exhaustion to a paused job before a session opens', async () => {
    const budgetError = new OutputBudgetExceededError('test-budget', 2n, 3n)
    const abort = vi.fn(async () => undefined)
    const authority: V2OutputAuthority = {
      confirmOutput: vi.fn(async () => { throw budgetError }),
      openOutput: vi.fn(async () => { throw budgetError }),
      abort,
    }

    const result = await new TransferJob({
      descriptor: {
        shareInstance: identity(1),
        syntheticRoot: identity(2),
        syntheticRootId: 'root',
      } as never,
      catalog: {} as V2CatalogClient,
      selection: new V2SelectionPolicy(),
      revisions: {} as V2RevisionReader,
      broker: {} as V2BlockRangeReader,
      lanes: { size: 1 },
      output: authority,
    }).run()

    expect(result.outcome.status).toBe('Paused')
    expect(result.abortReason).toBe(budgetError)
    expect(abort).toHaveBeenCalledWith(budgetError)
  })

  it('rejects a supplied final intent that disagrees with the frozen job before opening output', async () => {
    const transferJobId = identityText(70)
    const intent = await freezeTransferIntent(createTransferIntentDraft({
      shareInstance: identityText(1),
      syntheticRoot: identityText(2),
      selection: { mode: 'node-id', defaultSelected: false, rules: [] },
    }), {
      target: opaqueOutputIdentityText(44),
      targetKind: 2,
      backend: 'test/forged-final',
      format: 'directory',
    })
    const openOutput = vi.fn(async () => terminalBoundaryOutput())
    const abort = vi.fn(async () => undefined)
    const loadDirectory = vi.fn()

    const result = await new TransferJob({
      descriptor: {
        shareInstance: identity(1), syntheticRoot: identity(2), syntheticRootId: 'root',
      } as never,
      catalog: { loadDirectory } as unknown as V2CatalogClient,
      selection: new V2SelectionPolicy(true),
      revisions: {} as V2RevisionReader,
      broker: {} as V2BlockRangeReader,
      lanes: { size: 1 },
      output: { confirmOutput: vi.fn(), openOutput, abort },
      transferJobId,
      intent,
    }).run()

    expect(result.outcome.status).toBe('Paused')
    expect(result.abortReason).toBeInstanceOf(TypeError)
    expect(openOutput).not.toHaveBeenCalled()
    expect(loadDirectory).not.toHaveBeenCalled()
    expect(abort).toHaveBeenCalledOnce()
  })

  it('owns and pauses an authority session whose format violates the frozen intent', async () => {
    const sessionPause = vi.fn(async () => pausedJobSettlement('ProcessRestart'))
    const mismatchedSession: OutputSession = {
      ...terminalBoundaryOutput(),
      format: 'zip',
      pauseJob: sessionPause,
    }
    const authorityAbort = vi.fn(async () => undefined)
    const authority: V2OutputAuthority = {
      confirmOutput: async (draft) => ({
        intent: await freezeTransferIntent(draft, {
          target: opaqueOutputIdentityText(45),
          targetKind: 2,
          backend: mismatchedSession.identity.backend,
          format: 'directory',
        }),
        session: mismatchedSession,
      }),
      openOutput: vi.fn(),
      abort: authorityAbort,
    }
    const loadDirectory = vi.fn()

    const result = await new TransferJob({
      descriptor: {
        shareInstance: identity(1), syntheticRoot: identity(2), syntheticRootId: 'root',
      } as never,
      catalog: { loadDirectory } as unknown as V2CatalogClient,
      selection: new V2SelectionPolicy(),
      revisions: {} as V2RevisionReader,
      broker: {} as V2BlockRangeReader,
      lanes: { size: 1 },
      output: authority,
    }).run()

    expect(result.outcome.status).toBe('Paused')
    expect(sessionPause).toHaveBeenCalledOnce()
    expect(authorityAbort).not.toHaveBeenCalled()
    expect(loadDirectory).not.toHaveBeenCalled()
  })
})

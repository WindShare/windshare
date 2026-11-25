import { assert, describe, expect, it, vi } from 'vitest'
import { retainedSnapshot } from '../../src/application/receive/retained'
import type { CatalogClient } from '../../src/catalog/client'
import { CatalogPageStoreError, type CommittedDirectory } from '../../src/catalog/page-store'
import type { CatalogEntry, CatalogPage, ShareDescriptor } from '../../src/catalog/records'
import { SelectionPolicy } from '../../src/catalog/selection'
import type { BlockRangeReader } from '../../src/content/broker'
import { REVISION_CODE_BUSY } from '../../src/content/flow'
import { FileGeometry } from '../../src/content/geometry'
import type { FileRevisionDescriptor } from '../../src/content/records'
import { RemoteOperationError } from '../../src/content/session-operations'
import { RemoteRevisionError, type RevisionReader } from '../../src/content/session-services'
import { encodeBase64Url } from '../../src/crypto/bytes'
import type { BrowserDownloadWindow } from '../../src/output/backends/browser-handoff'
import { openPortableSession } from '../../src/output/backends/portable'
import { createReceivedProtocolError } from '../../src/session/diagnostics'
import { createProtocolOperationIdentity, createProtocolSessionIdentity } from '../../src/session/identities'
import { TransferJob } from '../../src/transfer/job'
import { selectionKey, selectionSpecFromPolicy } from '../../src/transfer/selection'
import { initialReceiveState } from '../../src/transfer/state'
import { presentTask, retainedTaskFacts } from '../../src/ui/tasks'
import { gate, id, makeShareFixture, outputFile } from '../transfer/share-fixture'

const REVISION_CODE_DRIFT = 0x3007
const SELECTION = new SelectionPolicy(true).snapshot()

function createMockDownloadWindow(): {
  readonly windowPort: BrowserDownloadWindow
  readonly blobs: Blob[]
} {
  const blobs: Blob[] = []
  const windowPort: BrowserDownloadWindow = {
    URL: {
      createObjectURL: (blob: Blob | MediaSource) => {
        if (blob instanceof Blob) blobs.push(blob)
        return `blob:mock-${blobs.length}`
      },
      revokeObjectURL: () => undefined,
    },
    document: {
      createElement: () => ({
        download: '',
        href: '',
        hidden: false,
        click: () => undefined,
        remove: () => undefined,
      }) as unknown as HTMLAnchorElement,
      body: { append: () => undefined } as unknown as HTMLElement,
    } as unknown as Pick<Document, 'createElement' | 'body'>,
    setTimeout: (() => 0) as unknown as typeof setTimeout,
    clearTimeout: () => undefined,
  }
  return { windowPort, blobs }
}

function protocolError(requestKind: 'open_revisions' | 'request_blocks', code: number) {
  return createReceivedProtocolError({
    requestKind,
    correlation: Object.freeze({
      protocolSessionId: createProtocolSessionIdentity(id(90)),
      protocolOperationId: createProtocolOperationIdentity(1n),
    }),
    content: { scope: 'revision', code, retryable: false },
  })
}

function makeDirectoryPage(
  shareInstance: Uint8Array<ArrayBuffer>,
  directoryId: Uint8Array<ArrayBuffer>,
  entries: readonly CatalogEntry[],
): { readonly committed: CommittedDirectory; readonly page: CatalogPage } {
  const generation = id(3)
  const committed: CommittedDirectory = Object.freeze({
    directoryId,
    directoryIdText: encodeBase64Url(directoryId),
    generation,
    generationText: encodeBase64Url(generation),
    pageCount: 1,
    entryCount: entries.length,
    omittedCount: 0n,
    terminalCommitment: new Uint8Array(32),
  })
  const page: CatalogPage = Object.freeze({
    shareInstance,
    directoryId,
    directoryIdText: committed.directoryIdText,
    generation,
    generationText: committed.generationText,
    pageIndex: 0,
    terminal: true,
    previousCommitment: new Uint8Array(32),
    entries: Object.freeze([...entries]),
    omittedCount: 0n,
    objectCommitment: new Uint8Array(32),
    encodedBytes: 64,
  })
  return { committed, page }
}

function openPortableZip(taskId: string, descriptor: ShareDescriptor) {
  const spec = selectionSpecFromPolicy(descriptor.shareInstanceId, descriptor.syntheticRootId, SELECTION)
  const { windowPort, blobs } = createMockDownloadWindow()
  const session = openPortableSession({
    task: {
      taskId,
      selection: spec,
      selectionKey: selectionKey(spec),
      artifact: Object.freeze({ kind: 'zip-archive', name: 'bundle.zip' }),
      lifecycle: initialReceiveState(taskId),
    },
    windowPort,
  })
  return { session, blobs }
}

describe('portable all-or-nothing abort semantics', () => {
  it('halts immediately on mid-stream source-changed without extra RPCs or leaking ZIP writer abort strings', async () => {
    const shareInstance = id(1)
    const rootId = id(2)
    const subDirId = id(10)
    const trailingDirId = id(11)
    const file1Id = id(20)
    const file2Id = id(21)
    const file3Id = id(22)
    const file4Id = id(23)
    const file1Text = encodeBase64Url(file1Id)
    const file2Text = encodeBase64Url(file2Id)
    const file3Text = encodeBase64Url(file3Id)
    const file4Text = encodeBase64Url(file4Id)
    const subDirText = encodeBase64Url(subDirId)
    const trailingDirText = encodeBase64Url(trailingDirId)
    const payload = new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8])

    const pages = new Map([
      [encodeBase64Url(rootId), makeDirectoryPage(shareInstance, rootId, [
        Object.freeze({ kind: 'file', id: file1Id, idText: file1Text, name: 'one.txt', expectedSize: 8n }),
        Object.freeze({ kind: 'file', id: file2Id, idText: file2Text, name: 'two.txt', expectedSize: 8n }),
        Object.freeze({ kind: 'directory', id: subDirId, idText: subDirText, name: 'sub' }),
        Object.freeze({ kind: 'directory', id: trailingDirId, idText: trailingDirText, name: 'trailing' }),
      ])],
      [subDirText, makeDirectoryPage(shareInstance, subDirId, [
        Object.freeze({ kind: 'file', id: file3Id, idText: file3Text, name: 'three.txt', expectedSize: 8n }),
      ])],
      [trailingDirText, makeDirectoryPage(shareInstance, trailingDirId, [
        Object.freeze({ kind: 'file', id: file4Id, idText: file4Text, name: 'four.txt', expectedSize: 8n }),
      ])],
    ])
    const descriptor = Object.freeze({
      shareInstance,
      shareInstanceId: encodeBase64Url(shareInstance),
      syntheticRoot: rootId,
      syntheticRootId: encodeBase64Url(rootId),
    }) as unknown as ShareDescriptor

    const loadedDirs: string[] = []
    const catalog = Object.freeze({
      loadDirectory: async (directoryId: Uint8Array, options?: { readonly signal?: AbortSignal }) => {
        options?.signal?.throwIfAborted()
        const key = encodeBase64Url(directoryId)
        loadedDirs.push(key)
        return pages.get(key)!.committed
      },
      page: async (committed: CommittedDirectory, _index: number, signal?: AbortSignal) => {
        signal?.throwIfAborted()
        return pages.get(committed.directoryIdText)!.page
      },
    }) as unknown as CatalogClient

    const openedRevisions: string[] = []
    const releasedRevisions: string[] = []
    const releaseHold = gate()
    const revisions: RevisionReader = {
      open: async fileId => {
        const key = encodeBase64Url(fileId)
        openedRevisions.push(key)
        const rev = id(70)
        return Object.freeze({
          descriptor: Object.freeze({
            shareInstance,
            shareInstanceId: descriptor.shareInstanceId,
            fileId,
            fileIdText: key,
            fileRevision: rev,
            fileRevisionText: encodeBase64Url(rev),
            exactSize: 8n,
            geometry: new FileGeometry(8n, 4n),
          }) as FileRevisionDescriptor,
          leaseId: id(80),
          release: async () => {
            if (key === file2Text) {
              // Yield across microtasks while releasing file2's lease so any concurrent
              // discovery loop would race ensureDirectory if abort were deferred.
              await Promise.resolve()
              await releaseHold.promise
            }
            releasedRevisions.push(key)
          },
        })
      },
    }

    const broker: BlockRangeReader = {
      readRange: async function* (desc) {
        if (desc.fileIdText === file2Text) {
          yield Object.freeze({ offset: 0n, data: payload.subarray(0, 4) })
          queueMicrotask(() => releaseHold.resolve())
          throw new RemoteOperationError(protocolError('request_blocks', REVISION_CODE_DRIFT))
        }
        yield Object.freeze({ offset: 0n, data: payload })
      },
    }

    const { session, blobs } = openPortableZip('portable-midstream-drift', descriptor)
    const result = await new TransferJob({
      descriptor,
      catalog,
      selection: SELECTION,
      revisions,
      broker,
      output: session.transferOutput,
    }).run()

    assert(result.status !== 'detached')
    expect(result.status).toBe('source-changed')
    expect(result.completedFiles).toBe(1n)
    expect(result.failedFiles).toBe(1n)
    expect(result.failedDirectories).toBe(0n)
    expect(result.error).toBeUndefined()
    expect(openedRevisions).toEqual([file1Text, file2Text])
    expect(releasedRevisions).toEqual([file1Text, file2Text])
    expect(loadedDirs).not.toContain(trailingDirText)
    expect(result.contentWarning?.missingFiles).toEqual([{ path: ['two.txt'], reason: 'source-changed' }])

    const settled = await session.settle(result)
    await session.close()
    expect(blobs).toHaveLength(0)
    expect(settled.lifecycle.kind).toBe('failed')
    if (settled.lifecycle.kind === 'failed') {
      expect(settled.lifecycle.reason).toBe('source-changed')
      expect(settled.lifecycle.detail).toBeUndefined()
    }
    const presented = presentTask(retainedTaskFacts(retainedSnapshot(settled, false)))
    expect(presented.stage).toBe('failed')
    expect(presented.transition.reason).toBe('source_invalidated')
    expect(presented.details.some(line => /aborted|Cannot open|Cannot add|three\.txt|four\.txt/u.test(line))).toBe(false)
  })

  it('halts immediately on pre-open source-busy instead of downloading remaining portable ZIP members', async () => {
    const fixture = makeShareFixture([
      { seed: 30, name: 'one.txt', bytes: new Uint8Array([1, 2, 3, 4]) },
      { seed: 31, name: 'budget.xlsx', bytes: new Uint8Array([5, 6, 7, 8]) },
      { seed: 32, name: 'three.txt', bytes: new Uint8Array([9, 10, 11, 12]) },
      { seed: 33, name: 'four.txt', bytes: new Uint8Array([13, 14, 15, 16]) },
    ])
    const busyId = encodeBase64Url(id(31))
    const opened: string[] = []
    const { session, blobs } = openPortableZip('portable-preopen-busy', fixture.descriptor)

    const result = await new TransferJob({
      ...fixture,
      selection: SELECTION,
      revisions: {
        open: async fileId => {
          const key = encodeBase64Url(fileId)
          opened.push(key)
          if (key === busyId) {
            throw new RemoteRevisionError(protocolError('open_revisions', REVISION_CODE_BUSY))
          }
          return fixture.revisions.open(fileId)
        },
      },
      output: session.transferOutput,
    }).run()

    assert(result.status !== 'detached')
    expect(result.status).toBe('source-busy')
    expect(result.completedFiles).toBe(1n)
    expect(result.failedFiles).toBe(1n)
    expect(opened).toEqual([encodeBase64Url(id(30)), busyId])

    const settled = await session.settle(result)
    await session.close()
    expect(blobs).toHaveLength(0)
    expect(settled.lifecycle).toMatchObject({ kind: 'failed', reason: 'source-busy' })
    if (settled.lifecycle.kind === 'failed') expect(settled.lifecycle.detail).toBeUndefined()
  })

  it('preserves the real mid-stream transport failure and stops portable ZIP before later files', async () => {
    const fixture = makeShareFixture([
      { seed: 40, name: 'one.txt', bytes: new Uint8Array([1, 2, 3, 4]) },
      { seed: 41, name: 'two.txt', bytes: new Uint8Array([5, 6, 7, 8]) },
      { seed: 42, name: 'three.txt', bytes: new Uint8Array([9, 10, 11, 12]) },
    ])
    const failingId = encodeBase64Url(id(41))
    const cause = new Error('transport connection dropped')
    const { session, blobs } = openPortableZip('portable-transport-fail', fixture.descriptor)

    const result = await new TransferJob({
      ...fixture,
      selection: SELECTION,
      broker: {
        readRange: async function* (desc, leaseId, span, options) {
          if (desc.fileIdText === failingId) {
            yield Object.freeze({ offset: 0n, data: new Uint8Array([5, 6]) })
            throw cause
          }
          yield* fixture.broker.readRange(desc, leaseId, span, options)
        },
      },
      output: session.transferOutput,
    }).run()

    assert(result.status !== 'detached')
    expect(result.status).toBe('failed')
    expect(result.error).toBe(cause)
    expect(result.failedFiles).toBe(1n)
    expect(fixture.openedFileIds).toEqual([encodeBase64Url(id(40)), failingId])
    expect(result.contentWarning?.missingFiles).toEqual([{ path: ['two.txt'], reason: 'file-failed' }])

    const settled = await session.settle(result)
    await session.close()
    expect(blobs).toHaveLength(0)
    expect(settled.lifecycle).toMatchObject({
      kind: 'failed',
      reason: 'error',
      detail: 'transport connection dropped',
    })
  })

  it('halts portable ZIP immediately on directory discovery failure without scanning sibling subtrees', async () => {
    const shareInstance = id(1)
    const rootId = id(2)
    const badDirId = id(50)
    const siblingDirId = id(51)
    const siblingFileId = id(52)
    const badDirText = encodeBase64Url(badDirId)
    const siblingDirText = encodeBase64Url(siblingDirId)
    const siblingFileText = encodeBase64Url(siblingFileId)
    const pages = new Map([
      [encodeBase64Url(rootId), makeDirectoryPage(shareInstance, rootId, [
        Object.freeze({ kind: 'directory', id: badDirId, idText: badDirText, name: 'oversized' }),
        Object.freeze({ kind: 'directory', id: siblingDirId, idText: siblingDirText, name: 'sibling' }),
      ])],
      [siblingDirText, makeDirectoryPage(shareInstance, siblingDirId, [
        Object.freeze({ kind: 'file', id: siblingFileId, idText: siblingFileText, name: 'ok.txt', expectedSize: 4n }),
      ])],
    ])
    const descriptor = Object.freeze({
      shareInstance,
      shareInstanceId: encodeBase64Url(shareInstance),
      syntheticRoot: rootId,
      syntheticRootId: encodeBase64Url(rootId),
    }) as unknown as ShareDescriptor

    const queriedDirs: string[] = []
    const openSpy = vi.fn<RevisionReader['open']>()
    const catalogError = new CatalogPageStoreError('resource-limit', 'Catalog cache full')
    const { session, blobs } = openPortableZip('portable-dir-fail', descriptor)

    const result = await new TransferJob({
      descriptor,
      catalog: Object.freeze({
        loadDirectory: async (directoryId: Uint8Array) => {
          const key = encodeBase64Url(directoryId)
          queriedDirs.push(key)
          if (key === badDirText) throw catalogError
          return pages.get(key)!.committed
        },
        page: async (committed: CommittedDirectory) => pages.get(committed.directoryIdText)!.page,
      }) as unknown as CatalogClient,
      selection: SELECTION,
      revisions: { open: openSpy },
      broker: { readRange: async function* () { yield { offset: 0n, data: new Uint8Array(0) } } },
      output: session.transferOutput,
    }).run()

    assert(result.status !== 'detached')
    expect(result.status).toBe('failed')
    expect(result.error).toBe(catalogError)
    expect(queriedDirs).toEqual([encodeBase64Url(rootId), badDirText])
    expect(openSpy).not.toHaveBeenCalled()

    const settled = await session.settle(result)
    await session.close()
    expect(blobs).toHaveLength(0)
    expect(settled.lifecycle).toMatchObject({ kind: 'failed', reason: 'browser-catalog-limit' })
  })

  it('settles a user-stopped portable ZIP as cancelled even after an earlier file completed', async () => {
    const fixture = makeShareFixture([
      { seed: 60, name: 'one.txt', bytes: new Uint8Array([1, 2, 3, 4]) },
      { seed: 61, name: 'two.txt', bytes: new Uint8Array([5, 6, 7, 8]) },
      { seed: 62, name: 'three.txt', bytes: new Uint8Array([9, 10, 11, 12]) },
    ])
    const secondId = encodeBase64Url(id(61))
    const { session, blobs } = openPortableZip('portable-user-stop', fixture.descriptor)
    const jobRef: { current?: TransferJob } = {}

    const job = new TransferJob({
      ...fixture,
      selection: SELECTION,
      broker: {
        readRange: async function* (desc, leaseId, span, options) {
          if (desc.fileIdText === secondId) {
            yield Object.freeze({ offset: 0n, data: new Uint8Array([5, 6]) })
            jobRef.current?.stop()
            options?.signal?.throwIfAborted()
          }
          yield* fixture.broker.readRange(desc, leaseId, span, options)
        },
      },
      output: session.transferOutput,
    })
    jobRef.current = job

    const result = await job.run()
    assert(result.status !== 'detached')
    expect(result.status).toBe('stopped')
    expect(fixture.openedFileIds).toEqual([encodeBase64Url(id(60)), secondId])

    const settled = await session.settle(result)
    await session.close()
    expect(blobs).toHaveLength(0)
    expect(settled.lifecycle).toMatchObject({ kind: 'failed', reason: 'cancelled' })
    if (settled.lifecycle.kind === 'failed') expect(settled.lifecycle.detail).toBeUndefined()
  })

  it('cancels concurrent sibling workers cleanly without fabricating sibling failures on all-or-nothing outputs', async () => {
    const fixture = makeShareFixture([
      { seed: 70, name: 'changed.txt', bytes: new Uint8Array([1, 2, 3, 4]) },
      { seed: 71, name: 'sibling.txt', bytes: new Uint8Array([5, 6, 7, 8]) },
    ])
    const changedId = encodeBase64Url(id(70))
    const siblingStarted = gate()
    const siblingAbort = vi.fn(async () => undefined)

    const result = await new TransferJob({
      ...fixture,
      selection: SELECTION,
      broker: {
        readRange: async function* (desc, _leaseId, _span, options) {
          if (desc.fileIdText === changedId) {
            await siblingStarted.promise
            throw new RemoteOperationError(protocolError('request_blocks', REVISION_CODE_DRIFT))
          }
          siblingStarted.resolve()
          await new Promise<never>((_resolve, reject) => {
            const onAbort = () => reject(options?.signal?.reason)
            if (options?.signal?.aborted) onAbort()
            else options?.signal?.addEventListener('abort', onAbort, { once: true })
          })
          yield { offset: 0n, data: new Uint8Array(0) }
        },
      },
      output: {
        sessionId: 'all-or-nothing-concurrent',
        capabilities: { maxConcurrentFiles: 2, resumable: false, partialOutput: false },
        openFile: async req => outputFile(req.fileId === changedId ? {} : { abort: siblingAbort }),
      },
    }).run()

    expect(result.status).toBe('source-changed')
    expect(result.failedFiles).toBe(1n)
    expect(result.error).toBeUndefined()
    expect(result.contentWarning?.missingFiles).toEqual([{ path: ['changed.txt'], reason: 'source-changed' }])
    expect(siblingAbort).toHaveBeenCalledOnce()
  })
})

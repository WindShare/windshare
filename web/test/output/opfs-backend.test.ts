import { BlobReader, Uint8ArrayWriter, ZipReader, type FileEntry } from '@zip.js/zip.js'
import { assert, describe, expect, it } from 'vitest'
import { executeRetainedTaskAction, loadRetainedTasks } from '../../src/application/receive/retained'
import type { CatalogClient } from '../../src/catalog/client'
import type { CommittedDirectory } from '../../src/catalog/page-store'
import type { CatalogEntry, CatalogPage, ShareDescriptor } from '../../src/catalog/records'
import { SelectionPolicy } from '../../src/catalog/selection'
import { BlockLaneAttemptsError, type BlockRangeReader } from '../../src/content/broker'
import { BlockInactivityTimeoutError } from '../../src/content/scheduling/block-response-wait'
import { actionsForState, canSavePartialState } from '../../src/application/receive/admission'
import { FileGeometry, byteRange } from '../../src/content/geometry'
import type { FileRevisionDescriptor } from '../../src/content/records'
import type { RevisionReader } from '../../src/content/session-services'
import { encodeBase64Url } from '../../src/crypto/bytes'
import type { BrowserDownloadWindow } from '../../src/output/backends/browser-handoff'
import type { BrowserOutputWindow } from '../../src/output/backends/contract'
import {
  exportPartialOPFSZip,
  openNewOPFSSession,
  reopenOPFSSession,
  type OPFSEnvironment,
} from '../../src/output/backends/opfs'
import type { OutputTraceEvent } from '../../src/output/diagnostics'
import type { NativeObjectIO } from '../../src/output/native-object/contracts'
import type { ReceiveStore } from '../../src/output/store/database'
import type { ReceiveFileRecord, ReceiveTaskRecord } from '../../src/output/store/types'
import { FaultScope, SourceFaultCode, TransferFaultError, sourceFault } from '../../src/transfer/fault'
import { TransferJob } from '../../src/transfer/job'
import { selectionKey, selectionSpecFromPolicy } from '../../src/transfer/selection'
import { initialReceiveState } from '../../src/transfer/state'

class MemoryReceiveStore {
  readonly tasks = new Map<string, ReceiveTaskRecord>()
  readonly files = new Map<string, ReceiveFileRecord>()

  async putTask(record: ReceiveTaskRecord): Promise<void> {
    this.tasks.set(record.taskId, record)
  }

  async getTask(taskId: string): Promise<ReceiveTaskRecord | undefined> {
    return this.tasks.get(taskId)
  }

  async listTasks(): Promise<readonly ReceiveTaskRecord[]> {
    return [...this.tasks.values()]
  }

  async putFile(record: ReceiveFileRecord): Promise<void> {
    this.files.set(record.id, record)
  }

  async getFile(taskId: string, fileId: string): Promise<ReceiveFileRecord | undefined> {
    return this.files.get(`${taskId}:${fileId}`)
  }

  async listTaskFiles(taskId: string): Promise<readonly ReceiveFileRecord[]> {
    return [...this.files.values()].filter(file => file.taskId === taskId)
  }

  async deleteTask(taskId: string): Promise<void> {
    this.tasks.delete(taskId)
    for (const [id, file] of this.files) {
      if (file.taskId === taskId) this.files.delete(id)
    }
  }
}

class MemoryOPFSFileHandle {
  readonly kind = 'file' as const
  readonly name: string
  bytes = new Uint8Array(0)

  constructor(name: string) {
    this.name = name
  }

  async getFile(): Promise<File> {
    return new File([this.bytes], this.name)
  }

  createNativeIO(): NativeObjectIO {
    return {
      writeAt: async (offset: bigint, data: Uint8Array) => {
        const start = Number(offset)
        const end = start + data.byteLength
        if (end > this.bytes.byteLength) {
          const grown = new Uint8Array(end)
          grown.set(this.bytes)
          this.bytes = grown
        }
        this.bytes.set(data, start)
      },
      truncate: async (length: bigint) => {
        const len = Number(length)
        const resized = new Uint8Array(len)
        resized.set(this.bytes.subarray(0, Math.min(len, this.bytes.byteLength)))
        this.bytes = resized
      },
      size: async () => BigInt(this.bytes.byteLength),
      flush: async () => undefined,
      close: async () => undefined,
    }
  }
}

class MemoryOPFSDirectoryHandle {
  readonly kind = 'directory' as const
  readonly name: string
  readonly dirs = new Map<string, MemoryOPFSDirectoryHandle>()
  readonly files = new Map<string, MemoryOPFSFileHandle>()

  constructor(name: string) {
    this.name = name
  }

  async getDirectoryHandle(name: string, options?: { create?: boolean }): Promise<MemoryOPFSDirectoryHandle> {
    const existing = this.dirs.get(name)
    if (existing !== undefined) return existing
    if (!options?.create) throw new DOMException('missing dir', 'NotFoundError')
    const created = new MemoryOPFSDirectoryHandle(name)
    this.dirs.set(name, created)
    return created
  }

  async getFileHandle(name: string, options?: { create?: boolean }): Promise<MemoryOPFSFileHandle> {
    const existing = this.files.get(name)
    if (existing !== undefined) return existing
    if (!options?.create) throw new DOMException('missing file', 'NotFoundError')
    const created = new MemoryOPFSFileHandle(name)
    this.files.set(name, created)
    return created
  }

  async removeEntry(name: string): Promise<void> {
    if (!this.files.delete(name) && !this.dirs.delete(name)) {
      throw new DOMException('missing entry', 'NotFoundError')
    }
  }
}

function createMemoryOPFSEnv(): {
  readonly env: OPFSEnvironment
  readonly root: MemoryOPFSDirectoryHandle
} {
  const root = new MemoryOPFSDirectoryHandle('opfs-root')
  const env: OPFSEnvironment = Object.freeze({
    getRootDirectory: async () => root as unknown as FileSystemDirectoryHandle,
    openNativeObject: async (handle: FileSystemFileHandle) =>
      (handle as unknown as MemoryOPFSFileHandle).createNativeIO(),
  })
  return { env, root }
}

function createMockDownloadWindow(): {
  readonly windowPort: BrowserDownloadWindow
  readonly downloads: Array<{ readonly blob: Blob; readonly fileName: string }>
} {
  const downloads: Array<{ readonly blob: Blob; readonly fileName: string }> = []
  const blobs = new Map<string, Blob>()
  let seq = 0
  const windowPort: BrowserDownloadWindow = {
    URL: {
      createObjectURL: (blob: Blob | MediaSource) => {
        const url = `blob:mock-${++seq}`
        if (blob instanceof Blob) blobs.set(url, blob)
        return url
      },
      revokeObjectURL: () => undefined,
    },
    document: {
      createElement: () => {
        const anchor = {
          download: '',
          href: '',
          hidden: false,
          click: () => {
            const blob = blobs.get(anchor.href)
            if (blob !== undefined) downloads.push({ blob, fileName: anchor.download })
          },
          remove: () => undefined,
        }
        return anchor as unknown as HTMLAnchorElement
      },
      body: { append: () => undefined } as unknown as HTMLElement,
    } as unknown as Pick<Document, 'createElement' | 'body'>,
    setTimeout: (() => 0) as unknown as typeof setTimeout,
    clearTimeout: () => undefined,
  }
  return { windowPort, downloads }
}

async function readZipFiles(blob: Blob): Promise<Map<string, Uint8Array>> {
  const reader = new ZipReader(new BlobReader(blob))
  try {
    const entries = await reader.getEntries()
    const files = new Map<string, Uint8Array>()
    for (const entry of entries) {
      if (!entry.directory && 'getData' in entry) {
        files.set(entry.filename, await (entry as FileEntry).getData(new Uint8ArrayWriter()))
      }
    }
    return files
  } finally {
    await reader.close()
  }
}

function fixedId(seed: number): Uint8Array<ArrayBuffer> {
  return new Uint8Array(16).fill(seed)
}

function makeShareFixture(files: readonly {
  readonly seed: number
  readonly name: string
  readonly bytes: Uint8Array<ArrayBuffer>
  readonly revSeed?: number
  readonly missing?: boolean
}[]) {
  const shareInstance = fixedId(1)
  const rootId = fixedId(2)
  const entries: CatalogEntry[] = files.map(file => Object.freeze({
    kind: 'file' as const,
    id: fixedId(file.seed),
    idText: encodeBase64Url(fixedId(file.seed)),
    name: file.name,
    expectedSize: BigInt(file.bytes.byteLength),
  }))
  const committed: CommittedDirectory = Object.freeze({
    directoryId: rootId,
    directoryIdText: encodeBase64Url(rootId),
    generation: fixedId(3),
    generationText: encodeBase64Url(fixedId(3)),
    pageCount: 1,
    entryCount: entries.length,
    omittedCount: 0n,
    terminalCommitment: new Uint8Array(32),
  })
  const page: CatalogPage = Object.freeze({
    shareInstance,
    directoryId: rootId,
    directoryIdText: committed.directoryIdText,
    generation: committed.generation,
    generationText: committed.generationText,
    pageIndex: 0,
    terminal: true,
    previousCommitment: new Uint8Array(32),
    entries: Object.freeze(entries),
    omittedCount: 0n,
    objectCommitment: new Uint8Array(32),
    encodedBytes: 64,
  })
  const descriptor: ShareDescriptor = Object.freeze({
    shareInstance,
    shareInstanceId: encodeBase64Url(shareInstance),
    syntheticRoot: rootId,
    syntheticRootId: committed.directoryIdText,
  }) as unknown as ShareDescriptor
  const catalog = Object.freeze({
    loadDirectory: async () => committed,
    page: async () => page,
  }) as unknown as CatalogClient

  const byId = new Map(files.map(file => [encodeBase64Url(fixedId(file.seed)), file]))
  const openedFileIds: string[] = []
  const revisions: RevisionReader = {
    open: async (fileId: Uint8Array) => {
      const key = encodeBase64Url(fileId)
      openedFileIds.push(key)
      const file = byId.get(key)
      if (file === undefined || file.missing) {
        throw new TransferFaultError(
          sourceFault(FaultScope.FileLocal, SourceFaultCode.RevisionInvalidated),
          'source file deleted',
        )
      }
      const rev = fixedId(file.revSeed ?? (file.seed + 50))
      const revDesc: FileRevisionDescriptor = Object.freeze({
        shareInstance,
        shareInstanceId: descriptor.shareInstanceId,
        fileId: fixedId(file.seed),
        fileIdText: key,
        fileRevision: rev,
        fileRevisionText: encodeBase64Url(rev),
        exactSize: BigInt(file.bytes.byteLength),
        geometry: new FileGeometry(BigInt(file.bytes.byteLength), 8n),
      })
      return Object.freeze({
        descriptor: revDesc,
        leaseId: fixedId(99),
        release: async () => undefined,
      })
    },
  }
  const broker: BlockRangeReader = {
    readRange: async function* (revDesc, _leaseId, span) {
      const file = byId.get(revDesc.fileIdText)!
      yield Object.freeze({
        offset: span.start,
        data: file.bytes.subarray(Number(span.start), Number(span.end)),
      })
    },
  }
  return { descriptor, catalog, revisions, broker, openedFileIds }
}

describe('OPFS partial settlement and export', () => {
  it('preserves partial state, failure metadata, and exportable members when a later source file vanishes', async () => {
    const store = new MemoryReceiveStore()
    const { env, root } = createMemoryOPFSEnv()
    const { windowPort, downloads } = createMockDownloadWindow()
    const traces: OutputTraceEvent[] = []
    const payloadA = new TextEncoder().encode('completed-member-bytes')
    const fixture = makeShareFixture([
      { seed: 10, name: 'a.txt', bytes: payloadA },
      { seed: 11, name: 'b.txt', bytes: new Uint8Array(64), missing: true },
    ])
    const selection = new SelectionPolicy(true).snapshot()
    const spec = selectionSpecFromPolicy(fixture.descriptor.shareInstanceId, fixture.descriptor.syntheticRootId, selection)

    const session = await openNewOPFSSession({
      task: {
        taskId: 'opfs-partial-missing',
        selection: spec,
        selectionKey: selectionKey(spec),
        artifact: Object.freeze({ kind: 'zip-archive', name: 'Bundle.zip' }),
        lifecycle: initialReceiveState('opfs-partial-missing'),
      },
      store: store as unknown as ReceiveStore,
      env,
      windowPort,
      diagnostics: { backend: 'origin_private', trace: { current: event => traces.push(event) } },
    })

    const result = await new TransferJob({
      descriptor: fixture.descriptor,
      catalog: fixture.catalog,
      selection,
      revisions: fixture.revisions,
      broker: fixture.broker,
      output: session.transferOutput,
    }).run()
    assert(result.status === 'partial')

    const settled = await session.settle(result)
    await session.close()

    expect(settled.lifecycle.kind).toBe('partial')
    if (settled.lifecycle.kind !== 'partial') throw new Error('expected partial lifecycle')
    expect(settled.lifecycle.reason).toBe('failures')
    expect(settled.lifecycle.savedFiles).toBe(1n)
    expect(settled.lifecycle.failedFiles).toBe(1n)
    expect(settled.lifecycle.contentWarning?.completedFileCount).toBe(1n)
    expect(settled.lifecycle.contentWarning?.selectedFileCount).toBe(2n)
    expect(settled.lifecycle.contentWarning?.missingFiles).toEqual([
      { path: ['b.txt'], reason: 'source-changed' },
    ])
    expect(settled.sourceRevisionFailures?.files.map(file => file.path.join('/'))).toEqual(['b.txt'])
    expect(traces.at(-1)).toEqual({
      eventName: 'settlement',
      payload: {
        backend: 'origin_private',
        task_id: 'opfs-partial-missing',
        job_status: 'partial',
        lifecycle: 'partial',
        completed_files: '1',
        failed_files: '1',
      },
    })

    const retainedEnv = {
      store: store as unknown as ReceiveStore,
      windowPort: windowPort as unknown as BrowserOutputWindow,
      opfsEnv: env,
    }
    const retained = await loadRetainedTasks(retainedEnv, null)
    expect(retained).toHaveLength(1)
    expect(retained[0]?.canSavePartial).toBe(true)
    expect(retained[0]?.actions).toEqual(['save-partial', 'discard'])

    // Forgetting must be rejected so OPFS bytes cannot be orphaned on disk.
    await expect(executeRetainedTaskAction(retainedEnv, 'opfs-partial-missing', 'forget'))
      .rejects.toThrow(/explicitly/u)

    const exported = await executeRetainedTaskAction(retainedEnv, 'opfs-partial-missing', 'save-partial')
    expect(exported.kind).toBe('completed')
    expect(downloads).toHaveLength(1)
    expect(downloads[0]?.fileName).toBe('Bundle-partial.zip')
    const zipEntries = await readZipFiles(downloads[0]!.blob)
    expect([...zipEntries.keys()]).toEqual(['a.txt'])
    expect(zipEntries.get('a.txt')).toEqual(payloadA)

    await executeRetainedTaskAction(retainedEnv, 'opfs-partial-missing', 'discard')
    expect(await store.getTask('opfs-partial-missing')).toBeUndefined()
    expect(root.dirs.get('windshare-tasks')?.files.has('opfs-partial-missing.part')).toBe(false)
  })

  it('preserves completed OPFS ZIP members on stop and on resumed source-change without slot bloat', async () => {
    const store = new MemoryReceiveStore()
    const { env } = createMemoryOPFSEnv()
    const { windowPort } = createMockDownloadWindow()
    const payloadA = new TextEncoder().encode('keep-me')
    const largeReservedSize = 64n * 1024n
    const spec = { shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] }

    // 1) Stopped task with one completed file and one large pre-allocated unfinished slot.
    const stoppedSession = await openNewOPFSSession({
      task: {
        taskId: 'opfs-partial-stop',
        selection: spec,
        selectionKey: 'sel-stop',
        artifact: Object.freeze({ kind: 'zip-archive', name: 'Photos.zip' }),
        lifecycle: initialReceiveState('opfs-partial-stop'),
      },
      store: store as unknown as ReceiveStore,
      env,
      windowPort,
    })
    const fileA = await stoppedSession.transferOutput.openFile({
      fileId: 'fa', path: ['a.txt'], sourcePath: ['a.txt'], revision: 'r1', exactSize: BigInt(payloadA.byteLength),
    }, new AbortController().signal)
    await fileA.writeAt(0n, payloadA)
    await fileA.commit([byteRange(0n, BigInt(payloadA.byteLength))])

    const fileB = await stoppedSession.transferOutput.openFile({
      fileId: 'fb', path: ['huge.bin'], sourcePath: ['huge.bin'], revision: 'r2', exactSize: largeReservedSize,
    }, new AbortController().signal)
    await fileB.writeAt(0n, new Uint8Array([1, 2, 3, 4]))
    await fileB.abort()

    const stoppedTask = await stoppedSession.settle({
      status: 'partial',
      completedFiles: 1n,
      completedBytes: BigInt(payloadA.byteLength),
      discoveredFiles: 2n,
      discoveredBytes: BigInt(payloadA.byteLength) + largeReservedSize,
      discoveryComplete: false,
      failedFiles: 0n,
      failedDirectories: 0n,
    })
    await stoppedSession.close()

    expect(stoppedTask.lifecycle.kind).toBe('partial')
    if (stoppedTask.lifecycle.kind !== 'partial') throw new Error('expected partial lifecycle')
    expect(stoppedTask.lifecycle.reason).toBe('stopped')
    expect(stoppedTask.lifecycle.savedFiles).toBe(1n)

    const partialBlob = await exportPartialOPFSZip(stoppedTask, store as unknown as ReceiveStore, env, windowPort)
    // Exporting via buildPartialZipBlob must exclude the 64 KiB failed slot rather than padding the ZIP.
    expect(partialBlob.size).toBeLessThan(1024)
    const stoppedEntries = await readZipFiles(partialBlob)
    expect(stoppedEntries.get('a.txt')).toEqual(payloadA)

    // 2) Resumed task whose second run fails with `source-changed` before re-discovering `a.txt`.
    const pausedSession = await openNewOPFSSession({
      task: {
        taskId: 'opfs-resume-source-changed',
        selection: spec,
        selectionKey: 'sel-resume',
        artifact: Object.freeze({ kind: 'zip-archive', name: 'Resume.zip' }),
        lifecycle: initialReceiveState('opfs-resume-source-changed'),
      },
      store: store as unknown as ReceiveStore,
      env,
      windowPort,
    })
    const resumedA = await pausedSession.transferOutput.openFile({
      fileId: 'fa', path: ['a.txt'], sourcePath: ['a.txt'], revision: 'r1', exactSize: BigInt(payloadA.byteLength),
    }, new AbortController().signal)
    await resumedA.writeAt(0n, payloadA)
    await resumedA.commit([byteRange(0n, BigInt(payloadA.byteLength))])
    const pausedTask = await pausedSession.settle({
      status: 'paused',
      pauseReason: 'user',
      completedFiles: 1n,
      completedBytes: BigInt(payloadA.byteLength),
      discoveredFiles: 2n,
      discoveredBytes: 100n,
      discoveryComplete: true,
      failedFiles: 0n,
      failedDirectories: 0n,
    })
    await pausedSession.close()

    const reopened = await reopenOPFSSession({
      task: pausedTask,
      store: store as unknown as ReceiveStore,
      env,
      windowPort,
    })
    const changedTask = await reopened.settle({
      status: 'source-changed',
      completedFiles: 0n,
      completedBytes: 0n,
      discoveredFiles: 1n,
      discoveredBytes: 50n,
      discoveryComplete: false,
      failedFiles: 1n,
      failedDirectories: 0n,
      sourceRevisionFailures: {
        shareInstance: 's1',
        count: 1n,
        files: [{ entryId: 'fb', path: ['b.txt'], sourcePath: ['b.txt'], reason: 'source-changed' }],
      },
    })
    await reopened.close()

    expect(changedTask.lifecycle.kind).toBe('partial')
    if (changedTask.lifecycle.kind !== 'partial') throw new Error('expected partial lifecycle')
    expect(changedTask.lifecycle.savedFiles).toBe(1n)
    expect(changedTask.lifecycle.reason).toBe('failures')
    expect(changedTask.sourceRevisionFailures?.count).toBe(1n)
  })

  it('settles zero-completed OPFS tasks to failed and completes single-file OPFS if already committed', async () => {
    const store = new MemoryReceiveStore()
    const { env } = createMemoryOPFSEnv()
    const { windowPort, downloads } = createMockDownloadWindow()
    const spec = { shareInstance: 's1', syntheticRoot: 'r1', defaultSelected: true, rules: [] }

    const emptySession = await openNewOPFSSession({
      task: {
        taskId: 'opfs-zero-changed',
        selection: spec,
        selectionKey: 'sel-zero',
        artifact: Object.freeze({ kind: 'zip-archive', name: 'Empty.zip' }),
        lifecycle: initialReceiveState('opfs-zero-changed'),
      },
      store: store as unknown as ReceiveStore,
      env,
      windowPort,
    })
    const failed = await emptySession.settle({
      status: 'source-changed',
      completedFiles: 0n,
      completedBytes: 0n,
      discoveredFiles: 1n,
      discoveredBytes: 10n,
      discoveryComplete: true,
      failedFiles: 1n,
      failedDirectories: 0n,
    })
    await emptySession.close()
    expect(failed.lifecycle.kind).toBe('failed')
    if (failed.lifecycle.kind === 'failed') expect(failed.lifecycle.reason).toBe('source-changed')

    const singlePayload = new TextEncoder().encode('single-file-complete')
    const singleSession = await openNewOPFSSession({
      task: {
        taskId: 'opfs-single-late-stop',
        selection: spec,
        selectionKey: 'sel-single',
        artifact: Object.freeze({
          kind: 'original-file',
          name: 'video.bin',
          singleFileId: 'f1',
          singleFilePath: ['video.bin'],
          singleFileSize: BigInt(singlePayload.byteLength),
        }),
        lifecycle: initialReceiveState('opfs-single-late-stop'),
      },
      store: store as unknown as ReceiveStore,
      env,
      windowPort,
    })
    const singleFile = await singleSession.transferOutput.openFile({
      fileId: 'f1', path: ['video.bin'], sourcePath: ['video.bin'], revision: 'r1', exactSize: BigInt(singlePayload.byteLength),
    }, new AbortController().signal)
    await singleFile.writeAt(0n, singlePayload)
    await singleFile.commit([byteRange(0n, BigInt(singlePayload.byteLength))])

    const settledSingle = await singleSession.settle({
      status: 'partial',
      completedFiles: 1n,
      completedBytes: BigInt(singlePayload.byteLength),
      discoveredFiles: 1n,
      discoveredBytes: BigInt(singlePayload.byteLength),
      discoveryComplete: true,
      failedFiles: 0n,
      failedDirectories: 0n,
    })
    await singleSession.close()
    expect(settledSingle.lifecycle.kind).toBe('download-started')
    expect(downloads.at(-1)?.fileName).toBe('video.bin')
  })
})

describe('OPFS resume completion skip', () => {
  it('skips remote revision opens for completed OPFS ZIP members on resume and finalizes even if a completed source vanished', async () => {
    const store = new MemoryReceiveStore()
    const { env } = createMemoryOPFSEnv()
    const { windowPort, downloads } = createMockDownloadWindow()
    const payloadA = new TextEncoder().encode('alpha-done')
    const payloadB = new TextEncoder().encode('beta-done-too')
    const payloadC = new TextEncoder().encode('gamma-pending')
    const idA = encodeBase64Url(fixedId(21))
    const idB = encodeBase64Url(fixedId(22))
    const idC = encodeBase64Url(fixedId(23))
    const fixture = makeShareFixture([
      { seed: 21, name: 'a.txt', bytes: payloadA, missing: true },
      { seed: 22, name: 'b.txt', bytes: payloadB },
      { seed: 23, name: 'c.txt', bytes: payloadC },
    ])
    const selection = new SelectionPolicy(true).snapshot()
    const spec = selectionSpecFromPolicy(fixture.descriptor.shareInstanceId, fixture.descriptor.syntheticRootId, selection)

    const initial = await openNewOPFSSession({
      task: {
        taskId: 'opfs-zip-resume-skip',
        selection: spec,
        selectionKey: selectionKey(spec),
        artifact: Object.freeze({ kind: 'zip-archive', name: 'Bundle.zip' }),
        lifecycle: initialReceiveState('opfs-zip-resume-skip'),
      },
      store: store as unknown as ReceiveStore,
      env,
      windowPort,
    })
    expect(initial.transferOutput.checkCompleted).toBeUndefined()

    for (const [fileId, name, data] of [[idA, 'a.txt', payloadA], [idB, 'b.txt', payloadB]] as const) {
      const writer = await initial.transferOutput.openFile({
        fileId, path: [name], sourcePath: [name], revision: `r-${name}`, exactSize: BigInt(data.byteLength),
      }, new AbortController().signal)
      await writer.writeAt(0n, data)
      await writer.commit([byteRange(0n, BigInt(data.byteLength))])
    }
    const paused = await initial.settle({
      status: 'paused',
      pauseReason: 'user',
      completedFiles: 2n,
      completedBytes: BigInt(payloadA.byteLength + payloadB.byteLength),
      discoveredFiles: 3n,
      discoveredBytes: BigInt(payloadA.byteLength + payloadB.byteLength + payloadC.byteLength),
      discoveryComplete: true,
      failedFiles: 0n,
      failedDirectories: 0n,
    })
    await initial.close()

    const reopened = await reopenOPFSSession({
      task: paused,
      store: store as unknown as ReceiveStore,
      env,
      windowPort,
    })
    const result = await new TransferJob({
      descriptor: fixture.descriptor,
      catalog: fixture.catalog,
      selection,
      revisions: fixture.revisions,
      broker: fixture.broker,
      output: reopened.transferOutput,
    }).run()
    assert(result.status !== 'detached')
    const settled = await reopened.settle(result)
    await reopened.close()

    expect(result.status).toBe('completed')
    expect(result.completedFiles).toBe(3n)
    expect(fixture.openedFileIds).toEqual([idC])
    expect(settled.lifecycle.kind).toBe('download-started')
    expect(downloads).toHaveLength(1)
    const entries = await readZipFiles(downloads[0]!.blob)
    expect(entries.get('a.txt')).toEqual(payloadA)
    expect(entries.get('b.txt')).toEqual(payloadB)
    expect(entries.get('c.txt')).toEqual(payloadC)

    // Single-file OPFS also verifies physical size before skipping remote open on resume.
    const singleFileId = encodeBase64Url(fixedId(24))
    const singleBytes = new TextEncoder().encode('single-done-bytes')
    const singleFixture = makeShareFixture([{ seed: 24, name: 'clip.bin', bytes: singleBytes, missing: true }])
    const singleSpec = selectionSpecFromPolicy(
      singleFixture.descriptor.shareInstanceId,
      singleFixture.descriptor.syntheticRootId,
      selection,
    )
    const singleNew = await openNewOPFSSession({
      task: {
        taskId: 'opfs-single-resume-skip',
        selection: singleSpec,
        selectionKey: selectionKey(singleSpec),
        artifact: Object.freeze({
          kind: 'original-file',
          name: 'clip.bin',
          singleFileId,
          singleFilePath: ['clip.bin'],
          singleFileSize: BigInt(singleBytes.byteLength),
        }),
        lifecycle: initialReceiveState('opfs-single-resume-skip'),
      },
      store: store as unknown as ReceiveStore,
      env,
      windowPort,
    })
    const singleWriter = await singleNew.transferOutput.openFile({
      fileId: singleFileId, path: ['clip.bin'], sourcePath: ['clip.bin'],
      revision: 'r-clip', exactSize: BigInt(singleBytes.byteLength),
    }, new AbortController().signal)
    await singleWriter.writeAt(0n, singleBytes)
    await singleWriter.commit([byteRange(0n, BigInt(singleBytes.byteLength))])
    const singlePaused = await singleNew.settle({
      status: 'paused', pauseReason: 'user',
      completedFiles: 1n, completedBytes: BigInt(singleBytes.byteLength),
      discoveredFiles: 1n, discoveredBytes: BigInt(singleBytes.byteLength),
      discoveryComplete: true, failedFiles: 0n, failedDirectories: 0n,
    })
    await singleNew.close()

    const singleReopened = await reopenOPFSSession({
      task: singlePaused, store: store as unknown as ReceiveStore, env, windowPort,
    })
    const singleResult = await new TransferJob({
      descriptor: singleFixture.descriptor,
      catalog: singleFixture.catalog,
      selection,
      revisions: singleFixture.revisions,
      broker: singleFixture.broker,
      output: singleReopened.transferOutput,
    }).run()
    await singleReopened.close()

    expect(singleResult.status).toBe('completed')
    expect(singleFixture.openedFileIds).toEqual([])
  })

  it('keeps multi-file OPFS ZIP resumable with both continue and save-partial after a network interruption', async () => {
    const store = new MemoryReceiveStore()
    const { env } = createMemoryOPFSEnv()
    const { windowPort, downloads } = createMockDownloadWindow()
    const smallPayload = new TextEncoder().encode('small-done')
    const largePayload = new TextEncoder().encode('first-half-second-half')
    const fixture = makeShareFixture([
      { seed: 31, name: 'readme.txt', bytes: smallPayload },
      { seed: 32, name: 'movie.bin', bytes: largePayload },
    ])
    const selection = new SelectionPolicy(true).snapshot()
    const spec = selectionSpecFromPolicy(
      fixture.descriptor.shareInstanceId,
      fixture.descriptor.syntheticRootId,
      selection,
    )

    let interruptedOnce = false
    const interruptingBroker: BlockRangeReader = {
      readRange: async function* (desc, leaseId, span, options) {
        if (!interruptedOnce && desc.exactSize === BigInt(largePayload.byteLength)) {
          interruptedOnce = true
          yield Object.freeze({ offset: 0n, data: largePayload.subarray(0, 10) })
          throw new BlockLaneAttemptsError([
            new BlockInactivityTimeoutError('receiving_fragments', 15_000, 0, { laneId: 1 }),
          ])
        }
        yield* fixture.broker.readRange(desc, leaseId, span, options)
      },
    }

    const first = await openNewOPFSSession({
      task: {
        taskId: 'opfs-zip-interrupted',
        selection: spec,
        selectionKey: selectionKey(spec),
        artifact: Object.freeze({ kind: 'zip-archive', name: 'bundle.zip' }),
        lifecycle: initialReceiveState('opfs-zip-interrupted'),
      },
      store: store as unknown as ReceiveStore,
      env,
      windowPort,
    })
    const firstResult = await new TransferJob({
      descriptor: fixture.descriptor,
      catalog: fixture.catalog,
      selection,
      revisions: fixture.revisions,
      broker: interruptingBroker,
      output: first.transferOutput,
    }).run()
    assert(firstResult.status !== 'detached')
    const pausedRecord = await first.settle(firstResult)
    await first.close()

    expect(firstResult.status).toBe('paused')
    expect(firstResult.pauseReason).toBe('interrupted')
    expect(pausedRecord.lifecycle.kind).toBe('paused')
    const canSavePartial = canSavePartialState(pausedRecord.lifecycle, 'opfs', true)
    expect(canSavePartial).toBe(true)
    expect(actionsForState(pausedRecord.lifecycle, 'opfs', canSavePartial))
      .toEqual(['continue', 'save-partial', 'discard'])

    const reopened = await reopenOPFSSession({
      task: pausedRecord,
      store: store as unknown as ReceiveStore,
      env,
      windowPort,
    })
    const resumedResult = await new TransferJob({
      descriptor: fixture.descriptor,
      catalog: fixture.catalog,
      selection,
      revisions: fixture.revisions,
      broker: fixture.broker,
      output: reopened.transferOutput,
    }).run()
    assert(resumedResult.status !== 'detached')
    const settled = await reopened.settle(resumedResult)
    await reopened.close()

    expect(resumedResult.status).toBe('completed')
    expect(settled.lifecycle.kind).toBe('download-started')
    const entries = await readZipFiles(downloads[0]!.blob)
    expect(entries.get('readme.txt')).toEqual(smallPayload)
    expect(entries.get('movie.bin')).toEqual(largePayload)
  })
})

describe('OPFS abandoned and failed output', () => {
  async function zipFixture(beforeFlush?: () => void) {
    const memory = new MemoryReceiveStore(), store = memory as unknown as ReceiveStore
    const { env } = createMemoryOPFSEnv()
    const { windowPort } = createMockDownloadWindow()
    const session = await openNewOPFSSession({
      task: {
        taskId: 'zip-outage', selection: { shareInstance: 'share', syntheticRoot: 'root', defaultSelected: true, rules: [] },
        selectionKey: 'key', artifact: { kind: 'zip-archive', name: 'Folder.zip' }, lifecycle: initialReceiveState('zip-outage'),
      },
      store, windowPort, env: { ...env, openNativeObject: async handle => {
        const io = await env.openNativeObject!(handle)
        return { ...io, flush: async () => {
          beforeFlush?.()
          await io.flush()
        } }
      } },
    })
    return { session, store, env, windowPort }
  }

  it('recovers a ZIP with only directory rows as zero downloadable files', async () => {
    const { session, store, windowPort } = await zipFixture()
    await session.transferOutput.ensureDirectory!({ directoryId: 'd', path: ['empty'] }, new AbortController().signal)
    await session.close()
    const [task] = await loadRetainedTasks({ store, windowPort }, null)
    expect(task?.lifecycle).toMatchObject({
      kind: 'paused', reason: 'interrupted', completedFiles: 0n, discoveredFiles: 0n, retainedBytes: 0n,
    })
    expect(task?.canSavePartial).toBe(false)
  })

  it.each([
    [new DOMException('full', 'QuotaExceededError'), { kind: 'paused', reason: 'storage-full', retainedBytes: 2n }],
    [new DOMException('denied', 'NotAllowedError'), { kind: 'paused', reason: 'permission-needed', retainedBytes: 2n }],
    [new Error('final flush failed'), { kind: 'partial', reason: 'failures', savedFiles: 1n }],
  ] as const)('preserves committed members when sealing fails with %s', async (cause, expected) => {
    let sealing = false
    const { session, store, env, windowPort } = await zipFixture(() => { if (sealing) throw cause })
    const file = await session.transferOutput.openFile({
      fileId: 'a', path: ['a.txt'], sourcePath: ['a.txt'], revision: 'r', exactSize: 2n,
    }, new AbortController().signal)
    const bytes = Uint8Array.of(1, 2)
    await file.writeAt(0n, bytes)
    await file.commit([byteRange(0n, 2n)])
    sealing = true
    const task = await session.settle({
      status: 'completed', completedFiles: 1n, completedBytes: 2n, discoveredFiles: 1n, discoveredBytes: 2n,
      discoveryComplete: true, failedFiles: 0n, failedDirectories: 0n,
    })
    await session.close()
    expect((await store.getTask(task.taskId))?.lifecycle).toMatchObject(expected)
    const exported = await exportPartialOPFSZip(task, store, env, windowPort)
    expect((await readZipFiles(exported)).get('a.txt')).toEqual(bytes)
  })

  it('ends all stalled ZIP readers when the shared output fails during a disconnect flush', async () => {
    let writers = 0
    let ready!: () => void
    const waiting = new Promise<void>(resolve => { ready = resolve })
    const { session } = await zipFixture(() => {
      if (writers === 2) throw new Error('shared object flush failed')
    })
    const fixture = makeShareFixture([
      { seed: 10, name: 'a.txt', bytes: new Uint8Array(8) }, { seed: 11, name: 'b.txt', bytes: new Uint8Array(8) },
    ])
    const job = new TransferJob({
      ...fixture, selection: new SelectionPolicy(true).snapshot(), output: session.transferOutput,
      broker: { readRange: async function* (_revision, _lease, _span, options) {
        yield { offset: 0n, data: Uint8Array.of(42) }
        if (++writers === 2) ready()
        const signal = options!.signal!
        await new Promise<void>((_, reject) => signal.addEventListener('abort', () => reject(signal.reason), { once: true }))
      } },
    })
    const run = job.run()
    await waiting
    expect(await job.checkpointActive(new AbortController().signal)).toMatchObject({ allReceivedSaved: false, failedFiles: 1 })
    const result = await run
    assert(result.status === 'failed')
    expect(result.error).toMatchObject({ fault: { scope: 'job' }, cause: { message: 'shared object flush failed' } })
    expect((await session.settle(result)).lifecycle).toMatchObject({ kind: 'failed', reason: 'error' })
    await session.close()
  })
})

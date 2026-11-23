import { BlobReader, Uint8ArrayWriter, ZipReader, type FileEntry } from '@zip.js/zip.js'
import { describe, expect, it } from 'vitest'
import { executeRetainedTaskAction, loadRetainedTasks } from '../../src/application/receive/retained'
import type { CatalogClient } from '../../src/catalog/client'
import type { CommittedDirectory } from '../../src/catalog/page-store'
import type { CatalogEntry, CatalogPage, ShareDescriptor } from '../../src/catalog/records'
import { SelectionPolicy } from '../../src/catalog/selection'
import type { BlockRangeReader } from '../../src/content/broker'
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
  const revisions: RevisionReader = {
    open: async (fileId: Uint8Array) => {
      const key = encodeBase64Url(fileId)
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
  return { descriptor, catalog, revisions, broker }
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
    expect(result.status).toBe('partial')

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

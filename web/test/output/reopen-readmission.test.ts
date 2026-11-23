import { BlobReader, Uint8ArrayWriter, ZipReader, type FileEntry } from '@zip.js/zip.js'
import { describe, expect, it } from 'vitest'
import type { CatalogClient } from '../../src/catalog/client'
import type { CommittedDirectory } from '../../src/catalog/page-store'
import type { CatalogEntry, CatalogPage, ShareDescriptor } from '../../src/catalog/records'
import { SelectionPolicy } from '../../src/catalog/selection'
import type { BlockRangeReader } from '../../src/content/broker'
import { FileGeometry } from '../../src/content/geometry'
import type { FileRevisionDescriptor } from '../../src/content/records'
import {
  RevisionReopenedDuringRecoveryError,
  type RevisionReader,
} from '../../src/content/session-services'
import { encodeBase64Url } from '../../src/crypto/bytes'
import type { BrowserDownloadWindow } from '../../src/output/backends/browser-handoff'
import { openNewFSASession } from '../../src/output/backends/fsa'
import { openNewOPFSSession, type OPFSEnvironment } from '../../src/output/backends/opfs'
import { openPortableSession } from '../../src/output/backends/portable'
import type { NativeObjectIO } from '../../src/output/native-object/contracts'
import type { ReceiveStore } from '../../src/output/store/database'
import type { ReceiveFileRecord, ReceiveTaskRecord } from '../../src/output/store/types'
import { TransferJob } from '../../src/transfer/job'
import { selectionKey, selectionSpecFromPolicy } from '../../src/transfer/selection'
import { initialReceiveState } from '../../src/transfer/state'

function fixedId(seed: number): Uint8Array<ArrayBuffer> {
  return new Uint8Array(16).fill(seed)
}

class MemoryReceiveStore {
  readonly tasks = new Map<string, ReceiveTaskRecord>()
  readonly files = new Map<string, ReceiveFileRecord>()

  async putTask(record: ReceiveTaskRecord): Promise<void> { this.tasks.set(record.taskId, record) }
  async getTask(taskId: string): Promise<ReceiveTaskRecord | undefined> { return this.tasks.get(taskId) }
  async listTasks(): Promise<readonly ReceiveTaskRecord[]> { return [...this.tasks.values()] }
  async putFile(record: ReceiveFileRecord): Promise<void> { this.files.set(record.id, record) }
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

class FakeFSAFileHandle {
  readonly kind = 'file' as const
  readonly name: string
  bytes = new Uint8Array(0)
  readonly writableCalls: boolean[] = []
  abortCalls = 0
  lastModified = Date.now()

  constructor(name: string) { this.name = name }

  async getFile(): Promise<File> {
    return new File([this.bytes], this.name, { lastModified: this.lastModified })
  }

  async createWritable(options?: { keepExistingData?: boolean }) {
    const keepExistingData = Boolean(options?.keepExistingData)
    this.writableCalls.push(keepExistingData)
    let buffer = keepExistingData ? this.bytes.slice() : new Uint8Array(0)
    return {
      write: async (chunk: { type: 'write'; position: number; data: Uint8Array }) => {
        const end = chunk.position + chunk.data.byteLength
        if (end > buffer.byteLength) {
          const grown = new Uint8Array(end)
          grown.set(buffer)
          buffer = grown
        }
        buffer.set(chunk.data, chunk.position)
      },
      close: async () => {
        this.bytes = buffer
        this.lastModified = Math.max(Date.now(), this.lastModified + 1)
      },
      abort: async () => { this.abortCalls += 1 },
    }
  }
}

class FakeFSADirectoryHandle {
  readonly kind = 'directory' as const
  readonly name: string
  readonly children = new Map<string, FakeFSADirectoryHandle | FakeFSAFileHandle>()

  constructor(name: string) { this.name = name }
  async queryPermission(): Promise<'granted'> { return 'granted' }
  async requestPermission(): Promise<'granted'> { return 'granted' }

  async getDirectoryHandle(name: string, options?: { create?: boolean }): Promise<FakeFSADirectoryHandle> {
    const existing = this.children.get(name)
    if (existing instanceof FakeFSADirectoryHandle) return existing
    if (existing instanceof FakeFSAFileHandle) throw new DOMException('type mismatch', 'TypeMismatchError')
    if (!options?.create) throw new DOMException('missing', 'NotFoundError')
    const created = new FakeFSADirectoryHandle(name)
    this.children.set(name, created)
    return created
  }

  async getFileHandle(name: string, options?: { create?: boolean }): Promise<FakeFSAFileHandle> {
    const existing = this.children.get(name)
    if (existing instanceof FakeFSAFileHandle) return existing
    if (existing instanceof FakeFSADirectoryHandle) throw new DOMException('type mismatch', 'TypeMismatchError')
    if (!options?.create) throw new DOMException('missing', 'NotFoundError')
    const created = new FakeFSAFileHandle(name)
    this.children.set(name, created)
    return created
  }

  async removeEntry(name: string): Promise<void> { this.children.delete(name) }
}

function createMemoryOPFSEnv() {
  let bytes = new Uint8Array(0)
  const fileHandle = {
    kind: 'file' as const,
    name: 'task.part',
    getFile: async () => new File([bytes], 'task.part'),
  }
  const io: NativeObjectIO = {
    writeAt: async (offset, data) => {
      const start = Number(offset)
      const end = start + data.byteLength
      if (end > bytes.byteLength) {
        const grown = new Uint8Array(end)
        grown.set(bytes)
        bytes = grown
      }
      bytes.set(data, start)
    },
    truncate: async length => {
      const len = Number(length)
      const resized = new Uint8Array(len)
      resized.set(bytes.subarray(0, Math.min(len, bytes.byteLength)))
      bytes = resized
    },
    size: async () => BigInt(bytes.byteLength),
    flush: async () => undefined,
    close: async () => undefined,
  }
  const dirHandle = {
    getDirectoryHandle: async () => dirHandle,
    getFileHandle: async () => fileHandle,
  } as unknown as FileSystemDirectoryHandle
  const env: OPFSEnvironment = Object.freeze({
    getRootDirectory: async () => dirHandle,
    openNativeObject: async () => io,
  })
  return { env }
}

function createMockDownloadWindow() {
  const blobs: Blob[] = []
  const windowPort: BrowserDownloadWindow = {
    URL: {
      createObjectURL: (blob: Blob | MediaSource) => {
        if (blob instanceof Blob) blobs.push(blob)
        return `blob:reopen-${blobs.length}`
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

function createReopenShareFixture(name: string, payload: Uint8Array<ArrayBuffer>) {
  const shareInstance = fixedId(1)
  const rootId = fixedId(2)
  const generation = fixedId(3)
  const fileId = fixedId(20)
  const fileIdText = encodeBase64Url(fileId)
  const rev1 = fixedId(41)
  const rev2 = fixedId(42)
  const sourceMetadata = encodeBase64Url(new Uint8Array(32).fill(0x7c))
  const exactSize = BigInt(payload.byteLength)

  const desc1: FileRevisionDescriptor = Object.freeze({
    shareInstance,
    shareInstanceId: encodeBase64Url(shareInstance),
    fileId,
    fileIdText,
    fileRevision: rev1,
    fileRevisionText: encodeBase64Url(rev1),
    exactSize,
    geometry: new FileGeometry(exactSize, 8n),
    sourceMetadata,
  })
  const desc2: FileRevisionDescriptor = Object.freeze({
    ...desc1,
    fileRevision: rev2,
    fileRevisionText: encodeBase64Url(rev2),
  })
  const entry: CatalogEntry = Object.freeze({
    kind: 'file',
    id: fileId,
    idText: fileIdText,
    name,
    expectedSize: exactSize,
  })
  const committed: CommittedDirectory = Object.freeze({
    directoryId: rootId,
    directoryIdText: encodeBase64Url(rootId),
    generation,
    generationText: encodeBase64Url(generation),
    pageCount: 1,
    entryCount: 1,
    omittedCount: 0n,
    terminalCommitment: new Uint8Array(32),
  })
  const page: CatalogPage = Object.freeze({
    shareInstance,
    directoryId: rootId,
    directoryIdText: committed.directoryIdText,
    generation,
    generationText: committed.generationText,
    pageIndex: 0,
    terminal: true,
    previousCommitment: new Uint8Array(32),
    entries: Object.freeze([entry]),
    omittedCount: 0n,
    objectCommitment: new Uint8Array(32),
    encodedBytes: 64,
  })
  const descriptor: ShareDescriptor = Object.freeze({
    shareInstance,
    shareInstanceId: desc1.shareInstanceId,
    syntheticRoot: rootId,
    syntheticRootId: committed.directoryIdText,
  }) as unknown as ShareDescriptor
  const catalog = Object.freeze({
    loadDirectory: async () => committed,
    page: async () => page,
  }) as unknown as CatalogClient

  let openCount = 0
  const revisions: RevisionReader = {
    open: async () => {
      openCount += 1
      return Object.freeze({
        descriptor: openCount === 1 ? desc1 : desc2,
        leaseId: fixedId(openCount),
        release: async () => undefined,
      })
    },
  }
  const split = BigInt(Math.floor(payload.byteLength / 2))
  const broker: BlockRangeReader = {
    readRange: async function* (revDesc, _leaseId, span) {
      if (revDesc.fileRevisionText === desc1.fileRevisionText) {
        yield Object.freeze({ offset: span.start, data: payload.subarray(Number(span.start), Number(split)) })
        throw new RevisionReopenedDuringRecoveryError(desc1, desc2)
      }
      yield Object.freeze({ offset: span.start, data: payload.subarray(Number(span.start), Number(span.end)) })
    },
  }
  return {
    descriptor,
    catalog,
    revisions,
    broker,
    fileIdText,
    rev2Text: desc2.fileRevisionText,
    openCount: () => openCount,
  }
}

async function readZipEntries(blob: Blob): Promise<Map<string, Uint8Array>> {
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

describe('backend reopened-revision readmission', () => {
  it('checkpoints FSA progress and finishes the remainder under a reopened revision with matching metadata', async () => {
    const payload = new TextEncoder().encode('movie-part-1::movie-part-2')
    const fixture = createReopenShareFixture('movie.mkv', payload)
    const selection = new SelectionPolicy(true).snapshot()
    const spec = selectionSpecFromPolicy(fixture.descriptor.shareInstanceId, fixture.descriptor.syntheticRootId, selection)
    const store = new MemoryReceiveStore()
    const parent = new FakeFSADirectoryHandle('Downloads')

    const session = await openNewFSASession({
      task: {
        taskId: 'fsa-reopen-resume',
        selection: spec,
        selectionKey: selectionKey(spec),
        artifact: Object.freeze({ kind: 'directory-tree', name: 'Movies' }),
        lifecycle: initialReceiveState('fsa-reopen-resume'),
      },
      parentHandle: parent as unknown as FileSystemDirectoryHandle,
      store: store as unknown as ReceiveStore,
    })

    let lastMaterialized = 0n
    const result = await new TransferJob({
      descriptor: fixture.descriptor,
      catalog: fixture.catalog,
      selection,
      revisions: fixture.revisions,
      broker: fixture.broker,
      output: session.transferOutput,
      onProgress: progress => { lastMaterialized = progress.materializedBytes },
    }).run()
    const settled = await session.settle(result)
    await session.close()

    expect(result.status).toBe('completed')
    expect(settled.lifecycle.kind).toBe('saved')
    expect(fixture.openCount()).toBe(2)
    expect(lastMaterialized).toBe(BigInt(payload.byteLength))

    const folder = parent.children.get('Movies') as FakeFSADirectoryHandle
    const file = folder.children.get('movie.mkv') as FakeFSAFileHandle
    expect(file.abortCalls).toBe(0)
    expect(file.writableCalls).toEqual([false, true])
    expect([...file.bytes]).toEqual([...payload])
    expect((await store.getFile('fsa-reopen-resume', fixture.fileIdText))?.revision).toBe(fixture.rev2Text)
  })

  it('resumes OPFS progressive ZIP files across metadata-agreement reopens and persists the new revision', async () => {
    const payload = new TextEncoder().encode('archive-first-half::archive-second-half')
    const fixture = createReopenShareFixture('clip.mp4', payload)
    const selection = new SelectionPolicy(true).snapshot()
    const spec = selectionSpecFromPolicy(fixture.descriptor.shareInstanceId, fixture.descriptor.syntheticRootId, selection)
    const store = new MemoryReceiveStore()
    const { env } = createMemoryOPFSEnv()
    const { windowPort, blobs } = createMockDownloadWindow()

    const session = await openNewOPFSSession({
      task: {
        taskId: 'opfs-zip-reopen',
        selection: spec,
        selectionKey: selectionKey(spec),
        artifact: Object.freeze({ kind: 'zip-archive', name: 'Clips.zip' }),
        lifecycle: initialReceiveState('opfs-zip-reopen'),
      },
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
      output: session.transferOutput,
    }).run()
    const settled = await session.settle(result)
    await session.close()

    expect(result.status).toBe('completed')
    expect(settled.lifecycle.kind).toBe('download-started')
    expect(fixture.openCount()).toBe(2)
    expect((await store.getFile('opfs-zip-reopen', fixture.fileIdText))?.revision).toBe(fixture.rev2Text)
    expect(blobs).toHaveLength(1)
    const entries = await readZipEntries(blobs[0]!)
    expect([...entries.keys()]).toEqual(['clip.mp4'])
    expect(entries.get('clip.mp4')).toEqual(payload)
  })

  it('aborts non-resumable portable ZIP cleanly instead of reopening an active streaming entry', async () => {
    const payload = new TextEncoder().encode('small-portable-payload')
    const fixture = createReopenShareFixture('small.bin', payload)
    const selection = new SelectionPolicy(true).snapshot()
    const spec = selectionSpecFromPolicy(fixture.descriptor.shareInstanceId, fixture.descriptor.syntheticRootId, selection)
    const { windowPort, blobs } = createMockDownloadWindow()

    const session = openPortableSession({
      task: {
        taskId: 'portable-zip-reopen',
        selection: spec,
        selectionKey: selectionKey(spec),
        artifact: Object.freeze({ kind: 'zip-archive', name: 'Small.zip' }),
        lifecycle: initialReceiveState('portable-zip-reopen'),
      },
      windowPort,
    })

    const result = await new TransferJob({
      descriptor: fixture.descriptor,
      catalog: fixture.catalog,
      selection,
      revisions: fixture.revisions,
      broker: fixture.broker,
      output: session.transferOutput,
    }).run()
    const settled = await session.settle(result)
    await session.close()

    expect(result.status).toBe('failed')
    expect(fixture.openCount()).toBe(1)
    expect(settled.lifecycle.kind).toBe('failed')
    expect(blobs).toHaveLength(0)
  })
})

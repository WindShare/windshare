import { describe, expect, it } from 'vitest'

import { BrowserFileLineageAuthority } from '../../src/output/browser/filesystem-file-lineage'
import {
  acquireFSARootMutationLease,
  type BrowserLockManagerRuntime,
  type FSANamespaceMutationKind,
  type FSARootMutationAuthority,
} from '../../src/output/browser/namespace-mutation'
import type { PersistedFSAOperationBinding } from '../../src/output/browser/indexeddb-root-binding'
import type {
  PersistentHandleRecord,
  PersistentHandleRepository,
} from '../../src/output/persistence/journal'
import type { OpenedFileRevision, PersistentTreeFile } from '../../src/output/persistent-tree/contracts'
import {
  createPersistentOutputStageAuthority,
  type PersistentOutputStageMilestone,
} from '../../src/output/persistent-tree/stage-diagnostics'
import {
  createFSANamedEntryReservation,
  createSingleFileDirectoryTreeArtifact,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import { identity } from './planning/fixture'

describe('browser file writer mutation lifecycle', () => {
  it('serializes writer open/write and close as owned browser mutations', async () => {
    const fixture = await writerFixture()

    fixture.timeline.length = 0
    await fixture.file.writeAt(7n, Uint8Array.of(3, 4))
    await fixture.file.flush()

    expect(fixture.timeline).toEqual([
      'admit:open-writer',
      'enter:open-writer',
      'browser:createWritable',
      'browser:write',
      'return:open-writer',
      'admit:commit-file',
      'enter:commit-file',
      'browser:close',
      'return:commit-file',
    ])
    expect(fixture.writer.writeCalls).toBe(1)
    expect(fixture.writer.closeCalls).toBe(1)
    fixture.retire()
    await fixture.lease.release()
  })

  it('keeps close queued behind an earlier root mutation', async () => {
    const fixture = await writerFixture()
    await fixture.file.writeAt(0n, Uint8Array.of(5))
    fixture.timeline.length = 0
    const entered = deferred<void>()
    const releaseBlocker = deferred<void>()

    const blocker = fixture.mutations.run('create-directory', async () => {
      fixture.timeline.push('browser:blocker-enter')
      entered.resolve()
      await releaseBlocker.promise
      fixture.timeline.push('browser:blocker-return')
    })
    await entered.promise
    const flush = fixture.file.flush()

    expect(fixture.writer.closeCalls).toBe(0)
    expect(fixture.timeline).toEqual([
      'admit:create-directory',
      'enter:create-directory',
      'browser:blocker-enter',
      'admit:commit-file',
    ])
    releaseBlocker.resolve()
    await blocker
    await flush
    expect(fixture.timeline).toEqual([
      'admit:create-directory',
      'enter:create-directory',
      'browser:blocker-enter',
      'admit:commit-file',
      'browser:blocker-return',
      'return:create-directory',
      'enter:commit-file',
      'browser:close',
      'return:commit-file',
    ])
    fixture.retire()
    await fixture.lease.release()
  })

  it('retains close-failed evidence and never retries an uncertain writer close', async () => {
    const closeFailure = new DOMException('close outcome is uncertain', 'UnknownError')
    const fixture = await writerFixture(closeFailure)
    await fixture.file.writeAt(0n, Uint8Array.of(9))
    fixture.timeline.length = 0

    await expect(fixture.file.flush()).rejects.toBe(closeFailure)
    const closeMilestone = fixture.milestones.find(milestone =>
      milestone.transition === 'failed' && milestone.stage === 'fsa.file.writer.close')
    expect(closeMilestone?.transition).toBe('failed')
    if (closeMilestone?.transition === 'failed') {
      expect(closeMilestone.exception.raw).toBe(closeFailure)
      expect(closeMilestone.facts.fsa?.writer).toMatchObject({
        state: 'close-failed',
        closeFailure: { raw: closeFailure },
      })
    }
    expect(fixture.timeline).toEqual([
      'admit:commit-file',
      'enter:commit-file',
      'browser:close',
      'reject:commit-file',
    ])

    await expect(fixture.file.flush()).rejects.toBe(closeFailure)
    expect(fixture.writer.closeCalls).toBe(1)
    expect(fixture.milestones.filter(milestone =>
      milestone.transition === 'failed' &&
      milestone.stage === 'fsa.file.writer.close')).toHaveLength(1)
    expect(fixture.timeline).toEqual([
      'admit:commit-file',
      'enter:commit-file',
      'browser:close',
      'reject:commit-file',
    ])
    fixture.retire()
    await fixture.lease.release()
  })
})

async function writerFixture(closeFailure?: unknown): Promise<Readonly<{
  file: PersistentTreeFile
  writer: InstrumentedWriter
  lease: Awaited<ReturnType<typeof acquireFSARootMutationLease>>
  mutations: FSARootMutationAuthority
  timeline: string[]
  milestones: PersistentOutputStageMilestone[]
  retire: () => void
}>> {
  const timeline: string[] = []
  const milestones: PersistentOutputStageMilestone[] = []
  const writer = new InstrumentedWriter(timeline, closeFailure)
  const fileHandle = new InstrumentedFileHandle('payload.bin', timeline, writer)
  const parent = new InstrumentedDirectoryHandle('downloads', fileHandle)
  const locks = new MemoryLockManager()
  const lease = await acquireFSARootMutationLease(
    parent as unknown as FileSystemDirectoryHandle,
    locks,
  )
  const mutations = recordingMutationAuthority(lease.authority, timeline)
  const operationId = identity(50)
  const authorityRef = identity(51, 32)
  const artifact = await createSingleFileDirectoryTreeArtifact({
    fileId: identity(53),
    sourcePath: 'payload.bin',
    outputName: 'payload.bin',
  })
  const reservation = await createFSANamedEntryReservation({
    operationId,
    reservationId: identity(56),
    artifact,
    authorityRef,
    logicalReservedName: 'payload.bin',
    physicalName: 'payload.bin',
    collisionIndex: 0,
  })
  const binding = {
    intent: { operationId } as ReceiveIntent,
    reservation,
    parent: parent as unknown as FileSystemDirectoryHandle,
    parentHandleId: 'test-parent',
  } satisfies PersistedFSAOperationBinding
  const handles = new MemoryHandleRepository()
  const lineage = new BrowserFileLineageAuthority({
    binding,
    fileHandles: handles,
    mutations,
    prepareRoot: async () => undefined,
    verifyParent: async () => parent as unknown as FileSystemDirectoryHandle,
    resolveParent: async (path) => Object.freeze({
      parent: parent as unknown as FileSystemDirectoryHandle,
      name: path.at(-1) ?? reservation.physicalName,
    }),
  })
  const revision: OpenedFileRevision = Object.freeze({
    shareInstance: identity(52),
    fileId: identity(53),
    fileRevision: identity(54),
    exactSize: 9n,
  })
  const ownedObjectId = identity(55, 32)
  const stageAuthority = createPersistentOutputStageAuthority({
    outputSessionId: 'writer-mutation-session',
    observe: milestone => milestones.push(milestone),
  }, {
    operationId,
    artifactId: artifact.digest,
  })!
  const stageScope = stageAuthority
    .fileScope(revision.fileId, [])
    .withCorrelation({ ownedObjectId })
  const file = await lineage.createAfterRevisionOpen(
    [],
    revision,
    ownedObjectId,
    stageScope,
  )
  expect(timeline).toContain('enter:create-file')
  return Object.freeze({
    file,
    writer,
    lease,
    mutations,
    timeline,
    milestones,
    retire: () => stageScope.retireFileEvidence(),
  })
}

function recordingMutationAuthority(
  authority: FSARootMutationAuthority,
  timeline: string[],
): FSARootMutationAuthority {
  return Object.freeze({
    async run<T>(
      kind: FSANamespaceMutationKind,
      operation: () => Promise<T>,
    ): Promise<T> {
      timeline.push(`admit:${kind}`)
      return authority.run(kind, async () => {
        timeline.push(`enter:${kind}`)
        try {
          const result = await operation()
          timeline.push(`return:${kind}`)
          return result
        } catch (error) {
          timeline.push(`reject:${kind}`)
          throw error
        }
      })
    },
  })
}

class MemoryHandleRepository implements PersistentHandleRepository {
  readonly #records = new Map<string, PersistentHandleRecord>()

  async putHandle(record: PersistentHandleRecord): Promise<void> {
    this.#records.set(record.id, record)
  }

  async readHandle(id: string): Promise<PersistentHandleRecord | undefined> {
    return this.#records.get(id)
  }

  async deleteHandle(id: string): Promise<void> {
    this.#records.delete(id)
  }
}

class InstrumentedDirectoryHandle {
  readonly kind = 'directory' as const
  readonly name: string
  readonly #file: InstrumentedFileHandle
  #created = false

  constructor(name: string, file: InstrumentedFileHandle) {
    this.name = name
    this.#file = file
  }

  async isSameEntry(other: FileSystemHandle): Promise<boolean> {
    return other === this as unknown as FileSystemHandle
  }

  async getFileHandle(
    name: string,
    options?: FileSystemGetFileOptions,
  ): Promise<FileSystemFileHandle> {
    if (name !== this.#file.name) throw new DOMException('missing', 'NotFoundError')
    if (!this.#created && options?.create !== true) {
      throw new DOMException('missing', 'NotFoundError')
    }
    this.#created = true
    return this.#file as unknown as FileSystemFileHandle
  }

  async getDirectoryHandle(): Promise<FileSystemDirectoryHandle> {
    throw new DOMException('missing', 'NotFoundError')
  }
}

class InstrumentedFileHandle {
  readonly kind = 'file' as const
  readonly name: string
  readonly #timeline: string[]
  readonly #writer: InstrumentedWriter

  constructor(name: string, timeline: string[], writer: InstrumentedWriter) {
    this.name = name
    this.#timeline = timeline
    this.#writer = writer
  }

  async isSameEntry(other: FileSystemHandle): Promise<boolean> {
    return other === this as unknown as FileSystemHandle
  }

  async createWritable(
    options?: FileSystemCreateWritableOptions,
  ): Promise<FileSystemWritableFileStream> {
    expect(options).toEqual({ keepExistingData: true })
    this.#timeline.push('browser:createWritable')
    return this.#writer as unknown as FileSystemWritableFileStream
  }

  async getFile(): Promise<File> {
    return new Blob([]) as File
  }
}

class InstrumentedWriter {
  readonly #timeline: string[]
  readonly #closeFailure: unknown
  writeCalls = 0
  closeCalls = 0

  constructor(timeline: string[], closeFailure?: unknown) {
    this.#timeline = timeline
    this.#closeFailure = closeFailure
  }

  async write(): Promise<void> {
    this.writeCalls += 1
    this.#timeline.push('browser:write')
  }

  async close(): Promise<void> {
    this.closeCalls += 1
    this.#timeline.push('browser:close')
    if (this.#closeFailure !== undefined) throw this.#closeFailure
  }
}

function deferred<T>(): Readonly<{
  promise: Promise<T>
  resolve: (value: T) => void
}> {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((complete) => { resolve = complete })
  return Object.freeze({ promise, resolve })
}

class MemoryLockManager implements BrowserLockManagerRuntime {
  #held = false

  async request(
    name: string,
    _options: { readonly mode: 'exclusive'; readonly ifAvailable: true },
    callback: (lock: { readonly name: string } | null) => Promise<void>,
  ): Promise<void> {
    if (this.#held) {
      await callback(null)
      return
    }
    this.#held = true
    try {
      await callback({ name })
    } finally {
      this.#held = false
    }
  }
}

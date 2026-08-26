import { describe, expect, it } from 'vitest'

import {
  createBrowserPersistentFile,
  type BrowserPersistentFile,
} from '../../src/output/browser/filesystem-persistent-file'
import {
  fsaAuthorityCacheForRoot,
  type FSAAuthorityCache,
  type FSAVerifiedDirectoryAuthority,
} from '../../src/output/browser/mutation-coordination/authority-cache'
import {
  acquireFSARootMutationLease,
  type BrowserLockHandle,
  type BrowserLockManagerRuntime,
} from '../../src/output/browser/namespace-mutation'
import type { PersistedFSAOperationBinding } from '../../src/output/browser/indexeddb-root-binding'
import {
  createPersistentOutputStageAuthority,
  type PersistentOutputStageMilestone,
} from '../../src/output/persistent-tree/stage-diagnostics'
import type { ReceiveIntent } from '../../src/transfer/intent'
import { identity } from './planning/fixture'

describe('browser file writer mutation lifecycle', () => {
  it('holds the same-parent lane from verification through native close', async () => {
    const fixture = await writerFixture()
    await fixture.file.openWriter('preserve')
    await fixture.file.writeAt(7n, Uint8Array.of(3, 4))
    let namespaceEntered = false
    const namespace = fixture.lease.scheduler.runNamespace(
      [fixture.parent.schedulerIdentity],
      'create-directory',
      async () => { namespaceEntered = true },
    )

    await Promise.resolve()
    expect(namespaceEntered).toBe(false)
    expect(fixture.lease.scheduler.diagnostics().activeWriters).toBe(1)
    await fixture.file.flush()
    await namespace

    expect(fixture.timeline).toEqual([
      'verify:writer-open',
      'browser:createWritable',
      'browser:write',
      'browser:close',
    ])
    expect(namespaceEntered).toBe(true)
    expect(fixture.lease.scheduler.diagnostics().activeWriters).toBe(0)
    await fixture.release()
    expect(fixture.authorities.diagnostics().closed).toBe(true)
  })

  it('allows independent owned files to keep native writers open concurrently', async () => {
    const firstWrite = deferred<void>()
    const secondWrite = deferred<void>()
    const fixture = await writerFixture({
      maximumActiveWriters: 2,
      writers: [
        new InstrumentedWriter([], { writeGate: firstWrite }),
        new InstrumentedWriter([], { writeGate: secondWrite }),
      ],
    })
    await Promise.all([
      fixture.file.openWriter('preserve'),
      fixture.otherFile!.openWriter('preserve'),
    ])
    const first = fixture.file.writeAt(0n, Uint8Array.of(1))
    const second = fixture.otherFile!.writeAt(0n, Uint8Array.of(2))
    await Promise.all(fixture.writers.map(writer => writer.writeStarted.promise))

    expect(fixture.lease.scheduler.diagnostics()).toMatchObject({
      activeWriters: 2,
      peakActiveWriters: 2,
    })
    firstWrite.resolve()
    secondWrite.resolve()
    await Promise.all([first, second])
    await Promise.all([fixture.file.flush(), fixture.otherFile!.flush()])
    expect(fixture.lease.scheduler.diagnostics().activeWriters).toBe(0)
    await fixture.release()
  })

  it('preserves the exact close failure, never retries it, and releases capacity once', async () => {
    const closeFailure = new DOMException('close outcome is uncertain', 'UnknownError')
    const writer = new InstrumentedWriter([], { closeFailure })
    const fixture = await writerFixture({ writers: [writer] })
    await fixture.file.openWriter('truncate')
    await fixture.file.writeAt(0n, Uint8Array.of(9))

    await expect(fixture.file.flush()).rejects.toBe(closeFailure)
    const failedClose = fixture.milestones.find(milestone =>
      milestone.transition === 'failed' && milestone.stage === 'fsa.file.writer.close')
    expect(failedClose?.transition).toBe('failed')
    if (failedClose?.transition === 'failed') {
      expect(failedClose.exception.raw).toBe(closeFailure)
    }
    expect(fixture.lease.scheduler.diagnostics()).toMatchObject({
      activeWriters: 0,
      acquiredWriterLeases: 1,
      releasedWriterLeases: 1,
    })
    await expect(fixture.file.flush()).rejects.toBe(closeFailure)
    expect(writer.closeCalls).toBe(1)
    expect(fixture.lease.scheduler.diagnostics().releasedWriterLeases).toBe(1)
    await fixture.release()
  })

  it('retains writer ownership after a write failure until close settles', async () => {
    const writeFailure = new DOMException('write failed', 'UnknownError')
    const fixture = await writerFixture({
      writers: [new InstrumentedWriter([], { writeFailure })],
    })
    await fixture.file.openWriter('preserve')
    await expect(fixture.file.writeAt(0n, Uint8Array.of(5))).rejects.toBe(writeFailure)
    let namespaceEntered = false
    const namespace = fixture.lease.scheduler.runNamespace(
      [fixture.parent.schedulerIdentity],
      'remove-entry',
      async () => { namespaceEntered = true },
    )

    await Promise.resolve()
    expect(namespaceEntered).toBe(false)
    expect(fixture.lease.scheduler.diagnostics().activeWriters).toBe(1)
    await fixture.file.close()
    await namespace
    expect(namespaceEntered).toBe(true)
    await fixture.release()
  })

  it('releases the lane immediately when native writer creation fails', async () => {
    const createFailure = new DOMException('writer unavailable', 'InvalidStateError')
    const fixture = await writerFixture({ createFailure })

    await expect(fixture.file.openWriter('preserve')).rejects.toBe(createFailure)
    expect(fixture.lease.scheduler.diagnostics()).toMatchObject({
      activeWriters: 0,
      acquiredWriterLeases: 1,
      releasedWriterLeases: 1,
    })
    await fixture.lease.scheduler.runNamespace(
      [fixture.parent.schedulerIdentity],
      'create-file',
      async () => undefined,
    )
    await fixture.release()
  })

  it('keeps namespace work blocked until a native abort attempt settles', async () => {
    const abortFinished = deferred<void>()
    const writer = new InstrumentedWriter([], { abortGate: abortFinished })
    const fixture = await writerFixture({ writers: [writer] })
    await fixture.file.openWriter('preserve')
    const retirement = fixture.file.abort('cancelled')
    await writer.abortStarted.promise
    await expect(fixture.file.flush()).rejects.toMatchObject({ name: 'InvalidStateError' })
    let namespaceEntered = false
    const namespace = fixture.lease.scheduler.runNamespace(
      [fixture.parent.schedulerIdentity],
      'remove-entry',
      async () => { namespaceEntered = true },
    )

    await Promise.resolve()
    expect(namespaceEntered).toBe(false)
    abortFinished.resolve()
    await retirement
    await namespace
    expect(namespaceEntered).toBe(true)
    expect(writer.abortReasons).toEqual(['cancelled'])
    await fixture.release()
  })

  it('releases writer capacity once and preserves the exact native abort failure', async () => {
    const abortFailure = new DOMException('abort outcome is uncertain', 'UnknownError')
    const writer = new InstrumentedWriter([], { abortFailure })
    const fixture = await writerFixture({ writers: [writer] })
    await fixture.file.openWriter('preserve')

    await expect(fixture.file.abort(abortFailure)).rejects.toBe(abortFailure)
    const failedAbort = fixture.milestones.find(milestone =>
      milestone.transition === 'failed' && milestone.stage === 'fsa.file.writer.abort')
    expect(failedAbort?.transition).toBe('failed')
    if (failedAbort?.transition === 'failed') expect(failedAbort.exception.raw).toBe(abortFailure)
    expect(fixture.lease.scheduler.diagnostics()).toMatchObject({
      activeWriters: 0,
      acquiredWriterLeases: 1,
      releasedWriterLeases: 1,
    })
    await expect(fixture.file.abort()).rejects.toBe(abortFailure)
    expect(writer.abortCalls).toBe(1)
    expect(fixture.lease.scheduler.diagnostics().releasedWriterLeases).toBe(1)
    await fixture.release()
  })

  it('maps explicit open modes and reports one incremental preserving-open cost', async () => {
    const preserve = await writerFixture()
    await preserve.file.openWriter('preserve')
    expect(preserve.handles[0]!.keepExistingData).toEqual([true])
    expect(preserve.file.preservingWriterCost(4096n)).toEqual({
      prefixCopyBytes: 4096n,
      writeAmplificationBytes: 4096n,
      temporaryBytes: 4096n,
    })
    await preserve.file.close()
    await preserve.release()

    const truncate = await writerFixture()
    await truncate.file.openWriter('truncate')
    expect(truncate.handles[0]!.keepExistingData).toEqual([false])
    expect(truncate.file.preservingWriterCost(0n)).toEqual({
      prefixCopyBytes: 0n,
      writeAmplificationBytes: 0n,
      temporaryBytes: 0n,
    })
    await truncate.file.close()
    await truncate.release()
  })
})

interface WriterFixtureOptions {
  readonly maximumActiveWriters?: number
  readonly writers?: readonly InstrumentedWriter[]
  readonly createFailure?: unknown
}

async function writerFixture(options: WriterFixtureOptions = {}): Promise<Readonly<{
  file: BrowserPersistentFile
  otherFile: BrowserPersistentFile | undefined
  writers: readonly InstrumentedWriter[]
  handles: readonly InstrumentedFileHandle[]
  parent: FSAVerifiedDirectoryAuthority
  authorities: FSAAuthorityCache
  lease: Awaited<ReturnType<typeof acquireFSARootMutationLease>>
  timeline: string[]
  milestones: readonly PersistentOutputStageMilestone[]
  release: () => Promise<void>
}>> {
  const timeline: string[] = []
  const milestones: PersistentOutputStageMilestone[] = []
  const writers = options.writers === undefined || options.writers.length === 0
    ? [new InstrumentedWriter(timeline)]
    : options.writers
  const handles = writers.map((writer, index) => new InstrumentedFileHandle(
    `payload-${index}.bin`,
    timeline,
    writer,
    options.createFailure,
  ))
  const directory = new InstrumentedDirectoryHandle('downloads')
  const lease = await acquireFSARootMutationLease(
    directory as unknown as FileSystemDirectoryHandle,
    new MemoryLockManager(),
    options.maximumActiveWriters ?? 2,
  )
  const binding = Object.freeze({
    intent: { operationId: 'writer-lifecycle-operation' } as ReceiveIntent,
    reservation: Object.freeze({}),
    parent: directory as unknown as FileSystemDirectoryHandle,
    parentHandleId: 'picked-parent-handle',
  }) as PersistedFSAOperationBinding
  const authorities = fsaAuthorityCacheForRoot({
    owner: lease.authority,
    binding,
    rootParentIdentity: lease.authority.rootParentIdentity,
  })
  const parent = authorities.pickedParent()
  const stageAuthority = createPersistentOutputStageAuthority({
    outputSessionId: 'writer-lifecycle-session',
    observe: milestone => milestones.push(milestone),
  }, {
    operationId: binding.intent.operationId,
    artifactId: identity(81),
  })!
  const stageScopes = handles.map((_, index) => stageAuthority
    .fileScope(identity(82 + index), [`payload-${index}.bin`])
    .withCorrelation({ ownedObjectId: `owned-file-${index}` }))
  const files = handles.map((handle, index) => createBrowserPersistentFile({
    authority: authorities.installFile({
      handleId: `file-handle-${index}`,
      ownedObjectId: `owned-file-${index}`,
      parent,
      canonicalPath: [`payload-${index}.bin`],
      physicalName: `payload-${index}.bin`,
      handle: handle as unknown as FileSystemFileHandle,
    }),
    persistedHandle: {
      id: `file-handle-${index}`,
      operationId: binding.intent.operationId,
      kind: 1,
      authorityRef: `writer-authority-${index}`,
      ownedObjectId: `owned-file-${index}`,
      handle: handle as unknown as FileSystemFileHandle,
    },
    scheduler: lease.scheduler,
    verify: async stage => { timeline.push(`verify:${stage}`) },
    stageScope: stageScopes[index]!,
  }))
  return Object.freeze({
    file: files[0]!,
    otherFile: files[1],
    writers,
    handles,
    parent,
    authorities,
    lease,
    timeline,
    milestones,
    release: async () => {
      for (const stageScope of stageScopes) stageScope.retireFileEvidence()
      await lease.release()
    },
  })
}

class InstrumentedWriter {
  readonly writeStarted = deferred<void>()
  readonly timeline: string[]
  readonly options: Readonly<{
    writeGate?: Deferred<void>
    writeFailure?: unknown
    closeFailure?: unknown
    abortGate?: Deferred<void>
    abortFailure?: unknown
  }>
  writeCalls = 0
  closeCalls = 0
  abortCalls = 0
  readonly abortStarted = deferred<void>()
  readonly abortReasons: unknown[] = []

  constructor(
    timeline: string[],
    options: Readonly<{
      writeGate?: Deferred<void>
      writeFailure?: unknown
      closeFailure?: unknown
      abortGate?: Deferred<void>
      abortFailure?: unknown
    }> = {},
  ) {
    this.timeline = timeline
    this.options = options
  }

  async write(): Promise<void> {
    this.writeCalls += 1
    this.timeline.push('browser:write')
    this.writeStarted.resolve()
    if (this.options.writeGate !== undefined) await this.options.writeGate.promise
    if (this.options.writeFailure !== undefined) throw this.options.writeFailure
  }

  async close(): Promise<void> {
    this.closeCalls += 1
    this.timeline.push('browser:close')
    if (this.options.closeFailure !== undefined) throw this.options.closeFailure
  }

  async abort(reason?: unknown): Promise<void> {
    this.abortCalls += 1
    this.abortReasons.push(reason)
    this.timeline.push('browser:abort')
    this.abortStarted.resolve()
    if (this.options.abortGate !== undefined) await this.options.abortGate.promise
    if (this.options.abortFailure !== undefined) throw this.options.abortFailure
  }
}

class InstrumentedFileHandle {
  readonly kind = 'file' as const
  readonly name: string
  readonly timeline: string[]
  readonly writer: InstrumentedWriter
  readonly createFailure: unknown
  readonly keepExistingData: boolean[] = []

  constructor(
    name: string,
    timeline: string[],
    writer: InstrumentedWriter,
    createFailure?: unknown,
  ) {
    this.name = name
    this.timeline = timeline
    this.writer = writer
    this.createFailure = createFailure
  }

  async createWritable(options?: FileSystemCreateWritableOptions): Promise<FileSystemWritableFileStream> {
    this.timeline.push('browser:createWritable')
    this.keepExistingData.push(options?.keepExistingData ?? false)
    if (this.createFailure !== undefined) throw this.createFailure
    return this.writer as unknown as FileSystemWritableFileStream
  }

  async getFile(): Promise<File> {
    return new File([], this.name)
  }

  async isSameEntry(other: FileSystemHandle): Promise<boolean> {
    return other === this
  }
}

class InstrumentedDirectoryHandle {
  readonly kind = 'directory' as const
  readonly name: string

  constructor(name: string) {
    this.name = name
  }

  async isSameEntry(other: FileSystemHandle): Promise<boolean> {
    return other === this
  }
}

class MemoryLockManager implements BrowserLockManagerRuntime {
  request(
    name: string,
    _options: { readonly mode: 'exclusive'; readonly ifAvailable: true },
    callback: (lock: BrowserLockHandle | null) => Promise<void>,
  ): Promise<void> {
    return callback({ name })
  }
}

interface Deferred<Value> {
  readonly promise: Promise<Value>
  readonly resolve: (value?: Value | PromiseLike<Value>) => void
}

function deferred<Value>(): Deferred<Value> {
  let resolve!: (value?: Value | PromiseLike<Value>) => void
  const promise = new Promise<Value>(complete => {
    resolve = value => { complete(value as Value | PromiseLike<Value>) }
  })
  return { promise, resolve }
}

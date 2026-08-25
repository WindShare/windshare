import {
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  newFileCheckpointV2,
  type FileCheckpointV2,
} from '../../src/output/persistence/checkpoint'
import type { FileCheckpointJournal, PersistentHandleRecord } from '../../src/output/persistence/journal'
import type {
  OpenedFileRevision,
  PersistentDirectoryMaterialization,
  PersistentMaterializationPort,
  PersistentOutputTree,
  PersistentTreeFile,
} from '../../src/output/persistent-tree/contracts'
import { TargetOwnershipUnknownError } from '../../src/output/persistent-tree/errors'
import { persistentInitialCheckpoint } from '../../src/output/persistent-tree/recovery'
import { snapshotMaterializationRootRelativePath } from '../../src/transfer/job/coordinate/direct-tree'
import { identity } from './planning/fixture'

export type VerificationStage = 'writer-open' | 'checkpoint' | 'commit'

export function preservingRecoveryPolicy() {
  return Object.freeze({
    pausedFile: 'preserve' as const,
    costBudget: Object.freeze({
      maximumPrefixCopyBytes: 1_024n,
      maximumCumulativeWriteAmplificationBytes: 2_048n,
      maximumPeakTemporaryBytes: 1_024n,
    }),
  })
}

export interface Deferred<T> {
  readonly promise: Promise<T>
  readonly resolve: (value: T | PromiseLike<T>) => void
  readonly reject: (reason?: unknown) => void
}

export interface FileCloseBarrier {
  readonly started: Deferred<void>
  readonly release: Deferred<void>
}

export interface FileWriteBarrier {
  readonly accepted: Deferred<void>
  readonly release: Deferred<void>
}

interface FileInspectionBarrier {
  readonly started: Deferred<void>
  readonly outcome: Deferred<'absent' | 'occupied'>
}

export interface ControlledFileInspection {
  readonly started: Promise<void>
  readonly resolve: (outcome?: 'absent' | 'occupied') => void
  readonly reject: (error: unknown) => void
}

export interface ControlledBarrier {
  readonly started: Promise<void>
  readonly release: () => void
}

export class MemoryFile implements PersistentTreeFile {
  readonly ownedObjectId: string
  readonly persistedHandle: PersistentHandleRecord<unknown>
  readonly #closeBarrier: FileCloseBarrier | undefined
  readonly #writeBarrier: FileWriteBarrier | undefined
  #bytes = new Uint8Array()
  readonly #verificationCounts = new Map<string, number>()
  #failure: { readonly stage: string; readonly occurrence: number } | undefined
  #failNextFlush = false
  writerOpenCount = 0
  flushCount = 0
  sizeCount = 0
  abortCount = 0
  preflightCount = 0
  requiresSpaceConfirmation = false
  readonly writerModes: string[] = []

  constructor(
    ownedObjectId: string,
    persistedHandle: PersistentHandleRecord<unknown>,
    closeBarrier?: FileCloseBarrier,
    writeBarrier?: FileWriteBarrier,
  ) {
    this.ownedObjectId = ownedObjectId
    this.persistedHandle = persistedHandle
    this.#closeBarrier = closeBarrier
    this.#writeBarrier = writeBarrier
  }

  async writeAt(offset: bigint, data: Uint8Array): Promise<void> {
    const start = Number(offset)
    const size = Math.max(this.#bytes.byteLength, start + data.byteLength)
    const next = new Uint8Array(size)
    next.set(this.#bytes)
    next.set(data, start)
    this.#bytes = next
    this.#writeBarrier?.accepted.resolve()
    await this.#writeBarrier?.release.promise
  }

  async openWriter(mode: 'preserve' | 'truncate'): Promise<void> {
    this.writerOpenCount += 1
    this.writerModes.push(mode)
    await this.verify('writer-open')
    if (mode === 'truncate') this.#bytes = new Uint8Array()
  }

  checkpointPreflight(durablePrefixBytes: bigint, cumulativeWriteAmplificationBytes: bigint) {
    this.preflightCount += 1
    return Object.freeze({
      cost: Object.freeze({
        prefixCopyBytes: durablePrefixBytes,
        cumulativeWriteAmplificationBytes:
          cumulativeWriteAmplificationBytes + durablePrefixBytes,
        peakTemporaryBytes: durablePrefixBytes,
      }),
      space: this.requiresSpaceConfirmation
        ? 'requires-user-confirmation' as const
        : 'within-modeled-budget' as const,
    })
  }

  async flush(): Promise<void> {
    this.flushCount += 1
    this.#closeBarrier?.started.resolve()
    await this.#closeBarrier?.release.promise
    if (this.#failNextFlush) {
      this.#failNextFlush = false
      throw new Error('simulated writer close failure')
    }
  }

  async size(): Promise<bigint> {
    this.sizeCount += 1
    return BigInt(this.#bytes.byteLength)
  }

  async verify(stage: VerificationStage): Promise<void> {
    const count = (this.#verificationCounts.get(stage) ?? 0) + 1
    this.#verificationCounts.set(stage, count)
    if (this.#failure?.stage === stage && this.#failure.occurrence === count) {
      throw new TargetOwnershipUnknownError(stage, identity(1))
    }
  }

  async close(): Promise<void> {}

  async abort(): Promise<void> { this.abortCount += 1 }

  async read(): Promise<Blob> {
    return new Blob([this.#bytes])
  }

  snapshot(): Uint8Array {
    return this.#bytes.slice()
  }

  failVerification(stage: string, occurrence: number): void {
    this.#failure = { stage, occurrence }
  }

  failNextFlush(): void { this.#failNextFlush = true }

  verificationCount(stage: VerificationStage): number {
    return this.#verificationCounts.get(stage) ?? 0
  }
}

export class MemoryTree implements PersistentOutputTree {
  readonly #events: string[]
  readonly #files = new Map<string, MemoryFile>()
  readonly #installed = new Set<string>()
  readonly #binding: FileCheckpointJournal['binding']
  readonly #directoryBarriers = new Map<string, FileCloseBarrier>()
  readonly #fileCloseBarriers = new Map<string, FileCloseBarrier>()
  readonly #fileWriteBarriers = new Map<string, FileWriteBarrier>()
  readonly #fileInspectionBarriers = new Map<string, FileInspectionBarrier>()
  readonly #inspectionLaneTails = new Map<string, Promise<void>>()
  readonly #proposedByPath = new Map<string, string>()
  readonly proposedOwnedObjectIds: string[] = []
  readonly inspectionStarts: string[] = []
  readonly inspectionCompletions: string[] = []
  activeInspections = 0
  peakActiveInspections = 0
  #failNextCreation = false
  #nextObject = 60

  constructor(events: string[], binding: FileCheckpointJournal['binding']) {
    this.#events = events
    this.#binding = binding
  }

  async authorize(): Promise<void> {
    this.#events.push('authorize')
  }

  async prepareRoot(): Promise<void> {
    this.#events.push('prepare-root')
  }

  async ensureDirectory(path: readonly string[]): Promise<PersistentDirectoryMaterialization> {
    if (path.some((component) => component.length === 0)) throw new TypeError('empty path component')
    const barrier = this.#directoryBarriers.get(path.join('/'))
    barrier?.started.resolve()
    await barrier?.release.promise
    return Object.freeze({ ownedObjectId: identity(this.#nextObject++, 32), created: true })
  }

  async validateDirectory(): Promise<boolean> {
    return true
  }

  async proposeFileOwnedObjectId(
    path: readonly string[],
    revision: OpenedFileRevision,
  ): Promise<string> {
    if (revision.exactSize < 0n) throw new RangeError('negative revision size')
    const proposed = identity(this.#nextObject++, 32)
    this.proposedOwnedObjectIds.push(proposed)
    this.#proposedByPath.set(path.join('/'), proposed)
    return proposed
  }

  async inspectFileDestination(path: readonly string[]): Promise<'absent' | 'occupied'> {
    const key = path.join('/')
    const parentKey = path.slice(0, -1).join('/')
    const predecessor = this.#inspectionLaneTails.get(parentKey) ?? Promise.resolve()
    const laneRelease = deferred<void>()
    const laneTail = predecessor.catch(() => undefined).then(() => laneRelease.promise)
    this.#inspectionLaneTails.set(parentKey, laneTail)
    await predecessor.catch(() => undefined)
    this.inspectionStarts.push(key)
    this.activeInspections += 1
    this.peakActiveInspections = Math.max(this.peakActiveInspections, this.activeInspections)
    const barrier = this.#fileInspectionBarriers.get(key)
    barrier?.started.resolve()
    try {
      if (barrier !== undefined) return await barrier.outcome.promise
      return this.#files.has(key) ? 'occupied' : 'absent'
    } finally {
      this.activeInspections -= 1
      this.inspectionCompletions.push(key)
      laneRelease.resolve()
      if (this.#inspectionLaneTails.get(parentKey) === laneTail) {
        this.#inspectionLaneTails.delete(parentKey)
      }
    }
  }

  async createFileAfterRevisionOpen(
    path: readonly string[],
    revision: OpenedFileRevision,
    selectedOwnedObjectId: string,
    _stageScope?: unknown,
    commitCreatedFile?: (handle: PersistentHandleRecord<unknown>) => Promise<void>,
  ): Promise<PersistentTreeFile> {
    if (revision.exactSize < 0n) throw new RangeError('negative revision size')
    const key = path.join('/')
    const existing = this.#files.get(key)
    if (existing !== undefined) {
      if (existing.ownedObjectId === selectedOwnedObjectId) {
        await commitCreatedFile?.(existing.persistedHandle)
        this.#installed.add(key)
        return existing
      }
      throw new DOMException('collision', 'InvalidModificationError')
    }
    if (this.#failNextCreation) {
      this.#failNextCreation = false
      throw new Error('simulated pre-object crash')
    }
    this.#events.push(`create:${key}`)
    const persistedHandle = Object.freeze({
      id: `handle:${selectedOwnedObjectId}`,
      operationId: this.#binding.operationId,
      kind: 1,
      authorityRef: this.#binding.authorityRef,
      ownedObjectId: selectedOwnedObjectId,
      handle: Object.freeze({ key }),
    })
    const file = new MemoryFile(
      selectedOwnedObjectId,
      persistedHandle,
      this.#fileCloseBarriers.get(key),
      this.#fileWriteBarriers.get(key),
    )
    this.#files.set(key, file)
    await commitCreatedFile?.(persistedHandle)
    this.#installed.add(key)
    return file
  }

  async openFile(
    path: readonly string[],
    ownedObjectId: string,
  ): Promise<PersistentTreeFile | undefined> {
    const file = this.#files.get(path.join('/'))
    return this.#installed.has(path.join('/')) && file?.ownedObjectId === ownedObjectId
      ? file
      : undefined
  }

  async removeFile(path: readonly string[], ownedObjectId: string): Promise<void> {
    const key = path.join('/')
    if (this.#files.get(key)?.ownedObjectId !== ownedObjectId) {
      throw new TargetOwnershipUnknownError('cleanup', identity(1))
    }
    this.#files.delete(key)
  }

  async removeDirectory(): Promise<void> {}

  failNextCreation(): void {
    this.#failNextCreation = true
  }

  deferDirectory(path: readonly string[]): ControlledBarrier {
    const started = deferred<void>()
    const release = deferred<void>()
    this.#directoryBarriers.set(path.join('/'), { started, release })
    return Object.freeze({ started: started.promise, release: () => release.resolve() })
  }

  deferFileClose(path: readonly string[]): ControlledBarrier {
    const started = deferred<void>()
    const release = deferred<void>()
    this.#fileCloseBarriers.set(path.join('/'), { started, release })
    return Object.freeze({ started: started.promise, release: () => release.resolve() })
  }

  deferFileWrite(path: readonly string[]): Readonly<{
    accepted: Promise<void>
    release: () => void
  }> {
    const accepted = deferred<void>()
    const release = deferred<void>()
    this.#fileWriteBarriers.set(path.join('/'), { accepted, release })
    return Object.freeze({ accepted: accepted.promise, release: () => release.resolve() })
  }

  deferFileInspection(path: readonly string[]): ControlledFileInspection {
    const started = deferred<void>()
    const outcome = deferred<'absent' | 'occupied'>()
    this.#fileInspectionBarriers.set(path.join('/'), { started, outcome })
    return Object.freeze({
      started: started.promise,
      resolve: (result: 'absent' | 'occupied' = 'absent') => outcome.resolve(result),
      reject: (error: unknown) => outcome.reject(error),
    })
  }

  occupy(path: readonly string[], ownedObjectId: string, installed = false): void {
    const key = path.join('/')
    this.#files.set(key, new MemoryFile(ownedObjectId, Object.freeze({
      id: `handle:${ownedObjectId}`,
      operationId: this.#binding.operationId,
      kind: 1,
      authorityRef: this.#binding.authorityRef,
      ownedObjectId,
      handle: Object.freeze({ key }),
    })))
    if (installed) this.#installed.add(key)
  }

  proposedOwnedObjectId(path: readonly string[]): string {
    const ownedObjectId = this.#proposedByPath.get(path.join('/'))
    if (ownedObjectId === undefined) throw new Error('test file identity was not proposed')
    return ownedObjectId
  }

  visible(path: readonly string[]): Uint8Array | undefined {
    return this.#files.get(path.join('/'))?.snapshot()
  }

  file(path: readonly string[]): MemoryFile {
    const file = this.#files.get(path.join('/'))
    if (file === undefined) throw new Error('test file is missing')
    return file
  }

  failVerification(path: readonly string[], stage: VerificationStage, occurrence: number): void {
    this.#files.get(path.join('/'))?.failVerification(stage, occurrence)
  }
}

export function deferred<T>(): Deferred<T> {
  let resolve!: Deferred<T>['resolve']
  let reject!: Deferred<T>['reject']
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise
    reject = rejectPromise
  })
  return { promise, resolve, reject }
}

export function beginInitialClaim(
  session: PersistentMaterializationPort,
  path: readonly string[],
  revision: OpenedFileRevision,
) {
  return session.beginFile({
    materializationRelativePath: path,
    openRevision: async () => revision,
  })
}

export function seedOccupiedClaim(
  fixture: Readonly<{
    binding: FileCheckpointJournal['binding']
    tree: MemoryTree
    checkpoints: { seedCommittedForTest(record: FileCheckpointV2): void }
  }>,
  path: readonly string[],
  opened: OpenedFileRevision,
): void {
  const ownedObjectId = fixture.tree.proposedOwnedObjectId(path)
  fixture.tree.occupy(path, ownedObjectId, true)
  fixture.checkpoints.seedCommittedForTest(newFileCheckpointV2({
    ...persistentInitialCheckpoint(
      fixture.binding,
      opened,
      snapshotMaterializationRootRelativePath(path),
      ownedObjectId,
    ),
    commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
  }))
}

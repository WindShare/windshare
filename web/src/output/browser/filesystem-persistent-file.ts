import { bigintToSafeNumber } from '../../content/geometry'
import { outputCheckpointCost } from '../../transfer/output-file-contract'
import type { PersistentHandleRecord } from '../persistence/journal'
import type {
  PersistentTreeFile,
  PersistentWriterOpenMode,
  PersistentWriterPreflight,
} from '../persistent-tree/contracts'
import {
  runPersistentOutputStage,
  type PersistentOutputStageScope,
} from '../persistent-tree/stage-diagnostics'
import type { FSAVerifiedFileAuthority } from './mutation-coordination/authority-cache'
import type {
  FSAOperationMutationScheduler,
  FSAWriterLifecycleLease,
} from './mutation-coordination/model'

type BrowserFileWriterState =
  | 'not-created'
  | 'open'
  | 'closed'
  | 'aborted'
  | 'close-failed'
  | 'abort-failed'

export interface BrowserPersistentFile extends PersistentTreeFile {
  readonly persistedHandle: PersistentHandleRecord<unknown>
  openWriter(mode: PersistentWriterOpenMode): Promise<void>
  checkpointPreflight(
    durablePrefixBytes: bigint,
    cumulativeWriteAmplificationBytes: bigint,
  ): PersistentWriterPreflight
  abort(reason?: unknown): Promise<void>
}

export interface CreateBrowserPersistentFileInput {
  readonly authority: FSAVerifiedFileAuthority
  readonly persistedHandle: PersistentHandleRecord<FileSystemFileHandle>
  readonly scheduler: FSAOperationMutationScheduler
  readonly verify: PersistentTreeFile['verify']
  readonly stageScope?: PersistentOutputStageScope
}

export function createBrowserPersistentFile(
  input: CreateBrowserPersistentFileInput,
): BrowserPersistentFile {
  return new BrowserPersistentFileAuthority(input)
}

class BrowserPersistentFileAuthority implements BrowserPersistentFile {
  readonly ownedObjectId: string
  readonly persistedHandle: PersistentHandleRecord<FileSystemFileHandle>
  readonly #authority: FSAVerifiedFileAuthority
  readonly #scheduler: FSAOperationMutationScheduler
  readonly #verify: PersistentTreeFile['verify']
  readonly #stageScope: PersistentOutputStageScope | undefined
  #writer: FileSystemWritableFileStream | undefined
  #writerLease: FSAWriterLifecycleLease | undefined
  #writerOpening: Promise<FileSystemWritableFileStream> | undefined
  #writerMode: PersistentWriterOpenMode | undefined
  #writerFinalization: Promise<void> | undefined
  #writerFinalizationKind: 'close' | 'abort' | undefined
  #writerState: BrowserFileWriterState = 'not-created'
  #writerFinalizationFailure: unknown

  constructor(input: CreateBrowserPersistentFileInput) {
    this.ownedObjectId = input.authority.ownedObjectId
    this.persistedHandle = input.persistedHandle
    this.#authority = input.authority
    this.#scheduler = input.scheduler
    this.#verify = input.verify
    this.#stageScope = input.stageScope
  }

  async openWriter(mode: PersistentWriterOpenMode): Promise<void> {
    await this.#requireWriter(mode)
  }

  checkpointPreflight(
    durablePrefixBytes: bigint,
    cumulativeWriteAmplificationBytes: bigint,
  ): PersistentWriterPreflight {
    const current = outputCheckpointCost({
      prefixCopyBytes: durablePrefixBytes,
      cumulativeWriteAmplificationBytes,
      peakTemporaryBytes: 0n,
    })
    const cost = outputCheckpointCost({
      prefixCopyBytes: current.prefixCopyBytes,
      cumulativeWriteAmplificationBytes:
        current.cumulativeWriteAmplificationBytes + current.prefixCopyBytes,
      peakTemporaryBytes: current.prefixCopyBytes,
    })
    return Object.freeze({
      cost,
      // Native FSA exposes no authoritative capacity query for a user-picked directory.
      space: current.prefixCopyBytes === 0n
        ? 'within-modeled-budget' as const
        : 'requires-user-confirmation' as const,
    })
  }

  async writeAt(offset: bigint, data: Uint8Array): Promise<void> {
    this.#throwIfFinalizationFailed()
    const writer = this.#writer
    if (writer === undefined || this.#writerState !== 'open' ||
        this.#writerFinalization !== undefined) {
      throw new DOMException('The File System Access writer is not open', 'InvalidStateError')
    }
    await runPersistentOutputStage(
      this.#stageScope,
      'fsa.file.writer.write',
      () => writer.write({
        type: 'write',
        position: bigintToSafeNumber(offset, 'output offset'),
        data: data.slice(),
      }),
    )
  }

  async flush(): Promise<void> {
    await this.#finalizeWriter('close')
  }

  async abort(reason?: unknown): Promise<void> {
    await this.#finalizeWriter('abort', reason)
  }

  #finalizeWriter(kind: 'close' | 'abort', reason?: unknown): Promise<void> {
    this.#throwIfFinalizationFailed()
    if (this.#writerFinalization !== undefined) {
      return this.#writerFinalizationKind === kind
        ? this.#writerFinalization
        : Promise.reject(new DOMException(
            'The File System Access writer is already finalizing',
            'InvalidStateError',
          ))
    }
    const finalization = this.#runWriterFinalization(kind, reason)
    this.#writerFinalization = finalization
    this.#writerFinalizationKind = kind
    finalization.finally(() => {
      if (this.#writerFinalization === finalization) {
        this.#writerFinalization = undefined
        this.#writerFinalizationKind = undefined
      }
    }).catch(() => undefined)
    return finalization
  }

  async #runWriterFinalization(kind: 'close' | 'abort', reason?: unknown): Promise<void> {
    if (this.#writerOpening !== undefined) await this.#writerOpening
    const writer = this.#writer
    if (writer === undefined) return
    try {
      await runPersistentOutputStage(
        this.#stageScope,
        kind === 'close' ? 'fsa.file.writer.close' : 'fsa.file.writer.abort',
        async () => {
          try {
            if (kind === 'close') await writer.close()
            else await writer.abort(reason)
          } catch (cause) {
            this.#writerState = kind === 'close' ? 'close-failed' : 'abort-failed'
            this.#writerFinalizationFailure = cause
            this.#stageScope?.recordWriterCloseFailure(cause)
            throw cause
          }
          this.#writer = undefined
          this.#writerMode = undefined
          this.#writerState = kind === 'close' ? 'closed' : 'aborted'
          this.#writerFinalizationFailure = undefined
          this.#stageScope?.recordWriterClosed()
        },
      )
    } finally {
      this.#releaseWriterLease()
    }
  }

  async size(): Promise<bigint> {
    await this.flush()
    const file = await runPersistentOutputStage(
      this.#stageScope,
      'fsa.file.committed-bytes.read',
      () => this.#authority.handle.getFile(),
    )
    return BigInt(file.size)
  }

  verify(stage: 'writer-open' | 'checkpoint' | 'commit'): Promise<void> {
    return this.#verify(stage)
  }

  close(): Promise<void> {
    return this.flush()
  }

  async read(): Promise<Blob> {
    await this.flush()
    return runPersistentOutputStage(
      this.#stageScope,
      'fsa.file.committed-bytes.read',
      () => this.#authority.handle.getFile(),
    )
  }

  #requireWriter(mode: PersistentWriterOpenMode): Promise<FileSystemWritableFileStream> {
    this.#throwIfFinalizationFailed()
    if (mode !== 'preserve' && mode !== 'truncate') {
      return Promise.reject(new TypeError('Persistent writer open mode is invalid'))
    }
    if (this.#writerFinalization !== undefined) {
      return Promise.reject(new DOMException(
        'The File System Access writer is finalizing',
        'InvalidStateError',
      ))
    }
    if (this.#writer !== undefined) {
      return this.#writerMode === mode
        ? Promise.resolve(this.#writer)
        : Promise.reject(new DOMException(
            'The File System Access writer is already open in another mode',
            'InvalidStateError',
          ))
    }
    if (this.#writerOpening !== undefined) {
      return this.#writerMode === mode
        ? this.#writerOpening
        : Promise.reject(new DOMException(
            'The File System Access writer is opening in another mode',
            'InvalidStateError',
          ))
    }
    this.#writerMode = mode
    const opening = this.#openWriter(mode)
    this.#writerOpening = opening
    opening.finally(() => {
      if (this.#writerOpening === opening) this.#writerOpening = undefined
    }).catch(() => undefined)
    return opening
  }

  async #openWriter(mode: PersistentWriterOpenMode): Promise<FileSystemWritableFileStream> {
    const lease = await this.#scheduler.acquireWriter(this.#authority.parent.schedulerIdentity)
    this.#writerLease = lease
    try {
      await this.#verify('writer-open')
      const writer = await runPersistentOutputStage(
        this.#stageScope,
        'fsa.file.writer.create',
        () => this.#authority.handle.createWritable({
          keepExistingData: mode === 'preserve',
        }),
      )
      this.#writer = writer
      this.#writerState = 'open'
      this.#stageScope?.recordWriterOpened()
      return writer
    } catch (error) {
      this.#writerMode = undefined
      this.#releaseWriterLease()
      throw error
    }
  }

  #releaseWriterLease(): void {
    const lease = this.#writerLease
    if (lease === undefined) return
    this.#writerLease = undefined
    lease.release()
  }

  #throwIfFinalizationFailed(): void {
    if (this.#writerState === 'close-failed' || this.#writerState === 'abort-failed') {
      throw this.#writerFinalizationFailure
    }
  }
}

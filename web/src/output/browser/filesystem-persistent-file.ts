import { bigintToSafeNumber } from '../../content/geometry'
import type { PersistentTreeFile } from '../persistent-tree/contracts'
import {
  runPersistentOutputStage,
  type PersistentOutputStageScope,
} from '../persistent-tree/stage-diagnostics'
import type { FSANamespaceMutationKind } from './namespace-mutation'

type BrowserFileWriterState = 'not-created' | 'open' | 'closed' | 'close-failed'

export interface CreateBrowserPersistentFileInput {
  readonly ownedObjectId: string
  readonly handle: FileSystemFileHandle
  readonly verify: PersistentTreeFile['verify']
  readonly mutate: <Value>(
    kind: FSANamespaceMutationKind,
    operation: () => Promise<Value>,
  ) => Promise<Value>
  readonly stageScope?: PersistentOutputStageScope
}

export function createBrowserPersistentFile(
  input: CreateBrowserPersistentFileInput,
): PersistentTreeFile {
  return new BrowserPersistentFile(input)
}

class BrowserPersistentFile implements PersistentTreeFile {
  readonly ownedObjectId: string
  readonly #handle: FileSystemFileHandle
  readonly #verify: PersistentTreeFile['verify']
  readonly #mutate: CreateBrowserPersistentFileInput['mutate']
  readonly #stageScope: PersistentOutputStageScope | undefined
  #writer: FileSystemWritableFileStream | undefined
  #writerState: BrowserFileWriterState = 'not-created'
  #writerCloseFailure: unknown

  constructor(input: CreateBrowserPersistentFileInput) {
    this.ownedObjectId = input.ownedObjectId
    this.#handle = input.handle
    this.#verify = input.verify
    this.#mutate = input.mutate
    this.#stageScope = input.stageScope
  }

  async writeAt(offset: bigint, data: Uint8Array): Promise<void> {
    await this.#mutate('open-writer', async () => {
      this.#throwIfCloseFailed()
      if (this.#writer === undefined) {
        await this.#verify('writer-open')
        this.#writer = await runPersistentOutputStage(
          this.#stageScope,
          'fsa.file.writer.create',
          () => this.#handle.createWritable({ keepExistingData: true }),
        )
        this.#writerState = 'open'
        this.#stageScope?.recordWriterOpened()
      }
      const writer = this.#writer
      if (this.#writerState !== 'open') {
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
    })
  }

  async flush(): Promise<void> {
    this.#throwIfCloseFailed()
    const writer = this.#writer
    if (writer === undefined) return
    await this.#mutate('commit-file', () => runPersistentOutputStage(
      this.#stageScope,
      'fsa.file.writer.close',
      async () => {
        try {
          await writer.close()
        } catch (cause) {
          this.#writerState = 'close-failed'
          this.#writerCloseFailure = cause
          this.#stageScope?.recordWriterCloseFailure(cause)
          throw cause
        }
        this.#writer = undefined
        this.#writerState = 'closed'
        this.#writerCloseFailure = undefined
        this.#stageScope?.recordWriterClosed()
      },
    ))
  }

  async size(): Promise<bigint> {
    await this.flush()
    const file = await runPersistentOutputStage(
      this.#stageScope,
      'fsa.file.committed-bytes.read',
      () => this.#handle.getFile(),
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
      () => this.#handle.getFile(),
    )
  }

  #throwIfCloseFailed(): void {
    if (this.#writerState === 'close-failed') throw this.#writerCloseFailure
  }
}

import type {
  ChunkIndex,
  FileManifestEntry,
  OrderedBlockSink,
} from '../contracts'
import { ChunkAvailabilityMap } from './availability'
import { DownloadError } from './errors'
import type { DownloadSinkContext } from './model'
import { BlockProjector } from './projector'

type SinkState = 'open' | 'finalized' | 'aborted'

export class SingleFileDownloadSink implements OrderedBlockSink {
  readonly deliveryOrder = 'ascending' as const

  readonly #file: FileManifestEntry
  readonly #projector: BlockProjector
  readonly #writer: WritableStreamDefaultWriter<Uint8Array>
  readonly #have: ChunkAvailabilityMap

  #state: SinkState = 'open'
  #nextOffset = 0
  #writing = false

  constructor(projector: BlockProjector, output: WritableStream<Uint8Array>) {
    if (projector.entries.length !== 1 || projector.files.length !== 1) {
      throw new DownloadError(
        'invalid-plan',
        'Single-file output requires exactly one selected file and no directories',
      )
    }
    const file = projector.files[0]
    if (file === undefined) {
      throw new DownloadError('invalid-plan', 'Single-file output has no selected file')
    }
    if (output.locked) {
      throw new DownloadError('invalid-state', 'The single-file output stream is already locked')
    }
    this.#file = file
    this.#projector = projector
    this.#have = new ChunkAvailabilityMap(projector.chunks)
    this.#writer = output.getWriter()
  }

  has(index: ChunkIndex): boolean {
    return this.#have.has(index)
  }

  async writeBlock(index: ChunkIndex, plaintext: Uint8Array): Promise<void> {
    this.#assertOpen()
    if (this.#writing) {
      throw new DownloadError('out-of-order', 'Ordered output accepts only one block at a time')
    }
    if (this.#have.has(index)) {
      throw new DownloadError('duplicate-block', 'The block was already written')
    }
    this.#writing = true
    try {
      const slices = this.#projector.project(index, plaintext)
      for (const slice of slices) {
        if (slice.path !== this.#file.path || slice.offset !== this.#nextOffset) {
          throw new DownloadError(
            'out-of-order',
            'Single-file blocks must cover the file in ascending offset order',
          )
        }
        try {
          await this.#writer.write(slice.data)
        } catch (error) {
          throw new DownloadError('output-write', 'Could not write single-file output', error)
        }
        this.#nextOffset += slice.data.byteLength
      }
      this.#assertOpen()
      this.#have.add(index)
    } finally {
      this.#writing = false
    }
  }

  async finalize(): Promise<void> {
    this.#assertOpen()
    if (this.#writing) {
      throw new DownloadError('invalid-state', 'Cannot finalize while a block write is active')
    }
    if (this.#nextOffset !== this.#file.size) {
      throw new DownloadError(
        'output-finalize',
        'Single-file output is incomplete and cannot be finalized',
      )
    }
    try {
      await this.#writer.close()
      this.#state = 'finalized'
    } catch (error) {
      this.#state = 'aborted'
      this.#have.clear()
      throw new DownloadError('output-finalize', 'Could not finalize single-file output', error)
    } finally {
      this.#writer.releaseLock()
    }
  }

  async abort(reason: unknown): Promise<void> {
    if (this.#state !== 'open') {
      return
    }
    this.#state = 'aborted'
    try {
      await this.#writer.abort(reason)
    } catch (error) {
      throw new DownloadError('cleanup-failed', 'Could not abort single-file output', error)
    } finally {
      this.#have.clear()
      this.#writer.releaseLock()
    }
  }

  #assertOpen(): void {
    if (this.#state !== 'open') {
      throw new DownloadError('invalid-state', 'The single-file output is no longer open')
    }
  }
}

export function createSingleFileDownloadSink(
  context: DownloadSinkContext,
  output: WritableStream<Uint8Array>,
): SingleFileDownloadSink {
  const projector = new BlockProjector(context)
  return new SingleFileDownloadSink(projector, output)
}

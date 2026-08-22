import type { SelectionMeasure } from '../measure'
import type {
  DirectZipOrderedFileV1,
  DirectZipOrderedOutputV1,
  DirectZipOrderedSourceV1,
} from './model'

export interface DirectZipOrderedCoordinatorOptionsV1 {
  readonly source: DirectZipOrderedSourceV1
  readonly output: DirectZipOrderedOutputV1
  readonly signal: AbortSignal
  readonly transferFile: (file: DirectZipOrderedFileV1) => Promise<void>
  readonly observeSelectedFile: (exactSize: bigint) => void
  readonly observeReplayedFile: (exactSize: bigint) => void
  readonly finishMeasure: () => SelectionMeasure
}

/**
 * Catalog I/O may prefetch internally, but this loop is deliberately serial: no
 * collaborator completion can reorder archive membership or overlap two file writers.
 */
export class DirectZipOrderedCoordinatorV1 {
  readonly #options: DirectZipOrderedCoordinatorOptionsV1

  constructor(options: DirectZipOrderedCoordinatorOptionsV1) {
    this.#options = options
  }

  async run(): Promise<SelectionMeasure> {
    const { signal } = this.#options
    signal.throwIfAborted()
    await this.#options.output.beginTraversal(await this.#options.source.root(signal), signal)
    let ordinal = 1n // Bootstrap owns the marker-bearing result-root member at ordinal zero.
    for await (const member of this.#options.source.members(signal)) {
      signal.throwIfAborted()
      if (member.kind === 'file') this.#options.observeSelectedFile(member.expectedSize)
      const visit = await this.#options.output.visit(ordinal, member, signal)
      if (member.kind === 'file') {
        if (visit === 'transfer-file') await this.#options.transferFile(member)
        else if (visit === 'replayed') this.#options.observeReplayedFile(member.expectedSize)
        else throw new TypeError('direct ZIP file admission returned a directory disposition')
      } else if (visit === 'transfer-file') {
        throw new TypeError('direct ZIP directory admission requested file transfer')
      }
      ordinal += 1n
    }
    await this.#options.output.finishTraversal(ordinal, signal)
    return this.#options.finishMeasure()
  }
}

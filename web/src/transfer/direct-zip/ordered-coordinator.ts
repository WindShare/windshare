import type { SelectionMeasure } from '../measure'
import { AsyncBoundedQueue, pendingFileMetadataBytes } from '../job/scheduler'
import { superviseWorkerFamily } from '../worker-family/supervisor'
import { DiscoveryQueue, type DiscoverySchedulingObservation } from '../discovery/queue'
import type {
  DirectZipOrderedFileV1,
  DirectZipOrderedMemberV1,
  DirectZipOrderedOutputV1,
  DirectZipOrderedSourceV1,
} from './model'

const MAXIMUM_PENDING_MEMBERS = 256
const MAXIMUM_PENDING_MEMBER_METADATA_BYTES = 16n * 1024n * 1024n
const MEMBER_STRUCTURAL_METADATA_BYTES = 1024n
const TEXT_CODE_UNIT_BYTES = 2n

export interface DirectZipOrderedCoordinatorOptionsV1 {
  readonly source: DirectZipOrderedSourceV1
  readonly output: DirectZipOrderedOutputV1
  readonly signal: AbortSignal
  readonly transferFile: (file: DirectZipOrderedFileV1, signal: AbortSignal) => Promise<void>
  readonly observeSelectedFile: (exactSize: bigint) => void
  readonly observeReplayedFile: (exactSize: bigint) => void
  readonly observeDiscovery?: (event: DiscoverySchedulingObservation) => void
  readonly finishMeasure: () => SelectionMeasure
}

/**
 * A bounded catalog producer can establish totals while the serial consumer
 * preserves canonical archive order and exclusive ownership of the ZIP writer.
 */
export class DirectZipOrderedCoordinatorV1 {
  readonly #options: DirectZipOrderedCoordinatorOptionsV1

  constructor(options: DirectZipOrderedCoordinatorOptionsV1) {
    this.#options = options
  }

  async run(): Promise<SelectionMeasure> {
    const lifetime = new AbortController()
    const signal = AbortSignal.any([this.#options.signal, lifetime.signal])
    signal.throwIfAborted()
    await this.#options.output.beginTraversal(await this.#options.source.root(signal), signal)
    const queue = new DiscoveryQueue<DirectZipOrderedMemberV1>({
      queue: 'zip_members',
      maximumItems: MAXIMUM_PENDING_MEMBERS,
      maximumMetadataBytes: MAXIMUM_PENDING_MEMBER_METADATA_BYTES,
      weight: memberMetadataBytes,
      ...(this.#options.observeDiscovery === undefined ? {} : { observe: this.#options.observeDiscovery }),
    })
    let measure: SelectionMeasure | undefined
    const producer = (async () => {
      for await (const member of this.#options.source.members(signal)) {
        signal.throwIfAborted()
        if (member.kind === 'file') this.#options.observeSelectedFile(member.expectedSize)
        await queue.push(member, signal)
      }
      signal.throwIfAborted()
      measure = this.#options.finishMeasure()
    })()
    const consumer = this.#consume(queue, signal)
    await superviseWorkerFamily({
      producer,
      workers: [consumer],
      queues: [queue],
      abort: failure => lifetime.abort(failure),
    })
    if (measure === undefined) throw new Error('Direct ZIP discovery ended without a selection measure')
    return measure
  }

  async #consume(queue: AsyncBoundedQueue<DirectZipOrderedMemberV1>, signal: AbortSignal): Promise<void> {
    let ordinal = 1n // Bootstrap owns the marker-bearing result-root member at ordinal zero.
    while (true) {
      const member = await queue.pop(signal)
      if (member === undefined) break
      const visit = await this.#options.output.visit(ordinal, member, signal)
      if (member.kind === 'file') {
        if (visit === 'transfer-file') await this.#options.transferFile(member, signal)
        else if (visit === 'replayed') this.#options.observeReplayedFile(member.expectedSize)
        else throw new TypeError('direct ZIP file admission returned a directory disposition')
      } else if (visit === 'transfer-file') {
        throw new TypeError('direct ZIP directory admission requested file transfer')
      }
      ordinal += 1n
    }
    await this.#options.output.finishTraversal(ordinal, signal)
  }
}

function memberMetadataBytes(member: DirectZipOrderedMemberV1): bigint {
  const textBytes = [...member.sourcePath, ...member.artifactPath]
    .reduce((bytes, value) => bytes + BigInt(value.length) * TEXT_CODE_UNIT_BYTES, 0n)
  return MEMBER_STRUCTURAL_METADATA_BYTES + textBytes +
    BigInt(member.layoutEvidence.byteLength + member.discoveryEvidence.byteLength) +
    (member.kind === 'file' ? pendingFileMetadataBytes(member.pending) : 0n)
}

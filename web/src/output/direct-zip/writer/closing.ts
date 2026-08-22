import {
  DIRECT_ZIP_CENTRAL_HEADER_FIXED_BYTES,
  DIRECT_ZIP_MAXIMUM_ROOT_NAME_BYTES,
  DIRECT_ZIP_OWNERSHIP_EXTRA_FIELD_BYTES,
  equalDirectZipOwnershipMarkersV1,
  parseDirectZipOwnershipCentralRecordV1,
  parseDirectZipOwnershipLocalHeaderV1,
  planDirectZipClosingLayoutV2,
  requireMatchingDirectZipOwnershipRecordsV1,
  validateDirectZipClosingTailV2,
} from '../format'
import { equalDirectZipBytes } from '../format/canonical'
import { ZIP_ENCODING_POLICY_V1, checkedZipAdd } from '../../zip-layout/policy'
import {
  requireCompletionSeal,
  snapshotCheckpoint,
} from './checkpoint-state'
import { DirectZipWriterGateError } from './gates'
import type {
  DirectZipCompletionProofV1,
  DirectZipCompletionSealV1,
  DirectZipWriterCheckpointV1,
} from './model'
import type {
  DirectZipTargetVerificationPort,
  DirectZipWriterContextV1,
  DirectZipWriterCutSink,
  DirectZipWriterPageSink,
  DirectZipWriterTraceEventV1,
} from './ports'

export interface DirectZipCompletionValidationInput {
  readonly seal: DirectZipCompletionSealV1
  readonly rootCentralRecord: Uint8Array
}

type DirectZipWriterEmit = (
  kind: DirectZipWriterTraceEventV1['kind'],
  extra?: Omit<DirectZipWriterTraceEventV1, 'kind' | 'operationId' | 'checkpointGeneration' |
    'phase' | 'archiveOffset'>,
) => void

type DirectZipArchiveWrite = (
  bytes: Uint8Array,
  offsetClass: NonNullable<DirectZipWriterTraceEventV1['offsetClass']>,
) => Promise<void>

const DIRECT_ZIP_MAXIMUM_ROOT_CENTRAL_RECORD_BYTES =
  DIRECT_ZIP_CENTRAL_HEADER_FIXED_BYTES + DIRECT_ZIP_MAXIMUM_ROOT_NAME_BYTES +
  DIRECT_ZIP_OWNERSHIP_EXTRA_FIELD_BYTES

/** Closing owns the bounded proof protocol so the mutable writer never interprets target bytes. */
export class DirectZipClosingCoordinator {
  readonly #context: DirectZipWriterContextV1
  readonly #pages: DirectZipWriterPageSink
  readonly #cuts: DirectZipWriterCutSink
  readonly #target: DirectZipTargetVerificationPort
  readonly #writeArchive: DirectZipArchiveWrite
  readonly #archiveOffset: () => bigint
  readonly #emit: DirectZipWriterEmit

  constructor(input: Readonly<{
    context: DirectZipWriterContextV1
    pages: DirectZipWriterPageSink
    cuts: DirectZipWriterCutSink
    target: DirectZipTargetVerificationPort
    writeArchive: DirectZipArchiveWrite
    archiveOffset: () => bigint
    emit: DirectZipWriterEmit
  }>) {
    this.#context = input.context
    this.#pages = input.pages
    this.#cuts = input.cuts
    this.#target = input.target
    this.#writeArchive = input.writeArchive
    this.#archiveOffset = input.archiveOffset
    this.#emit = input.emit
  }

  async stageClosingCheckpoint(
    predecessor: DirectZipWriterCheckpointV1,
    seal: DirectZipCompletionSealV1,
  ): Promise<DirectZipWriterCheckpointV1> {
    const closing = Object.freeze({
      centralDirectoryOffset: predecessor.committedLength,
      centralDirectoryBytes: seal.centralDirectoryBytes,
      replayStartOrdinal: 0n,
    })
    const checkpoint: DirectZipWriterCheckpointV1 = Object.freeze({
      ...snapshotCheckpoint(predecessor),
      generation: predecessor.generation + 1n,
      phase: 'closing',
      archiveOffset: predecessor.committedLength,
      closing,
    })
    await this.#cuts.enterClosing({
      predecessorGeneration: predecessor.generation,
      checkpoint,
    })
    return checkpoint
  }

  async writeClosingRecords(
    checkpoint: DirectZipWriterCheckpointV1,
    seal: DirectZipCompletionSealV1,
  ): Promise<DirectZipCompletionValidationInput> {
    const closing = checkpoint.closing
    if (closing === undefined) throw new Error('direct ZIP closing checkpoint is absent')
    let expectedOrdinal = closing.replayStartOrdinal
    let centralBytes = 0n
    let rootCentralRecord: Uint8Array | undefined
    for await (const record of this.#pages.replayCentral(checkpoint.pages)) {
      if (record.ordinal !== expectedOrdinal || !(record.bytes instanceof Uint8Array)) {
        throw new Error('direct ZIP central-directory replay order changed')
      }
      if (record.ordinal === 0n) rootCentralRecord = Uint8Array.from(record.bytes)
      await this.#writeArchive(record.bytes, 'central-directory')
      centralBytes = checkedZipAdd(centralBytes, BigInt(record.bytes.byteLength))
      expectedOrdinal += 1n
    }
    if (expectedOrdinal !== seal.entryCount || centralBytes !== seal.centralDirectoryBytes ||
        rootCentralRecord === undefined) {
      throw new Error('direct ZIP central-directory replay disagrees with its sealed pages')
    }
    const layout = planDirectZipClosingLayoutV2({
      entryCount: seal.entryCount,
      centralDirectoryOffset: closing.centralDirectoryOffset,
      centralDirectoryBytes: centralBytes,
    })
    const ends = ZIP_ENCODING_POLICY_V1.encodeEndRecords(layout)
    if (ends.zip64End !== undefined) await this.#writeArchive(ends.zip64End, 'closing-tail')
    if (ends.zip64Locator !== undefined) await this.#writeArchive(ends.zip64Locator, 'closing-tail')
    await this.#writeArchive(ends.classicEnd, 'closing-tail')
    if (this.#archiveOffset() !== layout.exactArchiveBytes) {
      throw new Error('direct ZIP closing length changed during replay')
    }
    return Object.freeze({ seal, rootCentralRecord })
  }

  async completionInput(
    checkpoint: DirectZipWriterCheckpointV1,
    seal: DirectZipCompletionSealV1,
    preClosingEpochRoot: Uint8Array = checkpoint.epochRoot,
  ): Promise<DirectZipCompletionValidationInput> {
    const pages = await this.#pages.snapshot()
    requireCompletionSeal(seal, pages, checkpoint.nextEntryOrdinal, preClosingEpochRoot)
    for await (const record of this.#pages.replayCentral(pages)) {
      if (record.ordinal === 0n) {
        return Object.freeze({ seal, rootCentralRecord: Uint8Array.from(record.bytes) })
      }
      break
    }
    throw new Error('direct ZIP root central record is absent')
  }

  async validateCompletion(
    checkpoint: DirectZipWriterCheckpointV1,
    input: DirectZipCompletionValidationInput,
  ): Promise<DirectZipCompletionProofV1> {
    const closing = checkpoint.closing
    if (closing === undefined) throw new Error('direct ZIP completion checkpoint is not closing')
    const layout = planDirectZipClosingLayoutV2({
      entryCount: input.seal.entryCount,
      centralDirectoryOffset: closing.centralDirectoryOffset,
      centralDirectoryBytes: closing.centralDirectoryBytes,
    })
    if (checkpoint.committedLength !== layout.exactArchiveBytes) {
      throw new Error('direct ZIP completion length disagrees with the closing layout')
    }
    if (input.rootCentralRecord.byteLength > DIRECT_ZIP_MAXIMUM_ROOT_CENTRAL_RECORD_BYTES) {
      throw new Error('direct ZIP ownership central record exceeds its derived read bound')
    }
    const bounded = await this.#target.readBoundedCompletionProof({
      exactArchiveBytes: layout.exactArchiveBytes,
      rootCentralRecordOffset: layout.centralDirectoryOffset,
      rootCentralRecordBytes: BigInt(input.rootCentralRecord.byteLength),
      closingTailBytes: Number(layout.closingTailBytes),
    })
    if (!equalDirectZipBytes(bounded.observationDigest, checkpoint.targetObservationDigest) ||
        !equalDirectZipBytes(bounded.rootCentralRecord, input.rootCentralRecord)) {
      throw new DirectZipWriterGateError(
        'target-verification-required',
        'bounded completion observation changed',
      )
    }
    const local = parseDirectZipOwnershipLocalHeaderV1(bounded.localOwnershipHeader)
    const central = parseDirectZipOwnershipCentralRecordV1(bounded.rootCentralRecord)
    requireMatchingDirectZipOwnershipRecordsV1(local, central)
    if (local.rootComponent !== this.#context.rootComponent ||
        !equalDirectZipOwnershipMarkersV1(local.marker, this.#context.ownershipMarker)) {
      throw new DirectZipWriterGateError(
        'target-verification-required',
        'completion ownership proof changed',
      )
    }
    validateDirectZipClosingTailV2(bounded.closingTail, layout)
    this.#emit('completion-verified', { offsetClass: 'closing-tail' })
    return Object.freeze({
      checkpoint: snapshotCheckpoint(checkpoint),
      exactArchiveBytes: layout.exactArchiveBytes,
      finalEpochRoot: Uint8Array.from(checkpoint.epochRoot),
      targetObservationDigest: Uint8Array.from(checkpoint.targetObservationDigest),
    })
  }
}

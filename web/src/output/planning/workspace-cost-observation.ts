import {
  ZIP_CLASSIC_END_BYTES,
  ZIP64_END_BYTES,
  ZIP_UINT32_SENTINEL,
  checkedZipAdd,
  normalizeZipEntry,
  planZipEntry,
  requireZipSpoolBudget,
  requiresZip64End,
  type ZipEntrySpec,
} from '../zip-layout/policy'
import type { WorkspaceCostObservationV1 } from '../../transfer/projection'

/**
 * This accumulator keeps only checked numeric totals. It is passive recommendation
 * evidence, never the manifest or capacity claim used by workspace activation.
 */
export class WorkspaceCostObservationAccumulatorV1 {
  #rawBytes = 0n
  #localBytes = 0n
  #centralBytes = 0n
  #entryCount = 0n
  #finished = false

  observe(spec: ZipEntrySpec): void {
    if (this.#finished) throw new TypeError('workspace cost observation is already complete')
    const entry = normalizeZipEntry(spec)
    const plan = planZipEntry(entry, this.#localBytes)
    this.#localBytes = checkedZipAdd(this.#localBytes, plan.entryStreamBytes)
    this.#centralBytes = checkedZipAdd(this.#centralBytes, plan.centralRecordBytes)
    this.#entryCount = checkedZipAdd(this.#entryCount, 1n)
    if (entry.kind === 'file') this.#rawBytes = checkedZipAdd(this.#rawBytes, entry.exactSize)
  }

  complete(): WorkspaceCostObservationV1 {
    if (this.#finished) throw new TypeError('workspace cost observation can complete only once')
    this.#finished = true
    requireZipSpoolBudget(this.#entryCount, this.#centralBytes)
    if (this.#localBytes >= ZIP_UINT32_SENTINEL) {
      throw new RangeError('workspace cost observation requires canonical ZIP64 entry ordering')
    }
    const endRequired = requiresZip64End({
      entryCount: this.#entryCount,
      centralDirectoryOffset: this.#localBytes,
      centralDirectoryBytes: this.#centralBytes,
    })
    const packageBytes = checkedZipAdd(
      this.#localBytes,
      this.#centralBytes,
      endRequired ? ZIP64_END_BYTES : ZIP_CLASSIC_END_BYTES,
    )
    // Recommendation metadata is deliberately projected from the canonical
    // central-record footprint; activation later measures its exact durable records.
    const durableMetadataBytes = this.#centralBytes
    const peakOwnedBytes = checkedZipAdd(
      this.#rawBytes,
      packageBytes,
      this.#centralBytes,
      durableMetadataBytes,
    )
    return Object.freeze({
      version: 1,
      rawBytes: this.#rawBytes,
      packageBytes,
      centralDirectorySpoolBytes: this.#centralBytes,
      durableMetadataBytes,
      peakOwnedBytes,
    })
  }
}

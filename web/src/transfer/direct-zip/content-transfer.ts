import type { V2ShareDescriptor } from '../../catalog/v2-records'
import { bigintToSafeNumber, byteRange } from '../../content/geometry'
import {
  V2_BLOCK_BROKER_PARALLEL_READS,
  type V2BlockRangeReader,
} from '../../content/v2-broker'
import type { V2OpenedRevision, V2RevisionReader } from '../../content/v2-session-services'
import {
  canonicalDigest, canonicalFrame, canonicalRecord, canonicalText, canonicalU64,
} from '../../output/workspace/canonical'
import { validateOpenedFileRevision } from '../job/file-authority'
import type { DirectZipOrderedFileV1, DirectZipOutputSessionV1 } from './model'

export interface DirectZipContentTransferOptionsV1 {
  readonly descriptor: V2ShareDescriptor
  readonly revisions: V2RevisionReader
  readonly broker: V2BlockRangeReader
  readonly output: DirectZipOutputSessionV1
  readonly signal: AbortSignal
  readonly onInitialDurable: (bytes: bigint) => void
  readonly onWriteAcknowledged: (bytes: bigint) => void
  readonly onComplete: (exactSize: bigint) => void
}

/** One authenticated revision lease remains live until its ordered member is settled. */
export async function transferDirectZipFileV1(
  options: DirectZipContentTransferOptionsV1,
  file: DirectZipOrderedFileV1,
): Promise<void> {
  let opened: V2OpenedRevision | undefined
  let primaryFailure: unknown
  try {
    opened = validateOpenedFileRevision(
      options.descriptor,
      file.pending.entry,
      await options.revisions.open(file.pending.entry.id, options.signal),
    )
    const transaction = await options.output.beginFile(file, Object.freeze({
      fileId: opened.descriptor.fileIdText,
      revision: opened.descriptor.fileRevisionText,
      exactSize: opened.descriptor.exactSize,
      // Revision identity authenticates the complete geometry; the explicit label
      // keeps range authority distinct from a transient remote lease identifier.
      rangeAuthority: await canonicalDigest(canonicalRecord('windshare/source-range', 1, [
        canonicalFrame(canonicalText(opened.descriptor.fileRevisionText)),
        canonicalU64(opened.descriptor.geometry.blockSize),
      ])),
    }), options.signal)
    let offset = transaction.resumeOffset
    options.onInitialDurable(offset)
    while (offset < opened.descriptor.exactSize) {
      const end = minimum(
        (offset / opened.descriptor.geometry.blockSize + 1n) * opened.descriptor.geometry.blockSize,
        opened.descriptor.exactSize,
      )
      const data = await readAtomicRange(options, opened, offset, end)
      await transaction.write(offset, data, options.signal)
      options.onWriteAcknowledged(BigInt(data.byteLength))
      offset = end
      // EOF leads straight into member completion and may be the archive's last
      // write. A cut here would copy the whole prefix merely to append its tail.
      if (offset < opened.descriptor.exactSize) await transaction.observeCheckpoint(options.signal)
    }
    await transaction.commit(options.signal)
    options.onComplete(opened.descriptor.exactSize)
  } catch (error) {
    primaryFailure = error
  }

  let releaseFailure: unknown
  if (opened !== undefined) {
    try {
      await opened.release()
    } catch (error) {
      releaseFailure = error
    }
  }
  if (primaryFailure !== undefined && releaseFailure !== undefined) {
    throw new AggregateError(
      [primaryFailure, releaseFailure],
      'direct ZIP content transfer and revision release both failed',
      { cause: primaryFailure },
    )
  }
  if (primaryFailure !== undefined) throw primaryFailure
  if (releaseFailure !== undefined) throw releaseFailure
}

async function readAtomicRange(
  options: DirectZipContentTransferOptionsV1,
  opened: V2OpenedRevision,
  start: bigint,
  end: bigint,
): Promise<Uint8Array<ArrayBuffer>> {
  const data = new Uint8Array(bigintToSafeNumber(end - start, 'direct ZIP atomic source range'))
  let covered = start
  for await (const slice of options.broker.readRange(
    opened.descriptor,
    opened.leaseId,
    byteRange(start, end),
    {
      signal: options.signal,
      maximumParallel: V2_BLOCK_BROKER_PARALLEL_READS,
      priority: 'download',
    },
  )) {
    options.signal.throwIfAborted()
    if (!(slice.data instanceof Uint8Array) || slice.data.byteLength === 0 ||
        slice.offset !== covered || covered + BigInt(slice.data.byteLength) > end) {
      throw new Error('direct ZIP range reader escaped, overlapped, or left a source gap')
    }
    data.set(slice.data, bigintToSafeNumber(covered - start, 'direct ZIP atomic slice offset'))
    covered += BigInt(slice.data.byteLength)
  }
  if (covered !== end) throw new Error('direct ZIP range reader ended before its authenticated range')
  return data
}

function minimum(left: bigint, right: bigint): bigint {
  return left < right ? left : right
}

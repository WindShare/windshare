import type { CatalogModifiedTime, ShareDescriptor } from '../catalog/records'
import { encodeBase64Url, equalBytes } from '../crypto/bytes'
import {
  decodeCanonicalCbor,
  requireBytes,
  requireNumericMap,
  requireSigned,
  requireUnsigned,
  CborError,
} from '../protocol/cbor'
import { FileGeometry } from './geometry'

export const CONTENT_SEGMENT_BYTES = 16n << 30n
export const MAXIMUM_BLOCK_RECORD_OBJECT_BYTES = (4 << 20) + 512
export const MAXIMUM_REVISION_OBJECT_BYTES = 16 << 10

const MAXIMUM_SAFE_INTEGER = BigInt(Number.MAX_SAFE_INTEGER)

export interface FileRevisionDescriptor {
  readonly shareInstance: Uint8Array<ArrayBuffer>
  readonly shareInstanceId: string
  readonly fileId: Uint8Array<ArrayBuffer>
  readonly fileIdText: string
  readonly fileRevision: Uint8Array<ArrayBuffer>
  readonly fileRevisionText: string
  readonly exactSize: bigint
  readonly geometry: FileGeometry
  readonly modifiedTime?: CatalogModifiedTime
  readonly sourceMetadata?: string
}

export interface BlockRecord {
  readonly descriptor: FileRevisionDescriptor
  readonly localBlockIndex: bigint
  readonly data: Uint8Array<ArrayBuffer>
}

export function decodeRevisionPayload(
  payload: Uint8Array, share: ShareDescriptor, fileId: Uint8Array,
): FileRevisionDescriptor {
  return decodeRevision(payload, share, fileId)
}

export function decodeBlockPayload(
  payload: Uint8Array, descriptor: FileRevisionDescriptor, localBlockIndex: bigint,
): BlockRecord {
  const range = descriptor.geometry.blockPlaintext(localBlockIndex)
  return decodeBlock(payload, descriptor, localBlockIndex, Number(range.end - range.start))
}

function decodeRevision(
  plaintext: Uint8Array,
  share: ShareDescriptor,
  expectedFileId: Uint8Array,
): FileRevisionDescriptor {
  const fields = requireNumericMap(
    decodeCanonicalCbor(plaintext, MAXIMUM_REVISION_OBJECT_BYTES, 'file revision'),
    [0, 1, 2, 3, 4, 5, 6, 7, 8],
    'file revision',
  )
  requireSchema(fields.get(0), 'file revision', 2n)
  const shareInstance = requireBytes(fields.get(1), 16, 'revision share instance', true)
  const fileId = requireBytes(fields.get(2), 16, 'revision file ID', true)
  if (!equalBytes(shareInstance, share.shareInstance) || !equalBytes(fileId, expectedFileId)) {
    throw new CborError('File revision changes its authenticated identity')
  }
  const fileRevision = requireBytes(fields.get(3), 16, 'file revision identity', true)
  const exactSize = requireUnsigned(fields.get(4), 'file revision exact size')
  if (exactSize > MAXIMUM_SAFE_INTEGER) {
    throw new CborError('File revision exact size exceeds the browser wire range')
  }
  const modifiedTime = decodeModifiedTime(fields.get(5), fields.get(6), fields.get(7))
  const metadata = requireBytes(fields.get(8), 32, 'source metadata')
  const sourceMetadata = metadata.every(byte => byte === 0) ? undefined : encodeBase64Url(metadata)
  return Object.freeze({
    shareInstance,
    shareInstanceId: encodeBase64Url(shareInstance),
    fileId,
    fileIdText: encodeBase64Url(fileId),
    fileRevision,
    fileRevisionText: encodeBase64Url(fileRevision),
    exactSize,
    geometry: new FileGeometry(exactSize, BigInt(share.chunkSize)),
    ...(modifiedTime === undefined ? {} : { modifiedTime }),
    ...(sourceMetadata === undefined ? {} : { sourceMetadata }),
  })
}

function decodeBlock(
  plaintext: Uint8Array,
  descriptor: FileRevisionDescriptor,
  expectedIndex: bigint,
  expectedLength: number,
): BlockRecord {
  const fields = requireNumericMap(
    decodeCanonicalCbor(plaintext, MAXIMUM_BLOCK_RECORD_OBJECT_BYTES, 'block record'),
    [0, 1, 2, 3, 4, 5],
    'block record',
  )
  requireSchema(fields.get(0), 'block record')
  const shareInstance = requireBytes(fields.get(1), 16, 'block share instance', true)
  const fileId = requireBytes(fields.get(2), 16, 'block file ID', true)
  const fileRevision = requireBytes(fields.get(3), 16, 'block revision', true)
  const index = requireUnsigned(fields.get(4), 'local block index')
  const data = requireBytes(fields.get(5), expectedLength, 'block data')
  if (
    !equalBytes(shareInstance, descriptor.shareInstance) ||
    !equalBytes(fileId, descriptor.fileId) ||
    !equalBytes(fileRevision, descriptor.fileRevision) ||
    index !== expectedIndex
  ) {
    throw new CborError('Block record changes its file-local authenticated identity')
  }
  return Object.freeze({ descriptor, localBlockIndex: index, data })
}

function decodeModifiedTime(
  secondsValue: unknown,
  nanosecondsValue: unknown,
  precisionValue: unknown,
): CatalogModifiedTime | undefined {
  const nanoseconds = requireUnsigned(nanosecondsValue, 'revision modified nanoseconds')
  const precision = requireUnsigned(precisionValue, 'revision modified precision')
  if (secondsValue === null) {
    if (nanoseconds !== 0n || precision !== 0n) {
      throw new CborError('Absent revision modified time carries data')
    }
    return undefined
  }
  const seconds = requireSigned(secondsValue, 'revision modified seconds')
  if (
    seconds < -MAXIMUM_SAFE_INTEGER || seconds > MAXIMUM_SAFE_INTEGER ||
    nanoseconds >= 1_000_000_000n || precision < 1n || precision > 3n ||
    (precision === 1n && nanoseconds !== 0n) ||
    (precision === 2n && nanoseconds % 1_000_000n !== 0n)
  ) {
    throw new CborError('Revision modified time is outside its portable precision')
  }
  return Object.freeze({
    seconds,
    nanoseconds: Number(nanoseconds),
    precision: Number(precision) as 1 | 2 | 3,
    milliseconds: seconds * 1_000n + nanoseconds / 1_000_000n,
  })
}

function requireSchema(value: unknown, label: string, version = 1n): void {
  if (requireUnsigned(value, `${label} schema`) !== version) {
    throw new CborError(`${label} schema is unsupported`)
  }
}

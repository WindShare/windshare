import type { V2CatalogModifiedTime, V2ShareDescriptor } from '../catalog/v2-records'
import { encodeBase64Url, equalBytes } from '../crypto/bytes'
import {
  createBlockRecordObjectBinding,
  createRevisionObjectBinding,
  openSenderObject,
} from '../crypto/sender-object'
import {
  deriveSuite02FileObjectKey,
  deriveSuite02FileSegmentKey,
  deriveSuite02RevisionKey,
} from '../crypto/suite02-key-derivation'
import {
  decodeCanonicalCbor,
  requireBytes,
  requireNumericMap,
  requireSigned,
  requireUnsigned,
  V2CborError,
} from '../protocol/cbor'
import { FileGeometry } from './geometry'

export const V2_CONTENT_SEGMENT_BYTES = 16n << 30n
export const V2_MAXIMUM_BLOCK_RECORD_OBJECT_BYTES = (4 << 20) + 512
export const V2_MAXIMUM_REVISION_OBJECT_BYTES = 16 << 10

const MAXIMUM_SAFE_INTEGER = BigInt(Number.MAX_SAFE_INTEGER)

export interface V2FileRevisionDescriptor {
  readonly shareInstance: Uint8Array<ArrayBuffer>
  readonly shareInstanceId: string
  readonly fileId: Uint8Array<ArrayBuffer>
  readonly fileIdText: string
  readonly fileRevision: Uint8Array<ArrayBuffer>
  readonly fileRevisionText: string
  readonly exactSize: bigint
  readonly geometry: FileGeometry
  readonly modifiedTime?: V2CatalogModifiedTime
}

export interface V2BlockRecord {
  readonly descriptor: V2FileRevisionDescriptor
  readonly localBlockIndex: bigint
  readonly data: Uint8Array<ArrayBuffer>
}

export async function openV2RevisionObject(
  object: Uint8Array,
  descriptor: V2ShareDescriptor,
  readSecret: Uint8Array,
  fileId: Uint8Array,
): Promise<V2FileRevisionDescriptor> {
  if (object.byteLength === 0 || object.byteLength > V2_MAXIMUM_REVISION_OBJECT_BYTES) {
    throw new V2CborError('File revision object exceeds its limit')
  }
  const fileObjectKey = await deriveSuite02FileObjectKey(
    readSecret,
    descriptor.shareInstance,
    fileId,
  )
  try {
    const binding = createRevisionObjectBinding(descriptor.shareInstance, fileId)
    const plaintext = await openSenderObject(
      binding,
      fileObjectKey,
      descriptor.senderPublicKey,
      object,
    )
    return decodeRevision(plaintext, descriptor, fileId)
  } finally {
    fileObjectKey.fill(0)
  }
}

export async function openV2BlockRecord(
  object: Uint8Array,
  share: V2ShareDescriptor,
  readSecret: Uint8Array,
  descriptor: V2FileRevisionDescriptor,
  localBlockIndex: bigint,
): Promise<V2BlockRecord> {
  const blockRange = descriptor.geometry.blockPlaintext(localBlockIndex)
  const dataLength = Number(blockRange.end - blockRange.start)
  if (object.byteLength === 0 || object.byteLength > V2_MAXIMUM_BLOCK_RECORD_OBJECT_BYTES) {
    throw new V2CborError('Block record object exceeds its limit')
  }
  const fileObjectKey = await deriveSuite02FileObjectKey(
    readSecret,
    share.shareInstance,
    descriptor.fileId,
  )
  let revisionKey: Uint8Array<ArrayBuffer> | undefined
  let segmentKey: Uint8Array<ArrayBuffer> | undefined
  try {
    revisionKey = await deriveSuite02RevisionKey(fileObjectKey, descriptor.fileRevision)
    const blocksPerSegment = V2_CONTENT_SEGMENT_BYTES / BigInt(share.chunkSize)
    const segment = localBlockIndex / blocksPerSegment
    segmentKey = await deriveSuite02FileSegmentKey(revisionKey, segment)
    const binding = createBlockRecordObjectBinding(
      descriptor.shareInstance,
      descriptor.fileId,
      descriptor.fileRevision,
      localBlockIndex,
      dataLength,
    )
    const plaintext = await openSenderObject(
      binding,
      segmentKey,
      share.senderPublicKey,
      object,
    )
    return decodeBlock(plaintext, descriptor, localBlockIndex, dataLength)
  } finally {
    segmentKey?.fill(0)
    revisionKey?.fill(0)
    fileObjectKey.fill(0)
  }
}

function decodeRevision(
  plaintext: Uint8Array,
  share: V2ShareDescriptor,
  expectedFileId: Uint8Array,
): V2FileRevisionDescriptor {
  const fields = requireNumericMap(
    decodeCanonicalCbor(plaintext, V2_MAXIMUM_REVISION_OBJECT_BYTES, 'file revision'),
    [0, 1, 2, 3, 4, 5, 6, 7],
    'file revision',
  )
  requireSchema(fields.get(0), 'file revision')
  const shareInstance = requireBytes(fields.get(1), 16, 'revision share instance', true)
  const fileId = requireBytes(fields.get(2), 16, 'revision file ID', true)
  if (!equalBytes(shareInstance, share.shareInstance) || !equalBytes(fileId, expectedFileId)) {
    throw new V2CborError('File revision changes its authenticated identity')
  }
  const fileRevision = requireBytes(fields.get(3), 16, 'file revision identity', true)
  const exactSize = requireUnsigned(fields.get(4), 'file revision exact size')
  if (exactSize > MAXIMUM_SAFE_INTEGER) {
    throw new V2CborError('File revision exact size exceeds the browser wire range')
  }
  const modifiedTime = decodeModifiedTime(fields.get(5), fields.get(6), fields.get(7))
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
  })
}

function decodeBlock(
  plaintext: Uint8Array,
  descriptor: V2FileRevisionDescriptor,
  expectedIndex: bigint,
  expectedLength: number,
): V2BlockRecord {
  const fields = requireNumericMap(
    decodeCanonicalCbor(plaintext, V2_MAXIMUM_BLOCK_RECORD_OBJECT_BYTES, 'block record'),
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
    throw new V2CborError('Block record changes its file-local authenticated identity')
  }
  return Object.freeze({ descriptor, localBlockIndex: index, data })
}

function decodeModifiedTime(
  secondsValue: unknown,
  nanosecondsValue: unknown,
  precisionValue: unknown,
): V2CatalogModifiedTime | undefined {
  const nanoseconds = requireUnsigned(nanosecondsValue, 'revision modified nanoseconds')
  const precision = requireUnsigned(precisionValue, 'revision modified precision')
  if (secondsValue === null) {
    if (nanoseconds !== 0n || precision !== 0n) {
      throw new V2CborError('Absent revision modified time carries data')
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
    throw new V2CborError('Revision modified time is outside its portable precision')
  }
  return Object.freeze({
    seconds,
    nanoseconds: Number(nanoseconds),
    precision: Number(precision) as 1 | 2 | 3,
    milliseconds: seconds * 1_000n + nanoseconds / 1_000_000n,
  })
}

function requireSchema(value: unknown, label: string): void {
  if (requireUnsigned(value, `${label} schema`) !== 1n) {
    throw new V2CborError(`${label} schema is unsupported`)
  }
}

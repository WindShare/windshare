import {
  createCatalogPageObjectBinding,
  createDescriptorObjectBinding,
  createDirectoryErrorObjectBinding,
  openDescriptorObjectBootstrap,
  openSenderObject,
} from '../crypto/sender-object'
import { encodeBase64Url, equalBytes } from '../crypto/bytes'
import { sha256 } from '../crypto/digest'
import {
  deriveSuite02CatalogKey,
  deriveSuite02DescriptorKey,
} from '../crypto/suite02-key-derivation'
import type { Suite02CapabilityKey } from '../crypto/suite02-link'
import {
  decodeCanonicalCbor,
  requireArray,
  requireBoolean,
  requireBytes,
  requireNumericMap,
  requireSigned,
  requireText,
  requireUnsigned,
  V2CborError,
} from '../protocol/cbor'
import { isPortableCatalogName, V2_PATH_POLICY } from './path-policy'

export { V2_PATH_POLICY } from './path-policy'

export const V2_CATALOG_IDENTITY_BYTES = 16
export const V2_CATALOG_COMMITMENT_BYTES = 32
export const V2_CATALOG_PAGE_ENTRIES = 256
export const V2_CATALOG_DIRECTORY_ENTRIES = 1_048_576
export const V2_CATALOG_PAGE_OBJECT_BYTES = 60 << 10
export const V2_DIRECTORY_ERROR_OBJECT_BYTES = 16 << 10
export const V2_DESCRIPTOR_OBJECT_BYTES = 16 << 10
export const V2_MINIMUM_CHUNK_BYTES = 1 << 10
export const V2_MAXIMUM_CHUNK_BYTES = 4 << 20

const MAXIMUM_SAFE_INTEGER = BigInt(Number.MAX_SAFE_INTEGER)
const ACTIVE_CAPABILITIES = 0b111n

export interface V2ShareDescriptor {
  readonly wireVersion: 2
  readonly suite: 2
  readonly shareInstance: Uint8Array<ArrayBuffer>
  readonly shareInstanceId: string
  readonly syntheticRoot: Uint8Array<ArrayBuffer>
  readonly syntheticRootId: string
  readonly chunkSize: number
  readonly capabilities: bigint
  readonly senderPublicKey: Uint8Array<ArrayBuffer>
  readonly createdAtSeconds: bigint
  readonly pathPolicy: typeof V2_PATH_POLICY
}

export interface V2CatalogModifiedTime {
  readonly seconds: bigint
  readonly nanoseconds: number
  readonly precision: 1 | 2 | 3
  readonly milliseconds: bigint
}

export type V2CatalogEntry =
  | {
      readonly kind: 'directory'
      readonly id: Uint8Array<ArrayBuffer>
      readonly idText: string
      readonly name: string
      readonly modifiedTime?: V2CatalogModifiedTime
    }
  | {
      readonly kind: 'file'
      readonly id: Uint8Array<ArrayBuffer>
      readonly idText: string
      readonly name: string
      readonly expectedSize: bigint
      readonly modifiedTime?: V2CatalogModifiedTime
    }

export interface V2CatalogPage {
  readonly shareInstance: Uint8Array<ArrayBuffer>
  readonly directoryId: Uint8Array<ArrayBuffer>
  readonly directoryIdText: string
  readonly generation: Uint8Array<ArrayBuffer>
  readonly generationText: string
  readonly pageIndex: number
  readonly terminal: boolean
  readonly previousCommitment: Uint8Array<ArrayBuffer>
  readonly entries: readonly V2CatalogEntry[]
  readonly omittedCount: bigint
  readonly objectCommitment: Uint8Array<ArrayBuffer>
  /** Exact authenticated sender-object bytes charged to durable catalog spill. */
  readonly senderObjectBytes: number
}

export type V2DirectoryFailureKind =
  | 'stale'
  | 'permission'
  | 'collision'
  | 'too-wide'
  | 'resource-limit'
  | 'permanent'
  | 'retryable'
  | 'cancelled'

export interface V2DirectoryFailure {
  readonly shareInstance: Uint8Array<ArrayBuffer>
  readonly directoryId: Uint8Array<ArrayBuffer>
  readonly directoryIdText: string
  readonly attemptId: Uint8Array<ArrayBuffer>
  readonly attemptIdText: string
  readonly code: number
  readonly kind: V2DirectoryFailureKind
  readonly retryable: boolean
  readonly retryAfterMilliseconds?: number
}

export interface V2CatalogPageRequest {
  readonly directoryId: Uint8Array
  readonly generation?: Uint8Array
  readonly pageIndex: number
}

export type V2CatalogObject =
  | { readonly kind: 'page'; readonly page: V2CatalogPage }
  | { readonly kind: 'failure'; readonly failure: V2DirectoryFailure }

export async function openV2ShareDescriptor(
  object: Uint8Array,
  capability: Suite02CapabilityKey,
): Promise<V2ShareDescriptor> {
  if (object.byteLength === 0 || object.byteLength > V2_DESCRIPTOR_OBJECT_BYTES) {
    throw new V2CborError('Share descriptor object exceeds its limit')
  }
  const binding = await createDescriptorObjectBinding(capability.pkHash, capability.shareIdRaw)
  const key = await deriveSuite02DescriptorKey(capability.readSecret, capability.pkHash)
  try {
    let decoded: V2ShareDescriptor | undefined
    const plaintext = await openDescriptorObjectBootstrap(
      binding,
      key,
      object,
      (candidate) => {
        decoded = decodeV2ShareDescriptor(candidate)
        return decoded.senderPublicKey
      },
    )
    decoded ??= decodeV2ShareDescriptor(plaintext)
    return decoded
  } finally {
    key.fill(0)
  }
}

export function decodeV2ShareDescriptor(plaintext: Uint8Array): V2ShareDescriptor {
  const fields = requireNumericMap(
    decodeCanonicalCbor(plaintext, V2_DESCRIPTOR_OBJECT_BYTES, 'share descriptor'),
    [0, 1, 2, 3, 4, 5, 6, 7, 8, 9],
    'share descriptor',
  )
  if (requireUnsigned(fields.get(0), 'descriptor schema') !== 1n) {
    throw new V2CborError('Share descriptor schema is unsupported')
  }
  const wire = requireUnsigned(fields.get(1), 'descriptor wire version')
  const suite = requireUnsigned(fields.get(2), 'descriptor suite')
  if (wire !== 2n || suite !== 2n) {
    throw new V2CborError('Share descriptor does not use suite-02 wire semantics')
  }
  const shareInstance = requireBytes(
    fields.get(3),
    V2_CATALOG_IDENTITY_BYTES,
    'share instance',
    true,
  )
  const syntheticRoot = requireBytes(
    fields.get(4),
    V2_CATALOG_IDENTITY_BYTES,
    'synthetic root',
    true,
  )
  const chunkSizeValue = requireUnsigned(fields.get(5), 'descriptor chunk size')
  if (
    chunkSizeValue < BigInt(V2_MINIMUM_CHUNK_BYTES) ||
    chunkSizeValue > BigInt(V2_MAXIMUM_CHUNK_BYTES) ||
    (chunkSizeValue & (chunkSizeValue - 1n)) !== 0n
  ) {
    throw new V2CborError('Share descriptor chunk size is outside the power-of-two range')
  }
  const capabilities = requireUnsigned(fields.get(6), 'descriptor capabilities')
  if ((capabilities & ~ACTIVE_CAPABILITIES) !== 0n) {
    throw new V2CborError('Share descriptor enables an unknown capability')
  }
  const senderPublicKey = requireBytes(fields.get(7), 32, 'sender public key', true)
  const createdAtSeconds = requireUnsigned(fields.get(8), 'descriptor creation time')
  const pathPolicy = requireText(fields.get(9), 'descriptor path policy')
  if (createdAtSeconds > MAXIMUM_SAFE_INTEGER || pathPolicy !== V2_PATH_POLICY) {
    throw new V2CborError('Share descriptor uses an unsupported time or path policy')
  }
  return Object.freeze({
    wireVersion: 2,
    suite: 2,
    shareInstance,
    shareInstanceId: encodeBase64Url(shareInstance),
    syntheticRoot,
    syntheticRootId: encodeBase64Url(syntheticRoot),
    chunkSize: Number(chunkSizeValue),
    capabilities,
    senderPublicKey,
    createdAtSeconds,
    pathPolicy: V2_PATH_POLICY,
  })
}

export async function openV2CatalogObject(
  object: Uint8Array,
  descriptor: V2ShareDescriptor,
  readSecret: Uint8Array,
  request: V2CatalogPageRequest,
): Promise<V2CatalogObject> {
  requirePageRequest(request)
  if (object.byteLength === 0 || object.byteLength > V2_CATALOG_PAGE_OBJECT_BYTES) {
    throw new V2CborError('Catalog sender object exceeds its page limit')
  }
  const key = await deriveSuite02CatalogKey(readSecret, descriptor.shareInstance)
  try {
    const pageBinding = createCatalogPageObjectBinding(
      descriptor.shareInstance,
      request.directoryId,
      request.pageIndex,
    )
    try {
      const plaintext = await openSenderObject(
        pageBinding,
        key,
        descriptor.senderPublicKey,
        object,
      )
      const page = await decodeV2CatalogPage(plaintext, object)
      requireCatalogPageIdentity(page, descriptor, request)
      return Object.freeze({ kind: 'page', page })
    } catch (pageError) {
      if (request.pageIndex !== 0 || request.generation !== undefined) throw pageError
    }
    const failureBinding = createDirectoryErrorObjectBinding(
      descriptor.shareInstance,
      request.directoryId,
    )
    const plaintext = await openSenderObject(
      failureBinding,
      key,
      descriptor.senderPublicKey,
      object,
    )
    const failure = decodeV2DirectoryFailure(plaintext)
    if (
      !equalBytes(failure.shareInstance, descriptor.shareInstance) ||
      !equalBytes(failure.directoryId, request.directoryId)
    ) {
      throw new V2CborError('Directory failure does not answer its authenticated request')
    }
    return Object.freeze({ kind: 'failure', failure })
  } finally {
    key.fill(0)
  }
}

export async function decodeV2CatalogPage(
  plaintext: Uint8Array,
  senderObject: Uint8Array,
): Promise<V2CatalogPage> {
  const fields = requireNumericMap(
    decodeCanonicalCbor(plaintext, V2_CATALOG_PAGE_OBJECT_BYTES, 'catalog page'),
    [0, 1, 2, 3, 4, 5, 6, 7, 8],
    'catalog page',
  )
  if (requireUnsigned(fields.get(0), 'catalog schema') !== 1n) {
    throw new V2CborError('Catalog page schema is unsupported')
  }
  const shareInstance = requireBytes(fields.get(1), 16, 'catalog share instance', true)
  const directoryId = requireBytes(fields.get(2), 16, 'catalog directory ID', true)
  const generation = requireBytes(fields.get(3), 16, 'catalog generation', true)
  const pageIndexValue = requireUnsigned(fields.get(4), 'catalog page index')
  if (pageIndexValue > 0xffff_ffffn) throw new V2CborError('Catalog page index is too large')
  const terminal = requireBoolean(fields.get(5), 'catalog terminal')
  const previousCommitment = requireBytes(fields.get(6), 32, 'catalog previous commitment')
  const rawEntries = requireArray(fields.get(7), V2_CATALOG_PAGE_ENTRIES, 'catalog entries')
  const entries = rawEntries.map(decodeV2CatalogEntry)
  const omittedCount = requireUnsigned(fields.get(8), 'catalog omitted count')
  if ((!terminal && omittedCount !== 0n) || omittedCount > BigInt(V2_CATALOG_DIRECTORY_ENTRIES)) {
    throw new V2CborError('Catalog omitted count is inconsistent')
  }
  return Object.freeze({
    shareInstance,
    directoryId,
    directoryIdText: encodeBase64Url(directoryId),
    generation,
    generationText: encodeBase64Url(generation),
    pageIndex: Number(pageIndexValue),
    terminal,
    previousCommitment,
    entries: Object.freeze(entries),
    omittedCount,
    objectCommitment: await sha256(senderObject),
    senderObjectBytes: senderObject.byteLength,
  })
}

export function decodeV2DirectoryFailure(plaintext: Uint8Array): V2DirectoryFailure {
  const fields = requireNumericMap(
    decodeCanonicalCbor(plaintext, V2_DIRECTORY_ERROR_OBJECT_BYTES, 'directory failure'),
    [0, 1, 2, 3, 4, 5, 6],
    'directory failure',
  )
  if (requireUnsigned(fields.get(0), 'directory failure schema') !== 1n) {
    throw new V2CborError('Directory failure schema is unsupported')
  }
  const shareInstance = requireBytes(fields.get(1), 16, 'failure share instance', true)
  const directoryId = requireBytes(fields.get(2), 16, 'failure directory ID', true)
  const attemptId = requireBytes(fields.get(3), 16, 'failure attempt ID', true)
  const codeValue = requireUnsigned(fields.get(4), 'directory failure code')
  const retryable = requireBoolean(fields.get(5), 'directory retryable')
  if (codeValue < 0x2001n || codeValue > 0x2008n) {
    throw new V2CborError('Directory failure code is outside its scope')
  }
  const retryAfter = fields.get(6)
  let retryAfterMilliseconds: number | undefined
  if (retryable) {
    const delay = requireUnsigned(retryAfter, 'directory retry delay')
    if (delay < 250n || delay > 30_000n) {
      throw new V2CborError('Directory retry delay is outside its frozen range')
    }
    retryAfterMilliseconds = Number(delay)
  } else if (retryAfter !== null) {
    throw new V2CborError('Permanent directory failure carries a retry delay')
  }
  const code = Number(codeValue)
  if (code === 0x2007 && !retryable) {
    throw new V2CborError('Directory transient I/O must be retryable')
  }
  return Object.freeze({
    shareInstance,
    directoryId,
    directoryIdText: encodeBase64Url(directoryId),
    attemptId,
    attemptIdText: encodeBase64Url(attemptId),
    code,
    kind: directoryFailureKind(code),
    retryable,
    ...(retryAfterMilliseconds === undefined ? {} : { retryAfterMilliseconds }),
  })
}

function decodeV2CatalogEntry(value: unknown): V2CatalogEntry {
  const fields = requireArray(value, 7, 'catalog entry')
  if (fields.length !== 7) throw new V2CborError('Catalog entry has the wrong field count')
  const kind = requireUnsigned(fields[0], 'catalog entry kind')
  const id = requireBytes(fields[1], 16, 'catalog entry ID', true)
  const name = requireCatalogName(requireText(fields[2], 'catalog entry name'))
  const modifiedTime = decodeModifiedTime(fields[4], fields[5], fields[6])
  if (kind === 1n) {
    if (fields[3] !== null) throw new V2CborError('Directory entry carries a file size')
    return Object.freeze({
      kind: 'directory',
      id,
      idText: encodeBase64Url(id),
      name,
      ...(modifiedTime === undefined ? {} : { modifiedTime }),
    })
  }
  if (kind !== 2n) throw new V2CborError('Catalog entry kind is unsupported')
  const expectedSize = requireUnsigned(fields[3], 'catalog expected size')
  if (expectedSize > MAXIMUM_SAFE_INTEGER) {
    throw new V2CborError('Catalog file size exceeds the browser-safe wire range')
  }
  return Object.freeze({
    kind: 'file',
    id,
    idText: encodeBase64Url(id),
    name,
    expectedSize,
    ...(modifiedTime === undefined ? {} : { modifiedTime }),
  })
}

function decodeModifiedTime(
  secondsValue: unknown,
  nanosecondsValue: unknown,
  precisionValue: unknown,
): V2CatalogModifiedTime | undefined {
  const nanoseconds = requireUnsigned(nanosecondsValue, 'modified nanoseconds')
  const precision = requireUnsigned(precisionValue, 'modified precision')
  if (secondsValue === null) {
    if (nanoseconds !== 0n || precision !== 0n) {
      throw new V2CborError('Absent modified time carries data')
    }
    return undefined
  }
  const seconds = requireSigned(secondsValue, 'modified seconds')
  if (
    seconds < -MAXIMUM_SAFE_INTEGER ||
    seconds > MAXIMUM_SAFE_INTEGER ||
    nanoseconds >= 1_000_000_000n ||
    precision < 1n ||
    precision > 3n ||
    (precision === 1n && nanoseconds !== 0n) ||
    (precision === 2n && nanoseconds % 1_000_000n !== 0n)
  ) {
    throw new V2CborError('Modified time is outside its portable precision')
  }
  return Object.freeze({
    seconds,
    nanoseconds: Number(nanoseconds),
    precision: Number(precision) as 1 | 2 | 3,
    milliseconds: seconds * 1_000n + nanoseconds / 1_000_000n,
  })
}

function requireCatalogName(name: string): string {
  if (!isPortableCatalogName(name)) {
    throw new V2CborError('Catalog entry name is not a safe path segment')
  }
  return name
}

function requirePageRequest(request: V2CatalogPageRequest): void {
  requireBytes(request.directoryId, 16, 'requested directory ID', true)
  if (!Number.isInteger(request.pageIndex) || request.pageIndex < 0 || request.pageIndex > 0xffff_ffff) {
    throw new V2CborError('Requested catalog page index is invalid')
  }
  if (request.pageIndex > 0 && request.generation === undefined) {
    throw new V2CborError('Later catalog pages require a generation identity')
  }
  if (request.generation !== undefined) {
    requireBytes(request.generation, 16, 'requested catalog generation', true)
  }
}

function requireCatalogPageIdentity(
  page: V2CatalogPage,
  descriptor: V2ShareDescriptor,
  request: V2CatalogPageRequest,
): void {
  if (
    !equalBytes(page.shareInstance, descriptor.shareInstance) ||
    !equalBytes(page.directoryId, request.directoryId) ||
    page.pageIndex !== request.pageIndex ||
    (request.generation !== undefined && !equalBytes(page.generation, request.generation))
  ) {
    throw new V2CborError('Catalog page does not answer its authenticated request')
  }
}

function directoryFailureKind(code: number): V2DirectoryFailureKind {
  switch (code) {
    case 0x2001: return 'stale'
    case 0x2002: return 'permission'
    case 0x2003: return 'collision'
    case 0x2004: return 'too-wide'
    case 0x2005: return 'resource-limit'
    case 0x2006: return 'permanent'
    case 0x2007: return 'retryable'
    case 0x2008: return 'cancelled'
    default: throw new V2CborError('Directory failure code is unknown')
  }
}

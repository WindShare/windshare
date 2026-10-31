import { catalogAlternativesFailure, catalogFailure } from './failure-evidence'
import { createEd25519Verifier, type Ed25519Verifier, type Ed25519VerifierOptions } from '../crypto/ed25519'
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
  CborError,
} from '../protocol/cbor'
import { isCatalogName, PATH_POLICY } from './path-policy'

export { PATH_POLICY } from './path-policy'

export const CATALOG_IDENTITY_BYTES = 16
export const CATALOG_COMMITMENT_BYTES = 32
export const CATALOG_PAGE_ENTRIES = 256
export const CATALOG_DIRECTORY_ENTRIES = 1_048_576
export const CATALOG_PAGE_OBJECT_BYTES = 60 << 10
export const DIRECTORY_ERROR_OBJECT_BYTES = 16 << 10
export const DESCRIPTOR_OBJECT_BYTES = 16 << 10
export const MINIMUM_CHUNK_BYTES = 1 << 10
export const MAXIMUM_CHUNK_BYTES = 4 << 20

const MAXIMUM_SAFE_INTEGER = BigInt(Number.MAX_SAFE_INTEGER)
const ACTIVE_CAPABILITIES = 0b111n

export interface ShareDescriptor {
  readonly wireVersion: 2
  readonly suite: 2
  readonly shareInstance: Uint8Array<ArrayBuffer>
  readonly shareInstanceId: string
  readonly syntheticRoot: Uint8Array<ArrayBuffer>
  readonly syntheticRootId: string
  readonly chunkSize: number
  readonly capabilities: bigint
  readonly sender: Ed25519Verifier
  readonly createdAtSeconds: bigint
  readonly pathPolicy: typeof PATH_POLICY
}

export interface CatalogModifiedTime {
  readonly seconds: bigint
  readonly nanoseconds: number
  readonly precision: 1 | 2 | 3
  readonly milliseconds: bigint
}

export type CatalogEntry =
  | {
      readonly kind: 'directory'
      readonly id: Uint8Array<ArrayBuffer>
      readonly idText: string
      readonly name: string
      readonly modifiedTime?: CatalogModifiedTime
    }
  | {
      readonly kind: 'file'
      readonly id: Uint8Array<ArrayBuffer>
      readonly idText: string
      readonly name: string
      readonly expectedSize: bigint
      readonly modifiedTime?: CatalogModifiedTime
    }

export interface CatalogPage {
  readonly shareInstance: Uint8Array<ArrayBuffer>
  readonly directoryId: Uint8Array<ArrayBuffer>
  readonly directoryIdText: string
  readonly generation: Uint8Array<ArrayBuffer>
  readonly generationText: string
  readonly pageIndex: number
  readonly terminal: boolean
  readonly previousCommitment: Uint8Array<ArrayBuffer>
  readonly entries: readonly CatalogEntry[]
  readonly omittedCount: bigint
  readonly objectCommitment: Uint8Array<ArrayBuffer>
  /** Exact authenticated sender-object bytes charged to durable catalog spill. */
  readonly senderObjectBytes: number
}

export type DirectoryFailureKind =
  | 'stale'
  | 'permission'
  | 'collision'
  | 'too-wide'
  | 'resource-limit'
  | 'permanent'
  | 'retryable'
  | 'cancelled'

export interface DirectoryFailure {
  readonly shareInstance: Uint8Array<ArrayBuffer>
  readonly directoryId: Uint8Array<ArrayBuffer>
  readonly directoryIdText: string
  readonly attemptId: Uint8Array<ArrayBuffer>
  readonly attemptIdText: string
  readonly code: number
  readonly kind: DirectoryFailureKind
  readonly retryable: boolean
  readonly retryAfterMilliseconds?: number
}

export interface CatalogPageRequest {
  readonly directoryId: Uint8Array
  readonly generation?: Uint8Array
  readonly pageIndex: number
}

export type CatalogObject =
  | { readonly kind: 'page'; readonly page: CatalogPage }
  | { readonly kind: 'failure'; readonly failure: DirectoryFailure }

export async function openShareDescriptor(
  object: Uint8Array,
  capability: Suite02CapabilityKey,
  verification: Ed25519VerifierOptions = {},
): Promise<ShareDescriptor> {
  if (object.byteLength === 0 || object.byteLength > DESCRIPTOR_OBJECT_BYTES) {
    throw catalogFailure(new CborError('Share descriptor object exceeds its limit'), { invariant: 'descriptor-bytes', actual: object.byteLength, maximum: DESCRIPTOR_OBJECT_BYTES })
  }
  const binding = await createDescriptorObjectBinding(capability.pkHash, capability.shareIdRaw)
  const key = await deriveSuite02DescriptorKey(capability.readSecret, capability.pkHash)
  try {
    let decoded: ShareDescriptor | undefined
    const plaintext = await openDescriptorObjectBootstrap(
      binding,
      key,
      object,
      (candidate) => {
        decoded = decodeShareDescriptor(candidate, verification)
        return decoded.sender
      },
    )
    decoded ??= decodeShareDescriptor(plaintext, verification)
    return decoded
  } finally {
    key.fill(0)
  }
}

export function decodeShareDescriptor(plaintext: Uint8Array, verification: Ed25519VerifierOptions = {}): ShareDescriptor {
  const fields = requireNumericMap(
    decodeCanonicalCbor(plaintext, DESCRIPTOR_OBJECT_BYTES, 'share descriptor'),
    [0, 1, 2, 3, 4, 5, 6, 7, 8, 9],
    'share descriptor',
  )
  if (requireUnsigned(fields.get(0), 'descriptor schema') !== 1n) {
    throw catalogFailure(new CborError('Share descriptor schema is unsupported'), { invariant: 'descriptor-schema', expected: '1', actual: String(fields.get(0)) })
  }
  const wire = requireUnsigned(fields.get(1), 'descriptor wire version')
  const suite = requireUnsigned(fields.get(2), 'descriptor suite')
  if (wire !== 2n || suite !== 2n) {
    throw catalogFailure(new CborError('Share descriptor does not use suite-02 wire semantics'), { invariant: 'suite-wire', expected: '2', actualWire: String(wire), actualSuite: String(suite) })
  }
  const shareInstance = requireBytes(
    fields.get(3),
    CATALOG_IDENTITY_BYTES,
    'share instance',
    true,
  )
  const syntheticRoot = requireBytes(
    fields.get(4),
    CATALOG_IDENTITY_BYTES,
    'synthetic root',
    true,
  )
  const chunkSizeValue = requireUnsigned(fields.get(5), 'descriptor chunk size')
  if (
    chunkSizeValue < BigInt(MINIMUM_CHUNK_BYTES) ||
    chunkSizeValue > BigInt(MAXIMUM_CHUNK_BYTES) ||
    (chunkSizeValue & (chunkSizeValue - 1n)) !== 0n
  ) {
    throw catalogFailure(new CborError('Share descriptor chunk size is outside the power-of-two range'), { invariant: 'chunk-size', minimum: MINIMUM_CHUNK_BYTES, maximum: MAXIMUM_CHUNK_BYTES, actual: String(chunkSizeValue) })
  }
  const capabilities = requireUnsigned(fields.get(6), 'descriptor capabilities')
  if ((capabilities & ~ACTIVE_CAPABILITIES) !== 0n) {
    throw catalogFailure(new CborError('Share descriptor enables an unknown capability'), { invariant: 'capabilities', allowedMask: String(ACTIVE_CAPABILITIES), actual: String(capabilities) })
  }
  const senderPublicKey = requireBytes(fields.get(7), 32, 'sender public key', true)
  const createdAtSeconds = requireUnsigned(fields.get(8), 'descriptor creation time')
  const pathPolicy = requireText(fields.get(9), 'descriptor path policy')
  if (createdAtSeconds > MAXIMUM_SAFE_INTEGER || pathPolicy !== PATH_POLICY) {
    throw catalogFailure(new CborError('Share descriptor uses an unsupported time or path policy'), { invariant: 'descriptor-policy', expectedPolicy: PATH_POLICY, actualPolicy: pathPolicy, actualTime: String(createdAtSeconds), maximumTime: String(MAXIMUM_SAFE_INTEGER) })
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
    sender: createEd25519Verifier(senderPublicKey, verification),
    createdAtSeconds,
    pathPolicy: PATH_POLICY,
  })
}

export async function openCatalogObject(
  object: Uint8Array,
  descriptor: ShareDescriptor,
  readSecret: Uint8Array,
  request: CatalogPageRequest,
): Promise<CatalogObject> {
  requirePageRequest(request)
  if (object.byteLength === 0 || object.byteLength > CATALOG_PAGE_OBJECT_BYTES) {
    throw catalogFailure(new CborError('Catalog sender object exceeds its page limit'), { invariant: 'page-object-bytes', actual: object.byteLength, maximum: CATALOG_PAGE_OBJECT_BYTES })
  }
  const key = await deriveSuite02CatalogKey(readSecret, descriptor.shareInstance)
  try {
    const pageBinding = createCatalogPageObjectBinding(
      descriptor.shareInstance,
      request.directoryId,
      request.pageIndex,
    )
    let rejectedPage: unknown
    try {
      const plaintext = await openSenderObject(
        pageBinding,
        key,
        descriptor.sender,
        object,
      )
      const page = await decodeCatalogPage(plaintext, object)
      requireCatalogPageIdentity(page, descriptor, request)
      return Object.freeze({ kind: 'page', page })
    } catch (pageError) {
      if (request.pageIndex !== 0 || request.generation !== undefined) throw pageError
      rejectedPage = pageError
    }
    try {
      const failureBinding = createDirectoryErrorObjectBinding(
        descriptor.shareInstance,
        request.directoryId,
      )
      const plaintext = await openSenderObject(
        failureBinding,
        key,
        descriptor.sender,
        object,
      )
      const failure = decodeDirectoryFailure(plaintext)
      if (
        !equalBytes(failure.shareInstance, descriptor.shareInstance) ||
        !equalBytes(failure.directoryId, request.directoryId)
      ) {
        throw catalogFailure(new CborError('Directory failure does not answer its authenticated request'), { invariant: 'directory-failure-binding', expectedDirectory: encodeBase64Url(request.directoryId), actualDirectory: failure.directoryIdText, expectedShare: descriptor.shareInstanceId, actualShare: encodeBase64Url(failure.shareInstance) })
      }
      return Object.freeze({ kind: 'failure', failure })
    } catch (error) {
      throw catalogAlternativesFailure(error, [rejectedPage], {
        invariant: 'catalog-object-type', directoryId: encodeBase64Url(request.directoryId), pageIndex: request.pageIndex,
      })
    }
  } finally {
    key.fill(0)
  }
}

export async function decodeCatalogPage(
  plaintext: Uint8Array,
  senderObject: Uint8Array,
): Promise<CatalogPage> {
  const fields = requireNumericMap(
    decodeCanonicalCbor(plaintext, CATALOG_PAGE_OBJECT_BYTES, 'catalog page'),
    [0, 1, 2, 3, 4, 5, 6, 7, 8],
    'catalog page',
  )
  if (requireUnsigned(fields.get(0), 'catalog schema') !== 1n) {
    throw catalogFailure(new CborError('Catalog page schema is unsupported'), { invariant: 'page-schema', expected: '1', actual: String(fields.get(0)) })
  }
  const shareInstance = requireBytes(fields.get(1), 16, 'catalog share instance', true)
  const directoryId = requireBytes(fields.get(2), 16, 'catalog directory ID', true)
  const generation = requireBytes(fields.get(3), 16, 'catalog generation', true)
  const pageIndexValue = requireUnsigned(fields.get(4), 'catalog page index')
  if (pageIndexValue > 0xffff_ffffn) throw catalogFailure(new CborError('Catalog page index is too large'), { invariant: 'page-index', maximum: '4294967295', actual: String(pageIndexValue) })
  const terminal = requireBoolean(fields.get(5), 'catalog terminal')
  const previousCommitment = requireBytes(fields.get(6), 32, 'catalog previous commitment')
  const rawEntries = requireArray(fields.get(7), CATALOG_PAGE_ENTRIES, 'catalog entries')
  const entries = rawEntries.map(decodeCatalogEntry)
  const omittedCount = requireUnsigned(fields.get(8), 'catalog omitted count')
  if ((!terminal && omittedCount !== 0n) || omittedCount > BigInt(CATALOG_DIRECTORY_ENTRIES)) {
    throw catalogFailure(new CborError('Catalog omitted count is inconsistent'), { invariant: 'omitted-count', terminal, maximum: CATALOG_DIRECTORY_ENTRIES, actual: String(omittedCount) })
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

export function decodeDirectoryFailure(plaintext: Uint8Array): DirectoryFailure {
  const fields = requireNumericMap(
    decodeCanonicalCbor(plaintext, DIRECTORY_ERROR_OBJECT_BYTES, 'directory failure'),
    [0, 1, 2, 3, 4, 5, 6],
    'directory failure',
  )
  if (requireUnsigned(fields.get(0), 'directory failure schema') !== 1n) {
    throw catalogFailure(new CborError('Directory failure schema is unsupported'), { invariant: 'directory-failure-schema', expected: '1', actual: String(fields.get(0)) })
  }
  const shareInstance = requireBytes(fields.get(1), 16, 'failure share instance', true)
  const directoryId = requireBytes(fields.get(2), 16, 'failure directory ID', true)
  const attemptId = requireBytes(fields.get(3), 16, 'failure attempt ID', true)
  const codeValue = requireUnsigned(fields.get(4), 'directory failure code')
  const retryable = requireBoolean(fields.get(5), 'directory retryable')
  if (codeValue < 0x2001n || codeValue > 0x2008n) {
    throw catalogFailure(new CborError('Directory failure code is outside its scope'), { invariant: 'directory-failure-code', actual: String(codeValue) })
  }
  const retryAfter = fields.get(6)
  let retryAfterMilliseconds: number | undefined
  if (retryable) {
    const delay = requireUnsigned(retryAfter, 'directory retry delay')
    if (delay < 250n || delay > 30_000n) {
      throw catalogFailure(new CborError('Directory retry delay is outside its frozen range'), { invariant: 'retry-delay', actual: String(delay) })
    }
    retryAfterMilliseconds = Number(delay)
  } else if (retryAfter !== null) {
    throw catalogFailure(new CborError('Permanent directory failure carries a retry delay'), { invariant: 'permanent-retry-delay', expected: null, actual: String(retryAfter) })
  }
  const code = Number(codeValue)
  if (code === 0x2007 && !retryable) {
    throw catalogFailure(new CborError('Directory transient I/O must be retryable'), { invariant: 'transient-retryable', expected: true, actual: retryable })
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

function decodeCatalogEntry(value: unknown): CatalogEntry {
  const fields = requireArray(value, 7, 'catalog entry')
  if (fields.length !== 7) throw catalogFailure(new CborError('Catalog entry has the wrong field count'), { invariant: 'entry-fields', expected: 7, actual: fields.length })
  const kind = requireUnsigned(fields[0], 'catalog entry kind')
  const id = requireBytes(fields[1], 16, 'catalog entry ID', true)
  const name = requireCatalogName(requireText(fields[2], 'catalog entry name'))
  const modifiedTime = decodeModifiedTime(fields[4], fields[5], fields[6])
  if (kind === 1n) {
    if (fields[3] !== null) throw catalogFailure(new CborError('Directory entry carries a file size'), { invariant: 'directory-size', expected: null, actual: String(fields[3]), nodeId: encodeBase64Url(id) })
    return Object.freeze({
      kind: 'directory',
      id,
      idText: encodeBase64Url(id),
      name,
      ...(modifiedTime === undefined ? {} : { modifiedTime }),
    })
  }
  if (kind !== 2n) throw catalogFailure(new CborError('Catalog entry kind is unsupported'), { invariant: 'entry-kind', expected: '1|2', actual: String(kind) })
  const expectedSize = requireUnsigned(fields[3], 'catalog expected size')
  if (expectedSize > MAXIMUM_SAFE_INTEGER) {
    throw catalogFailure(new CborError('Catalog file size exceeds the browser-safe wire range'), { invariant: 'file-size', maximum: String(MAXIMUM_SAFE_INTEGER), actual: String(expectedSize) })
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
): CatalogModifiedTime | undefined {
  const nanoseconds = requireUnsigned(nanosecondsValue, 'modified nanoseconds')
  const precision = requireUnsigned(precisionValue, 'modified precision')
  if (secondsValue === null) {
    if (nanoseconds !== 0n || precision !== 0n) {
      throw catalogFailure(new CborError('Absent modified time carries data'), { invariant: 'absent-modified-time', expected: '0', actualNanoseconds: String(nanoseconds), actualPrecision: String(precision) })
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
    throw catalogFailure(new CborError('Modified time is outside its portable precision'), { invariant: 'modified-time', seconds: String(seconds), nanoseconds: String(nanoseconds), precision: String(precision) })
  }
  return Object.freeze({
    seconds,
    nanoseconds: Number(nanoseconds),
    precision: Number(precision) as 1 | 2 | 3,
    milliseconds: seconds * 1_000n + nanoseconds / 1_000_000n,
  })
}

function requireCatalogName(name: string): string {
  if (!isCatalogName(name)) {
    throw catalogFailure(new CborError('Catalog entry name is not a safe path segment'), { invariant: 'safe-path-segment', actual: name })
  }
  return name
}

function requirePageRequest(request: CatalogPageRequest): void {
  requireBytes(request.directoryId, 16, 'requested directory ID', true)
  if (!Number.isInteger(request.pageIndex) || request.pageIndex < 0 || request.pageIndex > 0xffff_ffff) {
    throw catalogFailure(new CborError('Requested catalog page index is invalid'), { invariant: 'requested-page-index', actual: request.pageIndex })
  }
  if (request.pageIndex > 0 && request.generation === undefined) {
    throw catalogFailure(new CborError('Later catalog pages require a generation identity'), { invariant: 'requested-generation', pageIndex: request.pageIndex, actual: null })
  }
  if (request.generation !== undefined) {
    requireBytes(request.generation, 16, 'requested catalog generation', true)
  }
}

function requireCatalogPageIdentity(
  page: CatalogPage,
  descriptor: ShareDescriptor,
  request: CatalogPageRequest,
): void {
  if (
    !equalBytes(page.shareInstance, descriptor.shareInstance) ||
    !equalBytes(page.directoryId, request.directoryId) ||
    page.pageIndex !== request.pageIndex ||
    (request.generation !== undefined && !equalBytes(page.generation, request.generation))
  ) {
    throw catalogFailure(new CborError('Catalog page does not answer its authenticated request'), {
      invariant: 'authenticated-request', expectedDirectory: encodeBase64Url(request.directoryId), actualDirectory: page.directoryIdText,
      expectedPage: request.pageIndex, actualPage: page.pageIndex,
      expectedGeneration: request.generation === undefined ? null : encodeBase64Url(request.generation), actualGeneration: page.generationText,
      expectedShare: descriptor.shareInstanceId, actualShare: encodeBase64Url(page.shareInstance),
    })
  }
}

function directoryFailureKind(code: number): DirectoryFailureKind {
  switch (code) {
    case 0x2001: return 'stale'
    case 0x2002: return 'permission'
    case 0x2003: return 'collision'
    case 0x2004: return 'too-wide'
    case 0x2005: return 'resource-limit'
    case 0x2006: return 'permanent'
    case 0x2007: return 'retryable'
    case 0x2008: return 'cancelled'
    default: throw new CborError('Directory failure code is unknown')
  }
}

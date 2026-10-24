import { equalBytes } from '../crypto/bytes'
import { sha256 } from '../crypto/digest'
import {
  decodeCanonicalCbor,
  encodeCanonicalCbor,
  requireArray,
  requireBoolean,
  requireBytes,
  requireNumericMap,
  requireUnsigned,
  CborError,
} from '../protocol/cbor'

export const MAXIMUM_OPEN_BATCH = 64
export const MAXIMUM_REQUESTED_BLOCKS = 256
export const FRAGMENT_HEADER_BYTES = 52
export const FRAGMENT_PAYLOAD_BYTES = 65_440
export const MAXIMUM_FRAGMENTS = 128
export const FRAGMENT_INACTIVITY_TIMEOUT_MILLISECONDS = 15_000
export const FRAGMENT_TOMBSTONE_MILLISECONDS = 30_000
export const LEASE_TTL_MILLISECONDS = 120_000
export const LEASE_RENEW_AFTER_MILLISECONDS = 60_000
export const REVISION_RETRY_MINIMUM_MILLISECONDS = 1
export const REVISION_RETRY_MAXIMUM_MILLISECONDS = 30_000
export const REVISION_CODE_QUOTA = 0x3005
export const REVISION_CODE_LEASE_EXPIRED = 0x3006
export const REVISION_CODE_INVALID_LEASE = 0x3008

export interface OpenResult {
  readonly fileId: Uint8Array<ArrayBuffer>
  readonly revisionObject?: Uint8Array<ArrayBuffer>
  readonly lease?: RemoteLease
  readonly failure?: RevisionFailure
}

export interface RemoteLease {
  readonly id: Uint8Array<ArrayBuffer>
  readonly ttlMilliseconds: number
  readonly renewAfterMilliseconds: number
}

export interface RevisionFailure {
  readonly code: number
  readonly retryable: boolean
  readonly retryAfterMilliseconds?: number
}

export type FragmentAssemblyResult =
  | Readonly<{ status: 'duplicate' | 'tombstoned' }>
  | Readonly<{ status: 'accepted'; receivedBytes: number }>
  | Readonly<{ status: 'complete'; receivedBytes: number; object: Uint8Array<ArrayBuffer> }>

export class FragmentInactivityError extends Error {
  constructor() {
    super('Block fragment reassembly made no authenticated progress before timeout')
    this.name = 'FragmentInactivityError'
  }
}

export class FragmentAssembler {
  readonly #operationId: Uint8Array<ArrayBuffer>
  readonly #now: () => number
  #recordId: Uint8Array<ArrayBuffer> | undefined
  #fragments: Array<Uint8Array<ArrayBuffer> | undefined> = []
  #count = 0
  #totalLength = 0
  #received = 0
  #receivedFragments = 0
  #lastProgressAt = 0
  #cancelledAt: number | undefined

  constructor(operationId: Uint8Array, now: () => number = () => Date.now()) {
    this.#operationId = requireBytes(operationId, 16, 'fragment operation ID', true)
    this.#now = now
  }

  async accept(plaintext: Uint8Array): Promise<FragmentAssemblyResult> {
    const now = this.#now()
    if (this.#cancelledAt !== undefined) {
      if (now - this.#cancelledAt <= FRAGMENT_TOMBSTONE_MILLISECONDS) {
        return Object.freeze({ status: 'tombstoned' })
      }
      throw new CborError('Fragment arrived after its cancellation tombstone expired')
    }
    const fragment = decodeFragment(plaintext)
    if (!equalBytes(fragment.operationId, this.#operationId)) {
      throw new CborError('Fragment belongs to another operation')
    }
    if (this.#recordId === undefined) this.#start(fragment, now)
    this.#requireIdentity(fragment)
    if (now - this.#lastProgressAt >= FRAGMENT_INACTIVITY_TIMEOUT_MILLISECONDS) {
      throw new FragmentInactivityError()
    }
    const existing = this.#fragments[fragment.index]
    if (existing !== undefined) {
      if (!equalBytes(existing, fragment.payload)) {
        throw new CborError('Block fragment conflicts with an authenticated retransmission')
      }
      return Object.freeze({ status: 'duplicate' })
    }
    this.#fragments[fragment.index] = fragment.payload
    this.#received += fragment.payload.byteLength
    this.#receivedFragments += 1
    // Duplicate traffic is authenticated but does not earn more lifetime; this
    // prevents a replay loop from holding the bounded assembly budget forever.
    this.#lastProgressAt = now
    if (this.#receivedFragments !== this.#count) return Object.freeze({ status: 'accepted', receivedBytes: fragment.payload.byteLength })
    if (this.#received !== this.#totalLength) throw new CborError('Block fragments have a length gap')
    const object = new Uint8Array(this.#totalLength)
    let offset = 0
    for (const payload of this.#fragments) {
      if (payload === undefined) throw new CborError('Block fragment set is incomplete')
      object.set(payload, offset)
      offset += payload.byteLength
    }
    const digest = await sha256(object)
    const recordId = this.#recordId
    if (recordId === undefined || !equalBytes(digest.subarray(0, 16), recordId)) {
      throw new CborError('Reassembled block record has the wrong identity')
    }
    return Object.freeze({ status: 'complete', receivedBytes: fragment.payload.byteLength, object })
  }

  cancel(): void {
    this.#cancelledAt ??= this.#now()
    this.#fragments = []
    this.#received = 0
    this.#receivedFragments = 0
  }

  #start(fragment: DecodedFragment, now: number): void {
    this.#recordId = fragment.recordId
    this.#count = fragment.count
    this.#totalLength = fragment.totalLength
    this.#fragments = Array.from({ length: fragment.count })
    this.#lastProgressAt = now
    this.#received = 0
    this.#receivedFragments = 0
  }

  #requireIdentity(fragment: DecodedFragment): void {
    if (
      this.#recordId === undefined ||
      !equalBytes(fragment.recordId, this.#recordId) ||
      fragment.count !== this.#count ||
      fragment.totalLength !== this.#totalLength
    ) {
      throw new CborError('Block fragments splice different sealed records')
    }
  }
}

interface DecodedFragment {
  readonly operationId: Uint8Array<ArrayBuffer>
  readonly recordId: Uint8Array<ArrayBuffer>
  readonly index: number
  readonly count: number
  readonly totalLength: number
  readonly payload: Uint8Array<ArrayBuffer>
}

export function encodeListRequest(
  directoryId: Uint8Array,
  generation: Uint8Array | undefined,
  pageIndex: number,
): Uint8Array<ArrayBuffer> {
  if (!Number.isInteger(pageIndex) || pageIndex < 0 || pageIndex > 0xffff_ffff) {
    throw new CborError('Catalog page index is invalid')
  }
  if (pageIndex > 0 && generation === undefined) {
    throw new CborError('Later catalog page request has no generation')
  }
  return encodeCanonicalCbor([
    requireBytes(directoryId, 16, 'requested directory ID', true),
    generation === undefined ? null : requireBytes(generation, 16, 'requested generation', true),
    pageIndex,
  ])
}

export function decodeCatalogResult(body: Uint8Array): Uint8Array<ArrayBuffer> {
  const fields = requireNumericMap(
    decodeCanonicalCbor(body, 65_492, 'catalog result'),
    [0, 1],
    'catalog result',
  )
  requireSchema(fields.get(0), 'catalog result')
  return requireBytes(fields.get(1), undefined, 'catalog sender object', true)
}

export function encodeOpenRequest(
  fileId: Uint8Array,
  initialRanges: readonly { readonly start: bigint; readonly end: bigint }[] = [],
): Uint8Array<ArrayBuffer> {
  if (initialRanges.length > 256) throw new CborError('Open request has too many ranges')
  const ranges = initialRanges.map((range) => {
    if (range.start < 0n || range.end <= range.start) {
      throw new CborError('Open request range is not canonical')
    }
    return [range.start, range.end]
  })
  return encodeCanonicalCbor([[requireBytes(fileId, 16, 'open file ID', true), ranges]])
}

export function decodeOpenResults(
  body: Uint8Array,
  expectedFileId: Uint8Array,
): OpenResult {
  const fields = requireNumericMap(
    decodeCanonicalCbor(body, 65_492, 'open results'),
    [0, 1],
    'open results',
  )
  requireSchema(fields.get(0), 'open results')
  const items = requireArray(fields.get(1), MAXIMUM_OPEN_BATCH, 'open result items')
  if (items.length !== 1) throw new CborError('Single-file open returned another batch shape')
  const item = requireArray(items[0], 6, 'open result')
  const fileId = requireBytes(item[0], 16, 'open result file ID', true)
  if (!equalBytes(fileId, expectedFileId)) throw new CborError('Open result reordered its file')
  const status = requireUnsigned(item[1], 'open result status')
  if (status === 0n && item.length === 6) {
    const revisionObject = requireBytes(item[2], undefined, 'revision sender object', true)
    const leaseId = requireBytes(item[3], 16, 'revision lease ID', true)
    const ttl = requireUnsigned(item[4], 'revision lease TTL')
    const renewAfter = requireUnsigned(item[5], 'revision lease renew delay')
    if (
      ttl !== BigInt(LEASE_TTL_MILLISECONDS) ||
      renewAfter !== BigInt(LEASE_RENEW_AFTER_MILLISECONDS)
    ) {
      throw new CborError('Revision lease timing is invalid')
    }
    return Object.freeze({
      fileId,
      revisionObject,
      lease: Object.freeze({
        id: leaseId,
        ttlMilliseconds: Number(ttl),
        renewAfterMilliseconds: Number(renewAfter),
      }),
    })
  }
  if (status !== 1n || item.length !== 5) {
    throw new CborError('Open result has an unknown outcome shape')
  }
  const codeValue = requireUnsigned(item[2], 'revision failure code')
  const retryable = requireBoolean(item[3], 'revision failure retryable')
  if (codeValue < 0x3001n || codeValue > 0x3008n) {
    throw new CborError('Revision failure code is outside its scope')
  }
  const retry = item[4]
  let retryAfterMilliseconds: number | undefined
  if (retryable) {
    const delay = requireUnsigned(retry, 'revision retry delay')
    if (
      delay < BigInt(REVISION_RETRY_MINIMUM_MILLISECONDS) ||
      delay > BigInt(REVISION_RETRY_MAXIMUM_MILLISECONDS)
    ) {
      throw new CborError('Revision retry delay is outside its frozen range')
    }
    retryAfterMilliseconds = Number(delay)
  } else if (retry !== null) {
    throw new CborError('Permanent revision failure carries a retry delay')
  }
  return Object.freeze({
    fileId,
    failure: Object.freeze({
      code: Number(codeValue),
      retryable,
      ...(retryAfterMilliseconds === undefined ? {} : { retryAfterMilliseconds }),
    }),
  })
}

export function encodeBlockRequest(
  leaseId: Uint8Array,
  indices: readonly bigint[],
): Uint8Array<ArrayBuffer> {
  if (indices.length === 0 || indices.length > MAXIMUM_REQUESTED_BLOCKS) {
    throw new CborError('Block request count is outside its limit')
  }
  let previous: bigint | undefined
  for (const index of indices) {
    if (index < 0n || (previous !== undefined && index <= previous)) {
      throw new CborError('Block request indices must be strictly increasing')
    }
    previous = index
  }
  return encodeCanonicalCbor([
    requireBytes(leaseId, 16, 'block request lease ID', true),
    [...indices],
  ])
}

export function encodeLeaseRequest(leaseId: Uint8Array): Uint8Array<ArrayBuffer> {
  return encodeCanonicalCbor([requireBytes(leaseId, 16, 'lease ID', true)])
}

export function decodeLeaseResult(body: Uint8Array, expectedLeaseId: Uint8Array): RemoteLease {
  const fields = requireNumericMap(
    decodeCanonicalCbor(body, 65_492, 'lease result'),
    [0, 1, 2, 3],
    'lease result',
  )
  requireSchema(fields.get(0), 'lease result')
  const id = requireBytes(fields.get(1), 16, 'lease result ID', true)
  const ttl = requireUnsigned(fields.get(2), 'lease result TTL')
  const renewAfter = requireUnsigned(fields.get(3), 'lease result renew delay')
  if (
    !equalBytes(id, expectedLeaseId) ||
    ttl !== BigInt(LEASE_TTL_MILLISECONDS) ||
    renewAfter !== BigInt(LEASE_RENEW_AFTER_MILLISECONDS)
  ) {
    throw new CborError('Lease result changes identity or timing')
  }
  return Object.freeze({
    id,
    ttlMilliseconds: Number(ttl),
    renewAfterMilliseconds: Number(renewAfter),
  })
}

export function decodeOperationComplete(body: Uint8Array): number {
  const fields = requireNumericMap(
    decodeCanonicalCbor(body, 65_492, 'operation complete'),
    [0, 1],
    'operation complete',
  )
  requireSchema(fields.get(0), 'operation complete')
  const count = requireUnsigned(fields.get(1), 'operation result count')
  if (count > 0xffff_ffffn) throw new CborError('Operation result count is too large')
  return Number(count)
}

function decodeFragment(plaintext: Uint8Array): DecodedFragment {
  if (
    plaintext.byteLength < FRAGMENT_HEADER_BYTES ||
    plaintext.byteLength > 65_492 ||
    plaintext[0] !== 1 ||
    plaintext[1] !== 8 ||
    ((plaintext[2] ?? 0) & ~1) !== 0 ||
    plaintext[3] !== 0
  ) {
    throw new CborError('Authenticated block fragment has an invalid header')
  }
  const operationId = requireBytes(plaintext.subarray(4, 20), 16, 'fragment operation ID', true)
  const recordId = requireBytes(plaintext.subarray(20, 36), 16, 'fragment record ID', true)
  const view = new DataView(plaintext.buffer, plaintext.byteOffset, plaintext.byteLength)
  const index = view.getUint32(36, false)
  const count = view.getUint32(40, false)
  const totalLength = view.getUint32(44, false)
  const payloadLength = view.getUint32(48, false)
  const last = (plaintext[2] ?? 0) === 1
  const expectedCount = Math.ceil(totalLength / FRAGMENT_PAYLOAD_BYTES)
  const expectedPayload = last
    ? totalLength - (count - 1) * FRAGMENT_PAYLOAD_BYTES
    : FRAGMENT_PAYLOAD_BYTES
  if (
    count === 0 || count > MAXIMUM_FRAGMENTS || index >= count ||
    totalLength === 0 || totalLength > (4 << 20) + 512 ||
    count !== expectedCount || last !== (index === count - 1) ||
    payloadLength !== plaintext.byteLength - FRAGMENT_HEADER_BYTES ||
    payloadLength !== expectedPayload
  ) {
    throw new CborError('Authenticated block fragment geometry is invalid')
  }
  return Object.freeze({
    operationId,
    recordId,
    index,
    count,
    totalLength,
    payload: plaintext.slice(FRAGMENT_HEADER_BYTES),
  })
}

function requireSchema(value: unknown, label: string): void {
  if (requireUnsigned(value, `${label} schema`) !== 1n) {
    throw new CborError(`${label} schema is unsupported`)
  }
}

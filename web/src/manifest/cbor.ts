import {
  Tokenizer,
  Type,
  decode,
  encode,
  rfc8949EncodeOptions,
  type DecodeOptions,
  type TagDecoder,
  type Token,
} from 'cborg'
import type { DecodeTokenizer } from 'cborg/interface'

import {
  MANIFEST_VERSION,
  MAX_CHUNK_BYTES,
  MAX_MTIME_MILLISECONDS,
  MAX_SEALED_MANIFEST_BYTES,
  MAX_STREAM_BYTES,
  MIN_CHUNK_BYTES,
  MIN_MTIME_MILLISECONDS,
  type ByteLength,
  type ChunkSize,
  type ManifestEntry,
  type UnixMilliseconds,
  type ValidatedManifestV1,
} from '../contracts'
import { equalBytes } from '../crypto/bytes'
import { ManifestError } from './errors'
import { deriveGeometry } from './geometry'
import {
  compareCanonicalPaths,
  foldPathUnchecked,
  quotePathForDiagnostic,
  validateCanonicalPath,
} from './path-policy'

const MAX_DECODE_ARRAY_ELEMENTS = 1 << 20
// Go overrides the array budget but intentionally retains this decoder default.
const MAX_DECODE_MAP_PAIRS = 1 << 17
const MAX_DECODE_TOKENS = 8 << 20
const MAX_DECODE_NESTING_LEVELS = 32
const MAX_SIGNED_64 = (1n << 63n) - 1n
const KNOWN_MANIFEST_FIELDS = new Set(['v', 'chunkSize', 'entries'])
const KNOWN_ENTRY_FIELDS = new Set(['path', 'size', 'mtime', 'isDir'])

type ContainerKind = 'array' | 'map' | 'tag'

interface ContainerBudget {
  readonly kind: ContainerKind
  readonly indefinite: boolean
  remaining: number
  consumed: number
}

function indefiniteContainerSlotLimit(kind: ContainerKind): number {
  switch (kind) {
    case 'array':
      return MAX_DECODE_ARRAY_ELEMENTS
    case 'map':
      return MAX_DECODE_MAP_PAIRS * 2
    case 'tag':
      return 1
  }
}

class BoundedTokenizer implements DecodeTokenizer {
  readonly #inner: Tokenizer
  readonly #containers: ContainerBudget[] = []
  #tokens = 0

  constructor(data: Uint8Array, options: DecodeOptions) {
    this.#inner = new Tokenizer(data, options)
  }

  done(): boolean {
    return this.#inner.done()
  }

  pos(): number {
    return this.#inner.pos()
  }

  next(): Token {
    const token = this.#inner.next()
    if (Type.equals(token.type, Type.break)) {
      this.#closeIndefiniteContainer()
      return token
    }
    this.#consumeParentSlot()
    this.#tokens += 1
    if (this.#tokens > MAX_DECODE_TOKENS) {
      throw new Error('CBOR value exceeds the decode token budget')
    }
    if (
      Type.equals(token.type, Type.array) &&
      typeof token.value === 'number' &&
      Number.isFinite(token.value) &&
      token.value > MAX_DECODE_ARRAY_ELEMENTS
    ) {
      throw new Error(`CBOR array exceeds ${MAX_DECODE_ARRAY_ELEMENTS} elements`)
    }
    if (
      Type.equals(token.type, Type.map) &&
      typeof token.value === 'number' &&
      Number.isFinite(token.value) &&
      token.value > MAX_DECODE_MAP_PAIRS
    ) {
      throw new Error(`CBOR map exceeds ${MAX_DECODE_MAP_PAIRS} pairs`)
    }
    if (Type.equals(token.type, Type.array)) {
      this.#openContainer('array', token.value, 1)
    } else if (Type.equals(token.type, Type.map)) {
      this.#openContainer('map', token.value, 2)
    } else if (Type.equals(token.type, Type.tag)) {
      this.#openContainer('tag', 1, 1)
    } else {
      this.#closeCompletedContainers()
    }
    return token
  }

  #consumeParentSlot(): void {
    const parent = this.#containers.at(-1)
    if (parent === undefined) {
      return
    }
    if (!parent.indefinite) {
      if (parent.remaining <= 0) {
        throw new Error('CBOR container contains more values than declared')
      }
      parent.remaining -= 1
      return
    }

    parent.consumed += 1
    const limit = indefiniteContainerSlotLimit(parent.kind)
    if (parent.consumed > limit) {
      throw new Error(`CBOR indefinite ${parent.kind} exceeds the decode budget`)
    }
  }

  #openContainer(kind: ContainerKind, value: unknown, slotsPerItem: number): void {
    const indefinite = value === Infinity
    if (
      !indefinite &&
      (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 0)
    ) {
      throw new Error(`CBOR ${kind} has an invalid declared length`)
    }
    if (this.#containers.length >= MAX_DECODE_NESTING_LEVELS) {
      throw new Error(`CBOR nesting exceeds ${MAX_DECODE_NESTING_LEVELS} levels`)
    }
    this.#containers.push({
      kind,
      indefinite,
      remaining: indefinite ? Infinity : (value as number) * slotsPerItem,
      consumed: 0,
    })
    this.#closeCompletedContainers()
  }

  #closeIndefiniteContainer(): void {
    const container = this.#containers.at(-1)
    if (container === undefined || !container.indefinite || container.kind === 'tag') {
      throw new Error('CBOR break appears outside an indefinite container')
    }
    if (container.kind === 'map' && container.consumed % 2 !== 0) {
      throw new Error('CBOR indefinite map ends without a value')
    }
    this.#containers.pop()
    this.#closeCompletedContainers()
  }

  #closeCompletedContainers(): void {
    while (true) {
      const container = this.#containers.at(-1)
      if (container === undefined || container.indefinite || container.remaining !== 0) {
        return
      }
      this.#containers.pop()
    }
  }
}

function boundedDecode(data: Uint8Array, options: DecodeOptions): unknown {
  const tokenizer = new BoundedTokenizer(data, options)
  return decode(data, { ...options, tokenizer }) as unknown
}

const passthroughTags = new Proxy<Record<number, TagDecoder>>(
  {},
  {
    get: () => (decodeTagged: Parameters<TagDecoder>[0]) => decodeTagged(),
  },
)

const PROBE_OPTIONS: DecodeOptions = {
  allowBigInt: true,
  allowIndefinite: true,
  allowInfinity: true,
  allowNaN: true,
  allowUndefined: true,
  rejectDuplicateMapKeys: false,
  strict: false,
  tags: passthroughTags,
  useMaps: true,
}

const STRICT_OPTIONS: DecodeOptions = {
  allowBigInt: true,
  allowIndefinite: false,
  allowInfinity: false,
  allowNaN: false,
  allowUndefined: false,
  rejectDuplicateMapKeys: true,
  strict: true,
  useMaps: true,
}

function requireMap(value: unknown, label: string): Map<unknown, unknown> {
  if (!(value instanceof Map)) {
    throw new ManifestError('schema-mismatch', `${label} must be a CBOR map`)
  }
  return value
}

function exactFields(
  map: Map<unknown, unknown>,
  knownFields: ReadonlySet<string>,
  label: string,
): void {
  if (map.size !== knownFields.size) {
    throw new ManifestError('schema-mismatch', `${label} has missing or unknown fields`)
  }
  for (const key of map.keys()) {
    if (typeof key !== 'string' || !knownFields.has(key)) {
      throw new ManifestError('schema-mismatch', `${label} has an unknown field`)
    }
  }
}

function integerValue(value: unknown, label: string): bigint {
  if (typeof value === 'bigint') {
    return value
  }
  if (typeof value !== 'number' || !Number.isSafeInteger(value)) {
    throw new ManifestError('schema-mismatch', `${label} must be an integer`)
  }
  return BigInt(value)
}

function validatedChunkSize(value: bigint): number {
  if (value <= 0n || (value & (value - 1n)) !== 0n) {
    throw new ManifestError(
      'chunk-size-not-power-of-two',
      'Manifest chunkSize must be a positive power of two',
    )
  }
  if (value < BigInt(MIN_CHUNK_BYTES)) {
    throw new ManifestError(
      'chunk-size-too-small',
      `Manifest chunkSize must be at least ${MIN_CHUNK_BYTES} bytes`,
    )
  }
  if (value > BigInt(MAX_CHUNK_BYTES)) {
    throw new ManifestError(
      'chunk-size-too-large',
      `Manifest chunkSize must not exceed ${MAX_CHUNK_BYTES} bytes`,
    )
  }
  return Number(value)
}

function probeManifestVersion(plaintext: Uint8Array): void {
  let decoded: unknown
  try {
    decoded = boundedDecode(plaintext, PROBE_OPTIONS)
  } catch {
    // Decoder errors may render attacker-controlled map keys; keep them out of
    // browser telemetry while exposing the stable semantic category.
    throw new ManifestError('invalid-cbor', 'Unable to probe manifest version')
  }
  const map = requireMap(decoded, 'manifest')
  if (!map.has('v')) {
    throw new ManifestError('schema-mismatch', 'Manifest is missing version field v')
  }
  const version = integerValue(map.get('v'), 'manifest version')
  if (version !== BigInt(MANIFEST_VERSION)) {
    throw new ManifestError(
      'unsupported-version',
      `Manifest version ${version} is unsupported; upgrade required`,
    )
  }
}

interface WireEntry {
  readonly path: string
  readonly size: number
  readonly mtime: number
  readonly isDirectory: boolean
}

function parseEntry(value: unknown, index: number): WireEntry {
  const map = requireMap(value, `manifest entry ${index}`)
  exactFields(map, KNOWN_ENTRY_FIELDS, `manifest entry ${index}`)
  const path = map.get('path')
  const isDirectory = map.get('isDir')
  if (typeof path !== 'string' || typeof isDirectory !== 'boolean') {
    throw new ManifestError(
      'schema-mismatch',
      `Manifest entry ${index} has an invalid path or isDir field`,
    )
  }
  const sizeInteger = integerValue(map.get('size'), `manifest entry ${index} size`)
  if (sizeInteger < 0n) {
    throw new ManifestError('negative-size', `Manifest entry ${index} has a negative size`)
  }
  if (sizeInteger > MAX_SIGNED_64) {
    throw new ManifestError(
      'schema-mismatch',
      `Manifest entry ${index} size is outside the signed 64-bit wire range`,
    )
  }
  if (!isDirectory && sizeInteger > BigInt(MAX_STREAM_BYTES)) {
    throw new ManifestError(
      'stream-too-large',
      `Manifest entry ${index} size exceeds the shared stream ceiling`,
    )
  }
  const mtimeInteger = integerValue(map.get('mtime'), `manifest entry ${index} mtime`)
  if (
    mtimeInteger < BigInt(MIN_MTIME_MILLISECONDS) ||
    mtimeInteger > BigInt(MAX_MTIME_MILLISECONDS)
  ) {
    throw new ManifestError(
      'mtime-out-of-range',
      `Manifest entry ${index} mtime is outside the interoperable integer range`,
    )
  }
  validateCanonicalPath(path)
  return Object.freeze({
    path,
    // Directory size is authenticated wire data but never stream geometry. Avoid
    // narrowing an irrelevant int64 that JavaScript cannot represent exactly.
    size: isDirectory ? 0 : Number(sizeInteger),
    mtime: Number(mtimeInteger),
    isDirectory,
  })
}

interface PathRecord {
  readonly entry: WireEntry
  readonly collisionKey: string
}

function nextSegment(path: string, start: number): readonly [number, boolean] {
  const slash = path.indexOf('/', start)
  return slash === -1 ? [path.length, false] : [slash, true]
}

function foldedPrefixSpellingMismatch(
  left: string,
  right: string,
): readonly [string, string] | undefined {
  for (let leftStart = 0, rightStart = 0; ; ) {
    const [leftEnd, leftMore] = nextSegment(left, leftStart)
    const [rightEnd, rightMore] = nextSegment(right, rightStart)
    const leftSegment = left.slice(leftStart, leftEnd)
    const rightSegment = right.slice(rightStart, rightEnd)
    if (foldPathUnchecked(leftSegment) !== foldPathUnchecked(rightSegment)) {
      return undefined
    }
    if (leftSegment !== rightSegment) {
      return [left.slice(0, leftEnd), right.slice(0, rightEnd)]
    }
    if (!leftMore || !rightMore) {
      return undefined
    }
    leftStart = leftEnd + 1
    rightStart = rightEnd + 1
  }
}

function lowerBound(records: readonly PathRecord[], key: string): number {
  let low = 0
  let high = records.length
  while (low < high) {
    const middle = low + Math.floor((high - low) / 2)
    const record = records[middle]
    if (record === undefined || compareCanonicalPaths(record.collisionKey, key) >= 0) {
      high = middle
    } else {
      low = middle + 1
    }
  }
  return low
}

function collectPathRecords(entries: readonly WireEntry[]): PathRecord[] {
  const seen = new Map<string, string>()
  const records: PathRecord[] = []
  for (const entry of entries) {
    const collisionKey = foldPathUnchecked(entry.path)
    const first = seen.get(collisionKey)
    if (first !== undefined) {
      if (first === entry.path) {
        throw new ManifestError(
          'duplicate-path',
          `Manifest repeats path ${quotePathForDiagnostic(entry.path)}`,
        )
      }
      throw new ManifestError(
        'path-collision',
        `Manifest paths ${quotePathForDiagnostic(first)} and ${quotePathForDiagnostic(entry.path)} have the same cross-platform identity`,
      )
    }
    seen.set(collisionKey, entry.path)
    records.push({ entry, collisionKey })
  }
  return records
}

function rejectPrefixMismatch(mismatch: readonly [string, string]): never {
  throw new ManifestError(
    'path-collision',
    `Manifest path prefixes ${quotePathForDiagnostic(mismatch[0])} and ${quotePathForDiagnostic(mismatch[1])} have the same cross-platform identity`,
  )
}

function validateAdjacentPrefixes(previous: PathRecord | undefined, current: PathRecord): void {
  if (previous === undefined) {
    return
  }
  const mismatch = foldedPrefixSpellingMismatch(previous.entry.path, current.entry.path)
  if (mismatch !== undefined) {
    rejectPrefixMismatch(mismatch)
  }
}

function validateDescendant(records: readonly PathRecord[], current: PathRecord): void {
  const descendantPrefix = `${current.collisionKey}/`
  const descendant = records[lowerBound(records, descendantPrefix)]
  if (descendant === undefined || !descendant.collisionKey.startsWith(descendantPrefix)) {
    return
  }
  const mismatch = foldedPrefixSpellingMismatch(current.entry.path, descendant.entry.path)
  if (mismatch !== undefined) {
    rejectPrefixMismatch(mismatch)
  }
  if (!current.entry.isDirectory) {
    throw new ManifestError(
      'path-type-conflict',
      `File ${quotePathForDiagnostic(current.entry.path)} cannot contain descendants`,
    )
  }
}

function validatePathCollection(entries: readonly WireEntry[]): void {
  const records = collectPathRecords(entries)

  records.sort((left, right) =>
    compareCanonicalPaths(left.collisionKey, right.collisionKey),
  )
  for (let index = 0; index < records.length; index += 1) {
    const current = records[index]
    if (current === undefined) {
      continue
    }
    validateAdjacentPrefixes(records[index - 1], current)
    validateDescendant(records, current)
  }
}

function domainEntry(entry: WireEntry): ManifestEntry {
  const path = entry.path as ManifestEntry['path']
  const mtime = entry.mtime as UnixMilliseconds
  return entry.isDirectory
    ? Object.freeze({ kind: 'directory', path, mtime })
    : Object.freeze({ kind: 'file', path, size: entry.size as ByteLength, mtime })
}

function parseManifest(decoded: unknown): ValidatedManifestV1 {
  const map = requireMap(decoded, 'manifest')
  exactFields(map, KNOWN_MANIFEST_FIELDS, 'manifest')
  const version = integerValue(map.get('v'), 'manifest version')
  if (version !== BigInt(MANIFEST_VERSION)) {
    throw new ManifestError(
      'unsupported-version',
      `Manifest version ${version} is unsupported; upgrade required`,
    )
  }
  const chunkSizeInteger = integerValue(map.get('chunkSize'), 'manifest chunkSize')
  const chunkSize = validatedChunkSize(chunkSizeInteger)
  const entriesValue = map.get('entries')
  if (!Array.isArray(entriesValue)) {
    throw new ManifestError('schema-mismatch', 'Manifest entries must be a CBOR array')
  }
  const entries = entriesValue.map((entry, index) => parseEntry(entry, index))
  validatePathCollection(entries)
  const domainEntries = Object.freeze(entries.map(domainEntry))
  deriveGeometry(domainEntries, chunkSize)
  return Object.freeze({
    version: MANIFEST_VERSION,
    chunkSize: chunkSize as ChunkSize,
    entries: domainEntries,
  }) as unknown as ValidatedManifestV1
}

export function decodeCanonicalManifest(plaintext: Uint8Array): ValidatedManifestV1 {
  if (plaintext.byteLength > MAX_SEALED_MANIFEST_BYTES) {
    throw new ManifestError(
      'manifest-too-large',
      `Manifest CBOR exceeds the ${MAX_SEALED_MANIFEST_BYTES}-byte sealed-manifest ceiling`,
    )
  }
  probeManifestVersion(plaintext)
  let decoded: unknown
  try {
    decoded = boundedDecode(plaintext, STRICT_OPTIONS)
  } catch {
    throw new ManifestError('non-canonical', 'Manifest CBOR violates strict decoding rules')
  }
  let canonical: Uint8Array
  try {
    canonical = encode(decoded, rfc8949EncodeOptions)
  } catch {
    throw new ManifestError('non-canonical', 'Manifest CBOR cannot be deterministically encoded')
  }
  if (!equalBytes(canonical, plaintext)) {
    throw new ManifestError(
      'non-canonical',
      'Manifest CBOR differs from its RFC 8949 deterministic re-encoding',
    )
  }
  return parseManifest(decoded)
}

import { concatBytes, encodeBase64Url, equalBytes } from '../crypto/bytes'
import { sha256 } from '../crypto/digest'
import { snapshotPortableCatalogPath } from '../catalog/path-policy'
import type {
  V2CatalogModifiedTime,
  V2ShareDescriptor,
} from '../catalog/v2-records'
import {
  V2_MAXIMUM_SELECTION_RULE_OVERRIDES,
  type V2FrozenSelectionPolicy,
} from '../catalog/v2-selection'

export const V2_MAXIMUM_SELECTION_PLAN_BYTES = 64 << 20
export const V2_MAXIMUM_SELECTION_PLAN_ENTRIES = 4_194_304

const IDENTITY_BYTES = 16
const SELECTION_ENCODING_CHUNK_BYTES = 64 << 10
const SELECTION_NODE_CLAIM_HEAP_BYTES = 192
const SELECTION_DIRECTORY_HEAP_BYTES = 768
const SELECTION_FILE_HEAP_BYTES = 896
const SELECTION_PATH_PEAK_MULTIPLIER = 4
const MAXIMUM_SELECTION_NODE_CLAIMS = V2_MAXIMUM_SELECTION_PLAN_ENTRIES + 1
const DIRECTORY_KIND = 1
const FILE_KIND = 2
const NODE_SELECTION_MODE = 1
const MILLISECONDS_PER_SECOND = 1_000
const NANOSECONDS_PER_MILLISECOND = 1_000_000
const NANOSECONDS_PER_SECOND = 1_000_000_000
const encoder = new TextEncoder()

export interface V2OutputSelectionDirectory {
  readonly path: readonly string[]
  readonly directoryId: Uint8Array<ArrayBuffer>
  readonly generation: Uint8Array<ArrayBuffer>
  readonly modifiedTime?: V2CatalogModifiedTime
}

export interface V2OutputSelectionFile {
  readonly path: readonly string[]
  readonly fileId: Uint8Array<ArrayBuffer>
  readonly parentDirectoryId: Uint8Array<ArrayBuffer>
  readonly parentGeneration: Uint8Array<ArrayBuffer>
  readonly expectedSize: bigint
  readonly modifiedTime?: V2CatalogModifiedTime
}

export interface V2OutputSelection {
  readonly shareInstance: Uint8Array<ArrayBuffer>
  readonly syntheticRoot: Uint8Array<ArrayBuffer>
  readonly rootGeneration: Uint8Array<ArrayBuffer>
  readonly directories: readonly V2OutputSelectionDirectory[]
  readonly files: readonly V2OutputSelectionFile[]
  readonly selectionIdentity: Uint8Array<ArrayBuffer>
  readonly selectionIdentityText: string
  readonly canonicalSelection: Uint8Array<ArrayBuffer>
  readonly resumeIntent: Uint8Array<ArrayBuffer>
  readonly resumeIntentText: string
}

interface CanonicalSelectionDirectoryRecord {
  readonly kind: 1
  readonly path: string
  readonly pathBytes: Uint8Array<ArrayBuffer>
  readonly selection: V2OutputSelectionDirectory
}

interface CanonicalSelectionFileRecord {
  readonly kind: 2
  readonly path: string
  readonly pathBytes: Uint8Array<ArrayBuffer>
  readonly selection: V2OutputSelectionFile
}

type CanonicalSelectionRecord =
  | CanonicalSelectionDirectoryRecord
  | CanonicalSelectionFileRecord

/**
 * Discovery owns this bounded builder until it reaches a terminal catalog state.
 * Charging records as they arrive prevents a wide authenticated tree from first
 * becoming an unbounded JavaScript array and only then being rejected. Claims
 * include unselected nodes because selection must not hide malformed identity reuse.
 */
export class V2OutputSelectionPlan {
  readonly #directories: V2OutputSelectionDirectory[] = []
  readonly #files: V2OutputSelectionFile[] = []
  readonly #nodeClaims = new Set<string>()
  #estimatedBytes = 0
  #frozen = false

  claimNode(nodeId: Uint8Array): void {
    this.#requireOpen()
    requireIdentity(nodeId, 'catalog node')
    const claim = encodeBase64Url(nodeId)
    if (this.#nodeClaims.has(claim)) {
      throw new TypeError('Selection discovery repeats an opaque node identity')
    }
    if (this.#nodeClaims.size >= MAXIMUM_SELECTION_NODE_CLAIMS) {
      throw new V2OutputSelectionBudgetError(
        'Selection discovery node count exceeds its local budget',
      )
    }
    this.#chargeBytes(SELECTION_NODE_CLAIM_HEAP_BYTES)
    this.#nodeClaims.add(claim)
  }

  addDirectory(directory: V2OutputSelectionDirectory): void {
    this.#requireOpen()
    const owned = snapshotDirectory(directory)
    this.#charge(selectionRecordCharge(owned.path, SELECTION_DIRECTORY_HEAP_BYTES))
    this.#directories.push(owned)
  }

  addFile(file: V2OutputSelectionFile): void {
    this.#requireOpen()
    const owned = snapshotFile(file)
    this.#charge(selectionRecordCharge(owned.path, SELECTION_FILE_HEAP_BYTES))
    this.#files.push(owned)
  }

  async freeze(
    descriptor: V2ShareDescriptor,
    rules: V2FrozenSelectionPolicy,
    rootGeneration: Uint8Array,
  ): Promise<V2OutputSelection> {
    this.#requireOpen()
    this.#frozen = true
    // Claims guard discovery authority, not output identity. Releasing them at
    // the terminal boundary avoids retaining the full tree while canonicalizing
    // the selected subset.
    this.#nodeClaims.clear()
    try {
      return await createV2OutputSelection(
        descriptor,
        rules,
        rootGeneration,
        this.#directories,
        this.#files,
      )
    } finally {
      // The frozen result owns fresh records. Releasing discovery's copies keeps
      // the terminal plan from permanently doubling its browser heap footprint.
      this.#directories.length = 0
      this.#files.length = 0
      this.#nodeClaims.clear()
      this.#estimatedBytes = 0
    }
  }

  #charge(bytes: number): void {
    if (this.#directories.length + this.#files.length >= V2_MAXIMUM_SELECTION_PLAN_ENTRIES) {
      throw new V2OutputSelectionBudgetError('Selection plan entry count exceeds its local budget')
    }
    this.#chargeBytes(bytes)
  }

  #chargeBytes(bytes: number): void {
    this.#estimatedBytes = chargeSelectionPlan(this.#estimatedBytes, bytes)
  }

  #requireOpen(): void {
    if (this.#frozen) throw new Error('Selection plan is already frozen')
  }
}

export async function createV2OutputSelection(
  descriptor: V2ShareDescriptor,
  rules: V2FrozenSelectionPolicy,
  rootGeneration: Uint8Array,
  directories: readonly V2OutputSelectionDirectory[],
  files: readonly V2OutputSelectionFile[],
): Promise<V2OutputSelection> {
  requireIdentity(descriptor.shareInstance, 'share instance')
  requireIdentity(descriptor.syntheticRoot, 'synthetic root')
  requireIdentity(rootGeneration, 'root generation')
  if (directories.length + files.length > V2_MAXIMUM_SELECTION_PLAN_ENTRIES) {
    throw new V2OutputSelectionBudgetError('Selection plan entry count exceeds its local budget')
  }
  const directoryRecords: CanonicalSelectionDirectoryRecord[] = []
  const fileRecords: CanonicalSelectionFileRecord[] = []
  let planBytes = 0
  for (const directory of directories) {
    const record = canonicalDirectoryRecord(directory)
    planBytes = chargeSelectionPlan(
      planBytes,
      selectionEncodedRecordCharge(record.pathBytes.byteLength, SELECTION_DIRECTORY_HEAP_BYTES),
    )
    directoryRecords.push(record)
  }
  for (const file of files) {
    const record = canonicalFileRecord(file)
    planBytes = chargeSelectionPlan(
      planBytes,
      selectionEncodedRecordCharge(record.pathBytes.byteLength, SELECTION_FILE_HEAP_BYTES),
    )
    fileRecords.push(record)
  }
  directoryRecords.sort((left, right) => compareBytes(left.pathBytes, right.pathBytes))
  fileRecords.sort((left, right) => compareBytes(left.pathBytes, right.pathBytes))
  validatePlan(
    descriptor.syntheticRoot,
    rootGeneration,
    directoryRecords,
    fileRecords,
  )
  const ownedDirectories = directoryRecords.map((record) => record.selection)
  const ownedFiles = fileRecords.map((record) => record.selection)

  const canonicalRequest = encodeCanonicalRequest(descriptor, rules)
  const canonical = new BoundedByteBuilder()
  canonical.raw(canonicalRequest)
  canonical.field(rootGeneration)
  canonical.count(BigInt(ownedDirectories.length))
  for (const directory of directoryRecords) encodeCanonicalDirectory(canonical, directory)
  canonical.count(BigInt(ownedFiles.length))
  for (const file of fileRecords) encodeCanonicalFile(canonical, file)
  canonical.field(encoder.encode('native-tree'))
  canonical.field(encoder.encode('no-replace'))
  const canonicalSelection = canonical.finish()

  const identityInput = new BoundedByteBuilder()
  identityInput.field(encoder.encode('windshare/output-selection/v1'))
  identityInput.field(descriptor.shareInstance)
  identityInput.field(descriptor.syntheticRoot)
  identityInput.field(rootGeneration)
  identityInput.count(BigInt(directoryRecords.length + fileRecords.length))
  visitCanonicalSelectionRecords(directoryRecords, fileRecords, (record) => {
    identityInput.raw(Uint8Array.of(record.kind))
    if (record.kind === DIRECTORY_KIND) {
      encodeIdentityDirectory(identityInput, record)
    } else {
      encodeIdentityFile(identityInput, record)
    }
  })
  const selectionIdentity = await sha256(identityInput.finish())
  const resumeIntent = await sha256(concatBytes([
    encoder.encode('windshare/output-resume-intent/v3'),
    canonicalSelection,
  ]))
  return Object.freeze({
    shareInstance: descriptor.shareInstance.slice(),
    syntheticRoot: descriptor.syntheticRoot.slice(),
    rootGeneration: rootGeneration.slice(),
    directories: Object.freeze(ownedDirectories),
    files: Object.freeze(ownedFiles),
    selectionIdentity,
    selectionIdentityText: encodeBase64Url(selectionIdentity),
    canonicalSelection,
    resumeIntent,
    resumeIntentText: encodeBase64Url(resumeIntent),
  })
}

export class V2OutputSelectionBudgetError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'V2OutputSelectionBudgetError'
  }
}

function snapshotDirectory(
  directory: V2OutputSelectionDirectory,
): V2OutputSelectionDirectory {
  requireIdentity(directory.directoryId, 'selected directory')
  requireIdentity(directory.generation, 'selected directory generation')
  return Object.freeze({
    path: snapshotPortableCatalogPath(directory.path),
    directoryId: directory.directoryId.slice(),
    generation: directory.generation.slice(),
    ...(directory.modifiedTime === undefined
      ? {}
      : { modifiedTime: snapshotModifiedTime(directory.modifiedTime) }),
  })
}

function snapshotFile(file: V2OutputSelectionFile): V2OutputSelectionFile {
  requireIdentity(file.fileId, 'selected file')
  requireIdentity(file.parentDirectoryId, 'selected file parent')
  requireIdentity(file.parentGeneration, 'selected file parent generation')
  if (file.expectedSize < 0n || file.expectedSize > BigInt(Number.MAX_SAFE_INTEGER)) {
    throw new TypeError('Selected file size is outside the protocol range')
  }
  return Object.freeze({
    path: snapshotPortableCatalogPath(file.path),
    fileId: file.fileId.slice(),
    parentDirectoryId: file.parentDirectoryId.slice(),
    parentGeneration: file.parentGeneration.slice(),
    expectedSize: file.expectedSize,
    ...(file.modifiedTime === undefined
      ? {}
      : { modifiedTime: snapshotModifiedTime(file.modifiedTime) }),
  })
}

function canonicalDirectoryRecord(
  directory: V2OutputSelectionDirectory,
): CanonicalSelectionDirectoryRecord {
  const selection = snapshotDirectory(directory)
  const path = canonicalPath(selection.path)
  return Object.freeze({
    kind: DIRECTORY_KIND,
    path,
    pathBytes: encoder.encode(path),
    selection,
  })
}

function canonicalFileRecord(file: V2OutputSelectionFile): CanonicalSelectionFileRecord {
  const selection = snapshotFile(file)
  const path = canonicalPath(selection.path)
  return Object.freeze({
    kind: FILE_KIND,
    path,
    pathBytes: encoder.encode(path),
    selection,
  })
}

function snapshotModifiedTime(modified: V2CatalogModifiedTime): V2CatalogModifiedTime {
  const maximumSeconds = BigInt(Number.MAX_SAFE_INTEGER)
  const validPrecision = modified.precision === 1 || modified.precision === 2 || modified.precision === 3
  if (modified.seconds < -maximumSeconds || modified.seconds > maximumSeconds ||
      !Number.isSafeInteger(modified.nanoseconds) || modified.nanoseconds < 0 ||
      modified.nanoseconds >= NANOSECONDS_PER_SECOND || !validPrecision ||
      (modified.precision === 1 && modified.nanoseconds !== 0) ||
      (modified.precision === 2 && modified.nanoseconds % NANOSECONDS_PER_MILLISECOND !== 0) ||
      modified.milliseconds !== modified.seconds * BigInt(MILLISECONDS_PER_SECOND) +
        BigInt(Math.floor(modified.nanoseconds / NANOSECONDS_PER_MILLISECOND))) {
    throw new TypeError('Selected modified time is outside its authenticated portable precision')
  }
  return Object.freeze({ ...modified })
}

function validatePlan(
  root: Uint8Array,
  rootGeneration: Uint8Array,
  directories: readonly CanonicalSelectionDirectoryRecord[],
  files: readonly CanonicalSelectionFileRecord[],
): void {
  const ancestry: CanonicalSelectionDirectoryRecord[] = []
  const nodeClaims = new Set<string>([encodeBase64Url(root)])
  let estimatedBytes = 0
  let previousPathBytes: Uint8Array<ArrayBuffer> | undefined
  visitCanonicalSelectionRecords(directories, files, (record) => {
    if (previousPathBytes !== undefined && compareBytes(previousPathBytes, record.pathBytes) >= 0) {
      throw new TypeError('Selection plan repeats or reorders an output path')
    }
    previousPathBytes = record.pathBytes
    const nodeId = record.kind === DIRECTORY_KIND
      ? record.selection.directoryId
      : record.selection.fileId
    const nodeClaim = encodeBase64Url(nodeId)
    if (nodeClaims.has(nodeClaim)) {
      throw new TypeError('Selection plan repeats an opaque node identity')
    }
    nodeClaims.add(nodeClaim)
    const parent = parentPath(record.path)
    while (ancestry.length > 0 && ancestry[ancestry.length - 1]?.path !== parent) {
      ancestry.pop()
    }
    if (parent.length > 0 && ancestry.length === 0) {
      throw new TypeError('Selection entry is missing its selected parent')
    }
    if (record.kind === DIRECTORY_KIND) {
      ancestry.push(record)
      estimatedBytes += selectionEncodedRecordCharge(
        record.pathBytes.byteLength,
        SELECTION_DIRECTORY_HEAP_BYTES,
      )
      return
    }
    const file = record.selection
    if (parent.length === 0) {
      if (!equalBytes(file.parentDirectoryId, root) ||
          !equalBytes(file.parentGeneration, rootGeneration)) {
        throw new TypeError('Root selection file has inconsistent parent authority')
      }
    } else {
      const directory = ancestry[ancestry.length - 1]?.selection
      if (directory === undefined ||
          !equalBytes(file.parentDirectoryId, directory.directoryId) ||
          !equalBytes(file.parentGeneration, directory.generation)) {
        throw new TypeError('Selection file has inconsistent parent authority')
      }
    }
    estimatedBytes += selectionEncodedRecordCharge(record.pathBytes.byteLength, SELECTION_FILE_HEAP_BYTES)
  })
  if (estimatedBytes > V2_MAXIMUM_SELECTION_PLAN_BYTES) {
    throw new V2OutputSelectionBudgetError('Selection plan exceeds its local memory budget')
  }
}

function encodeCanonicalRequest(
  descriptor: V2ShareDescriptor,
  rules: V2FrozenSelectionPolicy,
): Uint8Array<ArrayBuffer> {
  if (typeof rules.defaultSelected !== 'boolean' ||
      rules.canonicalRules.length > V2_MAXIMUM_SELECTION_RULE_OVERRIDES) {
    throw new TypeError('Frozen selection rules are invalid')
  }
  const builder = new BoundedByteBuilder()
  builder.field(descriptor.shareInstance)
  builder.field(descriptor.syntheticRoot)
  builder.field(Uint8Array.of(NODE_SELECTION_MODE))
  builder.field(Uint8Array.of(rules.defaultSelected ? 1 : 0))
  const canonicalRules = [...rules.canonicalRules]
  for (const rule of canonicalRules) {
    if ((rule.kind !== 'directory' && rule.kind !== 'file') ||
        typeof rule.selected !== 'boolean') {
      throw new TypeError('Frozen selection rule has invalid semantics')
    }
    requireIdentity(rule.id, 'selection rule')
  }
  canonicalRules.sort((left, right) => compareBytes(left.id, right.id) ||
    ruleKind(left.kind) - ruleKind(right.kind))
  for (let index = 1; index < canonicalRules.length; index += 1) {
    const previous = canonicalRules[index - 1]
    const current = canonicalRules[index]
    if (previous !== undefined && current !== undefined &&
        previous.kind === current.kind && equalBytes(previous.id, current.id)) {
      throw new TypeError('Frozen selection rules repeat an opaque identity')
    }
  }
  builder.count(BigInt(canonicalRules.length))
  for (const rule of canonicalRules) {
    builder.field(Uint8Array.of(ruleKind(rule.kind)))
    builder.field(rule.id)
    builder.field(Uint8Array.of(rule.selected ? 1 : 0))
  }
  return builder.finish()
}

function encodeCanonicalDirectory(
  builder: BoundedByteBuilder,
  record: CanonicalSelectionDirectoryRecord,
): void {
  const directory = record.selection
  builder.field(record.pathBytes)
  builder.field(directory.directoryId)
  builder.field(directory.generation)
  builder.canonicalModifiedTime(directory.modifiedTime)
}

function encodeCanonicalFile(
  builder: BoundedByteBuilder,
  record: CanonicalSelectionFileRecord,
): void {
  const file = record.selection
  builder.field(record.pathBytes)
  builder.field(file.fileId)
  builder.field(file.parentDirectoryId)
  builder.field(file.parentGeneration)
  builder.field(uint64(file.expectedSize))
  builder.canonicalModifiedTime(file.modifiedTime)
}

function encodeIdentityDirectory(
  builder: BoundedByteBuilder,
  record: CanonicalSelectionDirectoryRecord,
): void {
  const directory = record.selection
  builder.field(record.pathBytes)
  builder.field(directory.directoryId)
  builder.field(directory.generation)
  builder.identityModifiedTime(directory.modifiedTime)
}

function encodeIdentityFile(
  builder: BoundedByteBuilder,
  record: CanonicalSelectionFileRecord,
): void {
  const file = record.selection
  builder.field(record.pathBytes)
  builder.field(file.fileId)
  builder.field(file.parentDirectoryId)
  builder.field(file.parentGeneration)
  builder.raw(uint64(file.expectedSize))
  builder.identityModifiedTime(file.modifiedTime)
}

function visitCanonicalSelectionRecords(
  directories: readonly CanonicalSelectionDirectoryRecord[],
  files: readonly CanonicalSelectionFileRecord[],
  visit: (record: CanonicalSelectionRecord) => void,
): void {
  let directoryIndex = 0
  let fileIndex = 0
  while (directoryIndex < directories.length || fileIndex < files.length) {
    const directory = directories[directoryIndex]
    const file = files[fileIndex]
    if (directory !== undefined &&
        (file === undefined || compareBytes(directory.pathBytes, file.pathBytes) <= 0)) {
      visit(directory)
      directoryIndex += 1
    } else if (file !== undefined) {
      visit(file)
      fileIndex += 1
    }
  }
}

function selectionRecordCharge(path: readonly string[], fixedBytes: number): number {
  // Freeze briefly owns discovery records, canonical records, encoded bytes and
  // the final immutable plan together. Charging that peak—not just wire bytes—
  // keeps the advertised browser budget meaningful across JavaScript engines.
  return selectionEncodedRecordCharge(encoder.encode(canonicalPath(path)).byteLength, fixedBytes)
}

function selectionEncodedRecordCharge(pathBytes: number, fixedBytes: number): number {
  return fixedBytes + pathBytes * SELECTION_PATH_PEAK_MULTIPLIER
}

function chargeSelectionPlan(current: number, added: number): number {
  if (!Number.isSafeInteger(added) || added < 0 ||
      added > V2_MAXIMUM_SELECTION_PLAN_BYTES - current) {
    throw new V2OutputSelectionBudgetError('Selection plan exceeds its local memory budget')
  }
  return current + added
}

class BoundedByteBuilder {
  readonly #chunks: Uint8Array<ArrayBuffer>[] = []
  #tail: Uint8Array<ArrayBuffer> | undefined
  #tailBytes = 0
  #bytes = 0

  raw(value: Uint8Array): void {
    if (value.byteLength > V2_MAXIMUM_SELECTION_PLAN_BYTES - this.#bytes) {
      throw new V2OutputSelectionBudgetError('Canonical selection exceeds its local memory budget')
    }
    let sourceOffset = 0
    while (sourceOffset < value.byteLength) {
      if (this.#tail === undefined || this.#tailBytes === this.#tail.byteLength) {
        this.#tail = new Uint8Array(SELECTION_ENCODING_CHUNK_BYTES)
        this.#tailBytes = 0
        this.#chunks.push(this.#tail)
      }
      const copied = Math.min(
        value.byteLength - sourceOffset,
        this.#tail.byteLength - this.#tailBytes,
      )
      this.#tail.set(value.subarray(sourceOffset, sourceOffset + copied), this.#tailBytes)
      sourceOffset += copied
      this.#tailBytes += copied
      this.#bytes += copied
    }
  }

  field(value: Uint8Array): void {
    this.raw(uint64(BigInt(value.byteLength)))
    this.raw(value)
  }

  count(value: bigint): void {
    this.raw(uint64(value))
  }

  canonicalModifiedTime(modified?: V2CatalogModifiedTime): void {
    this.field(Uint8Array.of(modified === undefined ? 0 : 1))
    this.field(int64(modified?.seconds ?? 0n))
    this.field(uint32(modified?.nanoseconds ?? 0))
    this.field(Uint8Array.of(modified?.precision ?? 0))
  }

  identityModifiedTime(modified?: V2CatalogModifiedTime): void {
    this.raw(Uint8Array.of(modified === undefined ? 0 : 1))
    this.raw(int64(modified?.seconds ?? 0n))
    this.raw(uint32(modified?.nanoseconds ?? 0))
    this.raw(Uint8Array.of(modified?.precision ?? 0))
  }

  finish(): Uint8Array<ArrayBuffer> {
    const result = new Uint8Array(this.#bytes)
    let offset = 0
    for (let index = 0; index < this.#chunks.length; index += 1) {
      const chunk = this.#chunks[index]
      if (chunk === undefined) continue
      const length = index === this.#chunks.length - 1 ? this.#tailBytes : chunk.byteLength
      result.set(chunk.subarray(0, length), offset)
      offset += length
    }
    return result
  }
}

function canonicalPath(path: readonly string[]): string {
  return path.join('/')
}

function parentPath(path: string): string {
  const separator = path.lastIndexOf('/')
  return separator < 0 ? '' : path.slice(0, separator)
}

function compareBytes(left: Uint8Array, right: Uint8Array): number {
  const length = Math.min(left.byteLength, right.byteLength)
  for (let index = 0; index < length; index += 1) {
    const comparison = (left[index] ?? 0) - (right[index] ?? 0)
    if (comparison !== 0) return comparison
  }
  return left.byteLength - right.byteLength
}

function ruleKind(kind: 'directory' | 'file'): number {
  return kind === 'directory' ? DIRECTORY_KIND : FILE_KIND
}

function requireIdentity(value: Uint8Array, label: string): void {
  if (value.byteLength !== IDENTITY_BYTES || value.every((byte) => byte === 0)) {
    throw new TypeError(`${label} identity is invalid`)
  }
}

function uint64(value: bigint): Uint8Array<ArrayBuffer> {
  if (value < 0n || value > 0xffff_ffff_ffff_ffffn) {
    throw new RangeError('Canonical unsigned integer is outside uint64')
  }
  const encoded = new Uint8Array(8)
  new DataView(encoded.buffer).setBigUint64(0, value, false)
  return encoded
}

function int64(value: bigint): Uint8Array<ArrayBuffer> {
  if (value < -0x8000_0000_0000_0000n || value > 0x7fff_ffff_ffff_ffffn) {
    throw new RangeError('Canonical signed integer is outside int64')
  }
  const encoded = new Uint8Array(8)
  new DataView(encoded.buffer).setBigInt64(0, value, false)
  return encoded
}

function uint32(value: number): Uint8Array<ArrayBuffer> {
  if (!Number.isSafeInteger(value) || value < 0 || value > 0xffff_ffff) {
    throw new RangeError('Canonical unsigned integer is outside uint32')
  }
  const encoded = new Uint8Array(4)
  new DataView(encoded.buffer).setUint32(0, value, false)
  return encoded
}

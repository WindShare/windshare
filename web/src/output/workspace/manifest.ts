import { snapshotPortableCatalogPath } from '../../catalog/path-policy'
import type { FinalFileCheckpointProof } from '../persistence/journal'
import type { PreparationManifestV1 } from './preparation'
import {
  canonicalDigest,
  canonicalFrame,
  canonicalIdentity,
  canonicalPath,
  canonicalRecord,
  canonicalU8,
  canonicalU64,
  concatCanonicalBytes,
  equalCanonicalBytes,
  snapshotIdentity,
  type CanonicalBytes,
} from './canonical'
import {
  createManifestPageRecord,
  MANIFEST_PAGE_ENTRY_LIMIT,
  RECEIVE_RECORD_MATERIALIZED_MANIFEST,
  type ManifestPageRecord,
} from './records'
import { CanonicalRecordReader } from './canonical-reader'

const TEXT_ENCODER = new TextEncoder()

export const MAX_ARTIFACT_ENTRIES = 1_000_000

export type PreparationBinding =
  | Readonly<{ kind: 'absent' }>
  | Readonly<{ kind: 'present'; preparationDigest: string }>

export interface AuthenticatedGenerationReference {
  readonly directoryId: string
  readonly generation: string
}

export interface MaterializedDirectoryEntry {
  readonly kind: 'directory'
  readonly artifactPath: readonly string[]
  readonly directoryId: string
  readonly generation: string
  readonly ownedObjectId: string
}

export interface MaterializedFileEntry {
  readonly kind: 'file'
  readonly artifactPath: readonly string[]
  readonly fileId: string
  readonly fileRevision: string
  readonly exactSize: bigint
  readonly ownedObjectId: string
  readonly checkpoint: Readonly<{
    recordId: string
    recordDigest: string
    checkpointGeneration: bigint
  }>
}

export type MaterializedManifestEntry = MaterializedDirectoryEntry | MaterializedFileEntry

export interface MaterializedManifestV1 {
  readonly schemaVersion: 1
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly materializationBindingDigest: string
  readonly preparationBinding: PreparationBinding
  readonly generations: readonly AuthenticatedGenerationReference[]
  readonly entries: readonly MaterializedManifestEntry[]
  readonly entryCount: bigint
  readonly fileCount: bigint
  readonly directoryCount: bigint
  readonly rawBytes: bigint
  readonly canonicalMetadataBytes: bigint
  readonly canonicalBytes: CanonicalBytes
  readonly digest: string
}

export interface FinalCheckpointReader {
  readFinalCheckpoint(
    recordId: string,
    checkpointGeneration: bigint,
  ): Promise<FinalFileCheckpointProof | undefined>
}

export type FinalCheckpointRecoveryEvidence = Readonly<{
  fileId: string
  fileRevision: string
  artifactPath: readonly string[]
  exactSize: bigint
  ownedObjectId: string
  recordDigest: string
  checkpointGeneration: bigint
}>

export interface FinalCheckpointRecoveryReader extends FinalCheckpointReader {
  recoverFinalCheckpoint(
    evidence: FinalCheckpointRecoveryEvidence,
  ): Promise<FinalFileCheckpointProof | undefined>
}

export async function sealMaterializedManifest(input: {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly materializationBindingDigest: string
  readonly preparationBinding: PreparationBinding
  readonly generations: readonly AuthenticatedGenerationReference[]
  readonly entries: readonly MaterializedManifestEntry[]
  readonly checkpoints: FinalCheckpointReader
  readonly preparation?: PreparationManifestV1
}): Promise<MaterializedManifestV1> {
  if (input.entries.length > MAX_ARTIFACT_ENTRIES ||
      input.generations.length > MAX_ARTIFACT_ENTRIES) {
    throw new TypeError('materialized manifest exceeds the artifact entry bound')
  }
  const operationId = snapshotIdentity(input.operationId, 16, 'operation ID')
  const receiveIntentDigest = snapshotIdentity(
    input.receiveIntentDigest,
    32,
    'receive intent digest',
  )
  const materializationBindingDigest = snapshotIdentity(
    input.materializationBindingDigest,
    32,
    'materialization binding digest',
  )
  const preparationBinding = snapshotPreparationBinding(input.preparationBinding)
  const generations = snapshotGenerations(input.generations)
  const entries = await snapshotAndVerifyEntries(
    input.entries,
    input.checkpoints,
    { operationId, receiveIntentDigest, materializationBindingDigest },
  )
  validateManifestTopology(entries, generations)
  validatePreparationAuthority(preparationBinding, input.preparation, entries, generations)
  const fileCount = BigInt(entries.filter((entry) => entry.kind === 'file').length)
  const directoryCount = BigInt(entries.length) - fileCount
  const rawBytes = entries.reduce(
    (total, entry) => checkedAdd(total, entry.kind === 'file' ? entry.exactSize : 0n),
    0n,
  )
  const canonicalBytes = canonicalMaterializedManifestBytes({
    receiveIntentDigest,
    preparationBinding,
    generations,
    entries,
    fileCount,
    directoryCount,
    rawBytes,
  })
  return Object.freeze({
    schemaVersion: 1,
    operationId,
    receiveIntentDigest,
    materializationBindingDigest,
    preparationBinding,
    generations,
    entries,
    entryCount: BigInt(entries.length),
    fileCount,
    directoryCount,
    rawBytes,
    canonicalMetadataBytes: BigInt(canonicalBytes.byteLength),
    canonicalBytes,
    digest: await canonicalDigest(canonicalBytes),
  })
}

export async function createMaterializedManifestPages(
  manifest: MaterializedManifestV1,
): Promise<readonly ManifestPageRecord[]> {
  const canonicalEntries = manifest.entries.map(canonicalMaterializedManifestEntry)
  if (canonicalEntries.length === 0) {
    throw new TypeError('materialized manifest cannot persist an empty page set')
  }
  const totalPageCount = Math.ceil(canonicalEntries.length / MANIFEST_PAGE_ENTRY_LIMIT)
  const pages: ManifestPageRecord[] = []
  for (let pageIndex = 0; pageIndex < totalPageCount; pageIndex += 1) {
    const start = pageIndex * MANIFEST_PAGE_ENTRY_LIMIT
    pages.push(await createManifestPageRecord({
      operationId: manifest.operationId,
      ownerKind: RECEIVE_RECORD_MATERIALIZED_MANIFEST,
      ownerDigest: manifest.digest,
      pageIndex,
      totalPageCount,
      canonicalEntries: canonicalEntries.slice(start, start + MANIFEST_PAGE_ENTRY_LIMIT),
    }))
  }
  return Object.freeze(pages)
}

/** Reopens a seal without inventing FileCheckpointV2 record IDs omitted from the aggregate. */
export async function decodeMaterializedManifestV1(input: {
  readonly canonicalBytes: Uint8Array
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly materializationBindingDigest: string
  readonly checkpoints: FinalCheckpointRecoveryReader
  readonly preparation?: PreparationManifestV1
}): Promise<MaterializedManifestV1> {
  const reader = CanonicalRecordReader.open(
    input.canonicalBytes,
    'windshare/materialized-manifest/v1',
    1,
  )
  const receiveIntentDigest = reader.framedIdentity(32, 'receive intent digest')
  const preparationBinding = decodePreparationBinding(reader.frame('preparation binding'))
  const generationCount = boundedManifestCount(reader.u64('materialized generation count'))
  const generations: AuthenticatedGenerationReference[] = []
  for (let index = 0; index < generationCount; index += 1) {
    generations.push(decodeMaterializedGeneration(reader.frame('materialized generation')))
  }
  const entryCount = boundedManifestCount(reader.u64('materialized entry count'), false)
  const entries: MaterializedManifestEntry[] = []
  for (let index = 0; index < entryCount; index += 1) {
    entries.push(await decodeMaterializedEntry(
      reader.frame('materialized entry'),
      input.checkpoints,
    ))
  }
  const projectedEntryCount = reader.framedU64('projected materialized entry count')
  const projectedFileCount = reader.framedU64('projected materialized file count')
  const projectedDirectoryCount = reader.framedU64('projected materialized directory count')
  const projectedRawBytes = reader.framedU64('projected materialized raw bytes')
  const projectedMetadataBytes = reader.framedU64('projected materialized metadata bytes')
  reader.finish('materialized manifest')

  const rebuilt = await sealMaterializedManifest({
    operationId: input.operationId,
    receiveIntentDigest: input.receiveIntentDigest,
    materializationBindingDigest: input.materializationBindingDigest,
    preparationBinding,
    generations,
    entries,
    checkpoints: input.checkpoints,
    ...(input.preparation === undefined ? {} : { preparation: input.preparation }),
  })
  if (receiveIntentDigest !== input.receiveIntentDigest ||
      projectedEntryCount !== rebuilt.entryCount || projectedFileCount !== rebuilt.fileCount ||
      projectedDirectoryCount !== rebuilt.directoryCount || projectedRawBytes !== rebuilt.rawBytes ||
      projectedMetadataBytes !== rebuilt.canonicalMetadataBytes ||
      !equalCanonicalBytes(rebuilt.canonicalBytes, input.canonicalBytes)) {
    throw new TypeError('persisted materialized manifest changed its canonical authority')
  }
  return rebuilt
}

export function materializedGenerationTableDigest(
  generations: readonly AuthenticatedGenerationReference[],
): Promise<string> {
  const snapshot = snapshotGenerations(generations)
  return canonicalDigest(canonicalRecord('windshare/materialized-manifest/v1/generation-table', 1, [
    canonicalU64(BigInt(snapshot.length)),
    ...snapshot.map((generation) => canonicalFrame(canonicalRecord(
      'windshare/materialized-manifest/v1/generation',
      1,
      [
        canonicalFrame(canonicalIdentity(generation.directoryId, 16, 'directory ID')),
        canonicalFrame(canonicalIdentity(generation.generation, 16, 'directory generation')),
      ],
    ))),
  ]))
}

function canonicalMaterializedManifestBytes(input: {
  readonly receiveIntentDigest: string
  readonly preparationBinding: PreparationBinding
  readonly generations: readonly AuthenticatedGenerationReference[]
  readonly entries: readonly MaterializedManifestEntry[]
  readonly fileCount: bigint
  readonly directoryCount: bigint
  readonly rawBytes: bigint
}): CanonicalBytes {
  const fields = [
    canonicalFrame(canonicalIdentity(input.receiveIntentDigest, 32, 'receive intent digest')),
    canonicalFrame(canonicalPreparationBinding(input.preparationBinding)),
    canonicalU64(BigInt(input.generations.length)),
    ...input.generations.map((reference) => canonicalFrame(canonicalGeneration(reference))),
    canonicalU64(BigInt(input.entries.length)),
    ...input.entries.map((entry) => canonicalFrame(canonicalMaterializedManifestEntry(entry))),
    canonicalFrame(canonicalU64(BigInt(input.entries.length))),
    canonicalFrame(canonicalU64(input.fileCount)),
    canonicalFrame(canonicalU64(input.directoryCount)),
    canonicalFrame(canonicalU64(input.rawBytes)),
  ]
  // The self-size field is fixed-width, so a zero placeholder yields the exact final size.
  const metadataBytes = BigInt(canonicalRecord(
    'windshare/materialized-manifest/v1',
    1,
    [...fields, canonicalFrame(canonicalU64(0n))],
  ).byteLength)
  return canonicalRecord('windshare/materialized-manifest/v1', 1, [
    ...fields,
    canonicalFrame(canonicalU64(metadataBytes)),
  ])
}

async function snapshotAndVerifyEntries(
  input: readonly MaterializedManifestEntry[],
  checkpoints: FinalCheckpointReader,
  binding: {
    readonly operationId: string
    readonly receiveIntentDigest: string
    readonly materializationBindingDigest: string
  },
): Promise<readonly MaterializedManifestEntry[]> {
  const entries = input.map(snapshotManifestEntry).sort(compareManifestEntries)
  for (let index = 1; index < entries.length; index += 1) {
    if (comparePaths(entries[index - 1]!.artifactPath, entries[index]!.artifactPath) === 0) {
      throw new TypeError('materialized manifest contains a duplicate artifact path')
    }
  }
  for (const entry of entries) {
    if (entry.kind === 'file') await verifyFileEntry(entry, checkpoints, binding)
  }
  return Object.freeze(entries)
}

async function verifyFileEntry(
  entry: MaterializedFileEntry,
  checkpoints: FinalCheckpointReader,
  binding: {
    readonly operationId: string
    readonly receiveIntentDigest: string
    readonly materializationBindingDigest: string
  },
): Promise<void> {
  const proof = await checkpoints.readFinalCheckpoint(
    entry.checkpoint.recordId,
    entry.checkpoint.checkpointGeneration,
  )
  if (proof === undefined || proof.complete !== true ||
      proof.operationId !== binding.operationId ||
      proof.receiveIntentDigest !== binding.receiveIntentDigest ||
      proof.materializationBindingDigest !== binding.materializationBindingDigest ||
      proof.fileId !== entry.fileId || proof.fileRevision !== entry.fileRevision ||
      comparePaths(proof.canonicalPath, entry.artifactPath) !== 0 ||
      proof.exactSize !== entry.exactSize || proof.ownedObjectId !== entry.ownedObjectId ||
      proof.recordId !== entry.checkpoint.recordId ||
      proof.recordDigest !== entry.checkpoint.recordDigest ||
      proof.checkpointGeneration !== entry.checkpoint.checkpointGeneration) {
    throw new TypeError('materialized file is not proven by its final checkpoint')
  }
}

function snapshotManifestEntry(entry: MaterializedManifestEntry): MaterializedManifestEntry {
  if (entry.kind === 'directory') {
    requireExactKeys(entry, ['kind', 'artifactPath', 'directoryId', 'generation', 'ownedObjectId'])
    return Object.freeze({
      kind: 'directory',
      artifactPath: snapshotPortableCatalogPath(entry.artifactPath),
      directoryId: snapshotIdentity(entry.directoryId, 16, 'directory ID'),
      generation: snapshotIdentity(entry.generation, 16, 'directory generation'),
      ownedObjectId: snapshotIdentity(entry.ownedObjectId, 32, 'owned object ID'),
    })
  }
  requireExactKeys(
    entry,
    ['kind', 'artifactPath', 'fileId', 'fileRevision', 'exactSize', 'ownedObjectId', 'checkpoint'],
  )
  requireExactKeys(entry.checkpoint, ['recordId', 'recordDigest', 'checkpointGeneration'])
  return Object.freeze({
    kind: 'file',
    artifactPath: snapshotPortableCatalogPath(entry.artifactPath),
    fileId: snapshotIdentity(entry.fileId, 16, 'file ID'),
    fileRevision: snapshotIdentity(entry.fileRevision, 16, 'file revision'),
    exactSize: checkedU64(entry.exactSize, 'file size'),
    ownedObjectId: snapshotIdentity(entry.ownedObjectId, 32, 'owned object ID'),
    checkpoint: Object.freeze({
      recordId: snapshotIdentity(entry.checkpoint.recordId, 32, 'checkpoint record ID'),
      recordDigest: snapshotIdentity(entry.checkpoint.recordDigest, 32, 'checkpoint digest'),
      checkpointGeneration: checkedU64(
        entry.checkpoint.checkpointGeneration,
        'checkpoint generation',
      ),
    }),
  })
}

function snapshotGenerations(
  input: readonly AuthenticatedGenerationReference[],
): readonly AuthenticatedGenerationReference[] {
  const generations = input.map((reference) => Object.freeze({
    directoryId: snapshotIdentity(reference.directoryId, 16, 'directory ID'),
    generation: snapshotIdentity(reference.generation, 16, 'directory generation'),
  })).sort((left, right) => compareIdentities(
    left.directoryId,
    right.directoryId,
  ) || compareIdentities(left.generation, right.generation))
  for (let index = 1; index < generations.length; index += 1) {
    if (generations[index - 1]?.directoryId === generations[index]?.directoryId) {
      throw new TypeError('materialized manifest contains duplicate directory generation authority')
    }
  }
  return Object.freeze(generations)
}

function validateManifestTopology(
  entries: readonly MaterializedManifestEntry[],
  generations: readonly AuthenticatedGenerationReference[],
): void {
  const directories = new Set(entries
    .filter((entry) => entry.kind === 'directory')
    .map((entry) => pathKey(entry.artifactPath)))
  const sources = new Set<string>()
  const generationKeys = new Set(generations.map((reference) =>
    `${reference.directoryId}:${reference.generation}`))
  for (const entry of entries) {
    const source = entry.kind === 'directory' ? `d:${entry.directoryId}` : `f:${entry.fileId}`
    if (sources.has(source)) throw new TypeError('materialized manifest duplicates a source identity')
    sources.add(source)
    if (entry.kind === 'directory' &&
        !generationKeys.has(`${entry.directoryId}:${entry.generation}`)) {
      throw new TypeError('materialized directory lacks authenticated generation authority')
    }
    for (let depth = 1; depth < entry.artifactPath.length; depth += 1) {
      if (!directories.has(pathKey(entry.artifactPath.slice(0, depth)))) {
        throw new TypeError('materialized manifest omits a necessary artifact ancestor')
      }
    }
  }
}

function snapshotPreparationBinding(binding: PreparationBinding): PreparationBinding {
  if (binding.kind === 'absent') return Object.freeze({ kind: 'absent' })
  if (binding.kind !== 'present') throw new TypeError('preparation binding kind is invalid')
  return Object.freeze({
    kind: 'present',
    preparationDigest: snapshotIdentity(binding.preparationDigest, 32, 'preparation digest'),
  })
}

function canonicalPreparationBinding(binding: PreparationBinding): CanonicalBytes {
  return binding.kind === 'absent'
    ? canonicalU8(2)
    : concatCanonicalBytes([
        canonicalU8(1),
        canonicalFrame(canonicalIdentity(binding.preparationDigest, 32, 'preparation digest')),
      ])
}

function canonicalGeneration(reference: AuthenticatedGenerationReference): CanonicalBytes {
  return concatCanonicalBytes([
    canonicalFrame(canonicalIdentity(reference.directoryId, 16, 'directory ID')),
    canonicalFrame(canonicalIdentity(reference.generation, 16, 'directory generation')),
  ])
}

export function canonicalMaterializedManifestEntry(
  entry: MaterializedManifestEntry,
): CanonicalBytes {
  if (entry.kind === 'directory') {
    return concatCanonicalBytes([
      canonicalU8(1),
      canonicalFrame(canonicalPath(entry.artifactPath)),
      canonicalFrame(canonicalIdentity(entry.directoryId, 16, 'directory ID')),
      canonicalFrame(canonicalIdentity(entry.generation, 16, 'directory generation')),
      canonicalFrame(canonicalIdentity(entry.ownedObjectId, 32, 'owned object ID')),
    ])
  }
  return concatCanonicalBytes([
    canonicalU8(2),
    canonicalFrame(canonicalPath(entry.artifactPath)),
    canonicalFrame(canonicalIdentity(entry.fileId, 16, 'file ID')),
    canonicalFrame(canonicalIdentity(entry.fileRevision, 16, 'file revision')),
    canonicalFrame(canonicalU64(entry.exactSize)),
    canonicalFrame(canonicalIdentity(entry.ownedObjectId, 32, 'owned object ID')),
    canonicalFrame(canonicalIdentity(entry.checkpoint.recordDigest, 32, 'checkpoint digest')),
    canonicalFrame(canonicalU64(entry.checkpoint.checkpointGeneration)),
  ])
}

function decodePreparationBinding(bytes: Uint8Array): PreparationBinding {
  const reader = CanonicalRecordReader.value(bytes)
  const discriminant = reader.byte('preparation binding discriminant')
  if (discriminant === 2) {
    reader.finish('absent preparation binding')
    return Object.freeze({ kind: 'absent' })
  }
  if (discriminant !== 1) throw new TypeError('preparation binding discriminant is invalid')
  const preparationDigest = reader.framedIdentity(32, 'preparation digest')
  reader.finish('present preparation binding')
  return Object.freeze({ kind: 'present', preparationDigest })
}

function decodeMaterializedGeneration(bytes: Uint8Array): AuthenticatedGenerationReference {
  const reader = CanonicalRecordReader.value(bytes)
  const value = Object.freeze({
    directoryId: reader.framedIdentity(16, 'directory ID'),
    generation: reader.framedIdentity(16, 'directory generation'),
  })
  reader.finish('materialized generation')
  return value
}

async function decodeMaterializedEntry(
  bytes: Uint8Array,
  checkpoints: FinalCheckpointRecoveryReader,
): Promise<MaterializedManifestEntry> {
  const reader = CanonicalRecordReader.value(bytes)
  const discriminant = reader.byte('materialized entry discriminant')
  const artifactPath = decodeMaterializedPath(reader.frame('materialized artifact path'))
  if (discriminant === 1) {
    const entry = Object.freeze({
      kind: 'directory' as const,
      artifactPath,
      directoryId: reader.framedIdentity(16, 'directory ID'),
      generation: reader.framedIdentity(16, 'directory generation'),
      ownedObjectId: reader.framedIdentity(32, 'owned object ID'),
    })
    reader.finish('materialized directory entry')
    return entry
  }
  if (discriminant !== 2) throw new TypeError('materialized entry discriminant is invalid')
  const evidence = Object.freeze({
    fileId: reader.framedIdentity(16, 'file ID'),
    fileRevision: reader.framedIdentity(16, 'file revision'),
    artifactPath,
    exactSize: reader.framedU64('file size'),
    ownedObjectId: reader.framedIdentity(32, 'owned object ID'),
    recordDigest: reader.framedIdentity(32, 'checkpoint digest'),
    checkpointGeneration: reader.framedU64('checkpoint generation'),
  })
  reader.finish('materialized file entry')
  const proof = await checkpoints.recoverFinalCheckpoint(evidence)
  if (proof === undefined || proof.fileId !== evidence.fileId ||
      proof.fileRevision !== evidence.fileRevision || proof.exactSize !== evidence.exactSize ||
      proof.ownedObjectId !== evidence.ownedObjectId || proof.recordDigest !== evidence.recordDigest ||
      proof.checkpointGeneration !== evidence.checkpointGeneration ||
      comparePaths(proof.canonicalPath, evidence.artifactPath) !== 0) {
    throw new TypeError('materialized file lacks a unique final checkpoint authority')
  }
  return Object.freeze({
    kind: 'file',
    artifactPath,
    fileId: evidence.fileId,
    fileRevision: evidence.fileRevision,
    exactSize: evidence.exactSize,
    ownedObjectId: evidence.ownedObjectId,
    checkpoint: Object.freeze({
      recordId: proof.recordId,
      recordDigest: evidence.recordDigest,
      checkpointGeneration: evidence.checkpointGeneration,
    }),
  })
}

function decodeMaterializedPath(bytes: Uint8Array): readonly string[] {
  const reader = CanonicalRecordReader.value(bytes)
  const count = reader.u64('materialized path segment count')
  if (count === 0n || count > 256n) {
    throw new TypeError('materialized path segment count is invalid')
  }
  const segments: string[] = []
  for (let index = 0n; index < count; index += 1n) {
    const canonical = reader.frame('materialized path segment')
    const value = new TextDecoder(undefined, { fatal: true }).decode(canonical)
    if (!equalCanonicalBytes(new TextEncoder().encode(value), canonical)) {
      throw new TypeError('materialized path text is not canonical UTF-8')
    }
    segments.push(value)
  }
  reader.finish('materialized path')
  return snapshotPortableCatalogPath(segments)
}

function boundedManifestCount(value: bigint, allowZero = true): number {
  if ((!allowZero && value === 0n) || value > BigInt(MAX_ARTIFACT_ENTRIES)) {
    throw new TypeError('persisted materialized aggregate exceeds its entry bound')
  }
  return Number(value)
}

function validatePreparationAuthority(
  binding: PreparationBinding,
  preparation: PreparationManifestV1 | undefined,
  materializedEntries: readonly MaterializedManifestEntry[],
  generations: readonly AuthenticatedGenerationReference[],
): void {
  if (binding.kind === 'absent') {
    if (preparation !== undefined) {
      throw new TypeError('unprepared materialization cannot import preparation evidence')
    }
    return
  }
  if (preparation === undefined || preparation.digest !== binding.preparationDigest) {
    throw new TypeError('prepared materialization lacks its sealed preparation authority')
  }
  if (preparation.entryCount !== BigInt(materializedEntries.length) ||
      preparation.fileCount !== BigInt(materializedEntries.filter((entry) => entry.kind === 'file').length) ||
      preparation.directoryCount !== BigInt(
        materializedEntries.filter((entry) => entry.kind === 'directory').length,
      ) || preparation.selectedRawBytes !== materializedEntries.reduce(
        (total, entry) => checkedAdd(total, entry.kind === 'file' ? entry.exactSize : 0n),
        0n,
      )) {
    throw new TypeError('materialized totals disagree with sealed preparation')
  }
  if (generations.length !== preparation.generations.length || generations.some((reference, index) => {
    const expected = preparation.generations[index]
    return expected === undefined || reference.directoryId !== expected.directoryId ||
      reference.generation !== expected.generation
  })) {
    throw new TypeError('materialized generation table changed after preparation')
  }
  for (let index = 0; index < materializedEntries.length; index += 1) {
    const prepared = preparation.entries[index]
    const materialized = materializedEntries[index]
    if (prepared === undefined || materialized === undefined || prepared.kind !== materialized.kind ||
        comparePaths(prepared.artifactPath, materialized.artifactPath) !== 0) {
      throw new TypeError('materialized artifact topology changed after preparation')
    }
    if (prepared.kind === 'directory') {
      if (materialized.kind !== 'directory' || prepared.directoryId !== materialized.directoryId ||
          prepared.generation !== materialized.generation) {
        throw new TypeError('materialized directory changed after preparation')
      }
    } else if (materialized.kind !== 'file' || prepared.fileId !== materialized.fileId ||
        prepared.exactSize !== materialized.exactSize) {
      throw new TypeError('materialized file changed after preparation')
    }
  }
}

function compareManifestEntries(
  left: MaterializedManifestEntry,
  right: MaterializedManifestEntry,
): number {
  const path = comparePaths(left.artifactPath, right.artifactPath)
  if (path !== 0) return path
  if (left.kind === right.kind) return 0
  return left.kind === 'directory' ? -1 : 1
}

function comparePaths(left: readonly string[], right: readonly string[]): number {
  const leftBytes = TEXT_ENCODER.encode(left.join('/'))
  const rightBytes = TEXT_ENCODER.encode(right.join('/'))
  const length = Math.min(leftBytes.length, rightBytes.length)
  for (let index = 0; index < length; index += 1) {
    const difference = leftBytes[index]! - rightBytes[index]!
    if (difference !== 0) return difference
  }
  return leftBytes.length - rightBytes.length
}

function compareIdentities(left: string, right: string): number {
  const leftBytes = canonicalIdentity(left, 16, 'identity')
  const rightBytes = canonicalIdentity(right, 16, 'identity')
  for (let index = 0; index < leftBytes.length; index += 1) {
    const difference = leftBytes[index]! - rightBytes[index]!
    if (difference !== 0) return difference
  }
  return 0
}

function checkedU64(value: bigint, label: string): bigint {
  if (typeof value !== 'bigint' || value < 0n || value > 0xffff_ffff_ffff_ffffn) {
    throw new TypeError(`${label} is not a u64`)
  }
  return value
}

function checkedAdd(left: bigint, right: bigint): bigint {
  return checkedU64(left + right, 'materialized byte total')
}

function pathKey(path: readonly string[]): string {
  return path.join('\0')
}

function requireExactKeys(
  value: object,
  expectedKeys: readonly string[],
): void {
  const actual = Object.keys(value).sort()
  const expected = [...expectedKeys].sort()
  if (actual.length !== expected.length || actual.some((key, index) => key !== expected[index])) {
    throw new TypeError('materialized manifest entry contains non-contract fields')
  }
}

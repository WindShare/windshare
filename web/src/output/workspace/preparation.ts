import { snapshotPortableCatalogPath } from '../../catalog/path-policy'
import {
  snapshotCanonicalModifiedTime,
  type CanonicalModifiedTime,
} from '../../transfer/directory-admission'
import {
  validateReceiveIntent,
  type ReceiveIntent,
  type ResultRootLayout,
} from '../../transfer/intent'
import {
  planZipLayout,
  validateSealedZipLayoutPlan,
  type SealedZipLayoutPlanV1,
} from '../zip-layout/layout'
import type { ZipEntryPlanV1, ZipEntrySpec } from '../zip-layout/policy'
import {
  canonicalBoolean,
  canonicalDigest,
  canonicalFrame,
  canonicalI64,
  canonicalIdentity,
  canonicalPath,
  canonicalRecord,
  canonicalText,
  canonicalU8,
  canonicalU32,
  canonicalU64,
  concatCanonicalBytes,
  equalCanonicalBytes,
  snapshotIdentity,
  type CanonicalBytes,
} from './canonical'
import type { AuthenticatedGenerationReference } from './manifest'
import {
  createManifestPageRecord,
  MANIFEST_PAGE_ENTRY_LIMIT,
  RECEIVE_RECORD_PREPARATION,
  type ManifestPageRecord,
} from './records'
import { CanonicalRecordReader } from './canonical-reader'
import type { PreparationAdmissionReason } from './state'

export const DEFAULT_PREPARATION_METADATA_LIMIT = 268_435_456n
export const DEFAULT_PORTABLE_PREPARATION_METADATA_LIMIT = 16_777_216n

const PREPARATION_SCHEMA_VERSION = 1 as const
const MAX_ARTIFACT_ENTRIES = 1_000_000
const U64_MAXIMUM = 0xffff_ffff_ffff_ffffn
const NANOSECONDS_PER_MILLISECOND = 1_000_000n
const TEXT_ENCODER = new TextEncoder()

export type PreparationDirectoryRole =
  | 'result-root'
  | 'necessary-ancestor'
  | 'explicitly-selected-empty'

interface PreparationEntryBase {
  readonly sourcePath: readonly string[]
  readonly artifactPath: readonly string[]
  readonly modifiedTime?: CanonicalModifiedTime
}

export interface PreparationDirectoryEntry extends PreparationEntryBase {
  readonly kind: 'directory'
  readonly directoryId: string
  readonly generation: string
  readonly role: PreparationDirectoryRole
}

export interface PreparationFileEntry extends PreparationEntryBase {
  readonly kind: 'file'
  readonly fileId: string
  readonly containingDirectoryId: string
  readonly generation: string
  readonly exactSize: bigint
}

export type PreparationManifestEntry = PreparationDirectoryEntry | PreparationFileEntry

export interface PreparationManifestV1 {
  readonly schemaVersion: typeof PREPARATION_SCHEMA_VERSION
  readonly operationId: string
  readonly preparationId: string
  readonly receiveIntentDigest: string
  readonly artifactSpecDigest: string
  readonly generations: readonly AuthenticatedGenerationReference[]
  readonly entries: readonly PreparationManifestEntry[]
  readonly entryCount: bigint
  readonly fileCount: bigint
  readonly directoryCount: bigint
  readonly selectedRawBytes: bigint
  readonly canonicalMetadataBytes: bigint
  readonly canonicalBytes: CanonicalBytes
  readonly digest: string
}

export interface SealedWorkspaceZipPreparationV1 {
  readonly manifest: PreparationManifestV1
  readonly pages: readonly ManifestPageRecord[]
  readonly zipLayout: SealedZipLayoutPlanV1
  readonly zipLayoutCanonicalBytes: CanonicalBytes
}

export class PreparationManifestError extends TypeError {
  readonly reason: PreparationAdmissionReason

  constructor(reason: PreparationAdmissionReason, message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'PreparationManifestError'
    this.reason = reason
  }
}

export async function sealWorkspaceZipPreparation(input: {
  readonly receiveIntent: ReceiveIntent
  readonly preparationId: string
  readonly generations: readonly AuthenticatedGenerationReference[]
  readonly entries: readonly PreparationManifestEntry[]
  readonly metadataLimitBytes?: bigint
}): Promise<SealedWorkspaceZipPreparationV1> {
  const intent = await validateReceiveIntent(input.receiveIntent)
  requireWorkspaceZipIntent(intent)
  const limit = checkedU64(
    input.metadataLimitBytes ?? DEFAULT_PREPARATION_METADATA_LIMIT,
    'preparation metadata limit',
  )
  const provisional = await sealPreparationManifest({
    receiveIntent: intent,
    preparationId: input.preparationId,
    generations: input.generations,
    entries: input.entries,
    canonicalMetadataBytes: 0n,
  })
  const provisionalPages = await createPreparationManifestPages(provisional)
  const provisionalLayout = await planZipLayout({
    receiveIntentDigest: intent.digest,
    artifactDigest: intent.artifact.digest,
    preparationManifestDigest: provisional.digest,
    entries: zipEntrySpecs(provisional.entries),
  })
  const provisionalLayoutBytes = canonicalSealedZipLayoutStorageBytes(provisionalLayout)
  const metadataBytes = checkedMetadataSum(
    BigInt(provisional.canonicalBytes.byteLength),
    sumPageBytes(provisionalPages),
    BigInt(provisionalLayoutBytes.byteLength),
  )
  if (metadataBytes > limit) {
    throw new PreparationManifestError(
      'metadata-limit',
      'workspace preparation exceeds its canonical metadata limit',
    )
  }

  const manifest = await sealPreparationManifest({
    receiveIntent: intent,
    preparationId: input.preparationId,
    generations: input.generations,
    entries: input.entries,
    canonicalMetadataBytes: metadataBytes,
  })
  const pages = await createPreparationManifestPages(manifest)
  const zipLayout = await planZipLayout({
    receiveIntentDigest: intent.digest,
    artifactDigest: intent.artifact.digest,
    preparationManifestDigest: manifest.digest,
    entries: zipEntrySpecs(manifest.entries),
  })
  const zipLayoutCanonicalBytes = canonicalSealedZipLayoutStorageBytes(zipLayout)
  const observedMetadataBytes = checkedMetadataSum(
    BigInt(manifest.canonicalBytes.byteLength),
    sumPageBytes(pages),
    BigInt(zipLayoutCanonicalBytes.byteLength),
  )
  if (observedMetadataBytes !== manifest.canonicalMetadataBytes) {
    throw new PreparationManifestError(
      'arithmetic-overflow',
      'preparation metadata changed while binding its final digest',
    )
  }
  return Object.freeze({ manifest, pages, zipLayout, zipLayoutCanonicalBytes })
}

export async function sealPreparationManifest(input: {
  readonly receiveIntent: ReceiveIntent
  readonly preparationId: string
  readonly generations: readonly AuthenticatedGenerationReference[]
  readonly entries: readonly PreparationManifestEntry[]
  readonly canonicalMetadataBytes: bigint
}): Promise<PreparationManifestV1> {
  const intent = await validateReceiveIntent(input.receiveIntent)
  requireWorkspaceZipIntent(intent)
  if (input.entries.length === 0 || input.entries.length > MAX_ARTIFACT_ENTRIES ||
      input.generations.length > MAX_ARTIFACT_ENTRIES) {
    throw new PreparationManifestError('entry-limit', 'preparation entry bound is invalid')
  }
  const preparationId = snapshotIdentity(input.preparationId, 16, 'preparation ID')
  const generations = snapshotGenerations(input.generations)
  const entries = snapshotEntries(input.entries, intent.artifact.layout)
  validatePreparationTopology(
    entries,
    generations,
    intent.artifact.layout,
    intent.selection.syntheticRoot,
  )
  const fileCount = BigInt(entries.filter((entry) => entry.kind === 'file').length)
  const directoryCount = BigInt(entries.length) - fileCount
  const selectedRawBytes = entries.reduce(
    (total, entry) => entry.kind === 'file'
      ? checkedMetadataSum(total, entry.exactSize)
      : total,
    0n,
  )
  const canonicalMetadataBytes = checkedU64(
    input.canonicalMetadataBytes,
    'preparation canonical metadata bytes',
  )
  const canonicalBytes = canonicalRecord('windshare/preparation-manifest/v1', 1, [
    canonicalFrame(canonicalIdentity(preparationId, 16, 'preparation ID')),
    canonicalFrame(canonicalIdentity(intent.digest, 32, 'receive intent digest')),
    canonicalFrame(canonicalIdentity(intent.artifact.digest, 32, 'artifact digest')),
    canonicalU64(BigInt(generations.length)),
    ...generations.map((generation) => canonicalFrame(canonicalGeneration(generation))),
    canonicalU64(BigInt(entries.length)),
    ...entries.map((entry) => canonicalFrame(canonicalPreparationEntry(entry))),
    canonicalFrame(canonicalU64(BigInt(entries.length))),
    canonicalFrame(canonicalU64(fileCount)),
    canonicalFrame(canonicalU64(directoryCount)),
    canonicalFrame(canonicalU64(selectedRawBytes)),
    canonicalFrame(canonicalU64(canonicalMetadataBytes)),
  ])
  return Object.freeze({
    schemaVersion: PREPARATION_SCHEMA_VERSION,
    operationId: intent.operationId,
    preparationId,
    receiveIntentDigest: intent.digest,
    artifactSpecDigest: intent.artifact.digest,
    generations,
    entries,
    entryCount: BigInt(entries.length),
    fileCount,
    directoryCount,
    selectedRawBytes,
    canonicalMetadataBytes,
    canonicalBytes,
    digest: await canonicalDigest(canonicalBytes),
  })
}

/** Rebuilds semantic preparation authority from the only persisted aggregate bytes. */
export async function decodePreparationManifestV1(
  canonicalInput: Uint8Array,
  receiveIntent: ReceiveIntent,
): Promise<PreparationManifestV1> {
  const intent = await validateReceiveIntent(receiveIntent)
  requireWorkspaceZipIntent(intent)
  const reader = CanonicalRecordReader.open(
    canonicalInput,
    'windshare/preparation-manifest/v1',
    PREPARATION_SCHEMA_VERSION,
  )
  const preparationId = reader.framedIdentity(16, 'preparation ID')
  const receiveIntentDigest = reader.framedIdentity(32, 'receive intent digest')
  const artifactSpecDigest = reader.framedIdentity(32, 'artifact digest')
  const generationCount = boundedEntryCount(reader.u64('preparation generation count'))
  const generations: AuthenticatedGenerationReference[] = []
  for (let index = 0; index < generationCount; index += 1) {
    generations.push(decodePreparationGeneration(reader.frame('preparation generation')))
  }
  const entryCount = boundedEntryCount(reader.u64('preparation entry count'), false)
  const entries: PreparationManifestEntry[] = []
  for (let index = 0; index < entryCount; index += 1) {
    entries.push(decodePreparationEntry(reader.frame('preparation entry')))
  }
  const projectedEntryCount = reader.framedU64('projected preparation entry count')
  const projectedFileCount = reader.framedU64('projected preparation file count')
  const projectedDirectoryCount = reader.framedU64('projected preparation directory count')
  const projectedSelectedRawBytes = reader.framedU64('projected selected raw bytes')
  const canonicalMetadataBytes = reader.framedU64('preparation canonical metadata bytes')
  reader.finish('preparation manifest')

  const rebuilt = await sealPreparationManifest({
    receiveIntent: intent,
    preparationId,
    generations,
    entries,
    canonicalMetadataBytes,
  })
  if (receiveIntentDigest !== intent.digest || artifactSpecDigest !== intent.artifact.digest ||
      projectedEntryCount !== rebuilt.entryCount || projectedFileCount !== rebuilt.fileCount ||
      projectedDirectoryCount !== rebuilt.directoryCount ||
      projectedSelectedRawBytes !== rebuilt.selectedRawBytes ||
      !equalCanonicalBytes(rebuilt.canonicalBytes, canonicalInput)) {
    throw new TypeError('persisted preparation manifest changed its canonical authority')
  }
  return rebuilt
}

export async function createPreparationManifestPages(
  manifest: PreparationManifestV1,
): Promise<readonly ManifestPageRecord[]> {
  const canonicalEntries = manifest.entries.map(canonicalPreparationEntry)
  const totalPageCount = Math.ceil(canonicalEntries.length / MANIFEST_PAGE_ENTRY_LIMIT)
  const pages: ManifestPageRecord[] = []
  for (let pageIndex = 0; pageIndex < totalPageCount; pageIndex += 1) {
    const start = pageIndex * MANIFEST_PAGE_ENTRY_LIMIT
    pages.push(await createManifestPageRecord({
      operationId: manifest.operationId,
      ownerKind: RECEIVE_RECORD_PREPARATION,
      ownerDigest: manifest.digest,
      pageIndex,
      totalPageCount,
      canonicalEntries: canonicalEntries.slice(start, start + MANIFEST_PAGE_ENTRY_LIMIT),
    }))
  }
  return Object.freeze(pages)
}

export function canonicalSealedZipLayoutStorageBytes(
  candidate: SealedZipLayoutPlanV1,
): CanonicalBytes {
  const plan = candidate
  const evidence = plan.evidence.kind === 'prepared'
    ? concatCanonicalBytes([
        canonicalU8(1),
        canonicalFrame(canonicalIdentity(
          plan.evidence.preparationManifestDigest,
          32,
          'preparation manifest digest',
        )),
      ])
    : concatCanonicalBytes([
        canonicalU8(2),
        canonicalFrame(canonicalIdentity(
          plan.evidence.discoveryLedgerDigest,
          32,
          'discovery ledger digest',
        )),
      ])
  return canonicalRecord('windshare/zip-layout/v1', 1, [
    canonicalFrame(canonicalIdentity(plan.receiveIntentDigest, 32, 'receive intent digest')),
    canonicalFrame(canonicalIdentity(plan.artifactDigest, 32, 'artifact digest')),
    canonicalFrame(evidence),
    canonicalFrame(canonicalText(plan.encodingPolicy)),
    canonicalFrame(canonicalU8(plan.encodingPolicyVersion)),
    canonicalU64(BigInt(plan.entries.length)),
    ...plan.entries.map((entry) => canonicalFrame(canonicalZipEntryPlan(entry))),
    canonicalFrame(canonicalU64(plan.centralDirectoryOffset)),
    canonicalFrame(canonicalU64(plan.centralDirectoryBytes)),
    canonicalFrame(canonicalBoolean(plan.zip64EndRequired)),
    canonicalFrame(canonicalU64(plan.zip64EndBytes)),
    canonicalFrame(canonicalU64(plan.classicEndBytes)),
    canonicalFrame(canonicalU64(plan.exactArchiveBytes)),
    canonicalFrame(canonicalU64(plan.maximumSpoolBytes)),
    canonicalFrame(canonicalIdentity(plan.digest, 32, 'ZIP layout digest')),
  ])
}

export async function validateWorkspaceZipPreparation(
  bundle: SealedWorkspaceZipPreparationV1,
  receiveIntent: ReceiveIntent,
  metadataLimitBytes: bigint = DEFAULT_PREPARATION_METADATA_LIMIT,
): Promise<SealedWorkspaceZipPreparationV1> {
  const intent = await validateReceiveIntent(receiveIntent)
  requireWorkspaceZipIntent(intent)
  const manifest = await sealPreparationManifest({
    receiveIntent: intent,
    preparationId: bundle.manifest.preparationId,
    generations: bundle.manifest.generations,
    entries: bundle.manifest.entries,
    canonicalMetadataBytes: bundle.manifest.canonicalMetadataBytes,
  })
  if (bundle.manifest.schemaVersion !== PREPARATION_SCHEMA_VERSION ||
      bundle.manifest.operationId !== intent.operationId ||
      bundle.manifest.entryCount !== manifest.entryCount ||
      bundle.manifest.fileCount !== manifest.fileCount ||
      bundle.manifest.directoryCount !== manifest.directoryCount ||
      bundle.manifest.selectedRawBytes !== manifest.selectedRawBytes ||
      bundle.manifest.digest !== manifest.digest ||
      !equalCanonicalBytes(bundle.manifest.canonicalBytes, manifest.canonicalBytes)) {
    throw new TypeError('preparation manifest projections disagree with canonical authority')
  }
  const layout = await validateSealedZipLayoutPlan(bundle.zipLayout)
  if (layout.evidence.kind !== 'prepared' ||
      layout.evidence.preparationManifestDigest !== manifest.digest ||
      layout.receiveIntentDigest !== manifest.receiveIntentDigest ||
      layout.artifactDigest !== manifest.artifactSpecDigest) {
    throw new TypeError('sealed ZIP layout escaped its preparation authority')
  }
  const expectedLayout = await planZipLayout({
    receiveIntentDigest: intent.digest,
    artifactDigest: intent.artifact.digest,
    preparationManifestDigest: manifest.digest,
    entries: zipEntrySpecs(manifest.entries),
  })
  if (layout.digest !== expectedLayout.digest) {
    throw new TypeError('sealed ZIP layout does not encode the preparation entries')
  }
  const canonicalBytes = canonicalSealedZipLayoutStorageBytes(layout)
  if (canonicalBytes.byteLength !== bundle.zipLayoutCanonicalBytes.byteLength ||
      canonicalBytes.some((byte, index) => byte !== bundle.zipLayoutCanonicalBytes[index])) {
    throw new TypeError('sealed ZIP layout storage bytes are not canonical')
  }
  const pages = await createPreparationManifestPages(manifest)
  if (pages.length !== bundle.pages.length || pages.some((page, index) => {
    const candidate = bundle.pages[index]
    return candidate === undefined || page.id !== candidate.id || page.digest !== candidate.digest ||
      !equalCanonicalBytes(page.canonicalBytes, candidate.canonicalBytes)
  })) {
    throw new TypeError('preparation manifest pages are not canonical')
  }
  const metadataBytes = checkedMetadataSum(
    BigInt(manifest.canonicalBytes.byteLength),
    sumPageBytes(pages),
    BigInt(canonicalBytes.byteLength),
  )
  if (metadataBytes !== manifest.canonicalMetadataBytes ||
      metadataBytes > checkedU64(metadataLimitBytes, 'preparation metadata limit')) {
    throw new TypeError('preparation metadata accounting is not canonical')
  }
  return Object.freeze({
    manifest,
    pages,
    zipLayout: layout,
    zipLayoutCanonicalBytes: canonicalBytes,
  })
}

function requireWorkspaceZipIntent(
  intent: ReceiveIntent,
): asserts intent is ReceiveIntent & {
  readonly artifact: Extract<ReceiveIntent['artifact'], { kind: 'zip-archive' }>
  readonly plan: Extract<ReceiveIntent['plan'], { kind: 'workspace-then-publish' }>
} {
  if (intent.artifact.kind !== 'zip-archive' ||
      intent.plan.kind !== 'workspace-then-publish' ||
      intent.plan.preparation !== 'exact-zip') {
    throw new TypeError('workspace ZIP preparation requires an exact-zip receive intent')
  }
}

function snapshotGenerations(
  input: readonly AuthenticatedGenerationReference[],
): readonly AuthenticatedGenerationReference[] {
  const result = input.map((reference) => Object.freeze({
    directoryId: snapshotIdentity(reference.directoryId, 16, 'directory ID'),
    generation: snapshotIdentity(reference.generation, 16, 'directory generation'),
  })).sort((left, right) => compareIdentity(left.directoryId, right.directoryId) ||
    compareIdentity(left.generation, right.generation))
  for (let index = 1; index < result.length; index += 1) {
    if (result[index - 1]?.directoryId === result[index]?.directoryId) {
      throw new PreparationManifestError(
        'generation-mismatch',
        'preparation repeats a directory generation authority',
      )
    }
  }
  return Object.freeze(result)
}

function snapshotEntries(
  input: readonly PreparationManifestEntry[],
  resultRoot: ResultRootLayout,
): readonly PreparationManifestEntry[] {
  try {
    const entries = input.map((entry) => {
      const sourcePath = snapshotPreparationSourcePath(entry, resultRoot)
      const artifactPath = snapshotPortableCatalogPath(entry.artifactPath)
      const modifiedTime = entry.modifiedTime === undefined
        ? undefined
        : snapshotCanonicalModifiedTime(entry.modifiedTime)
      if (entry.kind === 'directory') {
        if (entry.role !== 'result-root' && entry.role !== 'necessary-ancestor' &&
            entry.role !== 'explicitly-selected-empty') {
          throw new TypeError('preparation directory role is invalid')
        }
        return Object.freeze({
          kind: 'directory' as const,
          sourcePath,
          artifactPath,
          directoryId: snapshotIdentity(entry.directoryId, 16, 'directory ID'),
          generation: snapshotIdentity(entry.generation, 16, 'directory generation'),
          role: entry.role,
          ...(modifiedTime === undefined ? {} : { modifiedTime }),
        })
      }
      return Object.freeze({
        kind: 'file' as const,
        sourcePath,
        artifactPath,
        fileId: snapshotIdentity(entry.fileId, 16, 'file ID'),
        containingDirectoryId: snapshotIdentity(
          entry.containingDirectoryId,
          16,
          'containing directory ID',
        ),
        generation: snapshotIdentity(entry.generation, 16, 'directory generation'),
        exactSize: checkedU64(entry.exactSize, 'catalog file size'),
        ...(modifiedTime === undefined ? {} : { modifiedTime }),
      })
    })
    entries.sort(comparePreparationEntries)
    return Object.freeze(entries)
  } catch (error) {
    if (error instanceof PreparationManifestError) throw error
    throw new PreparationManifestError('generation-mismatch', 'preparation entry is invalid', {
      cause: error,
    })
  }
}

function snapshotPreparationSourcePath(
  entry: PreparationManifestEntry,
  resultRoot: ResultRootLayout,
): readonly string[] {
  const isSyntheticResultRoot = resultRoot.anchor.kind === 'synthetic-root' &&
    entry.kind === 'directory' && entry.role === 'result-root'
  if (!isSyntheticResultRoot) return snapshotPortableCatalogPath(entry.sourcePath)
  if (!Array.isArray(entry.sourcePath) || entry.sourcePath.length !== 0) {
    throw new TypeError('synthetic result root must retain its authenticated empty source path')
  }
  // The synthetic root is authenticated by SelectionSpec rather than a fabricated
  // catalog segment. Descendants still pass through the ordinary non-empty validator.
  return Object.freeze([])
}

function validatePreparationTopology(
  entries: readonly PreparationManifestEntry[],
  generations: readonly AuthenticatedGenerationReference[],
  resultRoot: ResultRootLayout,
  syntheticRootDirectoryId: string,
): void {
  const generationMap = new Map(generations.map((reference) => [
    reference.directoryId,
    reference.generation,
  ]))
  const artifactPaths = new Set<string>()
  const sourceIdentities = new Set<string>()
  const directories = new Set<string>()
  let rootCount = 0
  for (const entry of entries) {
    const artifactPath = pathKey(entry.artifactPath)
    validateUniquePreparationBinding(entry, artifactPath, artifactPaths, sourceIdentities)
    validatePreparationAuthority(entry, generationMap, resultRoot.name)
    validatePreparationAncestors(entry.artifactPath, directories)
    if (entry.kind === 'directory') {
      directories.add(artifactPath)
      rootCount += validateResultRoot(entry, resultRoot, syntheticRootDirectoryId)
    }
  }
  if (rootCount !== 1) {
    throw new PreparationManifestError('generation-mismatch', 'preparation has no unique result root')
  }
}

function validateUniquePreparationBinding(
  entry: PreparationManifestEntry,
  artifactPath: string,
  artifactPaths: Set<string>,
  sourceIdentities: Set<string>,
): void {
  if (artifactPaths.has(artifactPath)) {
    throw new PreparationManifestError('generation-mismatch', 'preparation duplicates an artifact path')
  }
  artifactPaths.add(artifactPath)
  const sourceIdentity = entry.kind === 'directory'
    ? `directory:${entry.directoryId}`
    : `file:${entry.fileId}`
  if (sourceIdentities.has(sourceIdentity)) {
    throw new PreparationManifestError('generation-mismatch', 'preparation duplicates a source identity')
  }
  sourceIdentities.add(sourceIdentity)
}

function validatePreparationAuthority(
  entry: PreparationManifestEntry,
  generationMap: ReadonlyMap<string, string>,
  resultRootName: string,
): void {
  if (entry.artifactPath[0] !== resultRootName) {
    throw new PreparationManifestError('generation-mismatch', 'preparation entry escaped its result root')
  }
  const directoryId = entry.kind === 'directory' ? entry.directoryId : entry.containingDirectoryId
  if (generationMap.get(directoryId) !== entry.generation) {
    throw new PreparationManifestError(
      'generation-mismatch',
      'preparation entry lacks its authenticated generation authority',
    )
  }
}

function validatePreparationAncestors(
  artifactPath: readonly string[],
  directories: ReadonlySet<string>,
): void {
  for (let depth = 1; depth < artifactPath.length; depth += 1) {
    if (!directories.has(pathKey(artifactPath.slice(0, depth)))) {
      throw new PreparationManifestError(
        'generation-mismatch',
        'preparation omitted a necessary artifact ancestor',
      )
    }
  }
}

function validateResultRoot(
  entry: PreparationDirectoryEntry,
  resultRoot: ResultRootLayout,
  syntheticRootDirectoryId: string,
): number {
  if (entry.role !== 'result-root') return 0
  if (entry.artifactPath.length !== 1) {
    throw new PreparationManifestError('generation-mismatch', 'result root is not the artifact root')
  }
  if (resultRoot.anchor.kind === 'synthetic-root') {
    if (entry.sourcePath.length !== 0 || entry.directoryId !== syntheticRootDirectoryId) {
      throw new PreparationManifestError(
        'generation-mismatch',
        'synthetic result root does not match its authenticated selection root',
      )
    }
  } else if (entry.directoryId !== resultRoot.anchor.directoryId ||
      entry.sourcePath.join('/') !== resultRoot.anchor.sourcePath) {
    throw new PreparationManifestError(
      'generation-mismatch',
      'directory result root does not match its frozen anchor',
    )
  }
  return 1
}

function canonicalPreparationEntry(entry: PreparationManifestEntry): CanonicalBytes {
  const common = [
    canonicalFrame(canonicalPreparationSourcePath(entry)),
    canonicalFrame(canonicalPath(entry.artifactPath)),
  ]
  if (entry.kind === 'directory') {
    return concatCanonicalBytes([
      canonicalU8(1),
      ...common,
      canonicalFrame(canonicalIdentity(entry.directoryId, 16, 'directory ID')),
      canonicalFrame(canonicalIdentity(entry.generation, 16, 'directory generation')),
      canonicalFrame(canonicalModifiedTime(entry.modifiedTime)),
      canonicalFrame(canonicalU8(directoryRoleByte(entry.role))),
    ])
  }
  return concatCanonicalBytes([
    canonicalU8(2),
    ...common,
    canonicalFrame(canonicalIdentity(entry.fileId, 16, 'file ID')),
    canonicalFrame(canonicalIdentity(entry.containingDirectoryId, 16, 'containing directory ID')),
    canonicalFrame(canonicalIdentity(entry.generation, 16, 'directory generation')),
    canonicalFrame(canonicalU64(entry.exactSize)),
    canonicalFrame(canonicalModifiedTime(entry.modifiedTime)),
  ])
}

function decodePreparationGeneration(bytes: Uint8Array): AuthenticatedGenerationReference {
  const reader = CanonicalRecordReader.value(bytes)
  const generation = Object.freeze({
    directoryId: reader.framedIdentity(16, 'directory ID'),
    generation: reader.framedIdentity(16, 'directory generation'),
  })
  reader.finish('preparation generation')
  return generation
}

function decodePreparationEntry(bytes: Uint8Array): PreparationManifestEntry {
  const reader = CanonicalRecordReader.value(bytes)
  const discriminant = reader.byte('preparation entry discriminant')
  const sourcePath = decodeCanonicalPath(reader.frame('preparation source path'), true)
  const artifactPath = decodeCanonicalPath(reader.frame('preparation artifact path'), false)
  if (discriminant === 1) {
    const directoryId = reader.framedIdentity(16, 'directory ID')
    const generation = reader.framedIdentity(16, 'directory generation')
    const modifiedTime = decodeCanonicalModifiedTime(reader.frame('directory modified time'))
    const role = decodePreparationDirectoryRole(reader.frame('preparation directory role'))
    reader.finish('preparation directory entry')
    return Object.freeze({
      kind: 'directory',
      sourcePath,
      artifactPath,
      directoryId,
      generation,
      ...(modifiedTime === undefined ? {} : { modifiedTime }),
      role,
    })
  }
  if (discriminant !== 2) throw new TypeError('preparation entry discriminant is invalid')
  const fileId = reader.framedIdentity(16, 'file ID')
  const containingDirectoryId = reader.framedIdentity(16, 'containing directory ID')
  const generation = reader.framedIdentity(16, 'directory generation')
  const exactSize = reader.framedU64('file size')
  const modifiedTime = decodeCanonicalModifiedTime(reader.frame('file modified time'))
  reader.finish('preparation file entry')
  return Object.freeze({
    kind: 'file',
    sourcePath,
    artifactPath,
    fileId,
    containingDirectoryId,
    generation,
    exactSize,
    ...(modifiedTime === undefined ? {} : { modifiedTime }),
  })
}

function decodeCanonicalModifiedTime(bytes: Uint8Array): CanonicalModifiedTime | undefined {
  const reader = CanonicalRecordReader.value(bytes)
  const discriminant = reader.byte('modified time discriminant')
  if (discriminant === 1) {
    reader.finish('absent modified time')
    return undefined
  }
  if (discriminant !== 2) throw new TypeError('modified time discriminant is invalid')
  const secondsBytes = reader.frame('modified seconds')
  const nanosecondsBytes = reader.frame('modified nanoseconds')
  const precisionBytes = reader.frame('modified precision')
  if (secondsBytes.byteLength !== 8 || nanosecondsBytes.byteLength !== 4 ||
      precisionBytes.byteLength !== 1) {
    throw new TypeError('modified time field width is invalid')
  }
  const modifiedTime = snapshotCanonicalModifiedTime({
    seconds: new DataView(
      secondsBytes.buffer,
      secondsBytes.byteOffset,
      secondsBytes.byteLength,
    ).getBigInt64(0, false),
    nanoseconds: new DataView(
      nanosecondsBytes.buffer,
      nanosecondsBytes.byteOffset,
      nanosecondsBytes.byteLength,
    ).getUint32(0, false),
    precision: precisionBytes[0] as CanonicalModifiedTime['precision'],
  })
  reader.finish('modified time')
  return modifiedTime
}

function decodePreparationDirectoryRole(bytes: Uint8Array): PreparationDirectoryRole {
  const reader = CanonicalRecordReader.value(bytes)
  const value = reader.byte('preparation directory role')
  reader.finish('preparation directory role')
  switch (value) {
    case 1: return 'result-root'
    case 2: return 'necessary-ancestor'
    case 3: return 'explicitly-selected-empty'
    default: throw new TypeError('preparation directory role is invalid')
  }
}

function decodeCanonicalPath(bytes: Uint8Array, allowEmpty: boolean): readonly string[] {
  const reader = CanonicalRecordReader.value(bytes)
  const count = reader.u64('canonical path segment count')
  if (count > 256n || (!allowEmpty && count === 0n)) {
    throw new TypeError('canonical preparation path segment count is invalid')
  }
  const segments: string[] = []
  for (let index = 0n; index < count; index += 1n) {
    const canonical = reader.frame('canonical path segment')
    const value = new TextDecoder(undefined, { fatal: true }).decode(canonical)
    if (!equalCanonicalBytes(canonicalText(value), canonical)) {
      throw new TypeError('canonical preparation path text changed during decoding')
    }
    segments.push(value)
  }
  reader.finish('canonical preparation path')
  if (segments.length === 0) return Object.freeze([])
  return snapshotPortableCatalogPath(segments)
}

function boundedEntryCount(value: bigint, allowZero = true): number {
  if ((!allowZero && value === 0n) || value > BigInt(MAX_ARTIFACT_ENTRIES)) {
    throw new TypeError('persisted preparation aggregate exceeds its entry bound')
  }
  return Number(value)
}

function canonicalPreparationSourcePath(entry: PreparationManifestEntry): CanonicalBytes {
  if (entry.sourcePath.length !== 0) return canonicalPath(entry.sourcePath)
  if (entry.kind !== 'directory' || entry.role !== 'result-root') {
    throw new TypeError('only a result-root directory may encode an empty preparation source path')
  }
  // canonicalPath intentionally keeps the repository-wide non-empty path policy;
  // this local zero-segment encoding is the synthetic catalog-root sentinel.
  return canonicalU64(0n)
}

function canonicalGeneration(reference: AuthenticatedGenerationReference): CanonicalBytes {
  return concatCanonicalBytes([
    canonicalFrame(canonicalIdentity(reference.directoryId, 16, 'directory ID')),
    canonicalFrame(canonicalIdentity(reference.generation, 16, 'directory generation')),
  ])
}

function canonicalModifiedTime(value: CanonicalModifiedTime | undefined): CanonicalBytes {
  if (value === undefined) return canonicalU8(1)
  const modified = snapshotCanonicalModifiedTime(value)
  return concatCanonicalBytes([
    canonicalU8(2),
    canonicalFrame(canonicalI64(modified.seconds)),
    canonicalFrame(canonicalU32(modified.nanoseconds)),
    canonicalFrame(canonicalU8(modified.precision)),
  ])
}

function canonicalZipEntryPlan(entry: ZipEntryPlanV1): CanonicalBytes {
  return concatCanonicalBytes([
    canonicalU8(entry.kind === 'directory' ? 1 : 2),
    canonicalFrame(canonicalPath(entry.path)),
    canonicalFrame(Uint8Array.from(entry.nameBytes)),
    canonicalFrame(canonicalU64(entry.exactSize)),
    canonicalFrame(canonicalU32(entry.dosTime)),
    canonicalFrame(canonicalU32(entry.dosDate)),
    canonicalFrame(canonicalBoolean(entry.zip64Size)),
    canonicalFrame(canonicalBoolean(entry.zip64Offset)),
    canonicalFrame(canonicalU8(entry.versionNeeded)),
    canonicalFrame(canonicalU64(entry.localHeaderOffset)),
    canonicalFrame(canonicalU64(entry.localExtraBytes)),
    canonicalFrame(canonicalU64(entry.localHeaderBytes)),
    canonicalFrame(canonicalU64(entry.descriptorBytes)),
    canonicalFrame(canonicalU64(entry.entryStreamBytes)),
    canonicalFrame(canonicalU8(entry.centralZip64ValueCount)),
    canonicalFrame(canonicalU64(entry.centralExtraBytes)),
    canonicalFrame(canonicalU64(entry.centralRecordBytes)),
  ])
}

function zipEntrySpecs(entries: readonly PreparationManifestEntry[]): readonly ZipEntrySpec[] {
  return Object.freeze(entries.map((entry) => {
    const modifiedTimeMilliseconds = entry.modifiedTime === undefined
      ? undefined
      // Precision 3 permits sub-millisecond timestamps. BigInt division floors the
      // non-negative fractional second without crossing a Number-to-BigInt boundary.
      : entry.modifiedTime.seconds * 1_000n +
        BigInt(entry.modifiedTime.nanoseconds) / NANOSECONDS_PER_MILLISECOND
    if (entry.kind === 'directory') {
      return Object.freeze({
        kind: 'directory' as const,
        path: entry.artifactPath,
        ...(modifiedTimeMilliseconds === undefined ? {} : { modifiedTimeMilliseconds }),
      })
    }
    return Object.freeze({
      kind: 'file' as const,
      path: entry.artifactPath,
      exactSize: entry.exactSize,
      ...(modifiedTimeMilliseconds === undefined ? {} : { modifiedTimeMilliseconds }),
    })
  }))
}

function directoryRoleByte(role: PreparationDirectoryRole): number {
  switch (role) {
    case 'result-root': return 1
    case 'necessary-ancestor': return 2
    case 'explicitly-selected-empty': return 3
  }
}

function comparePreparationEntries(
  left: PreparationManifestEntry,
  right: PreparationManifestEntry,
): number {
  const order = compareUnsignedBytes(
    TEXT_ENCODER.encode(left.artifactPath.join('/')),
    TEXT_ENCODER.encode(right.artifactPath.join('/')),
  )
  if (order !== 0 || left.kind === right.kind) return order
  return left.kind === 'directory' ? -1 : 1
}

function compareIdentity(left: string, right: string): number {
  return compareUnsignedBytes(
    canonicalIdentity(left, 16, 'identity'),
    canonicalIdentity(right, 16, 'identity'),
  )
}

function compareUnsignedBytes(left: ArrayLike<number>, right: ArrayLike<number>): number {
  const length = Math.min(left.length, right.length)
  for (let index = 0; index < length; index += 1) {
    const difference = (left[index] ?? 0) - (right[index] ?? 0)
    if (difference !== 0) return difference
  }
  return left.length - right.length
}

function sumPageBytes(pages: readonly ManifestPageRecord[]): bigint {
  return pages.reduce(
    (total, page) => checkedMetadataSum(total, BigInt(page.canonicalBytes.byteLength)),
    0n,
  )
}

function checkedMetadataSum(...values: readonly bigint[]): bigint {
  let total = 0n
  for (const value of values) {
    total += value
    if (total < 0n || total > U64_MAXIMUM) {
      throw new PreparationManifestError('arithmetic-overflow', 'preparation byte arithmetic overflow')
    }
  }
  return total
}

function checkedU64(value: bigint, label: string): bigint {
  if (typeof value !== 'bigint' || value < 0n || value > U64_MAXIMUM) {
    throw new PreparationManifestError('arithmetic-overflow', `${label} is not a u64`)
  }
  return value
}

function pathKey(path: readonly string[]): string {
  return path.join('\0')
}

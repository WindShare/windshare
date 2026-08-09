import { snapshotPortableCatalogPath } from '../../catalog/path-policy'
import {
  snapshotCanonicalModifiedTime,
} from '../../transfer/directory-admission'
import {
  validateReceiveIntent,
  type ReceiveIntent,
  type ResultRootLayout,
} from '../../transfer/intent'
import {
  planZipLayout,
  validateSealedZipLayoutPlan,
} from '../zip-layout/layout'
import {
  canonicalDigest,
  canonicalFrame,
  canonicalIdentity,
  canonicalRecord,
  canonicalU64,
  equalCanonicalBytes,
  snapshotIdentity,
} from './canonical'
import type { AuthenticatedGenerationReference } from './manifest'
import {
  createManifestPageRecord,
  MANIFEST_PAGE_ENTRY_LIMIT,
  RECEIVE_RECORD_PREPARATION,
  type ManifestPageRecord,
} from './records'
import { CanonicalRecordReader } from './canonical-reader'
import {
  boundedEntryCount,
  canonicalGeneration,
  canonicalPreparationEntry,
  canonicalSealedZipLayoutStorageBytes,
  decodePreparationEntry,
  decodePreparationGeneration,
  zipEntrySpecs,
} from './preparation/codec'
import {
  DEFAULT_PREPARATION_METADATA_LIMIT,
  MAX_ARTIFACT_ENTRIES,
  PREPARATION_SCHEMA_VERSION,
  PreparationManifestError,
  type PreparationDirectoryEntry,
  type PreparationManifestEntry,
  type PreparationManifestV1,
  type SealedWorkspaceZipPreparationV1,
} from './preparation/model'

export {
  DEFAULT_PORTABLE_PREPARATION_METADATA_LIMIT,
  DEFAULT_PREPARATION_METADATA_LIMIT,
  PreparationManifestError,
} from './preparation/model'
export type {
  PreparationDirectoryEntry,
  PreparationDirectoryRole,
  PreparationFileEntry,
  PreparationManifestEntry,
  PreparationManifestV1,
  SealedWorkspaceZipPreparationV1,
} from './preparation/model'

const U64_MAXIMUM = 0xffff_ffff_ffff_ffffn
const TEXT_ENCODER = new TextEncoder()

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

export { canonicalSealedZipLayoutStorageBytes } from './preparation/codec'

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

import type { ReceiveIntent } from '../../transfer/intent'
import type { ExactPreparationEvidence } from '../../transfer/output-session'
import type {
  V2PortableOriginalExecutionRoute,
  V2PortableZipExecutionRoute,
} from '../../transfer/settlement/v2-plan-authority'
import type { CanonicalModifiedTime } from '../../transfer/directory-admission'
import type { PreparationAdmissionReason } from '../workspace/state'
import type {
  PreparationDirectoryEntry,
  PreparationFileEntry,
  PreparationManifestEntry,
} from '../workspace/preparation'
import {
  canonicalDigest,
  canonicalFrame,
  canonicalI64,
  canonicalIdentity,
  canonicalPath,
  canonicalRecord,
  canonicalU8,
  canonicalU32,
  canonicalU64,
  concatCanonicalBytes,
} from '../workspace/canonical'
import {
  planZipLayout,
  type SealedZipLayoutPlanV1,
} from '../zip-layout/layout'
import type { ZipEntrySpec } from '../zip-layout/policy'

const PORTABLE_PREPARATION_DOMAIN = 'windshare/portable-preparation/v1'
const PORTABLE_PREPARATION_PAGE_DOMAIN = 'windshare/portable-preparation/v1/page'
const PORTABLE_PREPARATION_PAGE_ENTRIES = 256
const PORTABLE_PREPARATION_METADATA_LIMIT = 16_777_216n
const CATALOG_IDENTITY_BYTES = 16
const DIGEST_BYTES = 32
const U64_MAXIMUM = 0xffff_ffff_ffff_ffffn
const UTF8_ENCODER = new TextEncoder()

export class PortablePreparationAdmissionError extends TypeError {
  readonly reason: PreparationAdmissionReason
  readonly preparationManifestDigest: string | undefined

  constructor(
    reason: PreparationAdmissionReason,
    message: string,
    options?: ErrorOptions & { readonly preparationManifestDigest?: string },
  ) {
    super(message, options)
    this.name = 'PortablePreparationAdmissionError'
    this.reason = reason
    this.preparationManifestDigest = options?.preparationManifestDigest
  }
}

export function validateOriginalPreparation(
  intent: Parameters<V2PortableOriginalExecutionRoute['prepare']>[0],
  evidence: ExactPreparationEvidence,
): PreparationFileEntry {
  validateEvidenceEnvelope(evidence)
  if (evidence.entryCount !== 1n || evidence.fileCount !== 1n ||
      evidence.directoryCount !== 0n || evidence.entries.length !== 1) {
    throw admissionError('generation-mismatch', 'portable original preparation is not one file')
  }
  const entry = evidence.entries[0]
  if (entry?.kind !== 'file' ||
      entry.fileId !== intent.artifact.fileId ||
      entry.sourcePath.join('/') !== intent.artifact.sourcePath ||
      entry.artifactPath.length !== 1 ||
      entry.artifactPath[0] !== intent.artifact.suggestedName ||
      evidence.selectedRawBytes !== entry.exactSize) {
    throw admissionError(
      'generation-mismatch',
      'portable original preparation changed the frozen file artifact',
    )
  }
  requireGenerationAuthority(evidence, entry.containingDirectoryId, entry.generation)
  return entry
}

export function validateZipPreparation(
  intent: Parameters<V2PortableZipExecutionRoute['prepare']>[0],
  evidence: ExactPreparationEvidence,
): void {
  validateEvidenceEnvelope(evidence)
  const generations = generationMap(evidence)
  const sourceIdentities = new Set<string>()
  const roots: PreparationDirectoryEntry[] = []
  for (const entry of evidence.entries) {
    validateZipPreparationEntry(intent, entry, generations, sourceIdentities)
    if (entry.kind === 'directory' && entry.role === 'result-root') roots.push(entry)
  }
  if (roots.length !== 1 || roots[0]!.artifactPath.length !== 1) {
    throw admissionError('generation-mismatch', 'portable ZIP lacks one result root')
  }
  validateZipPreparationRoot(intent, roots[0]!)
}

function validateZipPreparationEntry(
  intent: Parameters<V2PortableZipExecutionRoute['prepare']>[0],
  entry: PreparationManifestEntry,
  generations: ReadonlyMap<string, string>,
  sourceIdentities: Set<string>,
): void {
  const sourceIdentity = entry.kind + ':' +
    (entry.kind === 'directory' ? entry.directoryId : entry.fileId)
  if (sourceIdentities.has(sourceIdentity)) {
    throw admissionError('generation-mismatch', 'portable preparation repeats a source identity')
  }
  sourceIdentities.add(sourceIdentity)
  const directoryId = entry.kind === 'directory'
    ? entry.directoryId
    : entry.containingDirectoryId
  if (generations.get(directoryId) !== entry.generation) {
    throw admissionError('generation-mismatch', 'portable preparation lacks generation authority')
  }
  if (entry.artifactPath[0] !== intent.artifact.layout.name) {
    throw admissionError('generation-mismatch', 'portable ZIP entry escaped its result root')
  }
}

function validateZipPreparationRoot(
  intent: Parameters<V2PortableZipExecutionRoute['prepare']>[0],
  root: PreparationDirectoryEntry,
): void {
  const anchor = intent.artifact.layout.anchor
  if (anchor.kind === 'synthetic-root') {
    if (root.sourcePath.length !== 0 || root.directoryId !== intent.syntheticRoot) {
      throw admissionError('generation-mismatch', 'portable ZIP synthetic root changed')
    }
    return
  }
  if (root.directoryId !== anchor.directoryId ||
      root.sourcePath.join('/') !== anchor.sourcePath) {
    throw admissionError('generation-mismatch', 'portable ZIP result-root anchor changed')
  }
}

function validateEvidenceEnvelope(evidence: ExactPreparationEvidence): void {
  if (!Array.isArray(evidence.entries) || !Array.isArray(evidence.generations)) {
    throw admissionError('generation-mismatch', 'portable exact preparation is malformed')
  }
  const fileCount = BigInt(evidence.entries.filter(entry => entry.kind === 'file').length)
  const directoryCount = BigInt(evidence.entries.length) - fileCount
  let selectedRawBytes = 0n
  for (const entry of evidence.entries) {
    if (entry.kind === 'file') {
      selectedRawBytes = checkedAdd(selectedRawBytes, entry.exactSize)
    }
  }
  if (evidence.entryCount !== BigInt(evidence.entries.length) ||
      evidence.fileCount !== fileCount ||
      evidence.directoryCount !== directoryCount ||
      evidence.selectedRawBytes !== selectedRawBytes) {
    throw admissionError('generation-mismatch', 'portable preparation aggregates changed')
  }
  for (let index = 1; index < evidence.entries.length; index += 1) {
    if (comparePreparationEntries(evidence.entries[index - 1]!, evidence.entries[index]!) >= 0) {
      throw admissionError('generation-mismatch', 'portable preparation entries are not canonical')
    }
  }
  for (let index = 1; index < evidence.generations.length; index += 1) {
    if (compareUTF8(evidence.generations[index - 1]!.directoryId,
      evidence.generations[index]!.directoryId) >= 0) {
      throw admissionError('generation-mismatch', 'portable generation evidence is not canonical')
    }
  }
}

function generationMap(evidence: ExactPreparationEvidence): ReadonlyMap<string, string> {
  const result = new Map<string, string>()
  for (const generation of evidence.generations) {
    canonicalIdentity(generation.directoryId, CATALOG_IDENTITY_BYTES, 'directory ID')
    canonicalIdentity(generation.generation, CATALOG_IDENTITY_BYTES, 'directory generation')
    if (result.has(generation.directoryId)) {
      throw admissionError('generation-mismatch', 'portable preparation repeats a generation')
    }
    result.set(generation.directoryId, generation.generation)
  }
  return result
}

function requireGenerationAuthority(
  evidence: ExactPreparationEvidence,
  directoryId: string,
  generation: string,
): void {
  if (generationMap(evidence).get(directoryId) !== generation) {
    throw admissionError('generation-mismatch', 'portable original lacks generation authority')
  }
}

export async function planPortableZipLayout(
  intent: Parameters<V2PortableZipExecutionRoute['prepare']>[0],
  evidence: ExactPreparationEvidence,
  preparationManifestDigest: string,
): Promise<SealedZipLayoutPlanV1> {
  try {
    return await planZipLayout({
      receiveIntentDigest: intent.digest,
      artifactDigest: intent.artifact.digest,
      preparationManifestDigest,
      entries: evidence.entries.map(zipEntrySpec),
    })
  } catch (error) {
    throw normalizeZipAdmissionError(error)
  }
}

function zipEntrySpec(entry: PreparationManifestEntry): ZipEntrySpec {
  const modifiedTimeMilliseconds = entry.modifiedTime === undefined
    ? undefined
    : entry.modifiedTime.seconds * 1_000n +
      BigInt(Math.trunc(entry.modifiedTime.nanoseconds / 1_000_000))
  if (entry.kind === 'directory') {
    return Object.freeze({
      kind: 'directory',
      path: entry.artifactPath,
      ...(modifiedTimeMilliseconds === undefined ? {} : { modifiedTimeMilliseconds }),
    })
  }
  return Object.freeze({
    kind: 'file',
    path: entry.artifactPath,
    exactSize: entry.exactSize,
    ...(modifiedTimeMilliseconds === undefined ? {} : { modifiedTimeMilliseconds }),
  })
}

export async function sealPortablePreparation(
  intent: ReceiveIntent,
  evidence: ExactPreparationEvidence,
): Promise<string> {
  let metadataBytes = 0n
  const account = (bytes: Uint8Array): Uint8Array => {
    metadataBytes = checkedAdd(metadataBytes, BigInt(bytes.byteLength))
    if (metadataBytes > PORTABLE_PREPARATION_METADATA_LIMIT) {
      throw admissionError('metadata-limit', 'portable preparation metadata exceeds its hard limit')
    }
    return bytes
  }
  const generationDigests = await digestPages(
    evidence.generations,
    canonicalGeneration,
    account,
  )
  const entryDigests = await digestPages(
    evidence.entries,
    canonicalPreparationEntry,
    account,
  )
  const envelope = account(canonicalRecord(PORTABLE_PREPARATION_DOMAIN, 1, [
    canonicalFrame(canonicalIdentity(intent.digest, DIGEST_BYTES, 'receive intent digest')),
    canonicalFrame(canonicalIdentity(intent.artifact.digest, DIGEST_BYTES, 'artifact digest')),
    canonicalFrame(canonicalU64(evidence.entryCount)),
    canonicalFrame(canonicalU64(evidence.fileCount)),
    canonicalFrame(canonicalU64(evidence.directoryCount)),
    canonicalFrame(canonicalU64(evidence.selectedRawBytes)),
    canonicalFrame(canonicalU64(BigInt(generationDigests.length))),
    ...generationDigests.map(digest =>
      canonicalFrame(canonicalIdentity(digest, DIGEST_BYTES, 'generation page digest'))),
    canonicalFrame(canonicalU64(BigInt(entryDigests.length))),
    ...entryDigests.map(digest =>
      canonicalFrame(canonicalIdentity(digest, DIGEST_BYTES, 'entry page digest'))),
  ]))
  return canonicalDigest(envelope)
}

async function digestPages<Entry>(
  entries: readonly Entry[],
  encode: (entry: Entry) => Uint8Array,
  account: (bytes: Uint8Array) => Uint8Array,
): Promise<readonly string[]> {
  const digests: string[] = []
  for (let start = 0; start < entries.length; start += PORTABLE_PREPARATION_PAGE_ENTRIES) {
    const pageEntries = entries
      .slice(start, start + PORTABLE_PREPARATION_PAGE_ENTRIES)
      .map(entry => canonicalFrame(account(encode(entry))))
    const page = account(canonicalRecord(PORTABLE_PREPARATION_PAGE_DOMAIN, 1, [
      canonicalFrame(canonicalU64(BigInt(start / PORTABLE_PREPARATION_PAGE_ENTRIES))),
      canonicalFrame(canonicalU64(BigInt(pageEntries.length))),
      ...pageEntries,
    ]))
    digests.push(await canonicalDigest(page))
  }
  return Object.freeze(digests)
}

function canonicalGeneration(
  generation: ExactPreparationEvidence['generations'][number],
): Uint8Array {
  return concatCanonicalBytes([
    canonicalFrame(canonicalIdentity(
      generation.directoryId,
      CATALOG_IDENTITY_BYTES,
      'directory ID',
    )),
    canonicalFrame(canonicalIdentity(
      generation.generation,
      CATALOG_IDENTITY_BYTES,
      'directory generation',
    )),
  ])
}

function canonicalPreparationEntry(entry: PreparationManifestEntry): Uint8Array {
  const common = [
    canonicalFrame(canonicalEvidencePath(entry.sourcePath)),
    canonicalFrame(canonicalPath(entry.artifactPath)),
    canonicalFrame(canonicalModifiedTime(entry.modifiedTime)),
  ]
  if (entry.kind === 'directory') {
    return concatCanonicalBytes([
      canonicalU8(1),
      ...common,
      canonicalFrame(canonicalIdentity(entry.directoryId, CATALOG_IDENTITY_BYTES, 'directory ID')),
      canonicalFrame(canonicalIdentity(
        entry.generation,
        CATALOG_IDENTITY_BYTES,
        'directory generation',
      )),
      canonicalFrame(canonicalU8(directoryRoleByte(entry.role))),
    ])
  }
  return concatCanonicalBytes([
    canonicalU8(2),
    ...common,
    canonicalFrame(canonicalIdentity(entry.fileId, CATALOG_IDENTITY_BYTES, 'file ID')),
    canonicalFrame(canonicalIdentity(
      entry.containingDirectoryId,
      CATALOG_IDENTITY_BYTES,
      'containing directory ID',
    )),
    canonicalFrame(canonicalIdentity(
      entry.generation,
      CATALOG_IDENTITY_BYTES,
      'directory generation',
    )),
    canonicalFrame(canonicalU64(entry.exactSize)),
  ])
}

function canonicalEvidencePath(path: readonly string[]): Uint8Array {
  return path.length === 0 ? canonicalU64(0n) : canonicalPath(path)
}

function canonicalModifiedTime(value: CanonicalModifiedTime | undefined): Uint8Array {
  if (value === undefined) return canonicalU8(0)
  return concatCanonicalBytes([
    canonicalU8(1),
    canonicalFrame(canonicalI64(value.seconds)),
    canonicalFrame(canonicalU32(value.nanoseconds)),
    canonicalFrame(canonicalU8(value.precision)),
  ])
}

function directoryRoleByte(role: PreparationDirectoryEntry['role']): number {
  switch (role) {
    case 'result-root': return 1
    case 'necessary-ancestor': return 2
    case 'explicitly-selected-empty': return 3
  }
}

export function requirePortableArtifactBudget(intent: ReceiveIntent, exactBytes: bigint): void {
  if (intent.plan.kind !== 'portable-handoff') {
    throw new TypeError('portable admission requires a portable intent')
  }
  if (typeof exactBytes !== 'bigint' || exactBytes < 0n || exactBytes > U64_MAXIMUM) {
    throw admissionError('arithmetic-overflow', 'portable artifact length is outside u64')
  }
  const binding = intent.plan.portable
  const requiredParts = exactBytes === 0n
    ? 0n
    : (exactBytes + binding.assemblyPartBytes - 1n) / binding.assemblyPartBytes
  if (exactBytes > binding.maximumArtifactBytes || requiredParts > binding.maximumParts) {
    throw admissionError('artifact-limit', 'portable artifact exceeds its frozen hard limit')
  }
}

export function normalizeOriginalAdmissionError(error: unknown): unknown {
  if (error instanceof PortablePreparationAdmissionError ||
      error instanceof DOMException) return error
  if (error instanceof RangeError) {
    return admissionError('arithmetic-overflow', 'portable original preparation overflowed', error)
  }
  if (error instanceof TypeError) {
    return admissionError('generation-mismatch', 'portable original preparation is invalid', error)
  }
  return error
}

export function normalizeZipAdmissionError(error: unknown): unknown {
  if (error instanceof PortablePreparationAdmissionError ||
      error instanceof DOMException) return error
  if (error instanceof RangeError) {
    return admissionError('entry-limit', 'portable ZIP layout exceeds its bounded policy', error)
  }
  if (error instanceof TypeError) {
    return admissionError('generation-mismatch', 'portable ZIP preparation is invalid', error)
  }
  return error
}

function admissionError(
  reason: PreparationAdmissionReason,
  message: string,
  cause?: unknown,
): PortablePreparationAdmissionError {
  return new PortablePreparationAdmissionError(
    reason,
    message,
    cause === undefined ? undefined : { cause },
  )
}

function comparePreparationEntries(
  left: PreparationManifestEntry,
  right: PreparationManifestEntry,
): number {
  const path = compareUTF8(left.artifactPath.join('/'), right.artifactPath.join('/'))
  if (path !== 0) return path
  if (left.kind === right.kind) return 0
  return left.kind === 'directory' ? -1 : 1
}

function compareUTF8(left: string, right: string): number {
  const a = UTF8_ENCODER.encode(left)
  const b = UTF8_ENCODER.encode(right)
  const length = Math.min(a.byteLength, b.byteLength)
  for (let index = 0; index < length; index += 1) {
    const difference = a[index]! - b[index]!
    if (difference !== 0) return difference
  }
  return a.byteLength - b.byteLength
}

function checkedAdd(left: bigint, right: bigint): bigint {
  if (typeof left !== 'bigint' || typeof right !== 'bigint' ||
      left < 0n || right < 0n || left > U64_MAXIMUM - right) {
    throw admissionError('arithmetic-overflow', 'portable preparation accounting overflowed u64')
  }
  return left + right
}

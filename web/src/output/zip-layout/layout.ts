import { decodeBase64Url, encodeBase64Url } from '../../crypto/bytes'
import { sha256 } from '../../crypto/digest'
import {
  MAX_ZIP_SPOOL_ENTRIES,
  ZIP_CLASSIC_END_BYTES,
  ZIP_ENCODING_POLICY,
  ZIP_ENCODING_POLICY_V1,
  ZIP_ENCODING_POLICY_VERSION,
  ZIP64_END_BYTES,
  checkedZipAdd,
  type NormalizedZipEntry,
  type ZipEntryPlanV1,
  type ZipEntrySpec,
} from './policy'

export const ZIP_LAYOUT_PLAN_VERSION = 1 as const
export const ZIP_LAYOUT_DIGEST_DOMAIN = 'windshare/zip-layout/v1' as const
export const ZIP_LAYOUT_DIGEST_BYTES = 32

const TEXT_ENCODER = new TextEncoder()
const ZIP_LAYOUT_ENTRY_PAGE_MAXIMUM = 256
const ZIP_LAYOUT_ENTRY_PAGE_DOMAIN = 'windshare/zip-layout/v1/entry-page'

export type ZipLayoutEvidence =
  | Readonly<{ kind: 'prepared'; preparationManifestDigest: string }>
  | Readonly<{ kind: 'progressive'; discoveryLedgerDigest: string }>

export interface SealedZipLayoutPlanV1 {
  readonly version: typeof ZIP_LAYOUT_PLAN_VERSION
  readonly receiveIntentDigest: string
  readonly artifactDigest: string
  readonly evidence: ZipLayoutEvidence
  readonly encodingPolicy: typeof ZIP_ENCODING_POLICY
  readonly encodingPolicyVersion: typeof ZIP_ENCODING_POLICY_VERSION
  readonly entries: readonly ZipEntryPlanV1[]
  readonly entryCount: bigint
  readonly centralDirectoryOffset: bigint
  readonly centralDirectoryBytes: bigint
  readonly zip64EndRequired: boolean
  readonly zip64EndBytes: bigint
  readonly classicEndBytes: typeof ZIP_CLASSIC_END_BYTES
  readonly exactArchiveBytes: bigint
  readonly maximumSpoolBytes: bigint
  readonly digest: string
}

export interface PlanZipLayoutInput {
  readonly receiveIntentDigest: string
  readonly artifactDigest: string
  readonly preparationManifestDigest: string
  readonly entries: readonly ZipEntrySpec[]
}

interface LayoutSummary {
  readonly entryCount: bigint
  readonly centralDirectoryOffset: bigint
  readonly centralDirectoryBytes: bigint
  readonly zip64EndRequired: boolean
  readonly zip64EndBytes: bigint
  readonly exactArchiveBytes: bigint
}

type LedgerState = 'discovering' | 'discovery-complete' | 'sealing' | 'sealed' | 'failed'

/** Pure prepared-layout planner; sorting and all byte accounting come from the encoding policy. */
export async function planZipLayout(input: PlanZipLayoutInput): Promise<SealedZipLayoutPlanV1> {
  const normalized = input.entries.map((entry) => ZIP_ENCODING_POLICY_V1.normalizeEntry(entry))
  normalized.sort(ZIP_ENCODING_POLICY_V1.compareEntries)
  const entries = planNormalizedEntries(normalized)
  return sealLayout({
    receiveIntentDigest: input.receiveIntentDigest,
    artifactDigest: input.artifactDigest,
    evidence: Object.freeze({
      kind: 'prepared',
      preparationManifestDigest: snapshotDigest(
        input.preparationManifestDigest,
        'preparation manifest digest',
      ),
    }),
    entries,
  })
}

/**
 * Progressive DirectAtomic authority. Appends are immutable and canonical-order only;
 * discovery or member failure permanently removes the ability to produce close proof.
 */
export class ZipLayoutLedgerV1 {
  readonly #receiveIntentDigest: string
  readonly #artifactDigest: string
  readonly #entries: ZipEntryPlanV1[] = []
  readonly #directories = new Set<string>()
  #state: LedgerState = 'discovering'
  #streamBytes = 0n
  #centralBytes = 0n
  #discoveryLedgerDigest: string | undefined
  #sealedPlan: SealedZipLayoutPlanV1 | undefined
  #sealPromise: Promise<SealedZipLayoutPlanV1> | undefined

  constructor(receiveIntentDigest: string, artifactDigest: string) {
    this.#receiveIntentDigest = snapshotDigest(receiveIntentDigest, 'receive intent digest')
    this.#artifactDigest = snapshotDigest(artifactDigest, 'artifact digest')
  }

  get receiveIntentDigest(): string {
    return this.#receiveIntentDigest
  }

  get artifactDigest(): string {
    return this.#artifactDigest
  }

  get entryCount(): number {
    return this.#entries.length
  }

  get streamBytes(): bigint {
    return this.#streamBytes
  }

  get centralDirectoryBytes(): bigint {
    return this.#centralBytes
  }

  entryAt(index: number): ZipEntryPlanV1 | undefined {
    if (!Number.isSafeInteger(index) || index < 0) throw new RangeError('ZIP ledger index is invalid')
    return this.#entries[index]
  }

  append(spec: ZipEntrySpec): ZipEntryPlanV1 {
    if (this.#state !== 'discovering') throw new Error('ZIP layout ledger is not accepting entries')
    const normalized = ZIP_ENCODING_POLICY_V1.normalizeEntry(spec)
    const previous = this.#entries.at(-1)
    if (previous !== undefined && ZIP_ENCODING_POLICY_V1.compareEntries(previous, normalized) >= 0) {
      throw new TypeError('ZIP progressive entries are not in canonical order')
    }
    requireNextTopology(normalized, this.#entries.length, this.#entries[0], this.#directories)
    const plan = ZIP_ENCODING_POLICY_V1.planEntry(normalized, this.#streamBytes)
    const nextCentralBytes = checkedZipAdd(this.#centralBytes, plan.centralRecordBytes)
    ZIP_ENCODING_POLICY_V1.requireSpoolBudget(BigInt(this.#entries.length + 1), nextCentralBytes)
    const nextStreamBytes = checkedZipAdd(this.#streamBytes, plan.entryStreamBytes)
    this.#entries.push(plan)
    if (plan.kind === 'directory') this.#directories.add(plan.artifactPath)
    this.#centralBytes = nextCentralBytes
    this.#streamBytes = nextStreamBytes
    return plan
  }

  completeDiscovery(discoveryLedgerDigest: string): void {
    if (this.#state !== 'discovering') throw new Error('ZIP discovery cannot be completed now')
    if (this.#entries.length === 0) throw new Error('ZIP discovery did not produce a result root')
    this.#discoveryLedgerDigest = snapshotDigest(discoveryLedgerDigest, 'discovery ledger digest')
    this.#state = 'discovery-complete'
  }

  recordSelectedMemberFailure(): void {
    this.#invalidate()
  }

  recordDiscoveryFailure(): void {
    this.#invalidate()
  }

  seal(): Promise<SealedZipLayoutPlanV1> {
    if (this.#state === 'sealed' && this.#sealedPlan !== undefined) {
      return Promise.resolve(this.#sealedPlan)
    }
    if (this.#sealPromise !== undefined) return this.#sealPromise
    if (this.#state !== 'discovery-complete' || this.#discoveryLedgerDigest === undefined) {
      return Promise.reject(new Error('ZIP layout cannot seal before successful discovery'))
    }
    this.#state = 'sealing'
    const operation = sealLayout({
      receiveIntentDigest: this.#receiveIntentDigest,
      artifactDigest: this.#artifactDigest,
      evidence: Object.freeze({
        kind: 'progressive',
        discoveryLedgerDigest: this.#discoveryLedgerDigest,
      }),
      entries: Object.freeze([...this.#entries]),
    }).then((plan) => {
      if (this.#state === 'failed') throw new Error('ZIP layout failed while sealing')
      this.#sealedPlan = plan
      this.#state = 'sealed'
      return plan
    }, (error: unknown) => {
      this.#state = 'failed'
      throw error
    }).finally(() => { this.#sealPromise = undefined })
    this.#sealPromise = operation
    return operation
  }

  acceptsSealedPlan(plan: SealedZipLayoutPlanV1): boolean {
    return this.#state === 'sealed' && this.#sealedPlan !== undefined &&
      plan === this.#sealedPlan
  }

  #invalidate(): void {
    if (this.#state === 'failed') return
    this.#state = 'failed'
    this.#sealedPlan = undefined
  }
}

export async function validateSealedZipLayoutPlan(
  candidate: SealedZipLayoutPlanV1,
): Promise<SealedZipLayoutPlanV1> {
  if (candidate === null || typeof candidate !== 'object' ||
      candidate.version !== ZIP_LAYOUT_PLAN_VERSION ||
      candidate.encodingPolicy !== ZIP_ENCODING_POLICY ||
      candidate.encodingPolicyVersion !== ZIP_ENCODING_POLICY_VERSION ||
      !Array.isArray(candidate.entries)) {
    throw new TypeError('sealed ZIP layout plan envelope is invalid')
  }
  const receiveIntentDigest = snapshotDigest(candidate.receiveIntentDigest, 'receive intent digest')
  const artifactDigest = snapshotDigest(candidate.artifactDigest, 'artifact digest')
  const evidence = snapshotEvidence(candidate.evidence)
  const entries: ZipEntryPlanV1[] = []
  let expectedOffset = 0n
  for (const candidateEntry of candidate.entries) {
    const entry = ZIP_ENCODING_POLICY_V1.validateEntryPlan(candidateEntry, expectedOffset)
    entries.push(entry)
    expectedOffset = checkedZipAdd(expectedOffset, entry.entryStreamBytes)
  }
  validatePlannedEntries(entries)
  const rebuilt = await sealLayout({
    receiveIntentDigest,
    artifactDigest,
    evidence,
    entries: Object.freeze(entries),
  })
  if (candidate.digest !== rebuilt.digest ||
      candidate.entryCount !== rebuilt.entryCount ||
      candidate.centralDirectoryOffset !== rebuilt.centralDirectoryOffset ||
      candidate.centralDirectoryBytes !== rebuilt.centralDirectoryBytes ||
      candidate.zip64EndRequired !== rebuilt.zip64EndRequired ||
      candidate.zip64EndBytes !== rebuilt.zip64EndBytes ||
      candidate.classicEndBytes !== rebuilt.classicEndBytes ||
      candidate.exactArchiveBytes !== rebuilt.exactArchiveBytes ||
      candidate.maximumSpoolBytes !== rebuilt.maximumSpoolBytes) {
    throw new TypeError('sealed ZIP layout plan does not match its canonical fields')
  }
  return rebuilt
}

async function canonicalZipLayoutDigestPreimage(
  plan: Omit<SealedZipLayoutPlanV1, 'digest'>,
): Promise<Uint8Array<ArrayBuffer>> {
  const fields: Uint8Array[] = [
    frame(TEXT_ENCODER.encode(ZIP_LAYOUT_DIGEST_DOMAIN)),
    uint64(BigInt(plan.version)),
    frame(digestBytes(plan.receiveIntentDigest, 'receive intent digest')),
    frame(digestBytes(plan.artifactDigest, 'artifact digest')),
  ]
  if (plan.evidence.kind === 'prepared') {
    fields.push(Uint8Array.of(1))
    fields.push(frame(digestBytes(plan.evidence.preparationManifestDigest, 'preparation manifest digest')))
  } else {
    fields.push(Uint8Array.of(2))
    fields.push(frame(digestBytes(plan.evidence.discoveryLedgerDigest, 'discovery ledger digest')))
  }
  fields.push(frame(TEXT_ENCODER.encode(plan.encodingPolicy)))
  fields.push(uint64(BigInt(plan.encodingPolicyVersion)))
  fields.push(uint64(plan.entryCount))
  const pageCount = Math.ceil(plan.entries.length / ZIP_LAYOUT_ENTRY_PAGE_MAXIMUM)
  fields.push(uint64(BigInt(pageCount)))
  for (let pageIndex = 0; pageIndex < pageCount; pageIndex += 1) {
    const start = pageIndex * ZIP_LAYOUT_ENTRY_PAGE_MAXIMUM
    const pageEntries = plan.entries.slice(start, start + ZIP_LAYOUT_ENTRY_PAGE_MAXIMUM)
    const pageFields: Uint8Array[] = [
      frame(TEXT_ENCODER.encode(ZIP_LAYOUT_ENTRY_PAGE_DOMAIN)),
      uint64(BigInt(pageIndex)),
      uint64(BigInt(pageEntries.length)),
    ]
    for (const entry of pageEntries) pageFields.push(canonicalZipEntryBytes(entry))
    fields.push(frame(await sha256(concatenate(pageFields))))
  }
  fields.push(uint64(plan.centralDirectoryOffset))
  fields.push(uint64(plan.centralDirectoryBytes))
  fields.push(Uint8Array.of(plan.zip64EndRequired ? 1 : 0))
  fields.push(uint64(plan.zip64EndBytes))
  fields.push(uint64(plan.classicEndBytes))
  fields.push(uint64(plan.exactArchiveBytes))
  fields.push(uint64(plan.maximumSpoolBytes))
  return concatenate(fields)
}

function canonicalZipEntryBytes(entry: ZipEntryPlanV1): Uint8Array<ArrayBuffer> {
  const fields: Uint8Array[] = [
    uint64(BigInt(entry.version)),
    Uint8Array.of(entry.kind === 'directory' ? 1 : 2),
    uint64(BigInt(entry.path.length)),
  ]
  for (const segment of entry.path) fields.push(frame(TEXT_ENCODER.encode(segment)))
  fields.push(frame(Uint8Array.from(entry.nameBytes)))
  fields.push(uint64(entry.exactSize))
  fields.push(uint64(BigInt(entry.dosTime)))
  fields.push(uint64(BigInt(entry.dosDate)))
  fields.push(Uint8Array.of(entry.zip64Size ? 1 : 0, entry.zip64Offset ? 1 : 0))
  fields.push(uint64(BigInt(entry.versionNeeded)))
  fields.push(uint64(entry.localHeaderOffset))
  fields.push(uint64(entry.localExtraBytes))
  fields.push(uint64(entry.localHeaderBytes))
  fields.push(uint64(entry.descriptorBytes))
  fields.push(uint64(entry.entryStreamBytes))
  fields.push(uint64(BigInt(entry.centralZip64ValueCount)))
  fields.push(uint64(entry.centralExtraBytes))
  fields.push(uint64(entry.centralRecordBytes))
  return concatenate(fields)
}

function planNormalizedEntries(normalized: readonly NormalizedZipEntry[]): readonly ZipEntryPlanV1[] {
  if (normalized.length > MAX_ZIP_SPOOL_ENTRIES) {
    throw new RangeError('ZIP central-directory entry budget exceeded')
  }
  const entries: ZipEntryPlanV1[] = []
  let offset = 0n
  let centralBytes = 0n
  for (const normalizedEntry of normalized) {
    const previous = entries.at(-1)
    if (previous !== undefined && ZIP_ENCODING_POLICY_V1.compareEntries(previous, normalizedEntry) >= 0) {
      throw new TypeError('ZIP layout contains duplicate entries')
    }
    const entry = ZIP_ENCODING_POLICY_V1.planEntry(normalizedEntry, offset)
    offset = checkedZipAdd(offset, entry.entryStreamBytes)
    centralBytes = checkedZipAdd(centralBytes, entry.centralRecordBytes)
    ZIP_ENCODING_POLICY_V1.requireSpoolBudget(BigInt(entries.length + 1), centralBytes)
    entries.push(entry)
  }
  validatePlannedEntries(entries)
  return Object.freeze(entries)
}

function validatePlannedEntries(entries: readonly ZipEntryPlanV1[]): void {
  if (entries.length === 0) throw new TypeError('ZIP layout must contain its result-root directory')
  const directories = new Set<string>()
  let previous: ZipEntryPlanV1 | undefined
  for (const [index, entry] of entries.entries()) {
    if (previous !== undefined && ZIP_ENCODING_POLICY_V1.compareEntries(previous, entry) >= 0) {
      throw new TypeError('ZIP layout entries are not in canonical order')
    }
    requireNextTopology(entry, index, entries[0], directories)
    if (entry.kind === 'directory') directories.add(entry.artifactPath)
    previous = entry
  }
}

function requireNextTopology(
  entry: NormalizedZipEntry,
  index: number,
  root: NormalizedZipEntry | undefined,
  directories: ReadonlySet<string>,
): void {
  if (index === 0) {
    if (entry.kind !== 'directory' || entry.path.length !== 1) {
      throw new TypeError('ZIP layout must begin with its result-root directory')
    }
    return
  }
  if (root === undefined || entry.path[0] !== root.path[0]) {
    throw new TypeError('ZIP layout entry is outside the result root')
  }
  const parentPath = entry.path.slice(0, -1).join('/')
  if (!directories.has(parentPath)) {
    throw new TypeError('ZIP layout omitted a necessary parent directory')
  }
}

async function sealLayout(input: {
  readonly receiveIntentDigest: string
  readonly artifactDigest: string
  readonly evidence: ZipLayoutEvidence
  readonly entries: readonly ZipEntryPlanV1[]
}): Promise<SealedZipLayoutPlanV1> {
  const receiveIntentDigest = snapshotDigest(input.receiveIntentDigest, 'receive intent digest')
  const artifactDigest = snapshotDigest(input.artifactDigest, 'artifact digest')
  const evidence = snapshotEvidence(input.evidence)
  validatePlannedEntries(input.entries)
  const entries = Object.freeze([...input.entries])
  const summary = summarize(entries)
  const withoutDigest: Omit<SealedZipLayoutPlanV1, 'digest'> = Object.freeze({
    version: ZIP_LAYOUT_PLAN_VERSION,
    receiveIntentDigest,
    artifactDigest,
    evidence,
    encodingPolicy: ZIP_ENCODING_POLICY,
    encodingPolicyVersion: ZIP_ENCODING_POLICY_VERSION,
    entries,
    entryCount: summary.entryCount,
    centralDirectoryOffset: summary.centralDirectoryOffset,
    centralDirectoryBytes: summary.centralDirectoryBytes,
    zip64EndRequired: summary.zip64EndRequired,
    zip64EndBytes: summary.zip64EndBytes,
    classicEndBytes: ZIP_CLASSIC_END_BYTES,
    exactArchiveBytes: summary.exactArchiveBytes,
    maximumSpoolBytes: summary.centralDirectoryBytes,
  })
  const digest = encodeBase64Url(await sha256(
    await canonicalZipLayoutDigestPreimage(withoutDigest),
  ))
  return Object.freeze({ ...withoutDigest, digest })
}

function summarize(entries: readonly ZipEntryPlanV1[]): LayoutSummary {
  let streamBytes = 0n
  let centralBytes = 0n
  for (const entry of entries) {
    if (entry.localHeaderOffset !== streamBytes) {
      throw new TypeError('ZIP entry offset does not match preceding stream bytes')
    }
    streamBytes = checkedZipAdd(streamBytes, entry.entryStreamBytes)
    centralBytes = checkedZipAdd(centralBytes, entry.centralRecordBytes)
  }
  const entryCount = BigInt(entries.length)
  ZIP_ENCODING_POLICY_V1.requireSpoolBudget(entryCount, centralBytes)
  const zip64EndRequired = ZIP_ENCODING_POLICY_V1.requiresZip64End({
    entryCount,
    centralDirectoryOffset: streamBytes,
    centralDirectoryBytes: centralBytes,
  })
  const zip64EndBytes = zip64EndRequired ? ZIP64_END_BYTES : 0n
  return Object.freeze({
    entryCount,
    centralDirectoryOffset: streamBytes,
    centralDirectoryBytes: centralBytes,
    zip64EndRequired,
    zip64EndBytes,
    exactArchiveBytes: checkedZipAdd(
      streamBytes,
      centralBytes,
      zip64EndBytes,
      ZIP_CLASSIC_END_BYTES,
    ),
  })
}

function snapshotEvidence(evidence: ZipLayoutEvidence): ZipLayoutEvidence {
  if (evidence === null || typeof evidence !== 'object') {
    throw new TypeError('ZIP layout evidence is invalid')
  }
  if (evidence.kind === 'prepared') {
    return Object.freeze({
      kind: 'prepared',
      preparationManifestDigest: snapshotDigest(
        evidence.preparationManifestDigest,
        'preparation manifest digest',
      ),
    })
  }
  if (evidence.kind === 'progressive') {
    return Object.freeze({
      kind: 'progressive',
      discoveryLedgerDigest: snapshotDigest(
        evidence.discoveryLedgerDigest,
        'discovery ledger digest',
      ),
    })
  }
  throw new TypeError('ZIP layout evidence kind is invalid')
}

function snapshotDigest(value: string, label: string): string {
  digestBytes(value, label)
  return value
}

function digestBytes(value: string, label: string): Uint8Array<ArrayBuffer> {
  const decoded = typeof value === 'string' ? decodeBase64Url(value) : undefined
  if (decoded === undefined || decoded.byteLength !== ZIP_LAYOUT_DIGEST_BYTES ||
      decoded.every((byte) => byte === 0) || encodeBase64Url(decoded) !== value) {
    throw new TypeError(`${label} is invalid`)
  }
  return decoded
}

function frame(bytes: Uint8Array): Uint8Array<ArrayBuffer> {
  return concatenate([uint64(BigInt(bytes.byteLength)), bytes])
}

function uint64(value: bigint): Uint8Array<ArrayBuffer> {
  const output = new Uint8Array(8)
  new DataView(output.buffer).setBigUint64(0, value, false)
  return output
}

function concatenate(parts: readonly Uint8Array[]): Uint8Array<ArrayBuffer> {
  let total = 0
  for (const part of parts) {
    if (total > Number.MAX_SAFE_INTEGER - part.byteLength) {
      throw new RangeError('ZIP layout canonical bytes exceed the allocation bound')
    }
    total += part.byteLength
  }
  const output = new Uint8Array(total)
  let offset = 0
  for (const part of parts) {
    output.set(part, offset)
    offset += part.byteLength
  }
  return output
}

import { canonicalDigest, canonicalFrame, canonicalIdentity, canonicalRecord, canonicalU8, canonicalU64 } from '../workspace/canonical'
import {
  canonicalMaterializationLedgerRootFact,
  checkedLedgerAdd,
  compareMaterializationLedgerEntryCursors,
  materializationLedgerEntryCursor,
  snapshotMaterializationLedgerEntryCursor,
} from './codec'
import {
  createMaterializationLedgerPageSummaryRecord,
  createMaterializationLedgerSealRecord,
  decodeMaterializationLedgerPageSummaryV1,
  decodeMaterializationLedgerSealV1,
  deriveMaterializationLedgerSealId,
} from './evidence'
import { decodeMaterializationLedgerEntryV1 } from './journal'
import {
  MATERIALIZATION_LEDGER_MAX_ENTRY_EVENTS,
  MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
  MaterializationLedgerDirectoryOutcome,
  MaterializationLedgerEntryKind,
  MaterializationLedgerSealPurpose,
  type MaterializationDirectoryAdmittedEntryV1,
  type MaterializationDirectoryFinalizedEntryV1,
  type MaterializationLedgerBindingV1,
  type MaterializationLedgerCounts,
  type MaterializationLedgerEntryCursor,
  type MaterializationLedgerEntryPage,
  type MaterializationLedgerEntryV1,
  type MaterializationLedgerPageRequest,
  type MaterializationLedgerPageSummaryV1,
  type MaterializationLedgerRootFact,
  type MaterializationLedgerSealV1,
  type MaterializationLedgerValidatedPage,
} from './model'

const ORDERED_PAGE_ENTRIES_DOMAIN = 'windshare/materialization-ledger/v1/page-entries'
const MERKLE_LEAF_DOMAIN = 'windshare/materialization-ledger/v1/merkle-leaf'
const MERKLE_NODE_DOMAIN = 'windshare/materialization-ledger/v1/merkle-node'
const MERKLE_PEAKS_DOMAIN = 'windshare/materialization-ledger/v1/merkle-peaks'
const AGGREGATE_ROOT_DOMAIN = 'windshare/materialization-ledger/v1/aggregate-root'
const RESUMABLE_PURPOSE_DISCRIMINANT = 1
const TERMINAL_PURPOSE_DISCRIMINANT = 2

export interface MaterializationLedgerPageBuildResult {
  readonly summary: MaterializationLedgerPageSummaryV1
  readonly continuation?: MaterializationLedgerEntryCursor
  readonly directoryCarry?: MaterializationDirectoryAdmittedEntryV1
}

export async function validateMaterializationLedgerEntryPage(
  pageInput: MaterializationLedgerEntryPage,
  requestInput: MaterializationLedgerPageRequest,
  binding: MaterializationLedgerBindingV1,
  directoryCarryInput?: MaterializationDirectoryAdmittedEntryV1,
): Promise<MaterializationLedgerValidatedPage> {
  requireExactPageKeys(pageInput)
  const request = snapshotPageRequest(requestInput)
  const entries = await Promise.all(pageInput.entries.map(entry =>
    decodeMaterializationLedgerEntryV1(entry, binding)))
  if (entries.length > MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT) {
    throw new TypeError('materialization ledger page exceeds its fixed entry bound')
  }
  const continuation = pageInput.continuation === undefined
    ? undefined
    : snapshotMaterializationLedgerEntryCursor(pageInput.continuation)
  if (entries.length === 0 && continuation !== undefined) {
    throw new TypeError('empty materialization ledger page cannot continue')
  }
  if (entries.length < MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT &&
      continuation !== undefined) {
    throw new TypeError('short materialization ledger page cannot continue')
  }
  const carry = directoryCarryInput === undefined
    ? undefined
    : await requireDirectoryAdmission(directoryCarryInput, binding)
  validatePageOrder(entries, request.after)
  validateContinuation(entries, continuation)
  validatePathGroups(entries, carry, request.after)
  const directoryCarry = trailingDirectoryCarry(entries)
  return Object.freeze({
    entries: Object.freeze(entries),
    ...(continuation === undefined ? {} : { continuation }),
    ...(directoryCarry === undefined ? {} : { directoryCarry }),
  })
}

export async function createMaterializationLedgerPageSummary(input: {
  readonly binding: MaterializationLedgerBindingV1
  readonly sealId: string
  readonly pageOrdinal: bigint
  readonly page: MaterializationLedgerEntryPage
  readonly request: MaterializationLedgerPageRequest
  readonly directoryCarry?: MaterializationDirectoryAdmittedEntryV1
}): Promise<MaterializationLedgerPageBuildResult> {
  const validated = await validateMaterializationLedgerEntryPage(
    input.page,
    input.request,
    input.binding,
    input.directoryCarry,
  )
  if (validated.entries.length === 0) {
    throw new TypeError('empty entry scans do not create ledger page summaries')
  }
  const counts = summarizeEntries(validated.entries)
  const firstEntry = materializationLedgerEntryCursor(validated.entries[0]!)
  const lastEntry = materializationLedgerEntryCursor(validated.entries.at(-1)!)
  const summary = await createMaterializationLedgerPageSummaryRecord({
    binding: input.binding,
    sealId: input.sealId,
    pageOrdinal: input.pageOrdinal,
    firstEntry,
    lastEntry,
    ...counts,
    canonicalEntryBytes: canonicalEntryBytes(validated.entries),
    orderedEntriesDigest: await orderedEntriesDigest(validated.entries),
    rootPathFact: pageRootFact(validated.entries, input.directoryCarry),
  })
  return Object.freeze({
    summary,
    ...(validated.continuation === undefined
      ? {}
      : { continuation: validated.continuation }),
    ...(validated.directoryCarry === undefined
      ? {}
      : { directoryCarry: validated.directoryCarry }),
  })
}

/**
 * Only one digest per occupied tree level is retained, so sealing cost grows
 * with page count while memory remains logarithmic.
 */
export class OrderedSha256MerkleAccumulator {
  readonly #peaks: Array<string | undefined> = []
  #leafCount = 0n

  get leafCount(): bigint { return this.#leafCount }
  get peakCount(): number { return this.#peaks.filter(value => value !== undefined).length }

  async append(pageDigestInput: string): Promise<void> {
    const pageDigest = snapshotDigest(pageDigestInput, 'page digest')
    let digest = await canonicalDigest(canonicalRecord(MERKLE_LEAF_DOMAIN, 1, [
      canonicalFrame(canonicalIdentity(pageDigest, 32, 'page digest')),
    ]))
    let level = 0
    while (this.#peaks[level] !== undefined) {
      digest = await merkleNode(level, this.#peaks[level]!, digest)
      this.#peaks[level] = undefined
      level += 1
    }
    this.#peaks[level] = digest
    this.#leafCount = checkedLedgerAdd(this.#leafCount, 1n, 'Merkle leaf count')
  }

  async finishRoot(): Promise<string> {
    const fields = [canonicalFrame(canonicalU64(this.#leafCount))]
    for (let level = this.#peaks.length - 1; level >= 0; level -= 1) {
      const digest = this.#peaks[level]
      if (digest === undefined) continue
      fields.push(canonicalFrame(canonicalRecord(
        MERKLE_PEAKS_DOMAIN,
        1,
        [
          canonicalFrame(canonicalU64(BigInt(level))),
          canonicalFrame(canonicalIdentity(digest, 32, 'Merkle peak digest')),
        ],
      )))
    }
    return canonicalDigest(canonicalRecord(MERKLE_PEAKS_DOMAIN, 1, fields))
  }
}

export async function sealMaterializationLedgerPages(input: {
  readonly binding: MaterializationLedgerBindingV1
  readonly sealSequence: bigint
  readonly purpose: MaterializationLedgerSealPurpose
  readonly candidateCheckpointCount: bigint
  readonly pages:
    | Iterable<MaterializationLedgerPageSummaryV1>
    | AsyncIterable<MaterializationLedgerPageSummaryV1>
}): Promise<MaterializationLedgerSealV1> {
  const sealId = await deriveMaterializationLedgerSealId(input.binding, input.sealSequence)
  const accumulator = new OrderedSha256MerkleAccumulator()
  let counts = emptyCounts()
  let pageCount = 0n
  let lastEntry: MaterializationLedgerEntryCursor | undefined
  let priorEntryCount: bigint | undefined
  let rootPathFact: MaterializationLedgerRootFact = Object.freeze({ kind: 'absent' })

  for await (const candidate of input.pages) {
    const page = await decodeMaterializationLedgerPageSummaryV1(candidate, input.binding)
    if (page.sealId !== sealId || page.pageOrdinal !== pageCount) {
      throw new TypeError('materialization ledger page belongs to another seal or ordinal')
    }
    if (priorEntryCount !== undefined &&
        priorEntryCount !== BigInt(MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT)) {
      throw new TypeError('only the last materialization ledger page may be short')
    }
    if (lastEntry !== undefined &&
        compareMaterializationLedgerEntryCursors(lastEntry, page.firstEntry) >= 0) {
      throw new TypeError('materialization ledger page cursor ranges overlap')
    }
    counts = addCounts(counts, page)
    if (counts.entryEventCount > BigInt(MATERIALIZATION_LEDGER_MAX_ENTRY_EVENTS)) {
      throw new TypeError('materialization ledger seal exceeds the entry-event bound')
    }
    rootPathFact = mergeMaterializationLedgerRootFacts(rootPathFact, page.rootPathFact)
    await accumulator.append(page.pageDigest)
    pageCount = checkedLedgerAdd(pageCount, 1n, 'ledger page count')
    priorEntryCount = page.entryEventCount
    lastEntry = page.lastEntry
  }

  const entryPageRoot = await accumulator.finishRoot()
  const aggregateRoot = await materializationLedgerAggregateRoot({
    binding: input.binding,
    sealId,
    purpose: input.purpose,
    candidateCheckpointCount: input.candidateCheckpointCount,
    pageCount,
    counts,
    entryPageRoot,
    rootPathFact,
  })
  return createMaterializationLedgerSealRecord({
    binding: input.binding,
    sealSequence: input.sealSequence,
    purpose: input.purpose,
    pageCount,
    candidateCheckpointCount: input.candidateCheckpointCount,
    ...(lastEntry === undefined ? {} : { lastEntry }),
    entryPageRoot,
    aggregateRoot,
    ...counts,
    rootPathFact,
  })
}

export async function validateMaterializationLedgerSealPages(input: {
  readonly binding: MaterializationLedgerBindingV1
  readonly seal: MaterializationLedgerSealV1
  readonly pages:
    | Iterable<MaterializationLedgerPageSummaryV1>
    | AsyncIterable<MaterializationLedgerPageSummaryV1>
}): Promise<MaterializationLedgerSealV1> {
  const seal = await decodeMaterializationLedgerSealV1(input.seal, input.binding)
  const rebuilt = await sealMaterializationLedgerPages({
    binding: input.binding,
    sealSequence: seal.sealSequence,
    purpose: seal.purpose,
    candidateCheckpointCount: seal.candidateCheckpointCount,
    pages: input.pages,
  })
  if (rebuilt.sealId !== seal.sealId || rebuilt.sealDigest !== seal.sealDigest ||
      rebuilt.aggregateRoot !== seal.aggregateRoot) {
    throw new TypeError('materialization ledger seal disagrees with its page summaries')
  }
  return seal
}

export function mergeMaterializationLedgerRootFacts(
  left: MaterializationLedgerRootFact,
  right: MaterializationLedgerRootFact,
): MaterializationLedgerRootFact {
  if (left.kind === 'absent') return right
  if (right.kind === 'absent') return left
  if (left.kind === MaterializationLedgerEntryKind.FileFinalized ||
      right.kind === MaterializationLedgerEntryKind.FileFinalized) {
    throw new TypeError('materialization ledger has conflicting root path claims')
  }
  if (!sameDirectoryRoot(left, right)) {
    throw new TypeError('materialization ledger root directory coordinates changed')
  }
  if (left.finalization.kind === 'missing' && right.finalization.kind !== 'missing') {
    return right
  }
  if (right.finalization.kind === 'missing' && left.finalization.kind !== 'missing') {
    return left
  }
  throw new TypeError('materialization ledger duplicates a root path event')
}

function validatePageOrder(
  entries: readonly MaterializationLedgerEntryV1[],
  after: MaterializationLedgerEntryCursor | undefined,
): void {
  let previous = after
  for (const entry of entries) {
    const cursor = materializationLedgerEntryCursor(entry)
    if (previous !== undefined &&
        compareMaterializationLedgerEntryCursors(previous, cursor) >= 0) {
      throw new TypeError('materialization ledger page is not in strict cursor order')
    }
    previous = cursor
  }
}

function validateContinuation(
  entries: readonly MaterializationLedgerEntryV1[],
  continuation: MaterializationLedgerEntryCursor | undefined,
): void {
  if (continuation === undefined) return
  const last = entries.at(-1)
  if (last === undefined ||
      compareMaterializationLedgerEntryCursors(
        materializationLedgerEntryCursor(last),
        continuation,
      ) !== 0) {
    throw new TypeError('materialization ledger continuation is not the exact page tail')
  }
}

function validatePathGroups(
  entries: readonly MaterializationLedgerEntryV1[],
  carry: MaterializationDirectoryAdmittedEntryV1 | undefined,
  after: MaterializationLedgerEntryCursor | undefined,
): void {
  if (carry !== undefined && (
    after === undefined ||
    compareMaterializationLedgerEntryCursors(
      materializationLedgerEntryCursor(carry),
      after,
    ) !== 0
  )) {
    throw new TypeError('directory carry does not match the prior page continuation')
  }
  for (let index = 0; index < entries.length; index += 1) {
    const current = entries[index]!
    const previous = entries[index - 1]
    if (previous !== undefined && previous.pathKey === current.pathKey) {
      if (!samePath(previous, current)) {
        throw new TypeError('materialization ledger path-key collision changed canonical path bytes')
      }
      requireDirectoryPair(previous, current)
      continue
    }
    if (current.kind !== MaterializationLedgerEntryKind.DirectoryFinalized) continue
    if (index !== 0 || carry === undefined || carry.pathKey !== current.pathKey ||
        !samePath(carry, current)) {
      throw new TypeError('directory finalization lacks its adjacent stable admission')
    }
    requireDirectoryPair(carry, current)
  }
}

function requireDirectoryPair(
  admission: MaterializationLedgerEntryV1,
  finalization: MaterializationLedgerEntryV1,
): void {
  if (admission.kind !== MaterializationLedgerEntryKind.DirectoryAdmitted ||
      finalization.kind !== MaterializationLedgerEntryKind.DirectoryFinalized ||
      finalization.admissionEntryId !== admission.entryId ||
      finalization.admissionEntryDigest !== admission.entryDigest ||
      !sameStableDirectory(admission, finalization)) {
    throw new TypeError('directory finalization changed its stable admission coordinates')
  }
}

function trailingDirectoryCarry(
  entries: readonly MaterializationLedgerEntryV1[],
): MaterializationDirectoryAdmittedEntryV1 | undefined {
  const tail = entries.at(-1)
  return tail?.kind === MaterializationLedgerEntryKind.DirectoryAdmitted ? tail : undefined
}

async function requireDirectoryAdmission(
  input: MaterializationDirectoryAdmittedEntryV1,
  binding: MaterializationLedgerBindingV1,
): Promise<MaterializationDirectoryAdmittedEntryV1> {
  const decoded = await decodeMaterializationLedgerEntryV1(input, binding)
  if (decoded.kind !== MaterializationLedgerEntryKind.DirectoryAdmitted) {
    throw new TypeError('ledger page carry must be a directory admission')
  }
  return decoded
}

function summarizeEntries(
  entries: readonly MaterializationLedgerEntryV1[],
): MaterializationLedgerCounts {
  let counts = emptyCounts()
  for (const entry of entries) {
    counts = addEntryCounts(counts, entry)
  }
  return Object.freeze(counts)
}

function addEntryCounts(
  counts: MaterializationLedgerCounts,
  entry: MaterializationLedgerEntryV1,
): MaterializationLedgerCounts {
  const withEvent = {
    ...counts,
    entryEventCount: checkedLedgerAdd(counts.entryEventCount, 1n, 'entry event count'),
  }
  switch (entry.kind) {
    case MaterializationLedgerEntryKind.FileFinalized:
      return {
        ...withEvent,
        fileCount: checkedLedgerAdd(counts.fileCount, 1n, 'file count'),
        fileBytes: checkedLedgerAdd(counts.fileBytes, entry.exactSize, 'file bytes'),
      }
    case MaterializationLedgerEntryKind.DirectoryAdmitted:
      return {
        ...withEvent,
        materializedDirectoryCount: checkedLedgerAdd(
          counts.materializedDirectoryCount,
          1n,
          'materialized directory count',
        ),
        visibleDirectoryCount: checkedLedgerAdd(
          counts.visibleDirectoryCount,
          entry.relativePath.length > 0 ? 1n : 0n,
          'visible directory count',
        ),
      }
    case MaterializationLedgerEntryKind.DirectoryFinalized:
      return entry.outcome.kind === MaterializationLedgerDirectoryOutcome.Finalized
        ? {
            ...withEvent,
            finalizedDirectoryCount: checkedLedgerAdd(
              counts.finalizedDirectoryCount,
              1n,
              'finalized directory count',
            ),
          }
        : {
            ...withEvent,
            isolatedDirectoryCount: checkedLedgerAdd(
              counts.isolatedDirectoryCount,
              1n,
              'isolated directory count',
            ),
          }
  }
}

function addCounts(
  left: MaterializationLedgerCounts,
  right: MaterializationLedgerCounts,
): MaterializationLedgerCounts {
  return Object.freeze({
    entryEventCount: checkedLedgerAdd(
      left.entryEventCount,
      right.entryEventCount,
      'entry event count',
    ),
    fileCount: checkedLedgerAdd(left.fileCount, right.fileCount, 'file count'),
    fileBytes: checkedLedgerAdd(left.fileBytes, right.fileBytes, 'file bytes'),
    materializedDirectoryCount: checkedLedgerAdd(
      left.materializedDirectoryCount,
      right.materializedDirectoryCount,
      'materialized directory count',
    ),
    visibleDirectoryCount: checkedLedgerAdd(
      left.visibleDirectoryCount,
      right.visibleDirectoryCount,
      'visible directory count',
    ),
    finalizedDirectoryCount: checkedLedgerAdd(
      left.finalizedDirectoryCount,
      right.finalizedDirectoryCount,
      'finalized directory count',
    ),
    isolatedDirectoryCount: checkedLedgerAdd(
      left.isolatedDirectoryCount,
      right.isolatedDirectoryCount,
      'isolated directory count',
    ),
  })
}

function emptyCounts(): MaterializationLedgerCounts {
  return Object.freeze({
    entryEventCount: 0n,
    fileCount: 0n,
    fileBytes: 0n,
    materializedDirectoryCount: 0n,
    visibleDirectoryCount: 0n,
    finalizedDirectoryCount: 0n,
    isolatedDirectoryCount: 0n,
  })
}

function canonicalEntryBytes(entries: readonly MaterializationLedgerEntryV1[]): bigint {
  return entries.reduce(
    (total, entry) => checkedLedgerAdd(
      total,
      BigInt(entry.canonicalBytes.byteLength),
      'canonical entry bytes',
    ),
    0n,
  )
}

async function orderedEntriesDigest(
  entries: readonly MaterializationLedgerEntryV1[],
): Promise<string> {
  return canonicalDigest(canonicalRecord(ORDERED_PAGE_ENTRIES_DOMAIN, 1, [
    canonicalFrame(canonicalU64(BigInt(entries.length))),
    ...entries.map(entry => canonicalFrame(canonicalRecord(
      ORDERED_PAGE_ENTRIES_DOMAIN,
      1,
      [
        canonicalFrame(canonicalIdentity(entry.entryId, 32, 'entry ID')),
        canonicalFrame(canonicalIdentity(entry.entryDigest, 32, 'entry digest')),
      ],
    ))),
  ]))
}

function pageRootFact(
  entries: readonly MaterializationLedgerEntryV1[],
  carry: MaterializationDirectoryAdmittedEntryV1 | undefined,
): MaterializationLedgerRootFact {
  let fact: MaterializationLedgerRootFact = Object.freeze({ kind: 'absent' })
  let prior = carry
  for (const entry of entries) {
    if (entry.relativePath.length !== 0) {
      prior = entry.kind === MaterializationLedgerEntryKind.DirectoryAdmitted ? entry : undefined
      continue
    }
    let next: MaterializationLedgerRootFact
    switch (entry.kind) {
      case MaterializationLedgerEntryKind.FileFinalized:
        next = Object.freeze({
          kind: MaterializationLedgerEntryKind.FileFinalized,
          relativePath: entry.relativePath,
          pathKey: entry.pathKey,
          entryId: entry.entryId,
          fileId: entry.fileId,
          fileRevision: entry.fileRevision,
        })
        break
      case MaterializationLedgerEntryKind.DirectoryAdmitted:
        next = directoryRootFact(entry, undefined)
        prior = entry
        break
      case MaterializationLedgerEntryKind.DirectoryFinalized:
        if (prior === undefined) {
          throw new TypeError('root directory finalization lacks its page carry')
        }
        next = directoryRootFact(prior, entry)
        prior = undefined
        break
    }
    fact = mergeMaterializationLedgerRootFacts(fact, next)
  }
  return fact
}

function directoryRootFact(
  admission: MaterializationDirectoryAdmittedEntryV1,
  finalization: MaterializationDirectoryFinalizedEntryV1 | undefined,
): MaterializationLedgerRootFact {
  return Object.freeze({
    kind: 'directory',
    relativePath: admission.relativePath,
    pathKey: admission.pathKey,
    admissionEntryId: admission.entryId,
    admissionEntryDigest: admission.entryDigest,
    directoryId: admission.directoryId,
    generation: admission.generation,
    ownedObjectId: admission.ownedObjectId,
    finalization: finalization === undefined
      ? Object.freeze({ kind: 'missing' })
      : Object.freeze({
          kind: finalization.outcome.kind,
          entryId: finalization.entryId,
          entryDigest: finalization.entryDigest,
        }),
  })
}

function sameDirectoryRoot(
  left: Extract<MaterializationLedgerRootFact, { kind: 'directory' }>,
  right: Extract<MaterializationLedgerRootFact, { kind: 'directory' }>,
): boolean {
  return left.pathKey === right.pathKey &&
    left.admissionEntryId === right.admissionEntryId &&
    left.admissionEntryDigest === right.admissionEntryDigest &&
    left.directoryId === right.directoryId &&
    left.generation === right.generation &&
    left.ownedObjectId === right.ownedObjectId
}

function sameStableDirectory(
  left: MaterializationDirectoryAdmittedEntryV1,
  right: MaterializationDirectoryFinalizedEntryV1,
): boolean {
  return left.directoryId === right.directoryId &&
    left.generation === right.generation &&
    left.ownedObjectId === right.ownedObjectId &&
    samePath(left, right) &&
    sameOptionalParent(left.parent, right.parent) &&
    sameModifiedTime(left.modifiedTime, right.modifiedTime)
}

function samePath(
  left: Pick<MaterializationLedgerEntryV1, 'relativePath'>,
  right: Pick<MaterializationLedgerEntryV1, 'relativePath'>,
): boolean {
  return left.relativePath.length === right.relativePath.length &&
    left.relativePath.every((segment, index) => segment === right.relativePath[index])
}

function sameOptionalParent(
  left: MaterializationDirectoryAdmittedEntryV1['parent'],
  right: MaterializationDirectoryFinalizedEntryV1['parent'],
): boolean {
  if (left === undefined || right === undefined) return left === right
  return left.directoryId === right.directoryId &&
    left.generation === right.generation &&
    left.ownedObjectId === right.ownedObjectId &&
    left.relativePath.length === right.relativePath.length &&
    left.relativePath.every((segment, index) => segment === right.relativePath[index])
}

function sameModifiedTime(
  left: MaterializationDirectoryAdmittedEntryV1['modifiedTime'],
  right: MaterializationDirectoryFinalizedEntryV1['modifiedTime'],
): boolean {
  if (left === undefined || right === undefined) return left === right
  return left.seconds === right.seconds &&
    left.nanoseconds === right.nanoseconds &&
    left.precision === right.precision
}

async function merkleNode(level: number, left: string, right: string): Promise<string> {
  return canonicalDigest(canonicalRecord(MERKLE_NODE_DOMAIN, 1, [
    canonicalFrame(canonicalU64(BigInt(level))),
    canonicalFrame(canonicalIdentity(left, 32, 'left Merkle digest')),
    canonicalFrame(canonicalIdentity(right, 32, 'right Merkle digest')),
  ]))
}

function snapshotDigest(input: string, label: string): string {
  return encodeCanonicalIdentity(input, label)
}

function encodeCanonicalIdentity(input: string, label: string): string {
  const bytes = canonicalIdentity(input, 32, label)
  return btoa(String.fromCharCode(...bytes))
    .replaceAll('+', '-')
    .replaceAll('/', '_')
    .replaceAll('=', '')
}

async function materializationLedgerAggregateRoot(input: {
  readonly binding: MaterializationLedgerBindingV1
  readonly sealId: string
  readonly purpose: MaterializationLedgerSealPurpose
  readonly candidateCheckpointCount: bigint
  readonly pageCount: bigint
  readonly counts: MaterializationLedgerCounts
  readonly entryPageRoot: string
  readonly rootPathFact: MaterializationLedgerRootFact
}): Promise<string> {
  return canonicalDigest(canonicalRecord(AGGREGATE_ROOT_DOMAIN, 1, [
    canonicalFrame(canonicalIdentity(
      input.binding.ledgerBindingDigest,
      32,
      'ledger binding digest',
    )),
    canonicalFrame(canonicalIdentity(input.sealId, 32, 'seal ID')),
    canonicalU8(input.purpose === MaterializationLedgerSealPurpose.ResumableSnapshot
      ? RESUMABLE_PURPOSE_DISCRIMINANT
      : TERMINAL_PURPOSE_DISCRIMINANT),
    canonicalFrame(canonicalU64(input.candidateCheckpointCount)),
    canonicalFrame(canonicalU64(input.pageCount)),
    canonicalFrame(canonicalU64(input.counts.entryEventCount)),
    canonicalFrame(canonicalU64(input.counts.fileCount)),
    canonicalFrame(canonicalU64(input.counts.fileBytes)),
    canonicalFrame(canonicalU64(input.counts.materializedDirectoryCount)),
    canonicalFrame(canonicalU64(input.counts.visibleDirectoryCount)),
    canonicalFrame(canonicalU64(input.counts.finalizedDirectoryCount)),
    canonicalFrame(canonicalU64(input.counts.isolatedDirectoryCount)),
    canonicalFrame(canonicalIdentity(input.entryPageRoot, 32, 'entry page root')),
    canonicalFrame(canonicalMaterializationLedgerRootFact(input.rootPathFact)),
  ]))
}

function snapshotPageRequest(
  input: MaterializationLedgerPageRequest,
): MaterializationLedgerPageRequest {
  const expected = ['limit', ...(input.after === undefined ? [] : ['after'])]
  requireExactKeys(input, expected, 'materialization ledger page request')
  if (input.limit !== MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT) {
    throw new TypeError('materialization ledger scans use the fixed 128-entry limit')
  }
  const after = input.after === undefined
    ? undefined
    : snapshotMaterializationLedgerEntryCursor(input.after)
  return Object.freeze({
    limit: MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
    ...(after === undefined ? {} : { after }),
  })
}

function requireExactPageKeys(input: MaterializationLedgerEntryPage): void {
  requireExactKeys(
    input,
    ['entries', ...(input.continuation === undefined ? [] : ['continuation'])],
    'materialization ledger entry page',
  )
  if (!Array.isArray(input.entries)) {
    throw new TypeError('materialization ledger page entries must be an array')
  }
}

function requireExactKeys(value: object, expected: readonly string[], label: string): void {
  const actual = Object.keys(value).sort()
  const wanted = [...expected].sort()
  if (actual.length !== wanted.length ||
      actual.some((key, index) => key !== wanted[index])) {
    throw new TypeError(`${label} fields are not exact`)
  }
}

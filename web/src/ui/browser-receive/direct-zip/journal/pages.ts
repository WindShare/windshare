import { encodeBase64Url } from '../../../../crypto/bytes'
import { directZipEpochGenesisRoot } from '../../../../output/direct-zip/format'
import {
  createDirectZipImmutablePageV1,
  type DirectZipCheckpointV1, type DirectZipImmutablePageV1,
  type DirectZipJournalBudgetUsageV1, type DirectZipJournalFenceV1,
  type DirectZipJournalRepository, type DirectZipPageChainV1, type DirectZipPageKind,
} from '../../../../output/direct-zip/journal'
import type {
  DirectZipEpochProofV1, DirectZipMemberAdmissionV1,
  DirectZipWriterCheckpointV1, DirectZipWriterPageSink, DirectZipWriterPageStateV1,
} from '../../../../output/direct-zip/writer'
import { equalCanonicalBytes } from '../../../../output/workspace/canonical'
import { chainForKind, chainFromPage, ZERO_DIGEST } from './bootstrap'
import { decodeCentral, decodeEpoch, encodeCentral, encodeEpoch, encodeLayout, digestBytes } from './records'

const MAXIMUM_RETAINED_PAGE_STATES = 6

export interface BrowserDirectZipPageAuthority {
  readonly layoutPages: DirectZipPageChainV1
  readonly centralPages: DirectZipPageChainV1
  readonly epochPages: DirectZipPageChainV1
  readonly journalUsage: DirectZipJournalBudgetUsageV1
  readonly accountingTailPageId?: string
  readonly centralBytes: bigint
}

export class BrowserDirectZipPages implements DirectZipWriterPageSink {
  readonly #repository: DirectZipJournalRepository
  readonly #fence: () => DirectZipJournalFenceV1
  readonly #states = new Map<string, BrowserDirectZipPageAuthority>()
  #current: BrowserDirectZipPageAuthority
  #committed: BrowserDirectZipPageAuthority
  #committedRollback: BrowserDirectZipPageAuthority | undefined
  #rollback: BrowserDirectZipPageAuthority | undefined
  #tail: DirectZipImmutablePageV1 | undefined

  private constructor(
    repository: DirectZipJournalRepository,
    fence: () => DirectZipJournalFenceV1,
    authority: BrowserDirectZipPageAuthority,
  ) {
    this.#repository = repository
    this.#fence = fence
    this.#current = authority
    this.#committed = authority
    this.#remember(authority)
  }

  static async open(
    repository: DirectZipJournalRepository,
    fence: () => DirectZipJournalFenceV1,
    checkpoint: DirectZipCheckpointV1,
  ): Promise<BrowserDirectZipPages> {
    const authority = await readPageAuthority(repository, checkpoint.operationId, checkpoint)
    const pages = new BrowserDirectZipPages(repository, fence, authority)
    if (checkpoint.currentMember !== undefined) {
      pages.#rollback = await readPageAuthority(repository, checkpoint.operationId, checkpoint.currentMember.rollback)
      pages.#committedRollback = pages.#rollback
      pages.#remember(pages.#rollback)
    }
    return pages
  }

  async retainCandidateAuthority(
    checkpoint: import('../../../../output/direct-zip/journal').DirectZipCheckpointProposalV1,
  ): Promise<BrowserDirectZipPageAuthority> {
    const authority = await readPageAuthority(this.#repository, checkpoint.operationId, checkpoint)
    this.#remember(authority)
    if (checkpoint.currentMember !== undefined) {
      this.#rollback = await readPageAuthority(this.#repository, checkpoint.operationId, checkpoint.currentMember.rollback)
      this.#remember(this.#rollback)
    }
    return authority
  }

  get authority(): BrowserDirectZipPageAuthority { return this.#current }
  get committedAuthority(): BrowserDirectZipPageAuthority { return this.#committed }

  authorityFor(state: DirectZipWriterPageStateV1): BrowserDirectZipPageAuthority {
    const found = this.#states.get(pageStateKey(state))
    if (found === undefined || found.centralBytes !== state.centralBytes ||
        found.layoutPages.recordCount !== state.layoutRecordCount ||
        found.centralPages.recordCount !== state.centralRecordCount) {
      throw new TypeError('Direct ZIP writer page roots have no retained journal authority')
    }
    return found
  }

  rollbackAuthority(state: DirectZipWriterPageStateV1): BrowserDirectZipPageAuthority {
    for (const authority of [this.#rollback, this.#committedRollback]) {
      if (authority !== undefined && pageStateKey(pageState(authority)) === pageStateKey(state)) {
        return authority
      }
    }
    return this.authorityFor(state)
  }

  async stageLayout(admission: DirectZipMemberAdmissionV1): Promise<void> {
    if (admission.plan.ordinal !== this.#current.layoutPages.recordCount) {
      throw new TypeError('Direct ZIP layout page skipped an ordinal')
    }
    if (admission.plan.zipEntry.kind === 'file') this.#rollback = this.#current
    await this.#stage('layout', encodeLayout(admission))
  }

  async stageCentral(input: Readonly<{ ordinal: bigint; bytes: Uint8Array }>): Promise<void> {
    if (input.ordinal !== this.#current.centralPages.recordCount) {
      throw new TypeError('Direct ZIP central page skipped an ordinal')
    }
    await this.#stage('central', encodeCentral(input.ordinal, input.bytes), BigInt(input.bytes.byteLength))
  }

  async stageEpoch(proof: DirectZipEpochProofV1): Promise<void> {
    await this.#stage('epoch', encodeEpoch(proof))
  }

  snapshot(): Promise<DirectZipWriterPageStateV1> {
    this.#remember(this.#current)
    return Promise.resolve(pageState(this.#current))
  }

  async restore(state: DirectZipWriterPageStateV1): Promise<void> {
    const restoresCommitted = pageStateKey(pageState(this.#committed)) === pageStateKey(state)
    this.#current = restoresCommitted
      ? this.#committed : this.authorityFor(state)
    if (restoresCommitted) this.#rollback = this.#committedRollback
    this.#tail = undefined
    // Retirement removes candidate reachability first. Only then may its uncommitted
    // suffix be discarded so the next attempt can reuse the immutable chain ordinal.
    await this.#repository.collectOrphanPages(this.#fence())
  }

  commit(authority: BrowserDirectZipPageAuthority, rollbackState: DirectZipWriterPageStateV1 | undefined): void {
    const rollback = rollbackState === undefined ? undefined : this.rollbackAuthority(rollbackState)
    this.#current = authority
    this.#committed = authority
    this.#committedRollback = rollback
    this.#rollback = rollback
    this.#tail = undefined
    this.#states.clear()
    this.#remember(authority)
    if (this.#rollback !== undefined) this.#remember(this.#rollback)
  }

  async *replayCentral(state: DirectZipWriterPageStateV1) {
    const authority = this.authorityFor(state)
    let ordinal = 0n
    for await (const bytes of streamEntries(this.#repository, this.#fence().operationId, 'central', authority.centralPages)) {
      const record = decodeCentral(bytes)
      if (record.ordinal !== ordinal++) throw new TypeError('Direct ZIP central records changed order')
      yield record
    }
  }

  async *committedEpochProofs(checkpoint: DirectZipWriterCheckpointV1): AsyncIterable<DirectZipEpochProofV1> {
    const authority = this.#committed
    let end = 0n
    let root: Uint8Array = directZipEpochGenesisRoot()
    for await (const bytes of streamEntries(this.#repository, checkpoint.operationId, 'epoch', authority.epochPages)) {
      const proof = decodeEpoch(bytes)
      if (proof.start !== end || !equalCanonicalBytes(proof.predecessorRoot, root)) {
        throw new TypeError('Direct ZIP epoch pages lost their contiguous lineage')
      }
      end = proof.end
      root = proof.epochRoot
      yield proof
    }
    if (end !== checkpoint.committedLength || !equalCanonicalBytes(root, checkpoint.epochRoot)) {
      throw new TypeError('Direct ZIP epoch pages disagree with committed target bytes')
    }
  }

  async #stage(kind: DirectZipPageKind, bytes: Uint8Array, centralBytes = 0n): Promise<void> {
    const fence = this.#fence()
    const chain = chainForKind(this.#current, kind)
    const predecessor = this.#tail === undefined
      ? { kind: 'checkpoint' as const, checkpointGeneration: fence.checkpointGeneration,
          checkpointDigest: (await this.#repository.readState(fence.operationId))?.checkpointDigest ?? '' }
      : { kind: 'page' as const, pageKind: this.#tail.pageKind,
          pageId: this.#tail.id, pageDigest: this.#tail.digest }
    const page = await createDirectZipImmutablePageV1({
      operationId: fence.operationId, pageKind: kind, chainId: chain.chainId,
      pageOrdinal: Number(chain.pageCount), predecessorRootDigest: chain.rootDigest,
      canonicalEntries: [bytes], accountingPredecessor: predecessor,
      previousBudgetUsage: this.#current.journalUsage,
      previousChainRecordCount: chain.recordCount,
      previousChainCanonicalMetadataBytes: chain.canonicalMetadataBytes,
    })
    await this.#repository.stagePage(fence, page)
    this.#tail = page
    const updated = chainFromPage(page)
    this.#current = Object.freeze({
      ...this.#current,
      ...updatedChain(kind, updated),
      journalUsage: page.budgetUsage, accountingTailPageId: page.id,
      centralBytes: this.#current.centralBytes + centralBytes,
    })
    this.#remember(this.#current)
  }

  #remember(authority: BrowserDirectZipPageAuthority): void {
    this.#states.set(pageStateKey(pageState(authority)), authority)
    if (this.#states.size > MAXIMUM_RETAINED_PAGE_STATES) {
      const keep = new Set([
        pageStateKey(pageState(this.#current)), pageStateKey(pageState(this.#committed)),
        ...(this.#rollback === undefined ? [] : [pageStateKey(pageState(this.#rollback))]),
        // Pending members can replace working rollback many times before a close.
        // The durable active member must still be resumable if that close fails.
        ...(this.#committedRollback === undefined ? [] : [pageStateKey(pageState(this.#committedRollback))]),
      ])
      for (const key of this.#states.keys()) if (!keep.has(key)) this.#states.delete(key)
    }
  }
}

function updatedChain(kind: DirectZipPageKind, chain: DirectZipPageChainV1) {
  switch (kind) {
    case 'layout': return { layoutPages: chain }
    case 'central': return { centralPages: chain }
    case 'epoch': return { epochPages: chain }
  }
}

export function pageState(authority: BrowserDirectZipPageAuthority): DirectZipWriterPageStateV1 {
  return Object.freeze({
    layoutRoot: digestBytes(authority.layoutPages.rootDigest),
    layoutRecordCount: authority.layoutPages.recordCount,
    centralRoot: digestBytes(authority.centralPages.rootDigest),
    centralRecordCount: authority.centralPages.recordCount,
    centralBytes: authority.centralBytes,
  })
}

function pageStateKey(state: DirectZipWriterPageStateV1): string {
  return encodeBase64Url(state.layoutRoot) + ':' + encodeBase64Url(state.centralRoot)
}

export async function readPageAuthority(
  repository: DirectZipJournalRepository,
  operationId: string,
  authority: Omit<BrowserDirectZipPageAuthority, 'centralBytes'>,
): Promise<BrowserDirectZipPageAuthority> {
  let centralBytes = 0n
  for (const kind of ['layout', 'central', 'epoch'] as const) {
    for await (const bytes of streamEntries(repository, operationId, kind, chainForKind(authority, kind))) {
      if (kind === 'central') centralBytes += BigInt(decodeCentral(bytes).bytes.byteLength)
      if (kind === 'epoch') decodeEpoch(bytes)
    }
  }
  return Object.freeze({ ...authority, centralBytes })
}

export async function* streamEntries(
  repository: DirectZipJournalRepository,
  operationId: string,
  kind: DirectZipPageKind,
  chain: DirectZipPageChainV1,
): AsyncIterable<Uint8Array> {
  let pageCount = 0n
  let recordCount = 0n
  let metadataBytes = 0n
  let root = ZERO_DIGEST
  for await (const page of repository.streamPages({ operationId, pageKind: kind, chainId: chain.chainId })) {
    if (pageCount === chain.pageCount) break
    if (BigInt(page.pageOrdinal) !== pageCount || page.predecessorRootDigest !== root ||
        page.chainRecordCount !== recordCount + BigInt(page.entryCount) ||
        page.chainCanonicalMetadataBytes !== metadataBytes + BigInt(page.canonicalBytes.byteLength)) {
      throw new TypeError('Direct ZIP immutable page chain changed')
    }
    root = page.chainRootDigest
    pageCount += 1n
    recordCount = page.chainRecordCount
    metadataBytes = page.chainCanonicalMetadataBytes
    for (const bytes of page.canonicalEntries) yield bytes
  }
  if (pageCount !== chain.pageCount || root !== chain.rootDigest ||
      recordCount !== chain.recordCount || metadataBytes !== chain.canonicalMetadataBytes) {
    throw new TypeError('Direct ZIP immutable page chain is incomplete')
  }
}

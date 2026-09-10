import { encodeBase64Url } from '../../../../crypto/bytes'
import { planDirectZipEntryV2 } from '../../../../output/direct-zip/format'
import {
  createDirectZipCheckpointProposalV1, createDirectZipCheckpointV1,
  createDirectZipCommitCandidateV1, validateDirectZipCheckpointV1,
  type DirectZipCheckpointV1, type DirectZipCommitCandidateV1,
  type DirectZipDiscoveryEvidenceV1, type DirectZipJournalRepository,
  type DirectZipTargetObservationV1,
} from '../../../../output/direct-zip/journal'
import type {
  DirectZipEpochCandidateV1, DirectZipWriterCheckpointV1, DirectZipWriterCutSink,
} from '../../../../output/direct-zip/writer'
import {
  canonicalDigest, equalCanonicalBytes, snapshotCanonicalBytes,
} from '../../../../output/workspace/canonical'
import type { PersistedReceiveRecord } from '../../../../output/workspace/records'
import type { ReceiveLifecycleState } from '../../../../output/workspace/state'
import type { DirectZipReplayAuthorityV1 } from '../../../../transfer/direct-zip/output-session'
import type {
  DirectZipAuthenticatedRootV1, DirectZipOrderedMemberV1,
} from '../../../../transfer/direct-zip/model'
import { rootDirectoryEvidence } from './bootstrap'
import { checkpointInput, writerCandidate, writerCheckpoint } from './checkpoint'
import {
  BrowserDirectZipPages, streamEntries, type BrowserDirectZipPageAuthority,
} from './pages'
import { decodeLayout, digestBytes, requireLayoutPlan, UNBOUND_DISCOVERY } from './records'

export interface BrowserDirectZipJournalOptions {
  readonly repository: DirectZipJournalRepository
  readonly checkpoint: DirectZipCheckpointV1
  readonly leaseId: string
  readonly expectedRootDirectoryId: string
  readonly observeTarget: (epochRoot: Uint8Array) => Promise<DirectZipTargetObservationV1>
  readonly lifecycleForCheckpoint: (checkpoint: DirectZipCheckpointV1) => Promise<Readonly<{
    lifecycle: ReceiveLifecycleState
    lifecycleRecord: PersistedReceiveRecord
  }>>
  readonly onCheckpointCommitted?: (checkpoint: DirectZipCheckpointV1, lifecycle: ReceiveLifecycleState) => void
}

export class BrowserDirectZipJournal implements DirectZipWriterCutSink, DirectZipReplayAuthorityV1 {
  readonly #options: BrowserDirectZipJournalOptions
  readonly pages: BrowserDirectZipPages
  readonly cuts: DirectZipWriterCutSink = this
  readonly replay: DirectZipReplayAuthorityV1 = this
  #persisted: DirectZipCheckpointV1
  #checkpoint: DirectZipWriterCheckpointV1
  #discovery: DirectZipDiscoveryEvidenceV1
  #candidate: DirectZipCommitCandidateV1 | undefined
  #candidatePages: BrowserDirectZipPageAuthority | undefined
  #pendingCandidate: DirectZipEpochCandidateV1 | undefined
  #replayEntries: AsyncIterator<Uint8Array> | undefined
  #replayOrdinal = 1n

  private constructor(
    options: BrowserDirectZipJournalOptions,
    pages: BrowserDirectZipPages,
    checkpoint: DirectZipWriterCheckpointV1,
  ) {
    this.#options = options
    this.pages = pages
    this.#persisted = options.checkpoint
    this.#checkpoint = checkpoint
    this.#discovery = options.checkpoint.discovery
  }

  static async open(options: BrowserDirectZipJournalOptions): Promise<BrowserDirectZipJournal> {
    const checkpoint = await validateDirectZipCheckpointV1(options.checkpoint)
    const stored = await options.repository.readState(checkpoint.operationId)
    if (stored?.checkpointDigest !== checkpoint.digest || stored.leaseId !== options.leaseId) {
      throw new TypeError('Direct ZIP journal open lost its checkpoint lease')
    }
    const pages = await BrowserDirectZipPages.open(options.repository, () => ({
      operationId: checkpoint.operationId, leaseId: options.leaseId,
      checkpointGeneration: journal.#persisted.generation,
    }), checkpoint)
    const journal = new BrowserDirectZipJournal(
      { ...options, checkpoint }, pages,
      await writerCheckpoint(options.repository, checkpoint, checkpoint.targetObservation.digest),
    )
    const candidate = await options.repository.readOperationCandidate(checkpoint.operationId)
    if (candidate !== undefined) {
      if (candidate.kind === 'bootstrap' || candidate.predecessorCheckpointDigest !== checkpoint.digest) {
        throw new TypeError('Direct ZIP writer candidate escaped its checkpoint')
      }
      journal.#candidate = candidate
      journal.#candidatePages = await pages.retainCandidateAuthority(candidate.proposedCheckpoint)
      journal.#pendingCandidate = await writerCandidate(options.repository, candidate)
    }
    // A crash during page staging can leave immutable suffixes without a candidate.
    // Fenced reachability retains both committed and pending authority before retry.
    await options.repository.collectOrphanPages({
      operationId: checkpoint.operationId, leaseId: options.leaseId,
      checkpointGeneration: checkpoint.generation,
    })
    return journal
  }

  get checkpoint(): DirectZipWriterCheckpointV1 { return this.#checkpoint }
  get persistedCheckpoint(): DirectZipCheckpointV1 { return this.#persisted }
  get pendingCandidate(): DirectZipEpochCandidateV1 | undefined { return this.#pendingCandidate }

  async stageCandidate(candidate: DirectZipEpochCandidateV1): Promise<void> {
    if (this.#candidate !== undefined || candidate.predecessorGeneration !== this.#persisted.generation ||
        candidate.operationId !== this.#persisted.operationId ||
        candidate.rangeStart !== this.#persisted.committedArchiveLength) {
      throw new TypeError('Direct ZIP writer candidate lost its predecessor')
    }
    await this.pages.stageEpoch({
      start: candidate.rangeStart, end: candidate.stagedEnd, contentDigest: candidate.contentDigest,
      predecessorRoot: digestBytes(this.#persisted.epochRootDigest), epochRoot: candidate.expectedEpochRoot,
    })
    const proposedCheckpoint = await createDirectZipCheckpointProposalV1(
      await checkpointInput(this.#persisted, candidate.proposed, this.pages, this.#discovery),
    )
    const persisted = await createDirectZipCommitCandidateV1({
      kind: candidate.kind, operationId: candidate.operationId, candidateId: candidate.candidateId,
      leaseId: this.#options.leaseId, predecessorCheckpointGeneration: this.#persisted.generation,
      predecessorCheckpointDigest: this.#persisted.digest,
      expectedRangeDigest: encodeBase64Url(candidate.contentDigest),
      predecessorTargetObservation: this.#persisted.targetObservation, proposedCheckpoint,
    })
    await this.#options.repository.bindCandidate(this.#fence(), persisted)
    this.#candidate = persisted
    this.#candidatePages = this.pages.authority
    this.#pendingCandidate = candidate
  }

  async promoteCandidate(input: Parameters<DirectZipWriterCutSink['promoteCandidate']>[0]): Promise<void> {
    const candidate = this.#requireCandidate(input.candidate)
    const observation = await this.#freshObservation(input.checkpoint)
    const checkpoint = await createDirectZipCheckpointV1({
      ...await checkpointInput(this.#persisted, input.checkpoint, this.pages,
        candidate.proposedCheckpoint.discovery, this.#candidatePages),
      targetObservation: observation, candidateLineageDigest: candidate.digest,
    })
    const lifecycle = await this.#options.lifecycleForCheckpoint(checkpoint)
    await this.#options.repository.promoteCandidate({
      fence: this.#fence(), candidate, checkpoint, ...lifecycle,
    })
    this.#adopt(checkpoint, input.checkpoint, this.#candidatePages!, lifecycle.lifecycle)
  }

  async retireCandidate(input: Parameters<DirectZipWriterCutSink['retireCandidate']>[0]): Promise<void> {
    const candidate = this.#requireCandidate(input.candidate)
    const checkpoint = input.disposition === 'replay-predecessor' ? this.#persisted :
      await createDirectZipCheckpointV1({
        ...await checkpointInput(this.#persisted, input.checkpoint, this.pages,
          this.#persisted.discovery, this.pages.committedAuthority),
        targetObservation: await this.#freshObservation(input.checkpoint),
        candidateLineageDigest: candidate.digest,
      })
    const lifecycle = await this.#options.lifecycleForCheckpoint(checkpoint)
    await this.#options.repository.retireCandidate({
      fence: this.#fence(), candidate, disposition: input.disposition, checkpoint, ...lifecycle,
    })
    this.#adopt(checkpoint, input.checkpoint, this.pages.committedAuthority, lifecycle.lifecycle)
  }

  async enterClosing(input: Parameters<DirectZipWriterCutSink['enterClosing']>[0]): Promise<void> {
    if (input.predecessorGeneration !== this.#persisted.generation || this.#candidate !== undefined) {
      throw new TypeError('Direct ZIP closing lost its predecessor')
    }
    const checkpoint = await createDirectZipCheckpointV1({
      ...await checkpointInput(this.#persisted, input.checkpoint, this.pages, this.#persisted.discovery),
      targetObservation: this.#persisted.targetObservation,
    })
    const lifecycle = await this.#options.lifecycleForCheckpoint(checkpoint)
    await this.#options.repository.enterClosing({ fence: this.#fence(), checkpoint, ...lifecycle })
    const stagedDiscovery = this.#discovery
    this.#adopt(checkpoint, input.checkpoint, this.pages.authority, lifecycle.lifecycle)
    this.#discovery = stagedDiscovery
  }

  async verifyRoot(checkpoint: DirectZipWriterCheckpointV1, root: DirectZipAuthenticatedRootV1): Promise<void> {
    this.#requireWriterCheckpoint(checkpoint)
    if (root.directoryId !== this.#options.expectedRootDirectoryId) {
      throw new TypeError('Direct ZIP traversal escaped its selected root')
    }
    const rootDigest = await canonicalDigest(rootDirectoryEvidence(root.directoryId))
    if (this.#persisted.discovery.directoryAdmissionDigest !== rootDigest) {
      throw new TypeError('Direct ZIP retained root authority changed')
    }
    if (!equalCanonicalBytes(this.#persisted.discovery.cursorCanonicalBytes, UNBOUND_DISCOVERY) &&
        !equalCanonicalBytes(this.#persisted.discovery.cursorCanonicalBytes, root.discoveryEvidence)) {
      throw new TypeError('Direct ZIP authenticated root changed since its checkpoint')
    }
    this.#discovery = Object.freeze({
      cursorCanonicalBytes: snapshotCanonicalBytes(root.discoveryEvidence),
      directoryAdmissionDigest: rootDigest,
      discoveryRootDigest: await canonicalDigest(root.discoveryEvidence),
    })
    this.#replayEntries = streamEntries(this.#options.repository, checkpoint.operationId, 'layout',
      this.#persisted.layoutPages)[Symbol.asyncIterator]()
    const first = await this.#replayEntries.next()
    if (first.done || decodeLayout(first.value).ordinal !== 0n ||
        !equalCanonicalBytes(decodeLayout(first.value).layoutEvidence, rootDirectoryEvidence(root.directoryId))) {
      throw new TypeError('Direct ZIP root layout authority is missing')
    }
    this.#replayOrdinal = 1n
  }

  async verifyMember(
    checkpoint: DirectZipWriterCheckpointV1, ordinal: bigint, member: DirectZipOrderedMemberV1,
  ): Promise<void> {
    this.#requireWriterCheckpoint(checkpoint)
    if (this.#replayEntries === undefined || ordinal !== this.#replayOrdinal ||
        ordinal >= checkpoint.nextEntryOrdinal) {
      throw new TypeError('Direct ZIP replay escaped its committed prefix')
    }
    const next = await this.#replayEntries.next()
    if (next.done) throw new TypeError('Direct ZIP retained member layout is missing')
    const layout = decodeLayout(next.value)
    const plan = planDirectZipEntryV2({
      ordinal, localHeaderOffset: layout.offset,
      entry: member.kind === 'file'
        ? { kind: 'file', path: member.artifactPath, exactSize: member.expectedSize,
            ...(member.modifiedTime === undefined ? {} : { modifiedTimeMilliseconds: member.modifiedTime.milliseconds }) }
        : { kind: 'directory', path: member.artifactPath,
            ...(member.modifiedTime === undefined ? {} : { modifiedTimeMilliseconds: member.modifiedTime.milliseconds }) },
    })
    requireLayoutPlan(next.value, plan)
    if (!equalCanonicalBytes(layout.layoutEvidence, member.layoutEvidence) ||
        !equalCanonicalBytes(layout.discoveryEvidence, member.discoveryEvidence)) {
      throw new TypeError('Direct ZIP replay changed authenticated discovery')
    }
    this.#replayOrdinal += 1n
  }

  #requireWriterCheckpoint(checkpoint: DirectZipWriterCheckpointV1): void {
    if (checkpoint.operationId !== this.#persisted.operationId ||
        checkpoint.generation !== this.#persisted.generation) {
      throw new TypeError('Direct ZIP writer checkpoint is stale')
    }
  }

  #requireCandidate(candidate: DirectZipEpochCandidateV1): DirectZipCommitCandidateV1 {
    if (this.#candidate === undefined || this.#candidate.candidateId !== candidate.candidateId ||
        this.#candidate.expectedRangeDigest !== encodeBase64Url(candidate.contentDigest)) {
      throw new TypeError('Direct ZIP candidate mutation lost its durable authority')
    }
    return this.#candidate
  }

  async #freshObservation(checkpoint: DirectZipWriterCheckpointV1): Promise<DirectZipTargetObservationV1> {
    const observation = await this.#options.observeTarget(checkpoint.epochRoot)
    if (observation.digest !== encodeBase64Url(checkpoint.targetObservationDigest)) {
      throw new TypeError('Direct ZIP target observation changed before the journal cut')
    }
    return observation
  }

  #fence() {
    return { operationId: this.#persisted.operationId, leaseId: this.#options.leaseId,
      checkpointGeneration: this.#persisted.generation }
  }

  #adopt(
    checkpoint: DirectZipCheckpointV1, writer: DirectZipWriterCheckpointV1,
    pages: BrowserDirectZipPageAuthority, lifecycle: ReceiveLifecycleState,
  ): void {
    this.#persisted = checkpoint
    this.#checkpoint = writer
    this.#discovery = checkpoint.discovery
    this.#candidate = undefined
    this.#candidatePages = undefined
    this.#pendingCandidate = undefined
    this.pages.commit(pages)
    this.#options.onCheckpointCommitted?.(checkpoint, lifecycle)
  }
}

import {
  DirectZipSha256Accumulator,
  chainDirectZipEpochDigestV1,
  encodeDirectZipCentralDirectoryRecordV2,
  encodeDirectZipDataDescriptorV2,
  encodeDirectZipLocalHeaderV2,
  validateDirectZipEntryPlanV2,
  type DirectZipEntryPlanV2,
} from '../format'
import { ZipCrc32, checkedZipAdd } from '../../zip-layout/policy'
import {
  DIRECT_ZIP_WRITER_CHECKPOINT_VERSION,
  decideDirectZipMemberResumeV1,
  type DirectZipCompletionProofV1,
  type DirectZipCompletionSealV1,
  type DirectZipEpochCandidateV1,
  type DirectZipMemberAdmissionV1,
  type DirectZipMemberResumeDecisionV1,
  type DirectZipSourceAuthorityV1,
  type DirectZipWriterCheckpointV1,
  type DirectZipWriterPageStateV1,
} from './model'
import {
  decideDirectZipAutomaticCheckpointV1,
  type DirectZipAutomaticCheckpointDecisionV1,
  type DirectZipAutomaticEpochBudgetV1,
} from './policy'
import {
  mutableMember,
  requireCheckpointShape,
  requireCompletionSeal,
  requireSourceMatchesPlan,
  snapshotCheckpoint,
  snapshotMutableMember,
  snapshotPageState,
  snapshotSource,
  type MutableDirectZipMemberState,
} from './checkpoint-state'
import {
  DirectZipClosingCoordinator,
  type DirectZipCompletionValidationInput,
} from './closing'
import {
  DirectZipEpochRecoveryCoordinator,
  type DirectZipCandidateResolutionV1,
} from './epoch-recovery'
import {
  DirectZipEpochStagingCoordinator,
  requireDirectZipWriterIdentity,
} from './epoch-staging'
import {
  DirectZipWriterGateError,
  gateFromOpen,
} from './gates'
import { DirectZipMemberWriterV1 } from './member-writer'
import type {
  DirectZipPositionedEpochWritable,
  DirectZipTargetVerificationPort,
  DirectZipWriterContextV1,
  DirectZipWriterCutSink,
  DirectZipWriterIdentityPort,
  DirectZipWriterObserver,
  DirectZipWriterPageSink,
  DirectZipWriterTraceEventV1,
} from './ports'

export { DirectZipWriterGateError } from './gates'
export type { DirectZipWriterGateV1 } from './gates'
export { DirectZipMemberWriterV1 } from './member-writer'

export type DirectZipCheckpointCutResultV1 =
  | Readonly<{
      kind: 'unchanged'
      checkpoint: DirectZipWriterCheckpointV1
      additionalTemporaryBytesUpperBound: bigint
      policyDecision?: DirectZipAutomaticCheckpointDecisionV1
    }>
  | Readonly<{
      kind: 'advanced'
      checkpoint: DirectZipWriterCheckpointV1
      additionalTemporaryBytesUpperBound: bigint
      policyDecision?: DirectZipAutomaticCheckpointDecisionV1
    }>
  | Readonly<{
      kind: 'replay-required'
      checkpoint: DirectZipWriterCheckpointV1
      additionalTemporaryBytesUpperBound: bigint
      policyDecision?: DirectZipAutomaticCheckpointDecisionV1
    }>

export type { DirectZipCandidateResolutionV1 } from './epoch-recovery'

export interface DirectZipEpochWriterOptionsV1 {
  readonly context: DirectZipWriterContextV1
  readonly checkpoint: DirectZipWriterCheckpointV1
  readonly pages: DirectZipWriterPageSink
  readonly cuts: DirectZipWriterCutSink
  readonly target: DirectZipTargetVerificationPort
  readonly identities: DirectZipWriterIdentityPort
  readonly automaticBudget?: DirectZipAutomaticEpochBudgetV1
  readonly cumulativePrefixCopyBytes?: bigint
  readonly observe?: DirectZipWriterObserver
}

/**
 * One owned target is the archive authority. Epoch close advances durability; it never
 * materializes a second archive or treats generic transfer checkpoints as forced closes.
 */
export class DirectZipEpochWriterV1 {
  readonly #context: DirectZipWriterContextV1
  readonly #pages: DirectZipWriterPageSink
  readonly #cuts: DirectZipWriterCutSink
  readonly #target: DirectZipTargetVerificationPort
  readonly #identities: DirectZipWriterIdentityPort
  readonly #automaticBudget: DirectZipAutomaticEpochBudgetV1 | undefined
  readonly #observe: DirectZipWriterObserver | undefined
  #checkpoint: DirectZipWriterCheckpointV1
  #phase: DirectZipWriterCheckpointV1['phase']
  #nextEntryOrdinal: bigint
  #archiveOffset: bigint
  #safeResumeBytes: bigint
  #member: MutableDirectZipMemberState | undefined
  #closing: DirectZipWriterCheckpointV1['closing']
  #writable: DirectZipPositionedEpochWritable | undefined
  #epochId: string | undefined
  #epochStart: bigint
  #epochDigest = new DirectZipSha256Accumulator()
  #cumulativePrefixCopyBytes: bigint
  #memberHandleGeneration = 0
  #mutationInFlight = false
  #published = false
  readonly #closingCoordinator: DirectZipClosingCoordinator
  readonly #recoveryCoordinator: DirectZipEpochRecoveryCoordinator
  readonly #stagingCoordinator: DirectZipEpochStagingCoordinator

  constructor(options: DirectZipEpochWriterOptionsV1) {
    this.#context = options.context
    this.#pages = options.pages
    this.#cuts = options.cuts
    this.#target = options.target
    this.#identities = options.identities
    this.#automaticBudget = options.automaticBudget
    this.#observe = options.observe
    this.#checkpoint = snapshotCheckpoint(options.checkpoint)
    this.#phase = this.#checkpoint.phase
    this.#nextEntryOrdinal = this.#checkpoint.nextEntryOrdinal
    this.#archiveOffset = this.#checkpoint.archiveOffset
    this.#safeResumeBytes = this.#checkpoint.safeResumeBytes
    this.#member = mutableMember(this.#checkpoint.member)
    this.#closing = this.#checkpoint.closing
    this.#published = this.#checkpoint.completion !== undefined
    this.#epochStart = this.#checkpoint.committedLength
    this.#cumulativePrefixCopyBytes = options.cumulativePrefixCopyBytes ?? 0n
    requireCheckpointShape(this.#checkpoint)
    this.#closingCoordinator = new DirectZipClosingCoordinator({
      context: this.#context,
      pages: this.#pages,
      cuts: this.#cuts,
      target: this.#target,
      writeArchive: (bytes, offsetClass) => this.#writeArchive(bytes, offsetClass),
      archiveOffset: () => this.#archiveOffset,
      emit: (kind, extra) => this.#emit(kind, extra),
    })
    this.#recoveryCoordinator = new DirectZipEpochRecoveryCoordinator({
      pages: this.#pages,
      cuts: this.#cuts,
      target: this.#target,
      emit: (kind, extra) => this.#emit(kind, extra),
      adoptCheckpoint: checkpoint => this.#adoptCheckpoint(checkpoint),
      restoreCommittedCheckpoint: () => this.#restoreCommittedCheckpoint(),
      validateCompletion: (checkpoint, completion) =>
        this.#closingCoordinator.validateCompletion(checkpoint, completion),
    })
    this.#stagingCoordinator = new DirectZipEpochStagingCoordinator({
      cuts: this.#cuts,
      identities: this.#identities,
      emit: (kind, extra) => this.#emit(kind, extra),
    })
  }

  get committedCheckpoint(): DirectZipWriterCheckpointV1 {
    return snapshotCheckpoint(this.#checkpoint)
  }

  get resumeTemporarySpaceUpperBound(): bigint {
    return this.#checkpoint.committedLength
  }

  decideMemberResume(source: DirectZipSourceAuthorityV1): DirectZipMemberResumeDecisionV1 {
    return decideDirectZipMemberResumeV1(this.#checkpoint, source)
  }

  async addDirectory(admission: DirectZipMemberAdmissionV1): Promise<void> {
    await this.#exclusive(async () => {
      this.#requireBetweenMembers()
      const plan = this.#requireAdmission(admission, 'directory')
      try {
        await this.#pages.stageLayout(admission)
        await this.#writeArchive(encodeDirectZipLocalHeaderV2(plan), 'member-header')
        await this.#writeArchive(encodeDirectZipDataDescriptorV2(plan, 0), 'member-descriptor')
        await this.#pages.stageCentral({
          ordinal: plan.ordinal,
          bytes: encodeDirectZipCentralDirectoryRecordV2(plan, 0),
        })
        this.#nextEntryOrdinal += 1n
        this.#emit('member-admitted', {
          entryOrdinal: plan.ordinal,
          offsetClass: 'member-descriptor',
        })
      } catch (error) {
        await this.#discardUncommitted(error)
        throw error
      }
    })
  }

  async beginFile(admission: DirectZipMemberAdmissionV1): Promise<DirectZipMemberWriterV1> {
    return this.#exclusive(async () => {
      this.#requireBetweenMembers()
      const plan = this.#requireAdmission(admission, 'file')
      if (admission.source === undefined) throw new TypeError('direct ZIP file source is absent')
      requireSourceMatchesPlan(admission.source, plan)
      const pagesBeforeAdmission = await this.#pages.snapshot()
      try {
        await this.#pages.stageLayout(admission)
        const rollbackEpochRoot = this.#workingEpochRoot()
        const rollback = Object.freeze({
          archiveOffset: this.#archiveOffset,
          safeResumeBytes: this.#safeResumeBytes,
          nextEntryOrdinal: this.#nextEntryOrdinal,
          epochStart: this.#epochStart,
          predecessorEpochRoot: Uint8Array.from(this.#checkpoint.epochRoot),
          epochContentDigest: this.#epochDigest.digest(),
          epochRoot: rollbackEpochRoot,
          pages: snapshotPageState(pagesBeforeAdmission),
        })
        await this.#writeArchive(encodeDirectZipLocalHeaderV2(plan), 'member-header')
        this.#phase = 'inside-member'
        this.#member = {
          plan,
          source: snapshotSource(admission.source),
          payloadOffset: 0n,
          crc32Accumulator: new ZipCrc32().snapshot(),
          rollback,
        }
        this.#memberHandleGeneration += 1
        this.#emit('member-admitted', {
          entryOrdinal: plan.ordinal,
          offsetClass: 'member-header',
        })
        return new DirectZipMemberWriterV1(this, this.#memberHandleGeneration)
      } catch (error) {
        await this.#discardUncommitted(error)
        throw error
      }
    })
  }

  resumeFile(source: DirectZipSourceAuthorityV1): DirectZipMemberWriterV1 | DirectZipMemberResumeDecisionV1 {
    const decision = decideDirectZipMemberResumeV1(this.#checkpoint, source)
    if (decision.kind === 'rollback-member') return decision
    if (this.#member === undefined) throw new Error('direct ZIP member state is absent')
    this.#memberHandleGeneration += 1
    this.#emit('member-resumed', {
      entryOrdinal: this.#member.plan.ordinal,
      offsetClass: 'member-payload',
      decision: `payload-offset:${decision.payloadOffset.toString()}`,
    })
    return new DirectZipMemberWriterV1(this, this.#memberHandleGeneration)
  }

  async automaticCheckpoint(): Promise<DirectZipCheckpointCutResultV1> {
    return this.#exclusive(async () => {
      const decision = decideDirectZipAutomaticCheckpointV1({
        committedLength: this.#archiveOffset,
        cumulativePrefixCopyBytes: this.#cumulativePrefixCopyBytes,
        ...(this.#automaticBudget === undefined ? {} : { budget: this.#automaticBudget }),
      })
      this.#emit('checkpoint-policy-decided', {
        decision: decision.kind === 'admit' ? 'admit' : `decline:${decision.reason}`,
      })
      if (decision.kind === 'decline') {
        return Object.freeze({
          kind: 'unchanged',
          checkpoint: snapshotCheckpoint(this.#checkpoint),
          additionalTemporaryBytesUpperBound: decision.additionalTemporaryBytesUpperBound,
          policyDecision: decision,
        })
      }
      const result = await this.#checkpointEpoch('epoch')
      if (result.kind === 'advanced') {
        this.#cumulativePrefixCopyBytes = decision.nextCumulativePrefixCopyBytes
      }
      return Object.freeze({ ...result, policyDecision: decision })
    })
  }

  async pause(): Promise<DirectZipCheckpointCutResultV1> {
    return this.#exclusive(() => this.#checkpointEpoch('epoch'))
  }

  async recoverCandidate(
    candidate: DirectZipEpochCandidateV1,
    closingSeal?: DirectZipCompletionSealV1,
  ): Promise<DirectZipCandidateResolutionV1> {
    return this.#exclusive(async () => {
      if (candidate.kind === 'closing' && closingSeal === undefined) {
        throw new TypeError('direct ZIP closing recovery requires the completion seal')
      }
      const completion = candidate.kind === 'closing'
        ? await this.#closingCoordinator.completionInput(this.#checkpoint, closingSeal!)
        : undefined
      return this.#recoveryCoordinator.resolveCandidate(
        candidate,
        this.#checkpoint,
        undefined,
        completion,
      )
    })
  }

  async closeArchive(seal: DirectZipCompletionSealV1): Promise<DirectZipCompletionProofV1> {
    return this.#exclusive(async () => {
      if (this.#checkpoint.completion !== undefined) {
        const completionInput = await this.#closingCoordinator.completionInput(
          this.#checkpoint,
          seal,
          this.#checkpoint.completion.preClosingEpochRoot,
        )
        return this.#closingCoordinator.validateCompletion(this.#checkpoint, completionInput)
      }
      this.#requireNotPublished()
      if (this.#phase === 'inside-member') {
        throw new Error('direct ZIP cannot close while a member is incomplete')
      }
      if (this.#phase === 'between-members' && this.#archiveOffset !== this.#checkpoint.committedLength) {
        const cut = await this.#checkpointEpoch('epoch')
        if (cut.kind !== 'advanced') throw new Error('direct ZIP member epoch requires replay before closing')
      }
      const pages = await this.#pages.snapshot()
      requireCompletionSeal(seal, pages, this.#nextEntryOrdinal, this.#checkpoint.epochRoot)
      if (this.#phase === 'between-members') await this.#enterClosing(seal)
      try {
        const completionInput = await this.#closingCoordinator.writeClosingRecords(
          this.#checkpoint,
          seal,
        )
        const result = await this.#checkpointEpoch('closing', completionInput)
        if (result.kind !== 'advanced') throw new Error('direct ZIP closing epoch did not publish')
        this.#published = true
        return Object.freeze({
          checkpoint: snapshotCheckpoint(this.#checkpoint),
          exactArchiveBytes: this.#checkpoint.committedLength,
          finalEpochRoot: Uint8Array.from(this.#checkpoint.epochRoot),
          targetObservationDigest: Uint8Array.from(this.#checkpoint.targetObservationDigest),
        })
      } catch (error) {
        if (this.#writable !== undefined) await this.#discardUncommitted(error)
        throw error
      }
    })
  }

  async writeMember(handleGeneration: number, bytes: Uint8Array): Promise<void> {
    await this.#exclusive(async () => {
      const member = this.#requireMemberHandle(handleGeneration)
      if (!(bytes instanceof Uint8Array)) throw new TypeError('direct ZIP member payload is invalid')
      const nextOffset = checkedZipAdd(member.payloadOffset, BigInt(bytes.byteLength))
      if (nextOffset > member.source.exactSize) {
        throw new RangeError('direct ZIP member received more payload than declared')
      }
      if (bytes.byteLength === 0) return
      const nextCrc = new ZipCrc32(member.crc32Accumulator)
      nextCrc.update(bytes)
      try {
        await this.#writeArchive(bytes, 'member-payload')
      } catch (error) {
        await this.#discardUncommitted(error)
        throw error
      }
      member.payloadOffset = nextOffset
      member.crc32Accumulator = nextCrc.snapshot()
      this.#safeResumeBytes = checkedZipAdd(this.#safeResumeBytes, BigInt(bytes.byteLength))
    })
  }

  async closeMember(handleGeneration: number): Promise<void> {
    await this.#exclusive(async () => {
      const member = this.#requireMemberHandle(handleGeneration)
      if (member.payloadOffset !== member.source.exactSize) {
        throw new Error('direct ZIP member payload is incomplete')
      }
      const crc32 = new ZipCrc32(member.crc32Accumulator).digest()
      try {
        await this.#writeArchive(
          encodeDirectZipDataDescriptorV2(member.plan, crc32),
          'member-descriptor',
        )
        await this.#pages.stageCentral({
          ordinal: member.plan.ordinal,
          bytes: encodeDirectZipCentralDirectoryRecordV2(member.plan, crc32),
        })
      } catch (error) {
        await this.#discardUncommitted(error)
        throw error
      }
      this.#phase = 'between-members'
      this.#nextEntryOrdinal += 1n
      this.#member = undefined
      this.#memberHandleGeneration += 1
    })
  }

  async #checkpointEpoch(
    kind: DirectZipEpochCandidateV1['kind'],
    completionInput?: DirectZipCompletionValidationInput,
  ): Promise<DirectZipCheckpointCutResultV1> {
    if (this.#archiveOffset === this.#checkpoint.committedLength) {
      return Object.freeze({
        kind: 'unchanged',
        checkpoint: snapshotCheckpoint(this.#checkpoint),
        additionalTemporaryBytesUpperBound: this.#checkpoint.committedLength,
      })
    }
    if (this.#writable === undefined || this.#epochId === undefined) {
      throw new Error('direct ZIP staged bytes have no open epoch')
    }
    const epochId = this.#epochId
    const pages = await this.#pages.snapshot()
    const contentDigest = this.#epochDigest.digest()
    const expectedEpochRoot = chainDirectZipEpochDigestV1({
      predecessorRoot: this.#checkpoint.epochRoot,
      start: this.#epochStart,
      end: this.#archiveOffset,
      contentDigest,
    })
    const candidate = await this.#stagingCoordinator.stage({
      kind,
      epochId,
      predecessor: this.#checkpoint,
      rangeStart: this.#epochStart,
      stagedEnd: this.#archiveOffset,
      contentDigest,
      expectedEpochRoot,
      proposed: this.#snapshotWorkingCheckpoint(
        this.#checkpoint.generation + 1n,
        this.#archiveOffset,
        expectedEpochRoot,
        pages,
      ),
    })
    try {
      await this.#recoveryCoordinator.verifyPredecessor(this.#checkpoint)
    } catch (error) {
      await this.#abortWritable(error)
      throw error
    }
    const writable = this.#writable
    this.#writable = undefined
    const closeAttempt = await writable.closeOnce()
    this.#emit('epoch-close-observed', {
      candidateId: candidate.candidateId,
      epochId: candidate.epochId,
      decision: closeAttempt.kind,
    })
    const resolution = await this.#recoveryCoordinator.resolveCandidate(
      candidate,
      this.#checkpoint,
      closeAttempt,
      completionInput,
    )
    const bound = resolution.checkpoint.committedLength
    return resolution.kind === 'promoted'
      ? Object.freeze({ kind: 'advanced', checkpoint: resolution.checkpoint, additionalTemporaryBytesUpperBound: bound })
      : Object.freeze({ kind: 'replay-required', checkpoint: resolution.checkpoint, additionalTemporaryBytesUpperBound: bound })
  }

  async #ensureWritable(): Promise<DirectZipPositionedEpochWritable> {
    this.#requireNotPublished()
    if (this.#writable !== undefined) return this.#writable
    await this.#recoveryCoordinator.verifyPredecessor(this.#checkpoint)
    const opened = await this.#target.openEpoch(this.#checkpoint)
    if (opened.kind !== 'opened') throw gateFromOpen(opened.kind)
    this.#writable = opened.writable
    this.#epochId = requireDirectZipWriterIdentity(this.#identities.nextEpochId(), 'epoch')
    this.#epochStart = this.#checkpoint.committedLength
    this.#epochDigest = new DirectZipSha256Accumulator()
    this.#emit('epoch-opened', { epochId: this.#epochId })
    return opened.writable
  }

  async #writeArchive(
    bytes: Uint8Array,
    offsetClass: NonNullable<DirectZipWriterTraceEventV1['offsetClass']>,
  ): Promise<void> {
    const writable = await this.#ensureWritable()
    const snapshot = Uint8Array.from(bytes)
    await writable.write(this.#archiveOffset, snapshot)
    this.#epochDigest.update(snapshot)
    this.#archiveOffset = checkedZipAdd(this.#archiveOffset, BigInt(snapshot.byteLength))
    if (offsetClass === 'central-directory') {
      this.#emit('central-record-replayed', { offsetClass })
    }
  }

  async #enterClosing(seal: DirectZipCompletionSealV1): Promise<void> {
    const checkpoint = await this.#closingCoordinator.stageClosingCheckpoint(this.#checkpoint, seal)
    this.#checkpoint = checkpoint
    this.#phase = 'closing'
    this.#closing = checkpoint.closing
    this.#archiveOffset = checkpoint.archiveOffset
    this.#epochStart = checkpoint.committedLength
    this.#emit('closing-entered', { offsetClass: 'central-directory' })
  }

  #requireAdmission(
    admission: DirectZipMemberAdmissionV1,
    kind: 'directory' | 'file',
  ): DirectZipEntryPlanV2 {
    if (!(admission.layoutEvidence instanceof Uint8Array) ||
        !(admission.discoveryEvidence instanceof Uint8Array)) {
      throw new TypeError('direct ZIP admission evidence is invalid')
    }
    const plan = validateDirectZipEntryPlanV2(admission.plan)
    if (plan.zipEntry.kind !== kind || plan.ordinal !== this.#nextEntryOrdinal ||
        plan.zipEntry.localHeaderOffset !== this.#archiveOffset) {
      throw new Error('direct ZIP admission changed canonical kind, ordinal, or offset')
    }
    return plan
  }

  #workingEpochRoot(): Uint8Array {
    if (this.#archiveOffset === this.#epochStart) return Uint8Array.from(this.#checkpoint.epochRoot)
    return chainDirectZipEpochDigestV1({
      predecessorRoot: this.#checkpoint.epochRoot,
      start: this.#epochStart,
      end: this.#archiveOffset,
      contentDigest: this.#epochDigest.digest(),
    })
  }

  #snapshotWorkingCheckpoint(
    generation: bigint,
    committedLength: bigint,
    epochRoot: Uint8Array,
    pages: DirectZipWriterPageStateV1,
  ): DirectZipWriterCheckpointV1 {
    return Object.freeze({
      version: DIRECT_ZIP_WRITER_CHECKPOINT_VERSION,
      operationId: this.#checkpoint.operationId,
      intentDigest: Uint8Array.from(this.#checkpoint.intentDigest),
      generation,
      phase: this.#phase,
      nextEntryOrdinal: this.#nextEntryOrdinal,
      archiveOffset: this.#archiveOffset,
      committedLength,
      safeResumeBytes: this.#safeResumeBytes,
      targetObservationDigest: Uint8Array.from(this.#checkpoint.targetObservationDigest),
      epochRoot: Uint8Array.from(epochRoot),
      pages: snapshotPageState(pages),
      ...(this.#member === undefined ? {} : { member: snapshotMutableMember(this.#member) }),
      ...(this.#closing === undefined ? {} : { closing: { ...this.#closing } }),
    })
  }

  async #adoptCheckpoint(checkpoint: DirectZipWriterCheckpointV1): Promise<void> {
    this.#checkpoint = snapshotCheckpoint(checkpoint)
    this.#phase = checkpoint.phase
    this.#nextEntryOrdinal = checkpoint.nextEntryOrdinal
    this.#archiveOffset = checkpoint.archiveOffset
    this.#safeResumeBytes = checkpoint.safeResumeBytes
    this.#member = mutableMember(checkpoint.member)
    this.#closing = checkpoint.closing
    this.#published = checkpoint.completion !== undefined
    this.#epochStart = checkpoint.committedLength
    this.#epochDigest = new DirectZipSha256Accumulator()
    this.#epochId = undefined
    this.#writable = undefined
    await this.#pages.restore(checkpoint.pages)
  }

  async #restoreCommittedCheckpoint(): Promise<void> {
    this.#memberHandleGeneration += 1
    await this.#adoptCheckpoint(this.#checkpoint)
  }

  async #discardUncommitted(reason: unknown): Promise<void> {
    let failure: unknown = reason
    try {
      await this.#abortWritable(reason)
    } catch (abortError) {
      failure = new AggregateError([reason, abortError], 'direct ZIP write and epoch abort failed')
    }
    await this.#restoreCommittedCheckpoint()
    if (!(failure instanceof DirectZipWriterGateError)) this.#emit('writer-failed', { error: failure })
    if (failure !== reason) throw failure
  }

  async #abortWritable(reason: unknown): Promise<void> {
    const writable = this.#writable
    this.#writable = undefined
    this.#epochId = undefined
    if (writable !== undefined) await writable.abort(reason)
  }

  #requireMemberHandle(handleGeneration: number): MutableDirectZipMemberState {
    if (handleGeneration !== this.#memberHandleGeneration || this.#phase !== 'inside-member' ||
        this.#member === undefined) {
      throw new Error('direct ZIP member handle is stale')
    }
    return this.#member
  }

  #requireBetweenMembers(): void {
    this.#requireNotPublished()
    if (this.#phase !== 'between-members' || this.#member !== undefined) {
      throw new Error('direct ZIP writer is not between members')
    }
  }

  #requireNotPublished(): void {
    if (this.#published) throw new Error('direct ZIP archive is already published')
  }

  #emit(
    kind: DirectZipWriterTraceEventV1['kind'],
    extra: Omit<DirectZipWriterTraceEventV1, 'kind' | 'operationId' | 'checkpointGeneration' |
      'phase' | 'archiveOffset'> = {},
  ): void {
    try {
      this.#observe?.(Object.freeze({
        kind,
        operationId: this.#checkpoint.operationId,
        checkpointGeneration: this.#checkpoint.generation,
        phase: this.#phase,
        archiveOffset: this.#archiveOffset,
        ...extra,
      }))
    } catch {
      // Diagnostics are deliberately unable to perturb target or journal authority.
    }
  }

  async #exclusive<T>(operation: () => Promise<T>): Promise<T> {
    if (this.#mutationInFlight) throw new Error('direct ZIP writer mutation is already in progress')
    this.#mutationInFlight = true
    try {
      return await operation()
    } catch (error) {
      if (error instanceof DirectZipWriterGateError) {
        this.#emit('writer-gated', { decision: error.gate, error })
      }
      throw error
    } finally {
      this.#mutationInFlight = false
    }
  }
}

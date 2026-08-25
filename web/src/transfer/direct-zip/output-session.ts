import {
  compareDirectZipEntryPlansV2,
  planDirectZipEntryV2,
  type DirectZipEntryPlanV2,
} from '../../output/direct-zip/format'
import {
  DirectZipEpochWriterV1,
  DirectZipMemberWriterV1,
  type DirectZipCheckpointCutResultV1,
  type DirectZipCompletionProofV1,
  type DirectZipMemberResumeDecisionV1,
  type DirectZipSourceAuthorityV1,
  type DirectZipWriterCheckpointV1,
  type DirectZipWriterPageSink,
} from '../../output/direct-zip/writer'
import {
  outputCapabilities,
  disabledOutputExecutionProfile,
  outputSessionIdentity,
  type MaterializationSummary,
  type OutputCapabilities,
  type OutputSessionIdentity,
} from '../output-session'
import type {
  DirectZipAuthenticatedRootV1,
  DirectZipFileTransactionV1,
  DirectZipOpenedSourceV1,
  DirectZipOrderedMemberV1,
  DirectZipOrderedOutputV1,
  DirectZipOrderedVisitV1,
  DirectZipOutputSessionV1,
  DirectZipPublishedEvidenceV1,
  DirectZipStableEvidenceV1,
} from './model'

export interface DirectZipReplayAuthorityV1 {
  verifyRoot(
    checkpoint: DirectZipWriterCheckpointV1,
    root: DirectZipAuthenticatedRootV1,
  ): Promise<void>
  verifyMember(
    checkpoint: DirectZipWriterCheckpointV1,
    ordinal: bigint,
    member: DirectZipOrderedMemberV1,
  ): Promise<void>
}

export interface DirectZipMemberRollbackAuthorityV1 {
  rollbackMember(input: Readonly<{
    decision: Extract<DirectZipMemberResumeDecisionV1, { readonly kind: 'rollback-member' }>
    checkpoint: DirectZipWriterCheckpointV1
    source: DirectZipSourceAuthorityV1
  }>): Promise<DirectZipEpochWriterV1>
}

export interface DirectZipTransferOutputOptionsV1 {
  readonly identity: OutputSessionIdentity
  readonly capabilities: OutputCapabilities
  readonly writer: DirectZipEpochWriterV1
  readonly pages: Pick<DirectZipWriterPageSink, 'snapshot'>
  readonly replay: DirectZipReplayAuthorityV1
  readonly rollback: DirectZipMemberRollbackAuthorityV1
}

interface OpenedDirectZipMember {
  readonly plan: DirectZipEntryPlanV2
  readonly writer: DirectZipMemberWriterV1
  readonly resumeOffset: bigint
}

/**
 * This adapter tracks source and archive coordinates independently. Its checkpoint
 * method reports only writer-promoted source bytes, never merely acknowledged blocks.
 */
export class DirectZipTransferOutputV1 implements
  DirectZipOrderedOutputV1, DirectZipOutputSessionV1 {
  readonly identity: OutputSessionIdentity
  readonly capabilities: OutputCapabilities
  readonly executionProfile = disabledOutputExecutionProfile(1)
  readonly #pages: Pick<DirectZipWriterPageSink, 'snapshot'>
  readonly #replay: DirectZipReplayAuthorityV1
  readonly #rollback: DirectZipMemberRollbackAuthorityV1
  #writer: DirectZipEpochWriterV1
  #nextOrdinal: bigint
  #workingOffset: bigint
  #pendingFile: Readonly<{ ordinal: bigint; member: Extract<DirectZipOrderedMemberV1, { kind: 'file' }> }> | undefined
  #activeFile = false
  #traversalStarted = false
  #traversalFinished = false
  #terminallyPaused = false
  #entryCount = 1n
  #fileCount = 0n
  #directoryCount = 1n
  #rawBytes = 0n

  constructor(options: DirectZipTransferOutputOptionsV1) {
    this.identity = outputSessionIdentity(options.identity)
    this.capabilities = outputCapabilities(options.capabilities)
    if (this.capabilities.randomWrite || this.capabilities.fileFailureIsolation) {
      throw new TypeError('direct ZIP output exposes contiguous source resume and job-scoped failure isolation')
    }
    this.#writer = options.writer
    this.#pages = options.pages
    this.#replay = options.replay
    this.#rollback = options.rollback
    const checkpoint = this.#writer.committedCheckpoint
    this.#nextOrdinal = checkpoint.nextEntryOrdinal
    this.#workingOffset = checkpoint.archiveOffset
  }

  async beginTraversal(root: DirectZipAuthenticatedRootV1, signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    if (this.#traversalStarted) throw new Error('direct ZIP traversal was already started')
    await this.#replay.verifyRoot(this.#writer.committedCheckpoint, root)
    signal.throwIfAborted()
    this.#traversalStarted = true
  }

  async visit(
    ordinal: bigint,
    member: DirectZipOrderedMemberV1,
    signal: AbortSignal,
  ): Promise<DirectZipOrderedVisitV1> {
    signal.throwIfAborted()
    if (!this.#traversalStarted || this.#traversalFinished || this.#activeFile || this.#pendingFile !== undefined) {
      throw new Error('direct ZIP member visit escaped its serial traversal cut')
    }
    if (ordinal < this.#nextOrdinal) {
      await this.#replay.verifyMember(this.#writer.committedCheckpoint, ordinal, member)
      signal.throwIfAborted()
      this.#observeCompletedMember(member)
      return 'replayed'
    }
    if (ordinal !== this.#nextOrdinal) throw new Error('direct ZIP traversal skipped an archive ordinal')
    if (member.kind === 'directory') {
      const plan = this.#plan(ordinal, member, this.#workingOffset)
      await this.#writer.addDirectory({
        plan,
        layoutEvidence: member.layoutEvidence,
        discoveryEvidence: member.discoveryEvidence,
      })
      this.#workingOffset += plan.entryStreamBytes
      this.#nextOrdinal += 1n
      this.#observeCompletedMember(member)
      return 'admitted'
    }
    this.#pendingFile = Object.freeze({ ordinal, member })
    return 'transfer-file'
  }

  async beginFile(
    file: Extract<DirectZipOrderedMemberV1, { kind: 'file' }>,
    source: DirectZipOpenedSourceV1,
    signal: AbortSignal,
  ): Promise<DirectZipFileTransactionV1> {
    signal.throwIfAborted()
    const pending = this.#pendingFile
    if (pending === undefined || pending.member !== file || this.#activeFile) {
      throw new Error('direct ZIP file did not match its ordered admission')
    }
    if (source.fileId !== file.fileId || source.exactSize !== file.expectedSize ||
        source.revision.length === 0 || source.rangeAuthority.length === 0) {
      throw new TypeError('direct ZIP opened source does not bind the ordered file')
    }
    const authority: DirectZipSourceAuthorityV1 = Object.freeze({ ...source })
    const checkpoint = this.#writer.committedCheckpoint
    const opened = checkpoint.phase === 'inside-member' && checkpoint.nextEntryOrdinal === pending.ordinal
      ? await this.#resumeMember(pending.ordinal, file, authority, checkpoint)
      : await this.#startMember(pending.ordinal, file, authority, checkpoint)
    this.#activeFile = true
    return new DirectZipFileTransaction(
      opened.writer,
      authority,
      this,
      opened.resumeOffset,
      opened.plan,
    )
  }

  async #resumeMember(
    ordinal: bigint,
    file: Extract<DirectZipOrderedMemberV1, { kind: 'file' }>,
    authority: DirectZipSourceAuthorityV1,
    checkpoint: DirectZipWriterCheckpointV1,
  ): Promise<OpenedDirectZipMember> {
    const active = checkpoint.member!
    const plan = this.#plan(ordinal, file, active.plan.zipEntry.localHeaderOffset)
    requireSamePlan(plan, active.plan)
    const resumed = this.#writer.resumeFile(authority)
    if (resumed instanceof DirectZipMemberWriterV1) {
      return Object.freeze({ plan, writer: resumed, resumeOffset: active.payloadOffset })
    }
    if (resumed.kind !== 'rollback-member') {
      throw new Error('direct ZIP writer returned an invalid resume decision')
    }
    this.#writer = await this.#rollback.rollbackMember({
      decision: resumed,
      checkpoint,
      source: authority,
    })
    const replacement = this.#writer.committedCheckpoint
    if (replacement.phase !== 'between-members' ||
        replacement.nextEntryOrdinal !== resumed.nextEntryOrdinal ||
        replacement.archiveOffset !== resumed.archiveOffset) {
      throw new Error('direct ZIP member rollback returned a non-authoritative checkpoint')
    }
    this.#workingOffset = replacement.archiveOffset
    return this.#startMember(ordinal, file, authority, replacement)
  }

  async #startMember(
    ordinal: bigint,
    file: Extract<DirectZipOrderedMemberV1, { kind: 'file' }>,
    authority: DirectZipSourceAuthorityV1,
    checkpoint: DirectZipWriterCheckpointV1,
  ): Promise<OpenedDirectZipMember> {
    if (checkpoint.phase !== 'between-members') {
      throw new Error('direct ZIP cannot admit content while closing')
    }
    const plan = this.#plan(ordinal, file, checkpoint.archiveOffset)
    const writer = await this.#writer.beginFile({
      plan,
      source: authority,
      layoutEvidence: file.layoutEvidence,
      discoveryEvidence: file.discoveryEvidence,
    })
    return Object.freeze({ plan, writer, resumeOffset: 0n })
  }

  async finishTraversal(nextOrdinal: bigint, signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    if (!this.#traversalStarted || this.#traversalFinished || this.#activeFile ||
        this.#pendingFile !== undefined || nextOrdinal !== this.#nextOrdinal) {
      throw new Error('direct ZIP traversal did not finish at the writer successor')
    }
    this.#traversalFinished = true
  }

  materializationSummary(): MaterializationSummary {
    return Object.freeze({
      entryCount: this.#entryCount,
      fileCount: this.#fileCount,
      directoryCount: this.#directoryCount,
      rawBytes: this.#rawBytes,
    })
  }

  async pause(): Promise<DirectZipStableEvidenceV1> {
    if (this.#terminallyPaused) throw new Error('direct ZIP output was already paused')
    const evidence = await this.#stableCut()
    this.#terminallyPaused = true
    this.#activeFile = false
    this.#pendingFile = undefined
    return evidence
  }

  async publish(): Promise<DirectZipPublishedEvidenceV1> {
    if (!this.#traversalFinished || this.#activeFile || this.#pendingFile !== undefined) {
      throw new Error('direct ZIP cannot publish before ordered materialization is quiescent')
    }
    if (this.#terminallyPaused) throw new Error('direct ZIP paused output cannot publish')
    const stable = await this.#stableCut()
    const pages = await this.#pages.snapshot()
    const completion = await this.#writer.closeArchive({
      entryCount: this.#nextOrdinal,
      centralDirectoryBytes: pages.centralBytes,
      layoutRoot: pages.layoutRoot,
      centralRoot: pages.centralRoot,
      preClosingEpochRoot: stable.checkpoint.completion?.preClosingEpochRoot ??
        stable.checkpoint.epochRoot,
    })
    requireExactCompletion(completion, this.#nextOrdinal)
    return Object.freeze({ ...stable, checkpoint: completion.checkpoint, completion })
  }

  async observeFileCheckpoint(signal: AbortSignal): Promise<bigint> {
    signal.throwIfAborted()
    const cut = await this.#writer.automaticCheckpoint()
    if (cut.kind === 'replay-required') {
      throw new Error('direct ZIP checkpoint observation requires candidate replay')
    }
    signal.throwIfAborted()
    return currentFileSafeOffset(cut.checkpoint)
  }

  fileCommitted(plan: DirectZipEntryPlanV2): void {
    if (!this.#activeFile || this.#pendingFile === undefined || plan.ordinal !== this.#nextOrdinal) {
      throw new Error('direct ZIP file completion lost its ordered ownership')
    }
    const completed = this.#pendingFile.member
    this.#workingOffset = plan.zipEntry.localHeaderOffset + plan.entryStreamBytes
    this.#nextOrdinal += 1n
    this.#pendingFile = undefined
    this.#activeFile = false
    this.#observeCompletedMember(completed)
  }

  async #stableCut(): Promise<DirectZipStableEvidenceV1> {
    const cut = await this.#writer.pause()
    requireStableCut(cut)
    return Object.freeze({
      checkpoint: cut.checkpoint,
      materialization: this.materializationSummary(),
      additionalTemporaryBytesUpperBound: cut.additionalTemporaryBytesUpperBound,
    })
  }

  #observeCompletedMember(member: DirectZipOrderedMemberV1): void {
    this.#entryCount += 1n
    if (member.kind === 'directory') this.#directoryCount += 1n
    else {
      this.#fileCount += 1n
      this.#rawBytes += member.expectedSize
    }
  }

  #plan(
    ordinal: bigint,
    member: DirectZipOrderedMemberV1,
    localHeaderOffset: bigint,
  ): DirectZipEntryPlanV2 {
    return planDirectZipEntryV2({
      ordinal,
      localHeaderOffset,
      entry: member.kind === 'directory'
        ? {
            kind: 'directory',
            path: member.artifactPath,
            ...(member.modifiedTime === undefined
              ? {}
              : { modifiedTimeMilliseconds: member.modifiedTime.milliseconds }),
          }
        : {
            kind: 'file',
            path: member.artifactPath,
            exactSize: member.expectedSize,
            ...(member.modifiedTime === undefined
              ? {}
              : { modifiedTimeMilliseconds: member.modifiedTime.milliseconds }),
          },
    })
  }
}

class DirectZipFileTransaction implements DirectZipFileTransactionV1 {
  readonly resumeOffset: bigint
  readonly #member: DirectZipMemberWriterV1
  readonly #source: DirectZipSourceAuthorityV1
  readonly #owner: DirectZipTransferOutputV1
  readonly #plan: DirectZipEntryPlanV2
  #offset: bigint
  #committed = false

  constructor(
    member: DirectZipMemberWriterV1,
    source: DirectZipSourceAuthorityV1,
    owner: DirectZipTransferOutputV1,
    resumeOffset: bigint,
    plan: DirectZipEntryPlanV2,
  ) {
    this.#member = member
    this.#source = source
    this.#owner = owner
    this.resumeOffset = resumeOffset
    this.#offset = resumeOffset
    this.#plan = plan
  }

  async write(offset: bigint, bytes: Uint8Array, signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    if (this.#committed || offset !== this.#offset || bytes.byteLength === 0 ||
        offset + BigInt(bytes.byteLength) > this.#source.exactSize) {
      throw new RangeError('direct ZIP source write is not its next contiguous range')
    }
    await this.#member.write(bytes)
    this.#offset += BigInt(bytes.byteLength)
    signal.throwIfAborted()
  }

  observeCheckpoint(signal: AbortSignal): Promise<bigint> {
    if (this.#committed) throw new Error('direct ZIP file transaction is already committed')
    return this.#owner.observeFileCheckpoint(signal)
  }

  async commit(signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    if (this.#committed || this.#offset !== this.#source.exactSize) {
      throw new Error('direct ZIP file cannot commit before its exact source size')
    }
    await this.#member.close()
    signal.throwIfAborted()
    this.#committed = true
    this.#owner.fileCommitted(this.#plan)
  }
}

function currentFileSafeOffset(checkpoint: DirectZipWriterCheckpointV1): bigint {
  return checkpoint.phase === 'inside-member' ? checkpoint.member!.payloadOffset : 0n
}

function requireStableCut(cut: DirectZipCheckpointCutResultV1): void {
  if (cut.kind === 'replay-required') {
    throw new Error('direct ZIP pause could not establish a stable predecessor')
  }
  if (cut.additionalTemporaryBytesUpperBound !== cut.checkpoint.committedLength) {
    throw new Error('direct ZIP pause temporary-space bound changed coordinate systems')
  }
}

function requireSamePlan(actual: DirectZipEntryPlanV2, expected: DirectZipEntryPlanV2): void {
  if (actual.ordinal !== expected.ordinal ||
      actual.zipEntry.localHeaderOffset !== expected.zipEntry.localHeaderOffset ||
      actual.entryStreamBytes !== expected.entryStreamBytes ||
      compareDirectZipEntryPlansV2(actual, expected) !== 0) {
    throw new Error('direct ZIP resumed member changed canonical layout')
  }
}

function requireExactCompletion(proof: DirectZipCompletionProofV1, entryCount: bigint): void {
  if (proof.checkpoint.phase !== 'closing' || proof.checkpoint.nextEntryOrdinal !== entryCount ||
      proof.exactArchiveBytes !== proof.checkpoint.committedLength ||
      proof.checkpoint.archiveOffset !== proof.exactArchiveBytes) {
    throw new Error('direct ZIP completion proof is not exact publication evidence')
  }
}

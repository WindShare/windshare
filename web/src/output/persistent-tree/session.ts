import { snapshotPortableCatalogPath } from '../../catalog/path-policy'
import {
  emitOutputTrace,
  outputTraceEvent,
  recordOutputException,
  type OutputDiagnosticsPorts,
} from '../diagnostics'
import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  deriveCheckpointLineageID,
  identityBytes,
  newFileCheckpointV2,
  type FileCheckpointV2,
} from '../persistence/checkpoint'
import type {
  CheckpointLineageDecision,
  CheckpointNamespaceBinding,
  InitialCheckpointCASResult,
} from '../persistence/journal'
import type {
  OpenedFileRevision,
  PersistentDirectoryMaterialization,
  PersistentFileRequest,
  PersistentMaterializationPort,
  PersistentTreeSessionOptions,
  PersistentTreeFile,
  RecoverableFileCheckpointJournal,
} from './contracts'
import {
  CheckpointLineageDecisionError,
  DestinationCollisionError,
  TargetOwnershipUnknownError,
} from './errors'
import { PersistentFileTransaction } from './file-transaction'
import { recoverFileCheckpointCandidates } from './recovery'
import {
  captureCheckpointFailureFacts,
  runPersistentOutputStage,
  type PersistentOutputStageScope,
} from './stage-diagnostics'
import { MutationAdmissionBarrier, type MutationAdmission } from './admission-barrier'

type PersistentTreeTraceInput =
  | Readonly<{
      eventName: 'output_reservation'
      transition: 'acquired' | 'failed'
    }>
  | Readonly<{
      eventName: 'checkpoint'
      transition: 'restored' | 'persisted' | 'failed'
      decision?: 'absent' | 'installed' | 'exact' | 'revision_conflict' | 'ownership_conflict' | 'invalid'
    }>

export class PersistentTreeOutputSession implements PersistentMaterializationPort {
  readonly #tree: PersistentTreeSessionOptions['tree']
  readonly #checkpoints: RecoverableFileCheckpointJournal
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  readonly #stageAuthority: PersistentTreeSessionOptions['stageAuthority']
  readonly #legacyTrace: PersistentTreeSessionOptions['trace']
  readonly #mutationAdmissions = new MutationAdmissionBarrier()
  readonly #recreatablePreObjectCandidates = new Set<string>()
  #needsAttentionReported = false
  #activated = false
  #activation: Promise<void> | undefined
  #closed = false
  #closePromise: Promise<void> | undefined

  private constructor(options: PersistentTreeSessionOptions) {
    this.#tree = options.tree
    this.#checkpoints = options.checkpoints
    this.#diagnostics = options.diagnostics
    this.#stageAuthority = options.stageAuthority
    this.#legacyTrace = options.trace
  }

  static async open(options: PersistentTreeSessionOptions): Promise<PersistentTreeOutputSession> {
    const session = new PersistentTreeOutputSession(options)
    try {
      await options.tree.authorize()
      await options.tree.prepareRoot()
      session.#activated = true
      const recovery = await recoverFileCheckpointCandidates(options.checkpoints, {
        observe: candidate => session.#observeCandidate(candidate),
      })
      const unresolvedOwnership = recovery.unknownRecordIds.filter(
        recordId => !session.#recreatablePreObjectCandidates.has(recordId),
      )
      if (unresolvedOwnership.length !== 0) {
        session.#reportLegacyNeedsAttention()
        throw new TargetOwnershipUnknownError(
          'checkpoint',
          options.checkpoints.binding.operationId,
        )
      }
      return session
    } catch (error) {
      recordOutputException(options.diagnostics?.failures?.outputReservation, error)
      session.#trace({ eventName: 'output_reservation', transition: 'failed' })
      throw error
    }
  }

  /**
   * A newly committed operation must exist before PrefixVisible namespace publication.
   * This path validates authority but leaves prepareRoot behind the bound execution.
   */
  static async createNew(
    options: PersistentTreeSessionOptions,
  ): Promise<PersistentTreeOutputSession> {
    const session = new PersistentTreeOutputSession(options)
    try {
      await options.tree.authorize()
      return session
    } catch (error) {
      recordOutputException(options.diagnostics?.failures?.outputReservation, error)
      session.#trace({ eventName: 'output_reservation', transition: 'failed' })
      throw error
    }
  }

  activate(): Promise<void> {
    this.#requireOpen()
    if (this.#activated) return Promise.resolve()
    this.#activation ??= this.#tree.prepareRoot().then(
      () => {
        this.#activated = true
      },
      (error: unknown) => {
        recordOutputException(this.#diagnostics?.failures?.outputReservation, error)
        this.#trace({ eventName: 'output_reservation', transition: 'failed' })
        if (error instanceof TargetOwnershipUnknownError) this.#reportLegacyNeedsAttention()
        throw error
      },
    )
    return this.#activation
  }

  beginFile(request: PersistentFileRequest): Promise<PersistentFileTransaction> {
    let admission: MutationAdmission | undefined
    try {
      admission = this.#mutationAdmissions.enter()
      this.#requireActivated()
      const artifactPath = snapshotPortableCatalogPath(request.artifactPath)
      return this.#beginAdmittedFile(request, artifactPath, admission)
    } catch (error) {
      admission?.leave()
      return Promise.reject(error)
    }
  }

  async #beginAdmittedFile(
    request: PersistentFileRequest,
    artifactPath: readonly string[],
    admission: MutationAdmission,
  ): Promise<PersistentFileTransaction> {
    let handedOff = false
    let stageScope: PersistentOutputStageScope | undefined
    try {
      // Awaiting authenticated source authority before local planning prevents both
      // checkpoint reservations and namespace placeholders for unopened revisions.
      const revision = snapshotOpenedRevision(await request.openRevision())
      stageScope = this.#stageAuthority?.fileScope(revision.fileId, artifactPath)
      stageScope?.addFailureFacts(
        'checkpoint',
        context => captureCheckpointFailureFacts(
          this.#checkpoints,
          revision.fileId,
          context,
        ),
      )
      try {
        const decision = await this.#selectInitialCheckpoint(revision, artifactPath, stageScope)
        this.#traceCheckpointDecision(revision.fileId, decision)
        const checkpoint = selectedCheckpoint(decision)
        const fileScope = stageScope?.withCorrelation({
          ownedObjectId: checkpoint.ownedObjectId,
          checkpointRecordId: checkpoint.recordId,
          checkpointGeneration: checkpoint.checkpointGeneration,
        })
        let handle = await this.#tree.openFile(
          artifactPath,
          checkpoint.ownedObjectId,
          fileScope,
        )
        if (handle === undefined) {
          if (!isPristinePreObjectCandidate(checkpoint)) {
            throw new TargetOwnershipUnknownError('checkpoint', checkpoint.operationId)
          }
          handle = await this.#tree.createFileAfterRevisionOpen(
            artifactPath,
            revision,
            checkpoint.ownedObjectId,
            fileScope,
          )
        }
        await verifySelectedFile(handle, checkpoint, revision)
        const committed = checkpoint.commitState === FILE_CHECKPOINT_COMMIT_CANDIDATE
          ? await this.#promoteInitialCheckpoint(checkpoint, fileScope)
          : checkpoint
        const transactionScope = fileScope?.withCorrelation({
          checkpointRecordId: committed.recordId,
          checkpointGeneration: committed.checkpointGeneration,
        })
        const transaction = new PersistentFileTransaction({
          revision,
          handle,
          checkpoint: committed,
          checkpoints: this.#checkpoints,
          onClose: () => {
            admission.leave()
            transactionScope?.retireFileEvidence()
          },
          onOwnershipUnknown: () => this.#reportLegacyNeedsAttention(),
          ...(transactionScope === undefined ? {} : { stageScope: transactionScope }),
          ...(this.#diagnostics === undefined
            ? {}
            : { diagnostics: this.#diagnostics }),
        })
        admission.transferTo(() => transaction.close())
        handedOff = true
        return transaction
      } catch (error) {
        recordOutputException(this.#diagnostics?.failures?.checkpoint, error)
        this.#trace({ eventName: 'checkpoint', transition: 'failed' })
        if (error instanceof TargetOwnershipUnknownError) this.#reportLegacyNeedsAttention()
        throw error
      }
    } finally {
      if (!handedOff) {
        admission.leave()
        stageScope?.retireFileEvidence()
      }
    }
  }

  ensureDirectory(path: readonly string[]): Promise<PersistentDirectoryMaterialization> {
    const admission = this.#mutationAdmissions.enter()
    try {
      this.#requireActivated()
      return this.#tree.ensureDirectory(path).finally(() => admission.leave())
    } catch (error) {
      admission.leave()
      throw error
    }
  }

  close(): Promise<void> {
    if (this.#closePromise !== undefined) return this.#closePromise
    this.#closed = true
    const drain = this.#mutationAdmissions.closeExternalAdmission()
    this.#closePromise = drain.finally(() => this.#stageAuthority?.retireAllFileEvidence())
    return this.#closePromise
  }

  async #selectInitialCheckpoint(
    revision: OpenedFileRevision,
    artifactPath: readonly string[],
    stageScope: PersistentOutputStageScope | undefined,
  ): Promise<InitialCheckpointCASResult | CheckpointLineageDecision> {
    const lookup = {
      lineageId: deriveCheckpointLineageID({
        ...this.#checkpoints.binding,
        fileId: revision.fileId,
        canonicalPath: artifactPath,
      }),
      fileId: revision.fileId,
      canonicalPath: artifactPath,
      fileRevision: revision.fileRevision,
      exactSize: revision.exactSize,
    } as const
    let decision: InitialCheckpointCASResult | CheckpointLineageDecision =
      await runPersistentOutputStage(
        stageScope,
        'indexeddb.checkpoint.lineage-read',
        () => this.#checkpoints.lookupLineage(lookup),
      )
    if (decision.kind !== 'absent') return decision

    const proposedOwnedObjectId = await this.#tree.proposeFileOwnedObjectId(
      artifactPath,
      revision,
    )
    const proposedScope = stageScope?.withCorrelation({ ownedObjectId: proposedOwnedObjectId })
    if (await this.#tree.inspectFileDestination(
      artifactPath,
      proposedOwnedObjectId,
      proposedScope,
    ) === 'occupied') {
      decision = await runPersistentOutputStage(
        proposedScope,
        'indexeddb.checkpoint.lineage-read',
        () => this.#checkpoints.lookupLineage(lookup),
      )
      if (decision.kind === 'absent') {
        this.#traceCheckpointDecision(revision.fileId, decision)
        throw new DestinationCollisionError()
      }
    }
    if (decision.kind !== 'absent') return decision

    const candidate = initialCheckpoint(
      this.#checkpoints.binding,
      revision,
      artifactPath,
      proposedOwnedObjectId,
    )
    return runPersistentOutputStage(
      proposedScope?.withCorrelation({
        checkpointRecordId: candidate.recordId,
        checkpointGeneration: candidate.checkpointGeneration,
      }),
      'indexeddb.checkpoint.candidate-install',
      () => this.#checkpoints.createInitialCheckpoint(candidate),
    )
  }

  async #promoteInitialCheckpoint(
    candidate: FileCheckpointV2,
    stageScope: PersistentOutputStageScope | undefined,
  ): Promise<FileCheckpointV2> {
    const committed = newFileCheckpointV2({
      ...candidate,
      commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
    })
    const committedScope = stageScope?.withCorrelation({
      checkpointRecordId: committed.recordId,
      checkpointGeneration: committed.checkpointGeneration,
    })
    const existing = await runPersistentOutputStage(
      committedScope,
      'indexeddb.checkpoint.committed-read',
      () => this.#checkpoints.readCommitted(committed.recordId),
    )
    if (existing?.checksum === committed.checksum) return existing
    try {
      await runPersistentOutputStage(
        committedScope,
        'indexeddb.checkpoint.commit',
        () => this.#checkpoints.commitCheckpointCandidate(candidate, committed),
      )
    } catch (error) {
      // Concurrent materializers may promote the same selected candidate. The
      // canonical reread, rather than the losing commit exception, decides authority.
      const concurrent = await runPersistentOutputStage(
        committedScope,
        'indexeddb.checkpoint.committed-read',
        () => this.#checkpoints.readCommitted(committed.recordId),
      )
      if (concurrent?.checksum === committed.checksum) return concurrent
      throw error
    }
    const reread = await runPersistentOutputStage(
      committedScope,
      'indexeddb.checkpoint.committed-read',
      () => this.#checkpoints.readCommitted(committed.recordId),
    )
    if (reread === undefined || reread.checksum !== committed.checksum) {
      throw new TargetOwnershipUnknownError('checkpoint', candidate.operationId)
    }
    return reread
  }

  async #observeCandidate(
    candidate: FileCheckpointV2,
  ): Promise<
    | Readonly<{ kind: 'verified'; committed: FileCheckpointV2 }>
    | Readonly<{ kind: 'ownership-unknown' }>
  > {
    const stageScope = this.#stageAuthority
      ?.fileScope(candidate.fileId, candidate.canonicalPath)
      .withCorrelation({
        ownedObjectId: candidate.ownedObjectId,
        checkpointRecordId: candidate.recordId,
        checkpointGeneration: candidate.checkpointGeneration,
      })
    stageScope?.addFailureFacts(
      'checkpoint',
      context => captureCheckpointFailureFacts(
        this.#checkpoints,
        candidate.fileId,
        context,
      ),
    )
    let handle: PersistentTreeFile | undefined
    try {
      handle = await this.#tree.openFile(
        candidate.canonicalPath,
        candidate.ownedObjectId,
        stageScope,
      )
      if (handle === undefined) {
        // A pristine candidate is the durable reservation created before the object.
        // Only that exact crash cut may survive recovery without escalating ownership.
        if (isPristinePreObjectCandidate(candidate)) {
          this.#recreatablePreObjectCandidates.add(candidate.recordId)
        }
        return Object.freeze({ kind: 'ownership-unknown' })
      }
      const actualSize = await handle.size()
      const durableEnd = candidate.verifiedRanges.at(-1)?.end ?? 0n
      if (actualSize < durableEnd || actualSize > candidate.exactSize) {
        return Object.freeze({ kind: 'ownership-unknown' })
      }
      await handle.verify('checkpoint')
      return Object.freeze({
        kind: 'verified',
        committed: newFileCheckpointV2({
          ...candidate,
          commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
        }),
      })
    } catch (error) {
      if (error instanceof TargetOwnershipUnknownError) {
        return Object.freeze({ kind: 'ownership-unknown' })
      }
      throw error
    } finally {
      await handle?.close().catch(() => undefined)
      stageScope?.retireFileEvidence()
    }
  }

  #traceCheckpointDecision(
    fileId: string,
    decision: InitialCheckpointCASResult | CheckpointLineageDecision,
  ): void {
    try {
      this.#legacyTrace?.(Object.freeze({
        name: 'receive.checkpoint.decision',
        operation_id: this.#checkpoints.binding.operationId,
        file_id: fileId,
        ...(decision.kind === 'installed' || decision.kind === 'exact'
          ? { record_id: decision.record.recordId }
          : {}),
        decision: decision.kind,
      }))
    } catch {
      // Checkpoint authority remains independent from optional telemetry.
    }
    this.#trace({
      eventName: 'checkpoint',
      transition: checkpointTraceTransition(decision.kind),
      decision: checkpointTraceDecision(decision.kind),
    })
  }

  #requireOpen(): void {
    if (this.#closed) throw new DOMException('Persistent tree session is closed', 'InvalidStateError')
  }

  #requireActivated(): void {
    if (!this.#activated) {
      throw new DOMException('Persistent tree session is not activated', 'InvalidStateError')
    }
  }

  #reportLegacyNeedsAttention(): void {
    if (this.#needsAttentionReported) return
    this.#needsAttentionReported = true
    try {
      this.#legacyTrace?.(Object.freeze({
        name: 'receive.operation.needs_attention',
        operation_id: this.#checkpoints.binding.operationId,
        prior_state: 'receiving',
        needs_attention_reason: 'target-ownership-unknown',
      }))
    } catch {
      // Legacy observation cannot acquire ownership authority during the cutover.
    }
  }

  #trace(input: PersistentTreeTraceInput): void {
    const diagnostics = this.#diagnostics
    emitOutputTrace(diagnostics?.trace, () =>
      outputTraceEvent(input.eventName, {
        backend: diagnostics?.backend === 'file_system_access'
          ? 'file_system_access'
          : 'origin_private',
        transition: input.transition,
        ...('decision' in input && input.decision !== undefined
          ? { decision: input.decision }
          : {}),
      }))
  }
}

function checkpointTraceTransition(
  decision: InitialCheckpointCASResult['kind'] | CheckpointLineageDecision['kind'],
): 'persisted' | 'restored' | 'failed' {
  switch (decision) {
    case 'installed': return 'persisted'
    case 'exact': return 'restored'
    default: return 'failed'
  }
}

function checkpointTraceDecision(
  decision: InitialCheckpointCASResult['kind'] | CheckpointLineageDecision['kind'],
): 'absent' | 'installed' | 'exact' | 'revision_conflict' | 'ownership_conflict' | 'invalid' {
  switch (decision) {
    case 'revision-conflict': return 'revision_conflict'
    case 'ownership-conflict': return 'ownership_conflict'
    default: return decision
  }
}

function snapshotOpenedRevision(input: OpenedFileRevision): OpenedFileRevision {
  identityBytes(input.fileId, 16, 'file ID')
  identityBytes(input.fileRevision, 16, 'file revision')
  if (typeof input.exactSize !== 'bigint' || input.exactSize < 0n ||
      input.exactSize > 0xffff_ffff_ffff_ffffn) {
    throw new TypeError('Opened file revision exact size is invalid')
  }
  return Object.freeze({ ...input })
}

function initialCheckpoint(
  binding: CheckpointNamespaceBinding,
  revision: OpenedFileRevision,
  artifactPath: readonly string[],
  ownedObjectId: string,
): FileCheckpointV2 {
  return newFileCheckpointV2({
    operationId: binding.operationId,
    receiveIntentDigest: binding.receiveIntentDigest,
    materializationBindingDigest: binding.materializationBindingDigest,
    fileId: revision.fileId,
    fileRevision: revision.fileRevision,
    canonicalPath: artifactPath,
    exactSize: revision.exactSize,
    materializerKind: binding.materializerKind,
    authorityRef: binding.authorityRef,
    ownedObjectId,
    stateGeneration: 1n,
    checkpointGeneration: 0n,
    verifiedRanges: [],
    phase: FILE_CHECKPOINT_PHASE_ACTIVE,
    commitState: FILE_CHECKPOINT_COMMIT_CANDIDATE,
  })
}

function selectedCheckpoint(
  decision: InitialCheckpointCASResult | CheckpointLineageDecision,
): FileCheckpointV2 {
  switch (decision.kind) {
    case 'installed':
    case 'exact':
      return decision.record
    case 'revision-conflict':
    case 'ownership-conflict':
    case 'invalid':
      throw new CheckpointLineageDecisionError(decision.kind)
    case 'absent':
      throw new TypeError('checkpoint lineage remained absent after atomic creation')
  }
}

function isPristinePreObjectCandidate(checkpoint: FileCheckpointV2): boolean {
  return checkpoint.commitState === FILE_CHECKPOINT_COMMIT_CANDIDATE &&
    checkpoint.phase === FILE_CHECKPOINT_PHASE_ACTIVE &&
    checkpoint.stateGeneration === 1n &&
    checkpoint.checkpointGeneration === 0n &&
    checkpoint.verifiedRanges.length === 0
}

async function verifySelectedFile(
  handle: PersistentTreeFile,
  checkpoint: FileCheckpointV2,
  revision: OpenedFileRevision,
): Promise<void> {
  if (handle.ownedObjectId !== checkpoint.ownedObjectId) {
    throw new TargetOwnershipUnknownError('checkpoint', checkpoint.operationId)
  }
  const actualSize = await handle.size()
  const durableEnd = checkpoint.verifiedRanges.at(-1)?.end ?? 0n
  if (actualSize < durableEnd || actualSize > revision.exactSize) {
    throw new TargetOwnershipUnknownError('checkpoint', checkpoint.operationId)
  }
  await handle.verify('checkpoint')
}

export type {
  PersistentMaterializationPort,
  PersistentFileRequest,
  PersistentDirectoryMaterialization,
} from './contracts'

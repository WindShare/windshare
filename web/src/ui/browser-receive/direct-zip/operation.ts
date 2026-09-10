import {
  decideDirectZipMemberResumeV1, DirectZipEpochWriterV1, DirectZipWriterGateError,
  type DirectZipWriterObserver,
} from '../../../output/direct-zip/writer'
import type { DirectZipMemberRollbackAuthorityV1 } from '../../../transfer/direct-zip/output-session'
import { DirectZipTransferDiagnosticsV1 } from '../../../transfer/direct-zip/diagnostics'
import type { DirectZipWriterJournalPortV1 } from '../../../transfer/direct-zip/execution'
import type { DirectZipRuntimeFactsV1 } from '../../../output/direct-zip/session'
import type { DirectZipCheckpointV1, DirectZipJournalRepository } from '../../../output/direct-zip/journal'
import { createDirectZipRecoveryGateV1 } from '../../../output/direct-zip/journal'
import { BoundedDirectZipDiagnosticHistory } from '../../../output/direct-zip/diagnostics'
import type { OutputTraceSource } from '../../../output/diagnostics'
import type { ReceiveOperationRepository } from '../../../output/workspace/repository'
import {
  nextReceiveLifecycleState,
  type ReceiveLifecycleState,
  type ReceiveLifecycleStatePayload,
} from '../../../output/workspace/state'
import { storedReceiveLifecycleState } from '../../../output/workspace/state-codec'
import {
  createDirectZipExecutionV1,
  type DirectZipIntent,
  type DirectZipPayloadProgressV1,
} from '../../../transfer/direct-zip'
import { createOutputSessionID, createTransferJobID } from '../../../transfer/intent'
import {
  TransferPauseRequestedError,
  type DirectResumableZipExecution,
  type V2PlanExecutionAuthority,
} from '../../../transfer/output-session'
import type {
  V2BoundReceiveOperation,
  V2DirectZipProgressSnapshot,
  V2LifecycleMutation,
} from '../../v2-receive-runtime'
import type { LifecycleUserAction, V2ActiveReceiveControl } from '../../v2-lifecycle-presentation'
import { operationDigest } from '../shared'
import { BrowserDirectZipJournal } from './journal'
import { traceDirectZipMemberRollback } from './member-rollback-trace'
import { BrowserDirectZipTarget, type BrowserDirectZipBinding, type DirectZipNamespaceMutationPort } from './target'
import { browserDirectZipFileSystem, randomId } from './resources'

export interface BrowserDirectZipOperationOptions {
  readonly intent: DirectZipIntent
  readonly lifecycle: ReceiveLifecycleState
  readonly leaseId: string
  readonly repository: ReceiveOperationRepository
  readonly journal: DirectZipJournalRepository
  readonly checkpoint: DirectZipCheckpointV1
  readonly binding: BrowserDirectZipBinding
  readonly facts: DirectZipRuntimeFactsV1
  readonly close: () => Promise<void>
  readonly namespaceMutations: DirectZipNamespaceMutationPort
  readonly trace?: OutputTraceSource
}

export class BrowserDirectZipOperation implements V2BoundReceiveOperation {
  readonly intent: DirectZipIntent
  readonly transferJobId = createTransferJobID()
  readonly activeControls = Object.freeze(['pause'] as const)
  readonly #input: BrowserDirectZipOperationOptions
  readonly #target: BrowserDirectZipTarget
  readonly #listeners = new Set<(value: V2DirectZipProgressSnapshot) => void>()
  #journal!: BrowserDirectZipJournal
  #lifecycle: ReceiveLifecycleState
  #execution: DirectResumableZipExecution | undefined
  #closed = false
  readonly #lifetime = new AbortController()
  #progressGeneration = 0n
  #executionProgressGeneration = 0n
  #payloadProgress: DirectZipPayloadProgressV1

  private constructor(input: BrowserDirectZipOperationOptions) {
    this.#input = input
    this.intent = input.intent
    this.#lifecycle = input.lifecycle
    this.#payloadProgress = checkpointPayloadProgress(input.checkpoint)
    this.#target = new BrowserDirectZipTarget({
      binding: input.binding, fileSystem: browserDirectZipFileSystem(),
      proofs: () => this.#journal.pages.committedEpochProofs(this.#journal.checkpoint),
      namespaceMutations: input.namespaceMutations,
    })
  }

  static async open(input: BrowserDirectZipOperationOptions): Promise<BrowserDirectZipOperation> {
    const operation = new BrowserDirectZipOperation(input)
    operation.#journal = await BrowserDirectZipJournal.open({
      repository: input.journal, checkpoint: input.checkpoint, leaseId: input.leaseId,
      expectedRootDirectoryId: input.intent.selection.syntheticRoot,
      observeTarget: root => operation.#target.observe(root),
      lifecycleForCheckpoint: async checkpoint => {
        const payload: ReceiveLifecycleStatePayload = checkpoint.closingReplay?.completion === undefined
          ? { kind: 'receiving', activeLeaseId: input.leaseId }
          : await operation.#publication(checkpoint.digest)
        const lifecycle = nextReceiveLifecycleState(operation.#lifecycle, payload)
        return { lifecycle, lifecycleRecord: await storedReceiveLifecycleState(lifecycle) }
      },
      onCheckpointCommitted: (_checkpoint, lifecycle) => {
        operation.#lifecycle = lifecycle
        operation.#notify()
      },
    })
    return operation
  }

  get lifecycle() { return this.#lifecycle }

  readonly outputProgress = {
    getSnapshot: (): V2DirectZipProgressSnapshot => this.#snapshot(),
    subscribe: (listener: (value: V2DirectZipProgressSnapshot) => void) => {
      this.#listeners.add(listener)
      return () => { this.#listeners.delete(listener) }
    },
  }

  readonly plans: V2PlanExecutionAuthority = {
    openDirectTree: unavailable, openDirectAtomic: unavailable,
    openWorkspaceOriginal: unavailable, openWorkspaceZip: unavailable, preparePortable: unavailable,
    openDirectResumableZip: async (intent, signal) => {
      signal.throwIfAborted()
      if (intent.digest !== this.intent.digest || this.#closed || this.#execution !== undefined ||
          this.#lifecycle.kind === 'published') {
        throw new DOMException('ZIP execution authority is unavailable', 'InvalidStateError')
      }
      await this.verify()
      signal.throwIfAborted()
      if (this.lifecycle.kind === 'published') {
        throw new DOMException('The saved ZIP is already complete', 'InvalidStateError')
      }
      await this.#commit({ kind: 'receiving', activeLeaseId: this.#input.leaseId })
      const progressGeneration = ++this.#executionProgressGeneration
      const outputSessionId = createOutputSessionID()
      const diagnostics = new BoundedDirectZipDiagnosticHistory({
        clock: { nowMilliseconds: Date.now },
        ...(this.#input.trace === undefined ? {} : { trace: this.#input.trace }),
      })
      const rollbackDiagnostics = new DirectZipTransferDiagnosticsV1({
        operationId: this.intent.operationId, sessionId: outputSessionId, observer: diagnostics,
      })
      this.#execution = await createDirectZipExecutionV1({
        intent: this.intent,
        outputIdentity: { backend: 'file_system_access', outputSessionId },
        support: { enabled: true, durability: 'ProcessRestart' },
        writer: {
          context: { ownershipMarker: this.#input.binding.marker,
            rootComponent: this.#input.binding.resultRootComponent },
          checkpoint: this.#journal.checkpoint,
          journal: this.#writerJournal(),
          target: this.#target,
          identities: { nextCandidateId: randomId, nextEpochId: randomId },
          automaticPolicy: this.#input.facts.automaticEpochPolicy,
        },
        onProgress: progress => {
          if (!this.#closed && progressGeneration === this.#executionProgressGeneration) {
            this.#updatePayloadProgress(progress)
          }
        },
        replay: this.#journal.replay,
        rollback: {
          rollbackMember: input => this.#rollbackMember(input, rollbackDiagnostics.writerObserver()),
        },
        settlement: {
          pause: async () => this.#pause(),
          settle: async (_intent, _request, evidence) => {
            if ((await this.#target.verifyPredecessor(evidence.checkpoint)).kind !== 'accepted-fast') {
              throw new DOMException('ZIP completion verification changed', 'DataError')
            }
            return this.#lifecycle.kind === 'published' ? this.#lifecycle :
              this.#commit(await this.#publication(this.#journal.persistedCheckpoint.digest))
          },
        },
        diagnostics,
      })
      return this.#execution
    },
    settleExecutionAdmissionFailure: async (_intent, reason) =>
      (await this.settleTransferAdmissionFailure(reason)).lifecycle,
    recordSettlementUnknown: async () => this.#commit({
      kind: 'needs-attention', reason: 'publication-unknown',
      lastVerifiedRecordDigest: this.#journal.persistedCheckpoint.digest,
    }) as Promise<Extract<ReceiveLifecycleState, { kind: 'needs-attention' }>>,
  }

  interrupt(control: V2ActiveReceiveControl, transfer: AbortController): void {
    if (control !== 'pause') throw new TypeError('Direct ZIP only supports pause')
    transfer.abort(new TransferPauseRequestedError())
  }

  async startLifecycleAction(action: Exclude<LifecycleUserAction, V2ActiveReceiveControl>): Promise<V2LifecycleMutation> {
    if (action === 'delete' || action === 'discard') {
      await this.deleteOwned()
      return { lifecycle: this.#lifecycle, activeControls: [] }
    }
    if (action === 'continue') {
      this.#executionProgressGeneration += 1n
      this.#execution = undefined
      this.#updatePayloadProgress(checkpointPayloadProgress(this.#journal.persistedCheckpoint))
      await this.verify()
      if (this.#lifecycle.kind === 'published') return { lifecycle: this.#lifecycle, activeControls: [] }
      return { lifecycle: await this.#commit({ kind: 'receiving', activeLeaseId: this.#input.leaseId }),
        activeControls: this.activeControls, resumeTransfer: true }
    }
    throw new DOMException('This action is unavailable for the saved ZIP', 'NotSupportedError')
  }

  resolveWorkspaceUsage() { return null }

  async settleTransferAdmissionFailure(reason: unknown): Promise<V2LifecycleMutation> {
    this.#executionProgressGeneration += 1n
    await this.#target.abort(reason).catch(() => undefined)
    this.#updatePayloadProgress(checkpointPayloadProgress(this.#journal.persistedCheckpoint))
    if (this.#journal.hasPendingMutation) {
      try { await this.verify() } catch (recoveryError) { reason = recoveryError }
    }
    if (this.#lifecycle.kind === 'restart-required' || this.#lifecycle.kind === 'published') {
      return { lifecycle: this.#lifecycle }
    }
    const gate = recoveryGateFor(reason)
    let kind = gate === 'target-deleted' || gate === 'needs-attention' ? undefined : gate
    if (gate === 'target-deleted') {
      return { lifecycle: await this.#commit({ kind: 'restart-required', reason: 'target-deleted',
        receiptDigest: await operationDigest(this.intent, 'target-deleted') }) }
    }
    if (gate === 'needs-attention') {
      return { lifecycle: await this.#commit({ kind: 'needs-attention', reason: 'target-ownership-unknown',
        lastVerifiedRecordDigest: this.#journal.persistedCheckpoint.digest }) }
    }
    if (kind === undefined && this.#journal.hasPendingMutation) kind = 'target-verification-required'
    if (kind !== undefined) {
      const candidate = await this.#pendingCandidate()
      const gate = await createDirectZipRecoveryGateV1({
        operationId: this.intent.operationId, receiveIntentDigest: this.intent.digest,
        kind, checkpointDigest: this.#journal.persistedCheckpoint.digest,
        ...(candidate === undefined ? {} : { candidateDigest: candidate.digest }),
      })
      const lifecycle = nextReceiveLifecycleState(this.#lifecycle, { kind, recoveryGateDigest: gate.digest })
      await this.#input.journal.commitRecoveryLifecycle({
        fence: this.#fence(), lifecycle, lifecycleRecord: await storedReceiveLifecycleState(lifecycle),
        recoveryGate: gate,
        ...(candidate === undefined ? {} : { candidate }),
      })
      this.#lifecycle = lifecycle
      return { lifecycle }
    }
    return { lifecycle: await this.#pause(), activeControls: [] }
  }

  async deleteOwned() {
    const rollback = this.#journal.pendingMemberRollback
    if (rollback === undefined) {
      await this.#target.deleteOwned(this.#journal.checkpoint, this.#journal.pendingCandidate)
    } else {
      await this.#target.deleteOwnedMemberRollback(this.#journal.checkpoint, rollback.checkpoint,
        () => this.#journal.pages.epochProofsFor(rollback.authority, rollback.checkpoint))
    }
    await this.#commit({ kind: 'discarded',
      cleanupReceiptDigest: await operationDigest(this.intent, 'owned-zip-deleted') })
  }

  async verify() {
    await this.#recoverMemberRollback(this.#lifetime.signal)
    const candidate = this.#journal.pendingCandidate
    if (candidate !== undefined) {
      const writer = new DirectZipEpochWriterV1({
        context: { ownershipMarker: this.#input.binding.marker,
          rootComponent: this.#input.binding.resultRootComponent },
        checkpoint: this.#journal.checkpoint, pages: this.#journal.pages, cuts: this.#journal.cuts,
        target: this.#target, identities: { nextCandidateId: randomId, nextEpochId: randomId },
      })
      const checkpoint = this.#journal.checkpoint
      await writer.recoverCandidate(candidate, candidate.kind === 'closing' ? {
        entryCount: candidate.proposed.nextEntryOrdinal,
        centralDirectoryBytes: candidate.proposed.pages.centralBytes,
        layoutRoot: candidate.proposed.pages.layoutRoot, centralRoot: candidate.proposed.pages.centralRoot,
        predecessorEpochRoot: checkpoint.epochRoot,
      } : undefined)
    }
    const verified = await this.#target.verifyPredecessor(this.#journal.checkpoint)
    if (verified.kind !== 'accepted-fast') {
      if (verified.kind === 'foreign-target') {
        throw new DirectZipWriterGateError('needs-attention', 'The saved ZIP ownership changed')
      }
      if (verified.kind === 'digest-readback-required') {
        throw new DirectZipWriterGateError('target-verification-required', 'The saved ZIP needs byte verification')
      }
      throw new DirectZipWriterGateError(verified.kind, 'The saved ZIP requires recovery before continuing')
    }
    // Continuing adopts only the verified prefix; an abandoned writable's bytes
    // cannot contribute to the replacement execution or be counted again on replay.
    this.#updatePayloadProgress(checkpointPayloadProgress(this.#journal.persistedCheckpoint))
    // A completed archive is local authority; replaying the sender would make a
    // successfully saved result depend on the share still being available.
    if (this.#journal.checkpoint.completion !== undefined && this.#lifecycle.kind !== 'published') {
      await this.#commit(await this.#publication(this.#journal.persistedCheckpoint.digest))
    }
  }

  async #rollbackMember(
    input: Parameters<DirectZipMemberRollbackAuthorityV1['rollbackMember']>[0],
    observe: DirectZipWriterObserver,
  ): Promise<DirectZipEpochWriterV1> {
    const signal = AbortSignal.any([input.signal, this.#lifetime.signal])
    signal.throwIfAborted()
    const previous = this.#journal.checkpoint
    const decision = decideDirectZipMemberResumeV1(previous, input.source)
    if (input.checkpoint.operationId !== previous.operationId ||
        input.checkpoint.generation !== previous.generation || decision.kind !== 'rollback-member' ||
        decision.reason !== input.decision.reason || decision.archiveOffset !== input.decision.archiveOffset ||
        decision.nextEntryOrdinal !== input.decision.nextEntryOrdinal ||
        decision.safeResumeBytes !== input.decision.safeResumeBytes) {
      throw new TypeError('Direct ZIP member rollback lost its current checkpoint authority')
    }
    const candidateId = randomId()
    const trace = {
      operationId: this.intent.operationId, sessionId: this.#input.leaseId, candidateId,
      oldCommittedLength: previous.committedLength, newCommittedLength: decision.archiveOffset,
      retainedSelectedPayloadBytes: decision.safeResumeBytes, memberOrdinal: decision.nextEntryOrdinal,
      sourceChangeReason: decision.reason,
    }
    traceDirectZipMemberRollback(this.#input.trace, { ...trace, phase: 'requested' })
    try {
      await this.#journal.stageMemberRollback(candidateId)
      traceDirectZipMemberRollback(this.#input.trace, { ...trace, phase: 'persisted' })
      signal.throwIfAborted()
    } catch (error) {
      traceDirectZipMemberRollback(this.#input.trace, { ...trace, phase: 'failed', error })
      throw error
    }
    await this.#recoverMemberRollback(signal, decision.reason)
    // A pause still adopts the promoted writer before settling; detach ends its authority entirely.
    this.#lifetime.signal.throwIfAborted()
    return new DirectZipEpochWriterV1({
      context: { ownershipMarker: this.#input.binding.marker,
        rootComponent: this.#input.binding.resultRootComponent },
      checkpoint: this.#journal.checkpoint, pages: this.#journal.pages, cuts: this.#journal.cuts,
      target: this.#target, identities: { nextCandidateId: randomId, nextEpochId: randomId },
      automaticPolicy: this.#input.facts.automaticEpochPolicy, observe,
    })
  }

  async #recoverMemberRollback(
    signal: AbortSignal,
    sourceChangeReason?: Parameters<typeof traceDirectZipMemberRollback>[1]['sourceChangeReason'],
  ): Promise<void> {
    const rollback = this.#journal.pendingMemberRollback
    if (rollback === undefined) return
    signal.throwIfAborted()
    const trace = {
      operationId: this.intent.operationId, sessionId: this.#input.leaseId,
      candidateId: rollback.candidate.candidateId,
      oldCommittedLength: this.#journal.checkpoint.committedLength,
      newCommittedLength: rollback.checkpoint.committedLength,
      retainedSelectedPayloadBytes: rollback.checkpoint.safeResumeBytes,
      memberOrdinal: rollback.checkpoint.nextEntryOrdinal,
      ...(sourceChangeReason === undefined ? {} : { sourceChangeReason }),
    }
    traceDirectZipMemberRollback(this.#input.trace, { ...trace, phase: 'recovering' })
    try {
      const observation = await this.#target.recoverMemberRollback(
        this.#journal.checkpoint, rollback.checkpoint,
        () => this.#journal.pages.epochProofsFor(rollback.authority, rollback.checkpoint), signal,
      )
      await this.#journal.promoteMemberRollback(observation)
      this.#updatePayloadProgress(checkpointPayloadProgress(this.#journal.persistedCheckpoint))
      traceDirectZipMemberRollback(this.#input.trace, { ...trace, phase: 'completed' })
    } catch (error) {
      traceDirectZipMemberRollback(this.#input.trace, { ...trace, phase: 'failed', error })
      throw error
    }
  }

  #writerJournal(): DirectZipWriterJournalPortV1 {
    const pages = this.#journal.pages
    const cuts = this.#journal.cuts
    return {
      stageLayout: input => pages.stageLayout(input),
      stageCentral: input => pages.stageCentral(input),
      snapshot: () => pages.snapshot(),
      restore: state => pages.restore(state),
      replayCentral: state => pages.replayCentral(state),
      committedEpochProofs: checkpoint => pages.committedEpochProofs(checkpoint),
      stageCandidate: candidate => cuts.stageCandidate(candidate),
      promoteCandidate: input => cuts.promoteCandidate(input),
      retireCandidate: input => cuts.retireCandidate(input),
    }
  }

  async detach(): Promise<void> {
    if (this.#closed) return
    this.#closed = true
    this.#lifetime.abort(new DOMException('ZIP session detached', 'AbortError'))
    this.#executionProgressGeneration += 1n
    try { await this.#target.abort(new DOMException('ZIP session detached', 'AbortError')) }
    finally { await this.#input.close() }
  }

  async #pause(): Promise<ReceiveLifecycleState> {
    const checkpoint = this.#journal.persistedCheckpoint
    return this.#commit({
      kind: 'resumable-receive', payloadKind: 'direct-zip',
      directZipCheckpointDigest: checkpoint.digest,
      safeSelectedPayloadBytes: checkpoint.committedSelectedPayloadBytes,
      committedArchiveLength: checkpoint.committedArchiveLength, checkpointPhase: checkpoint.phase,
    })
  }

  async #publication(checkpointDigest: string): Promise<Extract<ReceiveLifecycleStatePayload, { kind: 'published' }>> {
    return {
      kind: 'published',
      receiptDigest: await operationDigest(this.intent, `zip-complete:${checkpointDigest}`),
      cleanupState: 'clean',
    }
  }

  async #commit(payload: ReceiveLifecycleStatePayload) {
    const lifecycle = nextReceiveLifecycleState(this.#lifecycle, payload)
    const candidate = await this.#pendingCandidate()
    await this.#input.journal.commitRecoveryLifecycle({
      fence: this.#fence(), lifecycle, lifecycleRecord: await storedReceiveLifecycleState(lifecycle),
      ...(candidate === undefined ? {} : { candidate }),
    })
    this.#lifecycle = lifecycle
    this.#notify()
    return lifecycle
  }

  async #pendingCandidate() {
    const candidate = await this.#input.journal.readOperationCandidate(this.intent.operationId)
    if (candidate?.kind === 'bootstrap') throw new TypeError('Active ZIP retained a bootstrap candidate')
    return candidate
  }

  #fence() {
    return { operationId: this.intent.operationId, leaseId: this.#input.leaseId,
      checkpointGeneration: this.#journal.persistedCheckpoint.generation }
  }

  #snapshot(): V2DirectZipProgressSnapshot {
    const checkpoint = this.#journal?.persistedCheckpoint ?? this.#input.checkpoint
    return { kind: 'direct-zip', operationId: this.intent.operationId, generation: this.#progressGeneration,
      phase: checkpoint.phase === 'closing' ? 'closing' : 'receiving',
      // Candidate recovery can promote durable bytes before a replacement output
      // opens and starts reporting live progress from that recovered prefix.
      receivedSelectedBytes: maximum(this.#payloadProgress.receivedSelectedBytes, checkpoint.committedSelectedPayloadBytes),
      writtenSelectedBytes: maximum(this.#payloadProgress.writtenSelectedBytes, checkpoint.committedSelectedPayloadBytes),
      safeResumeBytes: checkpoint.committedSelectedPayloadBytes,
      resumeTemporarySpaceUpperBound: checkpoint.committedArchiveLength }
  }

  #updatePayloadProgress(progress: DirectZipPayloadProgressV1) {
    if (progress.receivedSelectedBytes === this.#payloadProgress.receivedSelectedBytes &&
        progress.writtenSelectedBytes === this.#payloadProgress.writtenSelectedBytes) return
    this.#payloadProgress = { ...progress }
    this.#notify()
  }

  #notify() {
    this.#progressGeneration += 1n
    for (const listener of this.#listeners) listener(this.#snapshot())
  }
}

function checkpointPayloadProgress(checkpoint: DirectZipCheckpointV1): DirectZipPayloadProgressV1 {
  return { receivedSelectedBytes: checkpoint.committedSelectedPayloadBytes,
    writtenSelectedBytes: checkpoint.committedSelectedPayloadBytes }
}

function maximum(left: bigint, right: bigint) { return left > right ? left : right }

async function unavailable(): Promise<never> {
  throw new DOMException('This operation owns a direct ZIP', 'NotSupportedError')
}

function recoveryGateFor(reason: unknown): DirectZipWriterGateError['gate'] | undefined {
  if (reason instanceof DirectZipWriterGateError) return reason.gate
  if (!(reason instanceof DOMException)) return undefined
  switch (reason.name) {
    case 'NotAllowedError': return 'authorization-required'
    case 'QuotaExceededError': return 'destination-space-required'
    case 'NotFoundError': return 'target-deleted'
    case 'DataError': return 'needs-attention'
    default: return undefined
  }
}

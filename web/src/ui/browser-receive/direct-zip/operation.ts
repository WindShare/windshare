import { DirectZipEpochWriterV1, DirectZipWriterGateError } from '../../../output/direct-zip/writer'
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
import { BrowserDirectZipTarget, type BrowserDirectZipBinding } from './target'
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
  #progressGeneration = 0n

  private constructor(input: BrowserDirectZipOperationOptions) {
    this.#input = input
    this.intent = input.intent
    this.#lifecycle = input.lifecycle
    this.#target = new BrowserDirectZipTarget({
      binding: input.binding, fileSystem: browserDirectZipFileSystem(),
      proofs: () => this.#journal.pages.committedEpochProofs(this.#journal.checkpoint),
    })
  }

  static async open(input: BrowserDirectZipOperationOptions): Promise<BrowserDirectZipOperation> {
    const operation = new BrowserDirectZipOperation(input)
    operation.#journal = await BrowserDirectZipJournal.open({
      repository: input.journal, checkpoint: input.checkpoint, leaseId: input.leaseId,
      expectedRootDirectoryId: input.intent.selection.syntheticRoot,
      observeTarget: root => operation.#target.observe(root),
      lifecycleForCheckpoint: async () => {
        const lifecycle = nextReceiveLifecycleState(operation.#lifecycle, {
          kind: 'receiving', activeLeaseId: input.leaseId,
        })
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
      if (intent.digest !== this.intent.digest || this.#closed || this.#execution !== undefined) {
        throw new DOMException('ZIP execution authority is unavailable', 'InvalidStateError')
      }
      await this.verify()
      signal.throwIfAborted()
      await this.#commit({ kind: 'receiving', activeLeaseId: this.#input.leaseId })
      this.#execution = await createDirectZipExecutionV1({
        intent: this.intent,
        outputIdentity: { backend: 'file_system_access', outputSessionId: createOutputSessionID() },
        support: { enabled: true, durability: 'ProcessRestart' },
        writer: {
          context: { ownershipMarker: this.#input.binding.marker,
            rootComponent: this.#input.binding.resultRootComponent },
          checkpoint: this.#journal.checkpoint,
          journal: this.#writerJournal(),
          target: this.#target,
          identities: { nextCandidateId: randomId, nextEpochId: randomId },
          automaticBudget: this.#input.facts.automaticEpochBudget,
        },
        replay: this.#journal.replay,
        rollback: {
          rollbackMember: async () => {
            await this.#commit({
              kind: 'restart-required', reason: 'source-revision-changed',
              receiptDigest: await operationDigest(this.intent, 'source-revision-changed'),
            })
            throw new DOMException('The source file changed; its old ZIP bytes were retained', 'InvalidStateError')
          },
        },
        settlement: {
          pause: async () => this.#pause(),
          settle: async (_intent, _request, evidence) => {
            if ((await this.#target.verifyPredecessor(evidence.checkpoint)).kind !== 'accepted-fast') {
              throw new DOMException('ZIP completion verification changed', 'DataError')
            }
            return this.#commit({
              kind: 'published',
              receiptDigest: await operationDigest(this.intent,
                `zip-complete:${this.#journal.persistedCheckpoint.digest}`),
              cleanupState: 'clean',
            })
          },
        },
        diagnostics: new BoundedDirectZipDiagnosticHistory({
          clock: { nowMilliseconds: Date.now },
          ...(this.#input.trace === undefined ? {} : { trace: this.#input.trace }),
        }),
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
      this.#execution = undefined
      await this.verify()
      return { lifecycle: await this.#commit({ kind: 'receiving', activeLeaseId: this.#input.leaseId }),
        activeControls: this.activeControls, resumeTransfer: true }
    }
    throw new DOMException('This action is unavailable for the saved ZIP', 'NotSupportedError')
  }

  resolveWorkspaceUsage() { return null }

  async settleTransferAdmissionFailure(reason: unknown): Promise<V2LifecycleMutation> {
    await this.#target.abort(reason).catch(() => undefined)
    if (this.#journal.pendingCandidate !== undefined) {
      try { await this.verify() } catch (recoveryError) { reason = recoveryError }
    }
    if (this.#lifecycle.kind === 'restart-required') return { lifecycle: this.#lifecycle }
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
    if (kind === undefined && this.#journal.pendingCandidate !== undefined) kind = 'target-verification-required'
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
    await this.#target.deleteOwned(this.#journal.checkpoint, this.#journal.pendingCandidate)
    await this.#commit({ kind: 'discarded',
      cleanupReceiptDigest: await operationDigest(this.intent, 'owned-zip-deleted') })
  }

  async verify() {
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
        entryCount: checkpoint.nextEntryOrdinal,
        centralDirectoryBytes: checkpoint.pages.centralBytes,
        layoutRoot: checkpoint.pages.layoutRoot, centralRoot: checkpoint.pages.centralRoot,
        preClosingEpochRoot: checkpoint.epochRoot,
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
      enterClosing: input => cuts.enterClosing(input),
    }
  }

  async detach(): Promise<void> {
    if (this.#closed) return
    this.#closed = true
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
      receivedSelectedBytes: checkpoint.committedSelectedPayloadBytes,
      safeResumeBytes: checkpoint.committedSelectedPayloadBytes,
      resumeTemporarySpaceUpperBound: checkpoint.committedArchiveLength }
  }

  #notify() {
    this.#progressGeneration += 1n
    for (const listener of this.#listeners) listener(this.#snapshot())
  }
}

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

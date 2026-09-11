import { encodeBase64Url } from '../../../crypto/bytes'
import { IndexedDbReceiveOperationRepository } from '../../../output/browser/indexeddb-repository'
import {
  IndexedDbDirectZipJournalRepository,
  createDirectZipBootstrapCandidateV1,
  type DirectZipBootstrapCandidateV1,
  type DirectZipBootstrapResumeDescriptorV1,
  type DirectZipJournalRepository,
} from '../../../output/direct-zip/journal'
import {
  admitDirectZipRuntimeV1,
  DEFAULT_DIRECT_ZIP_RUNTIME_POLICY_V1,
  type DirectZipRuntimeFactsV1,
} from '../../../output/direct-zip/session'
import {
  snapshotDirectZipReservationCandidate,
  type DirectZipParentBinding,
} from '../../../output/direct-zip/target'
import {
  createReceiveOperationV2, receiveOperationLeaseRecord, storedReceiveOperationRecord,
  type ReceiveOperationLeaseRecord,
} from '../../../output/workspace/records'
import type { ReceiveOperationRepository } from '../../../output/workspace/repository'
import { initialReceiveLifecycleState, nextReceiveLifecycleState } from '../../../output/workspace/state'
import { storedReceiveLifecycleState } from '../../../output/workspace/state-codec'
import {
  createFSAOwnedFileBinding, createOperationID, deriveArtifactChoiceIdentity,
  validateReceiveIntent,
} from '../../../transfer/intent'
import type { DirectZipIntent } from '../../../transfer/direct-zip'
import type { BoundReceiveIntent } from '../../../output/planning'
import type { ReopenedDirectZipOperation } from '../../../output/resume/reopen-authority'
import type { OutputTraceSource } from '../../../output/diagnostics'
import type {
  V2ArtifactPresentationAuthority, V2RouteCommitInput, V2RouteCommitResult,
} from '../../v2-receive-runtime'
import type { BrowserReceiveWindow } from '../contracts'
import { digestText } from '../shared'
import type { BrowserDirectZipCompositionPort, BrowserDirectZipFreshAuthorityInput } from './contracts'
import { BrowserDirectZipOperation } from './operation'
import { BrowserDirectZipCoordination } from './coordination'
import { BrowserDirectZipTarget, bytes, type BrowserDirectZipBinding } from './target'
import { createInitialBrowserDirectZipCheckpoint } from './journal'
import {
  acquireOperationLock, browserDirectZipFileSystem, browserDirectZipHandleId,
  browserTarget, envelopeRecord, journalPolicies, randomBytes, randomId, readEnvelope,
  requireBootstrapEnvelope, requestBrowserDirectZipAuthorization, type BrowserDirectZipEnvelope,
} from './resources'
import { observeBrowserDirectZipFeatureFacts } from './support'

export interface BrowserDirectZipProductionOptions {
  readonly openRepository?: () => Promise<ReceiveOperationRepository>
  readonly openJournal?: () => Promise<DirectZipJournalRepository>
  readonly outputTrace?: OutputTraceSource
}

const OWNED_TARGET_AUTHORITY = Object.freeze({
  kind: 'owned-target-session-v1' as const,
  recovery: 'persisted-handle-and-verified-checkpoint' as const,
  replacement: 'coordinated-no-replace' as const,
  cleanup: 'ownership-proof-required' as const,
})

/** Installs the complete FSA/IndexedDB/Web Locks session, with no release-machine identity input. */
export function createBrowserDirectZipComposition(
  windowPort: BrowserReceiveWindow,
  options: BrowserDirectZipProductionOptions = {},
): BrowserDirectZipCompositionPort {
  const dependencies = {
    openRepository: options.openRepository ?? (() => IndexedDbReceiveOperationRepository.open()),
    openJournal: options.openJournal ?? (() => IndexedDbDirectZipJournalRepository.open()),
    ...(options.outputTrace === undefined ? {} : { trace: options.outputTrace }),
  }
  const facts = async (signal: AbortSignal): Promise<DirectZipRuntimeFactsV1> => {
    signal.throwIfAborted()
    const admitted = await admitDirectZipRuntimeV1({
      capabilities: { featureFacts: observeBrowserDirectZipFeatureFacts(windowPort),
        authority: OWNED_TARGET_AUTHORITY },
    })
    if (admitted.kind !== 'available') throw new DOMException('Direct ZIP APIs are unavailable', 'NotSupportedError')
    return admitted.facts
  }
  const composition: BrowserDirectZipCompositionPort = {
    capabilities: {
      read: async signal => {
        signal.throwIfAborted()
        let journal: DirectZipJournalRepository | undefined
        try {
          journal = await dependencies.openJournal()
          signal.throwIfAborted()
          return { authority: OWNED_TARGET_AUTHORITY, policy: DEFAULT_DIRECT_ZIP_RUNTIME_POLICY_V1 }
        } catch {
          // Storage denial disables this installed route without hiding the other browser choices.
          signal.throwIfAborted()
          return { authority: { kind: 'unavailable' as const, reason: 'journal-unavailable' as const } }
        } finally { journal?.close() }
      },
    },
    runtime: {
      startFresh: input => new BrowserDirectZipPresentation(windowPort, input, dependencies),
      dispatchBootstrapCandidate: (candidate, signal) =>
        recoverBootstrap(windowPort, candidate, signal, dependencies),
      resume: async (operation, signal) => {
        const runtime = await openRetained(windowPort, operation, await facts(signal), dependencies)
        try {
          signal.throwIfAborted()
          await runtime.startLifecycleAction('continue')
          signal.throwIfAborted()
          return runtime
        }
        catch (error) {
          try { await runtime.settleTransferAdmissionFailure(error) } finally { await runtime.detach() }
          throw error
        }
      },
      deleteRetained: async (operation, signal) => {
        const runtime = await openRetained(windowPort, operation, await facts(signal), dependencies)
        try { signal.throwIfAborted(); await runtime.deleteOwned() }
        finally { await runtime.detach() }
      },
    },
  }
  return Object.freeze(composition)
}

type Dependencies = {
  readonly openRepository: () => Promise<ReceiveOperationRepository>
  readonly openJournal: () => Promise<DirectZipJournalRepository>
  readonly trace?: OutputTraceSource
}

class BrowserDirectZipPresentation implements V2ArtifactPresentationAuthority {
  readonly ready: Promise<void>
  readonly #window: BrowserReceiveWindow
  readonly #input: BrowserDirectZipFreshAuthorityInput
  readonly #dependencies: Dependencies
  #released = false
  #committed = false

  constructor(windowPort: BrowserReceiveWindow, input: BrowserDirectZipFreshAuthorityInput, dependencies: Dependencies) {
    this.#window = windowPort
    this.#input = input
    this.#dependencies = dependencies
    this.ready = input.pickedParent.then(() => undefined)
    this.ready.catch(() => undefined)
  }

  release() { this.#released = true }

  async commit(input: V2RouteCommitInput): Promise<V2RouteCommitResult> {
    if (this.#released || this.#committed) throw new DOMException('ZIP presentation is closed', 'InvalidStateError')
    this.#committed = true
    const parent = await this.#input.pickedParent
    input.signal.throwIfAborted()
    if (input.action.route.kind !== 'direct-resumable-zip' ||
        input.action.choiceId !== this.#input.offered.choice.choiceId ||
        input.action.artifact.kind !== 'zip-archive') throw new TypeError('ZIP route choice changed')
    const operationId = createOperationID()
    const storage = await openStorage(this.#window, operationId, this.#dependencies)
    const { repository, journal } = storage
    let coordination: BrowserDirectZipCoordination
    try {
      coordination = await BrowserDirectZipCoordination.open({
        parent, manager: this.#window.navigator.locks, operationId,
        ...(this.#dependencies.trace === undefined ? {} : { trace: this.#dependencies.trace }),
      })
    } catch (error) { await storage.close(); throw error }
    let closePromise: Promise<void> | undefined
    const close = () => {
      closePromise ??= (async () => {
        try { await coordination.close() } finally { await storage.close() }
      })()
      return closePromise
    }
    const lease = receiveOperationLeaseRecord({ operationId, leaseId: randomId(), acquiredAt: Date.now() })
    let durable: DirectZipBootstrapCandidateV1 | undefined
    let frozen: BoundReceiveIntent | undefined
    let envelope: BrowserDirectZipEnvelope | undefined
    try {
      const parentBinding: DirectZipParentBinding<FileSystemDirectoryHandle> = {
        handleRef: browserDirectZipHandleId(operationId),
        bindingDigest: bytes(await digestText(`windshare/direct-zip-parent/v1\n${operationId}\n${parent.name}`)),
        persistedHandle: parent,
      }
      const targetRef = randomBytes(32)
      const target = browserTarget({
        leaseId: lease.leaseId, parentLocks: coordination.parentLocks,
        claimFile: file => coordination.claimFile(file),
        reservations: {
          persistCandidate: async draft => {
            input.signal.throwIfAborted()
            const binding = await createFSAOwnedFileBinding({
              operationId, artifact: input.action.artifact, stableName: draft.stableName,
              targetRef: encodeBase64Url(targetRef), policies: this.#input.facts.support.policies,
            })
            frozen = await input.freezeAtFence({
              kind: 'fsa-owned-file-binding',
              targetRouteId: this.#input.offered.route.target.routeId, binding,
            })
            input.signal.throwIfAborted()
            const candidate = snapshotDirectZipReservationCandidate(draft, {
              targetRef, bindingDigest: bytes(binding.digest),
            })
            envelope = { version: 1, frozen, candidate }
            const choice = await deriveArtifactChoiceIdentity(frozen.intent.artifact, frozen.intent.plan)
            const canonical = await createDirectZipBootstrapCandidateV1({
              operationId, candidateId: encodeBase64Url(draft.candidateId),
              leaseId: lease.leaseId, leaseGeneration: 1n,
              selectionCanonicalBytes: frozen.intent.selection.canonicalBytes,
              artifactCanonicalBytes: frozen.intent.artifact.canonicalBytes,
              choiceIdentityCanonicalBytes: choice.canonicalBytes, choiceId: choice.id,
              preClickRanking: this.#input.preClickRanking,
              stablePhysicalName: draft.stableName, ownershipNonce: encodeBase64Url(draft.ownershipNonce),
              targetBindingDigest: binding.digest, policies: journalPolicies(binding.policies),
              parentHandleId: browserDirectZipHandleId(operationId),
            })
            await journal.createBootstrapCandidate({
              candidate: canonical, provisionalParentHandle: envelopeRecord(envelope), lease,
            })
            durable = canonical
            // Read the structured-cloned handle back before a user-visible file can be created.
            const saved = await readEnvelope(repository, operationId)
            if (!await parent.isSameEntry(saved.candidate.parentBinding.persistedHandle)) {
              throw new DOMException('The selected folder handle could not be persisted', 'DataCloneError')
            }
            return { targetRef, bindingDigest: bytes(binding.digest) }
          },
          retireCandidate: async () => {
            throw new DOMException('The chosen ZIP name became occupied; choose a new operation', 'InvalidModificationError')
          },
        },
      })
      const reserved = await target.reserveBootstrap({
        operationId: bytes(operationId), resultRootComponent: input.action.artifact.layout.name,
        parentBinding, currentParent: parent, trustedAction: true,
      })
      if (reserved.kind !== 'ready' || durable === undefined || frozen === undefined || envelope === undefined) {
        throw new DOMException('ZIP target requires ownership verification', 'InvalidStateError')
      }
      const initialized = await commitBootstrap({
        repository, journal, candidate: durable, envelope: { ...envelope, binding: reserved.value.binding },
        lease, binding: reserved.value.binding, coordination,
      })
      const operation = await BrowserDirectZipOperation.open({
        intent: frozen.intent as DirectZipIntent, lifecycle: initialized.lifecycle,
        leaseId: lease.leaseId, repository, journal, checkpoint: initialized.checkpoint,
        binding: reserved.value.binding, facts: this.#input.facts, close,
        namespaceMutations: coordination.mutations,
        ...(this.#dependencies.trace === undefined ? {} : { trace: this.#dependencies.trace }),
      })
      return { kind: 'bound-operation', operation }
    } catch (cause) {
      if (durable !== undefined && frozen !== undefined) {
        const intent = frozen.intent
        const lifecycle = nextReceiveLifecycleState(initialReceiveLifecycleState({
          operationId, receiveIntentDigest: intent.digest,
        }), { kind: 'needs-attention', reason: 'target-ownership-unknown', lastVerifiedRecordDigest: durable.digest })
        return { kind: 'owned-effects', cause, authority: {
          intent, lifecycle, settleActivationFailure: async () => ({ lifecycle }), detach: close,
        } }
      }
      await close()
      throw cause
    }
  }
}

async function commitBootstrap(input: {
  repository: ReceiveOperationRepository; journal: DirectZipJournalRepository
  candidate: DirectZipBootstrapCandidateV1; envelope: BrowserDirectZipEnvelope
  lease: ReceiveOperationLeaseRecord; binding: BrowserDirectZipBinding
  coordination: BrowserDirectZipCoordination
}) {
  const intent = await validateReceiveIntent(input.envelope.frozen.intent)
  const target = new BrowserDirectZipTarget({
    binding: input.binding, fileSystem: browserDirectZipFileSystem(),
    proofs: async function* () {}, namespaceMutations: input.coordination.mutations,
  })
  const initialized = await createInitialBrowserDirectZipCheckpoint({
    candidate: input.candidate, receiveIntentDigest: intent.digest,
    parentBindingDigest: encodeBase64Url(input.binding.parentBinding.bindingDigest),
    fileBindingDigest: encodeBase64Url(input.binding.fileBinding.bindingDigest),
    ownershipMarker: input.binding.marker, rootComponent: input.binding.resultRootComponent,
    expectedRootDirectoryId: intent.selection.syntheticRoot,
    observeTarget: root => target.observe(root),
  })
  const operation = await createReceiveOperationV2({
    receiveIntent: intent, preClickRanking: input.candidate.preClickRanking,
  })
  const lifecycle = nextReceiveLifecycleState(initialReceiveLifecycleState({
    operationId: intent.operationId, receiveIntentDigest: intent.digest,
  }), {
    kind: 'resumable-receive', payloadKind: 'direct-zip',
    directZipCheckpointDigest: initialized.checkpoint.digest,
    safeSelectedPayloadBytes: 0n, committedArchiveLength: initialized.checkpoint.committedArchiveLength,
    checkpointPhase: initialized.checkpoint.phase,
  })
  await input.journal.commitBootstrap({
    candidate: input.candidate, operation, operationRecord: storedReceiveOperationRecord(operation),
    lifecycle, lifecycleRecord: await storedReceiveLifecycleState(lifecycle),
    handles: [envelopeRecord(input.envelope)], lease: input.lease, ...initialized,
  })
  return { checkpoint: initialized.checkpoint, lifecycle }
}

async function openRetained(windowPort: BrowserReceiveWindow, operation: ReopenedDirectZipOperation,
  facts: DirectZipRuntimeFactsV1, dependencies: Dependencies) {
  const envelope = await readEnvelope(operation.repository, operation.intent.operationId)
  if (envelope.binding === undefined || envelope.frozen.intent.digest !== operation.intent.digest) {
    throw new DOMException('ZIP target binding does not match the retained operation', 'DataError')
  }
  const fileSystem = browserDirectZipFileSystem()
  const parent = envelope.binding.parentBinding.persistedHandle
  if (await fileSystem.queryPermission(parent) !== 'granted') {
    await requestBrowserDirectZipAuthorization(parent)
  }
  const coordination = await BrowserDirectZipCoordination.open({
    parent, manager: windowPort.navigator.locks, operationId: operation.intent.operationId,
    ...(dependencies.trace === undefined ? {} : { trace: dependencies.trace }),
  })
  try {
    await coordination.claimFile(envelope.binding.fileBinding.persistedHandle)
    return await BrowserDirectZipOperation.open({
      intent: operation.intent as DirectZipIntent, lifecycle: operation.lifecycle,
      leaseId: operation.lease.leaseId, repository: operation.repository, journal: operation.journal,
      checkpoint: operation.checkpoint, binding: envelope.binding, facts,
      namespaceMutations: coordination.mutations,
      close: async () => { try { await coordination.close() } finally { await operation.close() } },
      ...(dependencies.trace === undefined ? {} : { trace: dependencies.trace }),
    })
  } catch (error) { await coordination.close(); throw error }
}

async function recoverBootstrap(windowPort: BrowserReceiveWindow, descriptor: DirectZipBootstrapResumeDescriptorV1,
  signal: AbortSignal, dependencies: Dependencies) {
  signal.throwIfAborted()
  const storage = await openStorage(windowPort, descriptor.operationId, dependencies)
  const { repository, journal } = storage
  let coordination: BrowserDirectZipCoordination | undefined
  try {
    const current = await journal.readCandidate(descriptor.operationId, descriptor.candidateId)
    if (current?.kind !== 'bootstrap' || current.digest !== descriptor.candidateDigest) return
    const envelope = await readEnvelope(repository, descriptor.operationId)
    requireBootstrapEnvelope(current, envelope)
    const parent = envelope.candidate.parentBinding.persistedHandle
    coordination = await BrowserDirectZipCoordination.open({
      parent, manager: windowPort.navigator.locks, operationId: descriptor.operationId,
      ...(dependencies.trace === undefined ? {} : { trace: dependencies.trace }),
    })
    const ownedCoordination = coordination
    const lease = receiveOperationLeaseRecord({
      operationId: descriptor.operationId, leaseId: randomId(), acquiredAt: Date.now(),
    })
    const candidate = await createDirectZipBootstrapCandidateV1({
      ...current, leaseId: lease.leaseId, leaseGeneration: current.leaseGeneration + 1n,
    })
    await journal.replaceBootstrapLease({ expectedCandidate: current, candidate, lease })
    const target = browserTarget({
      leaseId: lease.leaseId, parentLocks: coordination.parentLocks,
      claimFile: file => ownedCoordination.claimFile(file),
      reservations: {
        persistCandidate: async () => { throw new Error('Recovery cannot reserve another target') },
        retireCandidate: async () => { throw new DOMException('ZIP bootstrap target changed', 'DataError') },
      },
    })
    const reopened = await target.resumeBootstrap({
      candidate: envelope.candidate, currentParent: parent, trustedAction: false,
    })
    if (reopened.kind !== 'ready' || 'disposition' in reopened.value) {
      throw new DOMException('An interrupted ZIP target requires verification', 'InvalidStateError')
    }
    await commitBootstrap({
      repository, journal, candidate, lease, binding: reopened.value.binding, coordination,
      envelope: { ...envelope, binding: reopened.value.binding },
    })
  } finally {
    try { await coordination?.close() } finally { await storage.close() }
  }
}

async function openStorage(windowPort: BrowserReceiveWindow, operationId: string, dependencies: Dependencies) {
  const unlock = await acquireOperationLock(windowPort, operationId)
  let repository: ReceiveOperationRepository | undefined
  let journal: DirectZipJournalRepository | undefined
  try {
    repository = await dependencies.openRepository()
    journal = await dependencies.openJournal()
    const ownedRepository = repository
    const ownedJournal = journal
    return {
      repository: ownedRepository, journal: ownedJournal,
      close: async () => {
        try { await unlock() } finally { ownedJournal.close(); ownedRepository.close() }
      },
    }
  } catch (error) {
    try { await unlock() } finally { journal?.close(); repository?.close() }
    throw error
  }
}

import { encodeBase64Url } from '../../../src/crypto/bytes'
import {
  createDirectorySelectionResultRoot, createDirectResumableZipPlan, createFSAOwnedFileBinding,
  createSelectionSpec, createZipArchiveArtifact, deriveArtifactChoiceIdentity,
} from '../../../src/transfer/intent'
import {
  bindReceiveIntent, materializationPlanSemantics,
  type OfferedArtifactChoice, type ResolvedArtifactAction,
} from '../../../src/output/planning'
import {
  admitDirectZipRuntimeV1,
} from '../../../src/output/direct-zip/session'
import { IndexedDbReceiveOperationRepository } from '../../../src/output/browser/indexeddb-repository'
import { IndexedDbDirectZipJournalRepository, directZipBootstrapResumeDescriptorV1 } from '../../../src/output/direct-zip/journal'
import { acquireBrowserReceiveOperationLease } from '../../../src/output/browser/session-lease'
import { decodeStoredReceiveLifecycleState } from '../../../src/output/workspace/state-codec'
import type { ReopenedDirectZipOperation } from '../../../src/output/resume/reopen-authority'
import {
  createBrowserDirectZipComposition,
} from '../../../src/ui/browser-receive/direct-zip/production'
import { observeBrowserDirectZipFeatureFacts, BROWSER_DIRECT_ZIP_TARGET_ROUTE_ID } from '../../../src/ui/browser-receive/direct-zip/support'
import { readEnvelope } from '../../../src/ui/browser-receive/direct-zip/resources'
import { createBrowserReceiveComposition } from '../../../src/ui/v2-browser-receive-composition'
import type { BrowserReceiveWindow } from '../../../src/ui/browser-receive/contracts'
import type { DirectZipIntent, DirectZipOrderedFileV1 } from '../../../src/transfer/direct-zip'
import type { V2BoundReceiveOperation } from '../../../src/ui/v2-receive-runtime'

const id = (width: number, fill: number) => encodeBase64Url(new Uint8Array(width).fill(fill))
const signal = new AbortController().signal

type ProductionMode = 'pause-resume' | 'complete' | 'delete' | 'delete-retry' | 'unpromoted-resume' |
  'unpromoted-delete' | 'unpromoted-continue' | 'unpromoted-settle' | 'bootstrap-recovery'

export async function probeBrowserDirectZipProduction(databaseName: string, mode: ProductionMode) {
  const root = await navigator.storage.getDirectory()
  const parent = await root.getDirectoryHandle(databaseName, { create: true })
  const originalPicker = Object.getOwnPropertyDescriptor(window, 'showDirectoryPicker')
  Object.defineProperty(window, 'showDirectoryPicker', {
    configurable: true, value: async () => parent,
  })
  const windowPort = window as BrowserReceiveWindow
  const openRepository = () => IndexedDbReceiveOperationRepository.open(databaseName)
  const openJournal = faultingJournal(databaseName, mode)
  const directZip = createBrowserDirectZipComposition(windowPort, { openRepository, openJournal })
  const receiver = createBrowserReceiveComposition(windowPort, { directZip })
  let active: V2BoundReceiveOperation | undefined
  try {
    const environment = await receiver.environment(signal)
    const source = await directZip.capabilities.read(signal)
    const admission = await admitDirectZipRuntimeV1({
      capabilities: { featureFacts: observeBrowserDirectZipFeatureFacts(windowPort), authority: source.authority },
    })
    if (admission.kind !== 'available') throw new Error('Browser Direct ZIP capability admission failed')
    const rootId = id(16, 2)
    const selection = await createSelectionSpec({
      shareInstance: id(16, 3), syntheticRoot: rootId,
      rules: { mode: 'node-id', defaultSelected: true, rules: [] },
    })
    const artifact = await createZipArchiveArtifact(createDirectorySelectionResultRoot(rootId, 'shared'))
    const seedBinding = await createFSAOwnedFileBinding({
      operationId: id(16, 4), targetRef: id(32, 5), artifact,
      stableName: 'shared.windshare-' + id(16, 6) + '.zip', policies: admission.facts.support.policies,
    })
    const seedPlan = await createDirectResumableZipPlan(artifact, seedBinding)
    const choiceIdentity = await deriveArtifactChoiceIdentity(artifact, seedPlan)
    const route = {
      kind: 'direct-resumable-zip' as const,
      target: environment.targets.find(target => target.routeId === BROWSER_DIRECT_ZIP_TARGET_ROUTE_ID)!,
    } as OfferedArtifactChoice['route']
    const choice = {
      choiceId: choiceIdentity.id, artifactKind: 'zip-archive',
      plan: materializationPlanSemantics(route),
    } as OfferedArtifactChoice['choice']
    const offered = { route, choice } as OfferedArtifactChoice
    const action = {
      kind: 'resolved-artifact-action', choiceId: choiceIdentity.id, choice, route, artifact,
      selectionDigest: selection.digest, resolvedArtifactDigest: artifact.digest,
    } as ResolvedArtifactAction
    const presentation = receiver.startArtifactAuthority(offered, [choiceIdentity.id])
    await presentation.ready
    const activated = await presentation.commit({
      action, signal, freezeAtFence: candidate => bindReceiveIntent({ selection, action, candidate }),
    })
    if (activated.kind === 'owned-effects' && mode === 'bootstrap-recovery') {
      await activated.authority.detach()
      const inventory = await openJournal()
      const candidates = []
      for await (const candidate of inventory.streamBootstrapCandidates()) candidates.push(candidate)
      inventory.close()
      if (candidates.length !== 1) throw new Error('Interrupted bootstrap lost its candidate')
      await directZip.runtime.dispatchBootstrapCandidate(directZipBootstrapResumeDescriptorV1(candidates[0]!), signal)
      active = await directZip.runtime.resume(
        await reopenPersisted(activated.authority.intent as DirectZipIntent, databaseName), signal)
    } else if (activated.kind === 'bound-operation') {
      active = activated.operation
    } else {
      if (activated.kind === 'owned-effects') throw activated.cause
      throw new Error('Direct ZIP activation did not bind an operation')
    }
    const intent = active.intent as DirectZipIntent
    const first = await active.plans.openDirectResumableZip(intent, signal)
    const authenticatedRoot = { directoryId: rootId, generation: id(16, 7),
      discoveryEvidence: new TextEncoder().encode('authenticated-root-generation') }
    const member = {
      kind: 'file', fileId: id(16, 8), expectedSize: 6n,
      sourcePath: ['a.txt'], artifactPath: ['shared', 'a.txt'],
      layoutEvidence: new TextEncoder().encode('layout-a'), discoveryEvidence: new TextEncoder().encode('member-a'),
      pending: {},
    } as unknown as DirectZipOrderedFileV1
    const sourceFile = { fileId: member.fileId, revision: id(16, 9), exactSize: 6n, rangeAuthority: id(32, 10) }
    await first.ordered.beginTraversal(authenticatedRoot, signal)
    await first.ordered.visit(1n, member, signal)
    const transaction = await first.output.beginFile(member, sourceFile, signal)
    await transaction.write(0n, Uint8Array.of(1, 2, 3), signal)
    let resumeOffset = 0n
    let execution = first
    if (mode !== 'complete' && mode !== 'bootstrap-recovery') {
      try {
        await first.pause({ worker: {} as never, materialization: first.ordered.materializationSummary(),
          selectionFacts: { discoveredFileCount: 1n, discoveredBytes: 6n, discovery: 'complete' },
          reason: new DOMException('User paused', 'AbortError') }, signal)
      } catch (error) {
        if (!mode.startsWith('unpromoted-')) throw error
      }
      const previous = active
      active = undefined
      active = await continuePausedOperation(previous, directZip, databaseName, mode, parent)
      if (active === undefined) {
        return { mode, contents: await entryNames(parent), directSupport: environment.directZipSupport.kind }
      }
      execution = await active.plans.openDirectResumableZip(intent, signal)
      await execution.ordered.beginTraversal(authenticatedRoot, signal)
      await execution.ordered.visit(1n, member, signal)
      const resumed = await execution.output.beginFile(member, sourceFile, signal)
      resumeOffset = resumed.resumeOffset
      await resumed.write(resumeOffset, Uint8Array.of(4, 5, 6), signal)
      await resumed.commit(signal)
    } else {
      await transaction.write(3n, Uint8Array.of(4, 5, 6), signal)
      await transaction.commit(signal)
    }
    await execution.ordered.finishTraversal(2n, signal)
    const lifecycle = await execution.settle({
      transferJobId: active.transferJobId, worker: {} as never,
      materialization: execution.ordered.materializationSummary(),
    }, signal)
    const read = await openRepository()
    const envelope = await readEnvelope(read, intent.operationId)
    read.close()
    const file = await parent.getFileHandle(envelope.candidate.stableName)
    const saved = await file.getFile()
    const archive = new Uint8Array(await saved.arrayBuffer())
    return { mode, lifecycle: lifecycle.kind, resumeOffset: resumeOffset.toString(),
      fileBytes: saved.size, signature: Array.from(archive.slice(-22, -18)),
      directSupport: environment.directZipSupport.kind }
  } finally {
    await active?.detach()
    if (originalPicker === undefined) Reflect.deleteProperty(window, 'showDirectoryPicker')
    else Object.defineProperty(window, 'showDirectoryPicker', originalPicker)
    await root.removeEntry(databaseName, { recursive: true })
  }
}

async function reopenPersisted(intent: DirectZipIntent, databaseName: string): Promise<ReopenedDirectZipOperation> {
  const repository = await IndexedDbReceiveOperationRepository.open(databaseName)
  const journal = await IndexedDbDirectZipJournalRepository.open({ databaseName })
  const lease = await acquireBrowserReceiveOperationLease(repository, intent.operationId, {
    acquisitionTransitionCommitter: { commitAcquisitionTransition: transition => journal.commitLeaseAcquisition(transition) },
  })
  const state = await journal.readState(intent.operationId)
  const lifecycleRecord = await repository.readLifecycle(intent.operationId)
  if (state === undefined || lifecycleRecord === undefined) throw new Error('ZIP lost its persisted checkpoint')
  return {
    kind: 'direct-zip', intent, lifecycle: decodeStoredReceiveLifecycleState(lifecycleRecord),
    lease, repository, journal, checkpoint: state.checkpoint,
    close: async () => { try { await lease.release() } finally { repository.close(); journal.close() } },
  }
}

function faultingJournal(databaseName: string, mode: ProductionMode) {
  let promotionFailurePending = mode.startsWith('unpromoted-')
  let bootstrapFailurePending = mode === 'bootstrap-recovery'
  let deleteFailurePending = mode === 'delete-retry'
  return async () => {
    const repository = await IndexedDbDirectZipJournalRepository.open({ databaseName })
    return new Proxy(repository, {
      get: (target, property) => {
        if (property === 'commitBootstrap') return async (
          cut: Parameters<IndexedDbDirectZipJournalRepository['commitBootstrap']>[0],
        ) => {
          if (bootstrapFailurePending) {
            bootstrapFailurePending = false
            throw new DOMException('Injected loss after bootstrap close', 'UnknownError')
          }
          return target.commitBootstrap(cut)
        }
        if (property === 'promoteCandidate') return async (
          cut: Parameters<IndexedDbDirectZipJournalRepository['promoteCandidate']>[0],
        ) => {
          if (promotionFailurePending) {
            promotionFailurePending = false
            throw new DOMException('Injected loss after filesystem close', 'UnknownError')
          }
          return target.promoteCandidate(cut)
        }
        if (property === 'commitRecoveryLifecycle') return async (
          cut: Parameters<IndexedDbDirectZipJournalRepository['commitRecoveryLifecycle']>[0],
        ) => {
          if (deleteFailurePending && cut.lifecycle.kind === 'discarded') {
            deleteFailurePending = false
            throw new DOMException('Injected loss after physical deletion', 'UnknownError')
          }
          return target.commitRecoveryLifecycle(cut)
        }
        const value = Reflect.get(target, property)
        return typeof value === 'function' ? value.bind(target) : value
      },
    })
  }
}

async function continuePausedOperation(active: V2BoundReceiveOperation,
  directZip: ReturnType<typeof createBrowserDirectZipComposition>, databaseName: string,
  mode: ProductionMode, parent: FileSystemDirectoryHandle): Promise<V2BoundReceiveOperation | undefined> {
  if (mode === 'unpromoted-settle') {
    const settled = await active.settleTransferAdmissionFailure(new Error('Filesystem close outlived its journal promotion'))
    if (settled.lifecycle.kind !== 'resumable-receive') throw new Error('Candidate failure did not preserve a resumable checkpoint')
  }
  if (mode === 'unpromoted-continue' || mode === 'unpromoted-settle') {
    await active.startLifecycleAction('continue', active.lifecycle)
    return active
  }
  try {
    if (mode === 'delete-retry') {
      let rejected = false
      try { await active.startLifecycleAction('delete', active.lifecycle) } catch { rejected = true }
      if (!rejected || (await entryNames(parent)).length !== 0) throw new Error('Deletion did not reach the injected journal failure')
    }
  } finally { await active.detach() }
  const reopened = await reopenPersisted(active.intent as DirectZipIntent, databaseName)
  if (mode === 'delete' || mode === 'delete-retry' || mode === 'unpromoted-delete') {
    await directZip.runtime.deleteRetained(reopened, signal)
    const repository = await IndexedDbReceiveOperationRepository.open(databaseName)
    try {
      const record = await repository.readLifecycle(active.intent.operationId)
      if (record === undefined || decodeStoredReceiveLifecycleState(record).kind !== 'discarded') {
        throw new Error('Retained cleanup did not durably discard the operation')
      }
    } finally { repository.close() }
    return undefined
  }
  return directZip.runtime.resume(reopened, signal)
}

async function entryNames(parent: FileSystemDirectoryHandle) {
  const names = []
  for await (const entry of parent.values()) names.push(entry.name)
  return names
}

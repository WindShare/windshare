import { encodeBase64Url } from '../../../src/crypto/bytes'
import { DEFAULT_DIRECT_ZIP_RUNTIME_POLICY_V1 } from '../../../src/output/direct-zip/session'
import { IndexedDbReceiveOperationRepository } from '../../../src/output/browser/indexeddb-repository'
import { IndexedDbReceiveResumeSource } from '../../../src/output/browser/indexeddb-resume-state'
import { IndexedDbDirectZipJournalRepository, directZipBootstrapResumeDescriptorV1 } from '../../../src/output/direct-zip/journal'
import { acquireBrowserReceiveOperationLease } from '../../../src/output/browser/session-lease'
import { decodeStoredReceiveLifecycleState } from '../../../src/output/workspace/state-codec'
import type { ReopenedDirectZipOperation } from '../../../src/output/resume/reopen-authority'
import { ReceiveOperationResumeAuthority } from '../../../src/output/resume/authority'
import {
  createBrowserDirectZipComposition,
} from '../../../src/ui/browser-receive/direct-zip/production'
import { readEnvelope } from '../../../src/ui/browser-receive/direct-zip/resources'
import { createBrowserReceiveComposition } from '../../../src/ui/v2-browser-receive-composition'
import type { BrowserReceiveWindow } from '../../../src/ui/browser-receive/contracts'
import type { DirectZipIntent, DirectZipOrderedFileV1 } from '../../../src/transfer/direct-zip'
import type { V2BoundReceiveOperation } from '../../../src/ui/v2-receive-runtime'
import { observeProductionDirectZipFileSystem } from './production-fsa-observation'
import { observeProductionDirectZipProgress } from './production-progress-observation'
import { prepareProductionDirectZipActivation } from './production-activation'

const id = (width: number, fill: number) => encodeBase64Url(new Uint8Array(width).fill(fill))
const signal = new AbortController().signal
const COMPLETION_FAULT_MESSAGE = 'Injected completion promotion loss after final close'
const SPACING_FIRST_WRITE_BYTES = 1_024
const SPACING_LATER_WRITE_BYTES = 256
const SPACING_TOTAL_BYTES = SPACING_FIRST_WRITE_BYTES + 2 * SPACING_LATER_WRITE_BYTES

type ProductionMode = 'pause-resume' | 'complete' | 'delete' | 'delete-retry' | 'unpromoted-resume' |
  'unpromoted-delete' | 'unpromoted-continue' | 'unpromoted-settle' | 'bootstrap-recovery' |
  'completion-journal-recovery' | 'completion-acknowledgement-recovery' | 'completion-continue' |
  'aborted-write-continue' | 'automatic-checkpoint-spacing' | 'activation-recovery'

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
  const installed = createBrowserDirectZipComposition(windowPort, { openRepository, openJournal })
  const directZip = mode === 'automatic-checkpoint-spacing' ? {
    ...installed,
    capabilities: { read: async (signal: AbortSignal) => ({
      ...await installed.capabilities.read(signal),
      policy: { ...DEFAULT_DIRECT_ZIP_RUNTIME_POLICY_V1, automaticEpochPolicy: { minimumAdvanceBytes: 1n } },
    }) },
  } : installed
  const receiver = createBrowserReceiveComposition(windowPort, { directZip })
  const fileSystem = observeProductionDirectZipFileSystem()
  const payload = mode === 'automatic-checkpoint-spacing'
    ? Uint8Array.from({ length: SPACING_TOTAL_BYTES }, (_, index) => index % 251)
    : Uint8Array.of(1, 2, 3, 4, 5, 6)
  const firstWriteBytes = mode === 'automatic-checkpoint-spacing' ? SPACING_FIRST_WRITE_BYTES : 3
  const progress = observeProductionDirectZipProgress(BigInt(payload.byteLength))
  let active: V2BoundReceiveOperation | undefined
  try {
    const activation = await prepareProductionDirectZipActivation(windowPort, receiver, directZip, signal)
    const { environment, rootId } = activation
    active = await resolveProductionActivation(await activation.commit(), mode, directZip, databaseName, openJournal)
    const intent = active.intent as DirectZipIntent
    progress.bind(active)
    progress.sample('initial', active)
    const transferred = await materializeProductionPayload({ active, rootId, payload, firstWriteBytes,
      mode, directZip, databaseName, parent, progress, fileSystem })
    active = transferred.active
    if (active === undefined) {
      return { mode, contents: await entryNames(parent), directSupport: environment.directZipSupport.kind }
    }
    const { execution, resumeOffset } = transferred
    progress.sample('before-finalization', active)
    const beforeFinalization = fileSystem.snapshot()
    await execution.ordered.finishTraversal(2n, signal)
    const settle = () => execution.settle({
      transferJobId: active!.transferJobId, worker: {} as never,
      materialization: execution.ordered.materializationSummary(),
    }, signal)
    let lifecycle
    let recovery
    if (mode.startsWith('completion-')) {
      await requireCompletionPromotionFailure(settle)
      progress.sample('failed-finalization', active)
      const beforeRecovery = fileSystem.snapshot()
      const closed = await inspectInterruptedCompletion(databaseName, intent, parent)
      const recovered = await continueCompletedOperation(active, directZip, databaseName, mode)
      active = recovered.active
      lifecycle = recovered.lifecycle
      recovery = { before: beforeRecovery, after: fileSystem.snapshot(), resumeTransfer: recovered.resumeTransfer,
        ...closed }
    } else {
      lifecycle = await settle()
    }
    progress.sample('published', active, lifecycle)
    const read = await openRepository()
    const envelope = await readEnvelope(read, intent.operationId)
    read.close()
    const file = await parent.getFileHandle(envelope.candidate.stableName)
    const saved = await file.getFile()
    const archive = new Uint8Array(await saved.arrayBuffer())
    return { mode, lifecycle: lifecycle.kind, resumeOffset: resumeOffset.toString(),
      fileBytes: saved.size, signature: Array.from(archive.slice(-22, -18)),
      archive: Array.from(archive),
      finalization: { before: beforeFinalization, after: recovery?.before ?? fileSystem.snapshot() },
      recovery, progress: progress.result(),
      directSupport: environment.directZipSupport.kind }
  } finally {
    await active?.detach()
    progress.close()
    fileSystem.restore()
    if (originalPicker === undefined) Reflect.deleteProperty(window, 'showDirectoryPicker')
    else Object.defineProperty(window, 'showDirectoryPicker', originalPicker)
    await root.removeEntry(databaseName, { recursive: true })
  }
}

async function resolveProductionActivation(
  activated: Awaited<ReturnType<Awaited<ReturnType<typeof prepareProductionDirectZipActivation>>['commit']>>,
  mode: ProductionMode,
  directZip: ReturnType<typeof createBrowserDirectZipComposition>,
  databaseName: string,
  openJournal: ReturnType<typeof faultingJournal>,
): Promise<V2BoundReceiveOperation> {
  if (activated.kind === 'owned-effects' && mode === 'bootstrap-recovery') {
    await activated.authority.detach()
    const inventory = await openJournal()
    const candidates = []
    for await (const candidate of inventory.streamBootstrapCandidates()) candidates.push(candidate)
    inventory.close()
    if (candidates.length !== 1) throw new Error('Interrupted bootstrap lost its candidate')
    await directZip.runtime.dispatchBootstrapCandidate(directZipBootstrapResumeDescriptorV1(candidates[0]!), signal)
    return directZip.runtime.resume(
      await reopenPersisted(activated.authority.intent as DirectZipIntent, databaseName), signal)
  } else if (activated.kind === 'owned-effects' && mode === 'activation-recovery') {
    if (!(activated.cause instanceof Error) || activated.cause.message !== 'Injected receive activation failure') {
      throw new Error('The receive activation failure was not preserved')
    }
    const settled = await activated.authority.settleActivationFailure(activated.cause)
    if (settled.lifecycle.kind !== 'resumable-receive') throw new Error('Activation lost its resumable checkpoint')
    await activated.authority.detach()
    return directZip.runtime.resume(
      await reopenPersisted(activated.authority.intent as DirectZipIntent, databaseName), signal)
  } else if (activated.kind === 'bound-operation') {
    if (mode === 'activation-recovery') throw new Error('Receive activation bypassed the injected failure')
    return activated.operation
  } else {
    if (activated.kind === 'owned-effects') throw activated.cause
    throw new Error('Direct ZIP activation did not bind an operation')
  }
}

async function materializeProductionPayload({ active, rootId, payload, firstWriteBytes, mode,
  directZip, databaseName, parent, progress, fileSystem }: {
  active: V2BoundReceiveOperation
  rootId: string
  payload: Uint8Array
  firstWriteBytes: number
  mode: ProductionMode
  directZip: ReturnType<typeof createBrowserDirectZipComposition>
  databaseName: string
  parent: FileSystemDirectoryHandle
  progress: ReturnType<typeof observeProductionDirectZipProgress>
  fileSystem: ReturnType<typeof observeProductionDirectZipFileSystem>
}) {
  const intent = active.intent as DirectZipIntent
  const first = await active.plans.openDirectResumableZip(intent, signal)
  const authenticatedRoot = { directoryId: rootId, generation: id(16, 7),
    discoveryEvidence: new TextEncoder().encode('authenticated-root-generation') }
  const member = {
    kind: 'file', fileId: id(16, 8), expectedSize: BigInt(payload.byteLength),
    sourcePath: ['a.txt'], artifactPath: ['shared', 'a.txt'],
    layoutEvidence: new TextEncoder().encode('layout-a'), discoveryEvidence: new TextEncoder().encode('member-a'),
    pending: {},
  } as unknown as DirectZipOrderedFileV1
  const sourceFile = { fileId: member.fileId, revision: id(16, 9),
    exactSize: member.expectedSize, rangeAuthority: id(32, 10) }
  await first.ordered.beginTraversal(authenticatedRoot, signal)
  await first.ordered.visit(1n, member, signal)
  const transaction = await first.output.beginFile(member, sourceFile, signal)
  progress.sample('metadata', active)
  await transaction.write(0n, payload.slice(0, firstWriteBytes), signal)
  progress.sample('first-write', active)
  let resumeOffset = 0n
  let execution = first
  if (mode !== 'complete' && mode !== 'bootstrap-recovery' &&
      mode !== 'automatic-checkpoint-spacing' && mode !== 'activation-recovery' && !mode.startsWith('completion-')) {
    try {
      if (mode === 'aborted-write-continue') {
        const failure = new DOMException('Injected write acknowledgement loss', 'UnknownError')
        fileSystem.rejectNextWrite(failure)
        let rejected = false
        try { await transaction.write(BigInt(firstWriteBytes), payload.slice(firstWriteBytes), signal) }
        catch (error) {
        if (error !== failure) throw error
        rejected = true
      }
        if (!rejected) throw new Error('The injected write failure was not reached')
        progress.sample('failed-write', active)
      }
      await first.pause({ worker: {} as never, materialization: first.ordered.materializationSummary(),
        selectionFacts: { discoveredFileCount: 1n, discoveredBytes: member.expectedSize, discovery: 'complete' },
        reason: new DOMException('User paused', 'AbortError') }, signal)
    } catch (error) {
      if (!mode.startsWith('unpromoted-')) throw error
    }
    progress.sample('paused', active)
    const continued = await continuePausedOperation(active, directZip, databaseName, mode, parent)
    if (continued === undefined) return { active: undefined, execution, resumeOffset }
    active = continued
    progress.bind(active)
    progress.sample('continued', active)
    execution = await active.plans.openDirectResumableZip(intent, signal)
    await execution.ordered.beginTraversal(authenticatedRoot, signal)
    await execution.ordered.visit(1n, member, signal)
    const resumed = await execution.output.beginFile(member, sourceFile, signal)
    resumeOffset = resumed.resumeOffset
    progress.sample('resumed-member', active)
    await resumed.write(resumeOffset, payload.slice(Number(resumeOffset)), signal)
    await resumed.commit(signal)
  } else if (mode === 'automatic-checkpoint-spacing') {
    await transaction.observeCheckpoint(signal)
    progress.sample('automatic-checkpoint', active)
    for (let offset = firstWriteBytes; offset < payload.byteLength; offset += SPACING_LATER_WRITE_BYTES) {
      await transaction.write(BigInt(offset), payload.slice(offset, offset + SPACING_LATER_WRITE_BYTES), signal)
      await transaction.observeCheckpoint(signal)
      progress.sample('spaced-write-' + offset, active)
    }
    await transaction.commit(signal)
  } else {
    await transaction.write(BigInt(firstWriteBytes), payload.slice(firstWriteBytes), signal)
    await transaction.commit(signal)
  }
  return { active, execution, resumeOffset }
}

async function continueCompletedOperation(active: V2BoundReceiveOperation,
  directZip: ReturnType<typeof createBrowserDirectZipComposition>, databaseName: string, mode: ProductionMode) {
  // A complete archive must recover with only local authority, even if the sender is gone.
  if (mode === 'completion-continue') {
    const continued = await active.startLifecycleAction('continue', active.lifecycle)
    return { active, lifecycle: continued.lifecycle, resumeTransfer: continued.resumeTransfer }
  }
  await active.detach()
  const reopened = await directZip.runtime.resume(
    await reopenPersisted(active.intent as DirectZipIntent, databaseName), signal)
  return { active: reopened, lifecycle: reopened.lifecycle, resumeTransfer: undefined }
}

async function requireCompletionPromotionFailure(settle: () => Promise<unknown>) {
  try { await settle() } catch (error) {
    if (error instanceof DOMException && error.message === COMPLETION_FAULT_MESSAGE) return
    throw error
  }
  throw new Error('Completion did not reach the injected journal failure')
}

async function inspectInterruptedCompletion(databaseName: string, intent: DirectZipIntent,
  parent: FileSystemDirectoryHandle) {
  const repository = await IndexedDbDirectZipJournalRepository.open({ databaseName })
  const operations = await IndexedDbReceiveOperationRepository.open(databaseName)
  try {
    const pending = await repository.readState(intent.operationId)
    const candidate = await repository.readOperationCandidate(intent.operationId)
    if (pending === undefined || (candidate?.kind !== 'closing' &&
      pending.checkpoint.closingReplay?.completion === undefined)) {
      throw new Error('The final close lost its completion candidate')
    }
    const lifecycle = await operations.readLifecycle(intent.operationId)
    if (lifecycle === undefined) throw new Error('The final close lost its operation lifecycle')
    const names = await entryNames(parent)
    const completedFile = await (await parent.getFileHandle(names[0]!)).getFile()
    return {
      candidateKind: candidate?.kind,
      lifecycleBeforeResume: decodeStoredReceiveLifecycleState(lifecycle).kind,
      continuationBeforeResume: await readCompletionContinuation(databaseName, intent.operationId),
      completedArchive: Array.from(new Uint8Array(await completedFile.arrayBuffer())),
      safePayloadBefore: pending.checkpoint.committedSelectedPayloadBytes.toString(),
      candidatePayload: candidate?.kind === 'closing'
        ? candidate.proposedCheckpoint.committedSelectedPayloadBytes.toString() : undefined,
    }
  } finally { operations.close(); repository.close() }
}

async function readCompletionContinuation(databaseName: string, operationId: string) {
  const source = await IndexedDbReceiveResumeSource.open(databaseName)
  const unexpectedMutation = async (): Promise<never> => { throw new Error('Inventory attempted a mutation') }
  try {
    const authority = new ReceiveOperationResumeAuthority({ source,
      mutations: { resume: unexpectedMutation, cleanup: unexpectedMutation, discard: unexpectedMutation } })
    const inventory = await authority.listResumeState()
    try {
      const retained = inventory.operations.find(operation => operation.descriptor.operationId === operationId)
      if (retained === undefined) throw new Error('The finished archive disappeared from retained inventory')
      return retained.descriptor.continuation
    } finally { inventory.close() }
  } finally { source.close() }
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
  let activationFailurePending = mode === 'activation-recovery'
  let completionFailurePending = mode.startsWith('completion-')
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
          if (completionFailurePending && cut.candidate.kind === 'closing') {
            completionFailurePending = false
            if (mode === 'completion-acknowledgement-recovery') await target.promoteCandidate(cut)
            throw new DOMException(COMPLETION_FAULT_MESSAGE, 'UnknownError')
          }
          if (promotionFailurePending) {
            promotionFailurePending = false
            throw new DOMException('Injected loss after filesystem close', 'UnknownError')
          }
          return target.promoteCandidate(cut)
        }
        if (property === 'commitRecoveryLifecycle') return async (
          cut: Parameters<IndexedDbDirectZipJournalRepository['commitRecoveryLifecycle']>[0],
        ) => {
          if (activationFailurePending && cut.lifecycle.kind === 'receiving') {
            activationFailurePending = false
            throw new DOMException('Injected receive activation failure', 'UnknownError')
          }
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
  if (mode === 'unpromoted-continue' || mode === 'unpromoted-settle' || mode === 'aborted-write-continue') {
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
  const resumed = await directZip.runtime.resume(reopened, signal)
  if (resumed.lifecycle.kind !== 'receiving') {
    throw new Error('Paused ZIP resume did not restore receiving before execution adoption')
  }
  return resumed
}

async function entryNames(parent: FileSystemDirectoryHandle) {
  const names = []
  for await (const entry of parent.values()) names.push(entry.name)
  return names
}
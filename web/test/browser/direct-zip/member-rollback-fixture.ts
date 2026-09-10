import { IndexedDbReceiveOperationRepository } from '../../../src/output/browser/indexeddb-repository'
import { acquireBrowserReceiveOperationLease } from '../../../src/output/browser/session-lease'
import { IndexedDbDirectZipJournalRepository, type DirectZipJournalRepository } from '../../../src/output/direct-zip/journal'
import type { ReopenedDirectZipOperation } from '../../../src/output/resume/reopen-authority'
import { decodeStoredReceiveLifecycleState } from '../../../src/output/workspace/state-codec'
import type { DirectZipIntent } from '../../../src/transfer/direct-zip'
import type { BrowserReceiveWindow } from '../../../src/ui/browser-receive/contracts'
import { createBrowserDirectZipComposition } from '../../../src/ui/browser-receive/direct-zip/production'
import { readEnvelope } from '../../../src/ui/browser-receive/direct-zip/resources'
import { createBrowserReceiveComposition } from '../../../src/ui/v2-browser-receive-composition'
import type { V2BoundReceiveOperation } from '../../../src/ui/v2-receive-runtime'
import { prepareProductionDirectZipActivation } from './production-activation'
import { memberRollbackSource, MEMBER_ROLLBACK_PAUSE, type MemberRollbackRevisionMode } from './member-rollback-source'

export const MEMBER_ROLLBACK_SIGNAL = new AbortController().signal

export async function createMemberRollbackFixture(databaseName: string, mode: MemberRollbackRevisionMode) {
  const root = await navigator.storage.getDirectory()
  const parent = await root.getDirectoryHandle(databaseName, { create: true })
  const originalPicker = Object.getOwnPropertyDescriptor(window, 'showDirectoryPicker')
  Object.defineProperty(window, 'showDirectoryPicker', { configurable: true, value: async () => parent })
  const windowPort = window as BrowserReceiveWindow
  const directZip = createBrowserDirectZipComposition(windowPort, {
    openRepository: () => IndexedDbReceiveOperationRepository.open(databaseName),
    openJournal: () => IndexedDbDirectZipJournalRepository.open({ databaseName }),
  })
  const receiver = createBrowserReceiveComposition(windowPort, { directZip })
  let active: V2BoundReceiveOperation | undefined
  const close = async () => {
    await active?.detach()
    if (originalPicker === undefined) Reflect.deleteProperty(window, 'showDirectoryPicker')
    else Object.defineProperty(window, 'showDirectoryPicker', originalPicker)
    await root.removeEntry(databaseName, { recursive: true })
  }
  try {
    const activation = await prepareProductionDirectZipActivation(windowPort, receiver, directZip, MEMBER_ROLLBACK_SIGNAL)
    const activated = await activation.commit()
    if (activated.kind !== 'bound-operation') throw new Error('Production Direct ZIP activation did not bind')
    active = activated.operation
    const intent = active.intent as DirectZipIntent
    const initial = (await readMemberRollbackState(databaseName, intent.operationId)).checkpoint
    const source = memberRollbackSource(activation.rootId, mode)
    const first = await active.plans.openDirectResumableZip(intent, MEMBER_ROLLBACK_SIGNAL)
    let pausedMidMember = false
    try { await source.run(first, 'initial', MEMBER_ROLLBACK_SIGNAL) }
    catch (error) {
      if (error !== MEMBER_ROLLBACK_PAUSE) throw error
      pausedMidMember = true
    }
    if (!pausedMidMember) throw new Error('Transfer did not reach the active member partial block')
    await first.pause({
      worker: {} as never, materialization: first.ordered.materializationSummary(),
      selectionFacts: { discoveredFileCount: 3n, discoveredBytes: 12n, discovery: 'complete' },
      reason: MEMBER_ROLLBACK_PAUSE,
    }, MEMBER_ROLLBACK_SIGNAL)
    const paused = (await readMemberRollbackState(databaseName, intent.operationId)).checkpoint
    const pausedMember = paused.currentMember
    if (paused.phase !== 'inside-member' || pausedMember === undefined) {
      throw new Error('Pause did not persist an active-member checkpoint')
    }
    const repository = await IndexedDbReceiveOperationRepository.open(databaseName)
    const envelope = await readEnvelope(repository, intent.operationId)
    repository.close()
    const file = await parent.getFileHandle(envelope.candidate.stableName)
    const original = new Uint8Array(await (await file.getFile()).arrayBuffer())
    const rollbackOffset = Number(pausedMember.rollback.archiveOffset)
    await active.detach()
    active = undefined
    return {
      databaseName, intent, source, file, original, initial, paused, pausedMember, rollbackOffset,
      ownershipNonce: envelope.candidate.ownershipNonce, prefix: original.slice(0, rollbackOffset),
      resume: async (openJournal?: () => Promise<DirectZipJournalRepository>) => {
        await active?.detach()
        active = undefined
        active = await directZip.runtime.resume(
          await reopenMemberRollbackOperation(intent, databaseName, openJournal), MEMBER_ROLLBACK_SIGNAL)
        return active
      },
      detach: async () => { await active?.detach(); active = undefined },
      close,
    }
  } catch (error) {
    await close()
    throw error
  }
}

export async function readMemberRollbackState(databaseName: string, operationId: string) {
  const journal = await IndexedDbDirectZipJournalRepository.open({ databaseName })
  try {
    const state = await journal.readState(operationId)
    if (state === undefined) throw new Error('Direct ZIP checkpoint disappeared')
    return { checkpoint: state.checkpoint, candidate: await journal.readOperationCandidate(operationId) }
  } finally { journal.close() }
}

async function reopenMemberRollbackOperation(
  intent: DirectZipIntent, databaseName: string,
  openJournal: () => Promise<DirectZipJournalRepository> = () => IndexedDbDirectZipJournalRepository.open({ databaseName }),
): Promise<ReopenedDirectZipOperation> {
  const repository = await IndexedDbReceiveOperationRepository.open(databaseName)
  const journal = await openJournal()
  const lease = await acquireBrowserReceiveOperationLease(repository, intent.operationId, {
    acquisitionTransitionCommitter: { commitAcquisitionTransition: transition => journal.commitLeaseAcquisition(transition) },
  })
  const state = await journal.readState(intent.operationId)
  const lifecycle = await repository.readLifecycle(intent.operationId)
  if (state === undefined || lifecycle === undefined) throw new Error('Direct ZIP checkpoint or lifecycle disappeared')
  return {
    kind: 'direct-zip', intent, lifecycle: decodeStoredReceiveLifecycleState(lifecycle),
    lease, repository, journal, checkpoint: state.checkpoint,
    close: async () => { try { await lease.release() } finally { repository.close(); journal.close() } },
  }
}

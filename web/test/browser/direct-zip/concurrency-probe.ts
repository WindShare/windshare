import { encodeBase64Url } from '../../../src/crypto/bytes'
import { IndexedDbReceiveOperationRepository } from '../../../src/output/browser/indexeddb-repository'
import { acquireBrowserReceiveOperationLease } from '../../../src/output/browser/session-lease'
import { acquireFSAEntryMutationLease, FSAEntryMutationBusyError } from '../../../src/output/browser/namespace-mutation'
import { IndexedDbDirectZipJournalRepository } from '../../../src/output/direct-zip/journal'
import type { DirectZipIntent, DirectZipOrderedFileV1 } from '../../../src/transfer/direct-zip'
import type { BrowserReceiveWindow } from '../../../src/ui/browser-receive/contracts'
import {
  createBrowserDirectZipComposition, type BrowserDirectZipProductionOptions,
} from '../../../src/ui/browser-receive/direct-zip/production'
import { readEnvelope } from '../../../src/ui/browser-receive/direct-zip/resources'
import { createBrowserReceiveComposition } from '../../../src/ui/v2-browser-receive-composition'
import { prepareProductionDirectZipActivation } from './production-activation'
import { observeProductionDirectZipFileSystem } from './production-fsa-observation'

const id = (width: number, fill: number) => encodeBase64Url(new Uint8Array(width).fill(fill))
const signal = new AbortController().signal
const FIRST_WRITE_BYTES = 3
const ACTIVATION_FAILURE = 'Injected ZIP bootstrap persistence failure'

export interface ConcurrentZipInput {
  readonly databaseName: string
  readonly branch: string
  readonly savedParentOperationId?: string
  readonly payload: number[]
}

interface ArchiveResult {
  readonly operationId: string
  readonly stableName: string
  readonly lifecycle: string
  readonly archive: number[]
}

let running: {
  finish(): Promise<ArchiveResult>
  detach(): Promise<void>
  snapshot(): ReturnType<ReturnType<typeof observeProductionDirectZipFileSystem>['snapshot']>
} | undefined

export async function startProductionConcurrentZip(input: ConcurrentZipInput) {
  if (running !== undefined) throw new Error('The probe page already owns a ZIP')
  const parent = await selectedParent(input)
  const activation = await prepareActivation(input.databaseName, parent)
  const observation = observeProductionDirectZipFileSystem()
  const activated = await activation.commit().catch(error => { observation.restore(); throw error })
  if (activated.kind !== 'bound-operation') {
    if (activated.kind === 'owned-effects') await activated.authority.detach()
    observation.restore()
    throw new Error('Concurrent Direct ZIP activation did not bind an operation')
  }
  const active = activated.operation
  try {
    const intent = active.intent as DirectZipIntent
    const execution = await active.plans.openDirectResumableZip(intent, signal)
    const payload = Uint8Array.from(input.payload)
    const member = {
      kind: 'file', fileId: id(16, 8), expectedSize: BigInt(payload.byteLength),
      sourcePath: ['a.txt'], artifactPath: ['shared', 'a.txt'],
      layoutEvidence: new TextEncoder().encode('layout-a'),
      discoveryEvidence: new TextEncoder().encode('member-a'), pending: {},
    } as unknown as DirectZipOrderedFileV1
    await execution.ordered.beginTraversal({
      directoryId: activation.rootId, generation: id(16, 7),
      discoveryEvidence: new TextEncoder().encode('authenticated-root-generation'),
    }, signal)
    await execution.ordered.visit(1n, member, signal)
    const transaction = await execution.output.beginFile(member, {
      fileId: member.fileId, revision: id(16, 9), exactSize: member.expectedSize,
      rangeAuthority: id(32, 10),
    }, signal)
    await transaction.write(0n, payload.slice(0, FIRST_WRITE_BYTES), signal)
    // Returning only after the native write is the barrier: the next page must
    // activate while this archive still owns its unclosed writable and lease.
    running = {
      snapshot: observation.snapshot,
      detach: async () => { try { await active.detach() } finally { observation.restore() } },
      finish: async () => {
        await transaction.write(BigInt(FIRST_WRITE_BYTES), payload.slice(FIRST_WRITE_BYTES), signal)
        await transaction.commit(signal)
        await execution.ordered.finishTraversal(2n, signal)
        const lifecycle = await execution.settle({
          transferJobId: active.transferJobId, worker: {} as never,
          materialization: execution.ordered.materializationSummary(),
        }, signal)
        const envelope = await persistedEnvelope(input.databaseName, intent.operationId)
        const file = await (await parent.getFileHandle(envelope.candidate.stableName)).getFile()
        return { operationId: intent.operationId, stableName: envelope.candidate.stableName,
          lifecycle: lifecycle.kind, archive: Array.from(new Uint8Array(await file.arrayBuffer())) }
      },
    }
    return { operationId: intent.operationId, parentName: parent.name,
      directSupport: activation.environment.directZipSupport.kind, writer: observation.snapshot() }
  } catch (error) {
    try { await active.detach() } finally { observation.restore() }
    throw error
  }
}

export function productionConcurrentWriterSnapshot() {
  if (running === undefined) throw new Error('No ZIP is active on this page')
  return running.snapshot()
}

export async function finishProductionConcurrentZip() {
  if (running === undefined) throw new Error('No ZIP is active on this page')
  return running.finish()
}

export async function detachProductionConcurrentZip() {
  const current = running
  running = undefined
  await current?.detach()
}

export async function probeConcurrentOperationLease(databaseName: string, operationId: string) {
  const repository = await IndexedDbReceiveOperationRepository.open(databaseName)
  try {
    const lease = await acquireBrowserReceiveOperationLease(repository, operationId)
    await lease.release()
    return 'acquired'
  } catch (error) {
    if (error instanceof DOMException && error.name === 'InvalidStateError') return 'busy'
    throw error
  } finally { repository.close() }
}

export async function probeConcurrentTargetLease(databaseName: string, operationId: string,
  source: 'persisted-handle' | 'reacquired-handle') {
  const envelope = await persistedEnvelope(databaseName, operationId)
  const handle = source === 'persisted-handle' && envelope.binding !== undefined
    ? envelope.binding.fileBinding.persistedHandle
    : await envelope.candidate.parentBinding.persistedHandle.getFileHandle(envelope.candidate.stableName)
  try {
    const lease = await acquireFSAEntryMutationLease(handle)
    await lease.release()
    return { status: 'acquired' }
  } catch (error) {
    if (error instanceof FSAEntryMutationBusyError) {
      return { status: 'busy', message: error.message, scope: error.scope }
    }
    throw error
  }
}

export async function probeDeletedZipTargetLease(databaseName: string, operationId: string) {
  const envelope = await persistedEnvelope(databaseName, operationId)
  if (envelope.binding === undefined) throw new Error('ZIP has no persisted file binding')
  const parent = envelope.binding.parentBinding.persistedHandle
  const savedFile = envelope.binding.fileBinding.persistedHandle
  const before = await acquireFSAEntryMutationLease(savedFile)
  await before.release()
  await parent.removeEntry(envelope.binding.stableName)
  // Delete acknowledgement may be lost. The persisted, now deleted handle must
  // still admit cleanup retry without blocking unrelated targets.
  const deleted = await acquireFSAEntryMutationLease(savedFile)
  try {
    const sibling = await parent.getFileHandle('independent.zip', { create: true })
    const independent = await acquireFSAEntryMutationLease(sibling)
    try {
      const otherParent = await parent.getDirectoryHandle('another-parent', { create: true })
      const otherFile = await otherParent.getFileHandle(envelope.binding.stableName, { create: true })
      const other = await acquireFSAEntryMutationLease(otherFile)
      await other.release()
      return { persistedIdentityRetained: before.name === deleted.name,
        otherParentIdentityDistinct: other.name !== deleted.name,
        siblingIdentityDistinct: independent.name !== deleted.name && independent.name !== other.name }
    } finally { await independent.release() }
  } finally { await deleted.release() }
}

export async function failProductionConcurrentZipActivation(databaseName: string) {
  const parent = await selectedParent({ databaseName, branch: 'A', payload: [] })
  const activation = await prepareActivation(databaseName, parent, async () => {
    const journal = await IndexedDbDirectZipJournalRepository.open({ databaseName })
    return new Proxy(journal, {
      get: (target, property) => {
        if (property === 'commitBootstrap') return async () => {
          throw new DOMException(ACTIVATION_FAILURE, 'UnknownError')
        }
        const value = Reflect.get(target, property)
        return typeof value === 'function' ? value.bind(target) : value
      },
    })
  })
  const failed = await activation.commit()
  if (failed.kind !== 'owned-effects') throw new Error('Activation did not retain the injected bootstrap failure')
  try {
    if (!(failed.cause instanceof DOMException) || failed.cause.message !== ACTIVATION_FAILURE) throw failed.cause
    return { operationId: failed.authority.intent.operationId }
  } finally { await failed.authority.detach() }
}

export async function cleanupProductionConcurrentZip(databaseName: string) {
  await (await navigator.storage.getDirectory()).removeEntry(databaseName, { recursive: true })
}

async function selectedParent(input: ConcurrentZipInput) {
  if (input.savedParentOperationId !== undefined) {
    return (await persistedEnvelope(input.databaseName, input.savedParentOperationId))
      .candidate.parentBinding.persistedHandle
  }
  const root = await navigator.storage.getDirectory()
  const workspace = await root.getDirectoryHandle(input.databaseName, { create: true })
  const branch = await workspace.getDirectoryHandle(input.branch, { create: true })
  return branch.getDirectoryHandle('Downloads', { create: true })
}

export async function persistedEnvelope(databaseName: string, operationId: string) {
  const repository = await IndexedDbReceiveOperationRepository.open(databaseName)
  try { return await readEnvelope(repository, operationId) } finally { repository.close() }
}

async function prepareActivation(databaseName: string, parent: FileSystemDirectoryHandle,
  openJournal?: BrowserDirectZipProductionOptions['openJournal']) {
  const originalPicker = Object.getOwnPropertyDescriptor(window, 'showDirectoryPicker')
  Object.defineProperty(window, 'showDirectoryPicker', { configurable: true, value: async () => parent })
  try {
    const windowPort = window as BrowserReceiveWindow
    const directZip = createBrowserDirectZipComposition(windowPort, {
      openRepository: () => IndexedDbReceiveOperationRepository.open(databaseName),
      openJournal: openJournal ?? (() => IndexedDbDirectZipJournalRepository.open({ databaseName })),
    })
    const receiver = createBrowserReceiveComposition(windowPort, { directZip })
    return await prepareProductionDirectZipActivation(windowPort, receiver, directZip, signal)
  } finally {
    if (originalPicker === undefined) Reflect.deleteProperty(window, 'showDirectoryPicker')
    else Object.defineProperty(window, 'showDirectoryPicker', originalPicker)
  }
}

import { createBrowserReceiveOperationMutationPort } from '../../../src/output/resume/reopen-authority'
import { receiveOperationResumeDescriptor } from '../../../src/output/resume/descriptor'
import { offerArtifacts } from '../../../src/output/planning/offers'
import { createSelectionSpec } from '../../../src/transfer/intent'
import { SelectionPolicy } from '../../../src/catalog/selection'
import { TransferJob } from '../../../src/transfer/job'
import { BoundaryFaultError, FaultScope, SourceFaultCode, sourceFault } from '../../../src/transfer/failure/fault'
import { createBrowserReceiveComposition, type BrowserReceiveWindow } from '../../../src/application/browser-receive/composition'
import { saveProgressivePartial } from '../../../src/output/portable/partial-export'
import { catalogFixture, directoryEntry, fileEntry, identity, identityText, readerFixture } from '../../transfer/v2-job-fixture'
import { commitProductionChoice, productZipProjection } from '../durable-preparation-harness'

const DIRECTORY = directoryEntry(identity(31), 'micro-share')
const KEPT_FILE = fileEntry(identity(21), 'keep.bin', 6n)
const VANISHED_FILE = fileEntry(identity(22), 'vanished.bin', 4n)
const scheduledLeaseCallbacks: (() => void)[] = []

function nonPickerWindow(): BrowserReceiveWindow {
  if (!('showSaveFilePicker' in window)) return window as unknown as BrowserReceiveWindow
  return new Proxy(window, {
    has(target, property) {
      return property === 'showSaveFilePicker' ? false : Reflect.has(target, property)
    },
    get(target, property): unknown {
      if (property === 'showSaveFilePicker') return undefined
      const value = Reflect.get(target, property, target)
      return property === 'setTimeout' || property === 'clearTimeout'
        ? (value as (...args: unknown[]) => unknown).bind(target)
        : value
    },
  }) as BrowserReceiveWindow
}

function createCatalog() {
  return catalogFixture([
    { id: identity(2), entries: [DIRECTORY], generation: identity(92) },
    { id: DIRECTORY.id, entries: [KEPT_FILE, VANISHED_FILE], generation: identity(93) },
  ])
}

function createComposition() {
  return createBrowserReceiveComposition(nonPickerWindow(), {
    resumeMutations: createBrowserReceiveOperationMutationPort(),
  })
}

export async function createUnallocatedMissingFilePause() {
  const selection = await createSelectionSpec({
    shareInstance: identityText(1), syntheticRoot: identityText(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const projection = productZipProjection(selection.digest, DIRECTORY.idText)
  const composition = createComposition()
  const signal = new AbortController().signal
  const environment = await composition.environment(signal)
  const offered = await offerArtifacts(projection, { kind: 'complete' }, environment)
  if (offered.kind !== 'artifact-actions') throw new Error('Missing artifact offers')
  const choice = [offered.primary, ...offered.alternatives].find(candidate =>
    candidate.route.kind === 'workspace-then-publish' && candidate.choice.artifactKind === 'zip-archive')
  if (choice === undefined) throw new Error('Missing durable ZIP route')
  const runtime = await commitProductionChoice(
    composition, selection, projection, environment, choice, signal,
    { objectLabel: 'Partial folder', createdAtMilliseconds: 2345 },
  )
  try {
    const catalog = createCatalog()
    const readers = readerFixture([KEPT_FILE, VANISHED_FILE], [], {
      beforeOpen: async id => {
        if (id === VANISHED_FILE.idText) {
          throw new BoundaryFaultError(sourceFault(FaultScope.FileLocal, SourceFaultCode.Permanent))
        }
      },
    })
    const result = await new TransferJob({
      descriptor: { shareInstance: identity(1), syntheticRoot: identity(2),
        syntheticRootId: identityText(2), chunkSize: 2 } as never,
      catalog: catalog.catalog, selection: new SelectionPolicy(true), intent: runtime.intent,
      plans: runtime.plans, revisions: readers.revisions, broker: readers.broker,
      lanes: { size: 1 }, maximumConcurrentFiles: 1, transferJobId: runtime.transferJobId,
      revisionCapacity: { generation: { waitForProtocolSessionReplacement: async () => {
        throw new Error('Fixture does not replace its protocol session')
      } } },
    }).run(signal)
    await runtime.detach()

    const inventory = await composition.retained.list(signal)
    try {
      const row = inventory.operations.find(candidate => candidate.operationId === runtime.intent.operationId)
      if (row === undefined) throw new Error('Retained operation disappeared after detach')
      return {
        operationId: row.operationId,
        workerStatus: result.worker.status,
        lifecycle: row.lifecycle.kind,
        continuation: row.continuation,
        actions: [...row.actions],
        discoveryComplete: row.lifecycle.kind === 'resumable-receive' &&
          row.lifecycle.payloadKind === 'workspace-archive' ? row.lifecycle.discoveryComplete : null,
        completeFileCount: row.lifecycle.kind === 'resumable-receive' &&
          row.lifecycle.payloadKind === 'workspace-archive' ? row.lifecycle.completedFileCount.toString() : null,
        hasPickerApi: 'showSaveFilePicker' in nonPickerWindow(),
      }
    } finally { inventory.close() }
  } finally { await runtime.detach() }
}

export async function exportPartialViaBrowserDownload(operationId: string) {
  const composition = createComposition()
  const signal = new AbortController().signal
  const inventory = await composition.retained.list(signal)
  try {
    const row = inventory.operations.find(candidate => candidate.operationId === operationId)
    if (row === undefined) throw new Error('Retained operation missing before partial export')
    const actionResult = await inventory.act(row, 'save-partial', signal)
    if (actionResult.kind !== 'completed') throw new Error(`Unexpected partial export result: ${actionResult.kind}`)
  } finally { inventory.close() }

  const refreshed = await composition.retained.list(signal)
  try {
    const row = refreshed.operations.find(candidate => candidate.operationId === operationId)
    if (row === undefined) throw new Error('Retained operation disappeared after partial export')
    return {
      continuation: row.continuation,
      actions: [...row.actions],
      lifecycle: row.lifecycle.kind,
    }
  } finally { refreshed.close() }
}

export async function continueAndCompleteRetainedDownload(operationId: string) {
  const composition = createComposition()
  const signal = new AbortController().signal
  const inventory = await composition.retained.list(signal)
  let actionResult: Awaited<ReturnType<typeof inventory.act>>
  try {
    const row = inventory.operations.find(candidate => candidate.operationId === operationId)
    if (row === undefined) throw new Error('Retained operation missing before continuation')
    actionResult = await inventory.act(row, 'continue', signal)
  } finally { inventory.close() }
  if (actionResult.kind !== 'receive-continuation') {
    throw new Error(`Expected receive continuation, got ${actionResult.kind}`)
  }
  const { runtime } = actionResult
  try {
    const catalog = createCatalog()
    // Fail revision open for the already-completed file to prove OPFS bytes are reused intact.
    const readers = readerFixture([KEPT_FILE, VANISHED_FILE], [], { failRevisionFor: KEPT_FILE.idText })
    const resumed = await new TransferJob({
      descriptor: { shareInstance: identity(1), syntheticRoot: identity(2),
        syntheticRootId: identityText(2), chunkSize: 2 } as never,
      catalog: catalog.catalog, selection: new SelectionPolicy(true), intent: runtime.intent,
      plans: runtime.plans, revisions: readers.revisions, broker: readers.broker,
      lanes: { size: 1 }, maximumConcurrentFiles: 1, transferJobId: runtime.transferJobId,
      revisionCapacity: { generation: { waitForProtocolSessionReplacement: async () => {
        throw new Error('Fixture does not replace its protocol session')
      } } },
    }).run(signal)
    return {
      workerStatus: resumed.worker.status,
      lifecycle: resumed.lifecycle.kind,
      revisionRequests: readers.revisionRequests,
      expectedMissingId: VANISHED_FILE.idText,
    }
  } finally { await runtime.detach() }
}

export async function startFileBackedPartialExport(operationId: string) {
  scheduledLeaseCallbacks.length = 0
  const baseWindow = nonPickerWindow()
  const controlledWindow = new Proxy(baseWindow, {
    get(target, property): unknown {
      if (property === 'setTimeout') {
        return (callback: () => void) => {
          scheduledLeaseCallbacks.push(callback)
          return scheduledLeaseCallbacks.length
        }
      }
      if (property === 'clearTimeout') return () => undefined
      return Reflect.get(target, property, target)
    },
  }) as BrowserReceiveWindow
  const port = createBrowserReceiveOperationMutationPort()
  const composition = createComposition()
  const signal = new AbortController().signal
  const inventory = await composition.retained.list(signal)
  let descriptor: Parameters<typeof port.resume>[0]
  try {
    const row = inventory.operations.find(candidate => candidate.operationId === operationId)
    if (row === undefined) throw new Error('Retained operation missing before file-backed partial export')
    const projected = receiveOperationResumeDescriptor(row.lifecycle)
    if (projected === undefined) throw new Error('Retained lifecycle could not project a resume descriptor')
    descriptor = Object.freeze({ ...projected, continuation: row.continuation })
  } finally { inventory.close() }
  const resumed = await port.resume(descriptor, { purpose: 'partial-export' })
  if (resumed.kind !== 'continuation' || resumed.continuation.kind !== 'workspace-progressive-zip-partial') {
    throw new Error('Expected partial-export continuation')
  }
  try {
    await saveProgressivePartial(
      resumed.continuation.operation.partialContinuation,
      { kind: 'browser-download', windowPort: controlledWindow, suggestedName: 'large-share.zip' },
      signal,
      undefined,
      { detachMaxBytes: 0n },
    )
  } finally {
    await resumed.continuation.operation.close()
  }
  return { scheduledLeaseCount: scheduledLeaseCallbacks.length }
}

export async function attemptContinueWhileFileBackedLeaseActive(operationId: string) {
  const composition = createComposition()
  const signal = new AbortController().signal
  const inventory = await composition.retained.list(signal)
  let rejectedName: string | null = null
  try {
    const row = inventory.operations.find(candidate => candidate.operationId === operationId)
    if (row === undefined) throw new Error('Retained operation missing while lease is active')
    try {
      await inventory.act(row, 'continue', signal)
    } catch (error) {
      rejectedName = error instanceof Error ? error.name : String(error)
    }
  } finally { inventory.close() }

  const refreshed = await composition.retained.list(signal)
  try {
    const row = refreshed.operations.find(candidate => candidate.operationId === operationId)
    return {
      rejectedName,
      retainedLifecycle: row?.lifecycle.kind ?? null,
      retainedContinuation: row?.continuation ?? null,
    }
  } finally { refreshed.close() }
}

export function releaseFileBackedPartialLease(): number {
  const callbacks = scheduledLeaseCallbacks.splice(0)
  for (const callback of callbacks) callback()
  return callbacks.length
}

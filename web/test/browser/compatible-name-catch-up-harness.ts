import { IndexedDbCompatibleNameLedger } from '../../src/output/browser/indexeddb-compatible-name-ledger'
import { IndexedDbReceiveOperationRepository } from '../../src/output/browser/indexeddb-repository'
import { IndexedDbReceiveResumeSource } from '../../src/output/browser/indexeddb-resume-state'
import { acquireBrowserReceiveOperationLease } from '../../src/output/browser/session-lease'
import { openFileSystemAccessCompatibleNameCatchUp, reopenFileSystemAccessOutput } from '../../src/output/file-system-access/session'
import { catchUpFileSystemAccessCompatibleNames } from '../../src/output/file-system-access/settlement'
import { decodeCompatibleNameSidecar } from '../../src/output/file-system-access/compatible-name/sidecar-codec'
import { initialReceiveLifecycleState } from '../../src/output/workspace/state'
import { reduceReceiveLifecycle } from '../../src/output/workspace/lifecycle'
import { receiveOperationResumeDescriptor } from '../../src/output/resume/descriptor'
import { ReceiveOperationResumeAuthority } from '../../src/output/resume/authority'
import { createBrowserReceiveOperationMutationPort } from '../../src/output/resume/reopen-authority'
import { decodeStoredReceiveLifecycleState } from '../../src/output/workspace/state-codec'
import { presentCompatibleNameRepair } from '../../src/ui/compatible-name-repair-presentation'
import { listBrowserRetainedOperations } from '../../src/ui/browser-receive/retained'
import { retainedPresentationActions } from '../../src/ui/controller/retained-inventory-presentation'
import { installNativeLookupInterceptor } from './fsa-namespace-atomicity-harness'
import { createCompatibleNameRecoveryCut, reopenCompatibleNameRecovery, type CompatibleNameRecoveryFixture } from './durable-recovery-harness'

const COMMITTED_DIRECTORY = 'committed-before-reload'
const SIGNAL = new AbortController().signal
// Retain live authorities until page.reload destroys the receiving context.
const abandonedReceiveAuthorities = new Map<string, readonly unknown[]>()

export async function createActiveCompatibleNameCatchUpCut(key: string) {
  const { fixture } = await createCompatibleNameRecoveryCut(key)
  const repository = await IndexedDbReceiveOperationRepository.open(fixture.databaseName)
  const session = await reopenFileSystemAccessOutput({
    intent: fixture.intent,
    operationRepository: repository,
    databaseName: fixture.databaseName,
    compatibleNamePreparation: { platform: 'windows', randomBits: () => 0 },
  })
  const parent = await (await navigator.storage.getDirectory()).getDirectoryHandle(fixture.parentName)
  const root = await parent.getDirectoryHandle(session.reservation.physicalName)
  const ledger = await IndexedDbCompatibleNameLedger.open(fixture.databaseName)
  const header = await ledger.readHeader(fixture.operationId)
  if (header === undefined) throw new TypeError('missing compatible-name header')
  const sidecar = await root.getFileHandle(header.pair.sidecar.physicalName)
  const interception = installNativeLookupInterceptor({
    parent: root,
    rejection: { kind: 'directory', cause: new TypeError('injected native name refusal') },
    contentBlockRequestCount: () => 0,
  })
  const lease = await startReceiving(fixture, repository)
  const failure = refuseSidecarWrites(sidecar)
  try {
    // Only native sidecar writes fail: directory ownership and its mapping still commit in IndexedDB.
    await session.ensureDirectory([COMMITTED_DIRECTORY])
    await failure.observed
    const snapshot = await ledger.loadOperation(fixture.operationId)
    if (snapshot?.header.repairSummary === undefined) throw new TypeError('missing durable repair summary')
    const decoded = await decodeSidecar(sidecar)
    const descriptor = await readDescriptor(fixture, repository)
    abandonedReceiveAuthorities.set(fixture.operationId, [session, repository, ledger, lease])
    return Object.freeze({
      restoreCommandAvailable: presentCompatibleNameRepair({
        state: descriptor.lifecycle, summary: snapshot.header.repairSummary, context: 'retained-operation',
      }).runCommand !== null,
      fixture,
      committedCount: snapshot.header.repairSummary.committedCount,
      observedCount: decoded.footer.committedCount,
      footer: decoded.footer.state,
      sidecarSync: snapshot.header.repairSummary.sidecarSync,
      terminalSettlement: snapshot.header.repairSummary.terminalSettlement,
      pendingOutcomePresent: snapshot.header.pendingTerminalOutcome !== undefined,
      injectedWriteFailures: failure.count(),
      lifecycle: descriptor.lifecycle.kind,
      durableReceiveLeasePresent: (await repository.readLease(fixture.operationId))?.leaseId === lease.leaseId,
      continuation: descriptor.continuation,
    })
  } catch (error) {
    interception.restore()
    failure.restore()
    await session.close()
    await lease.release()
    ledger.close()
    repository.close()
    throw error
  }
}

export async function catchUpActiveCompatibleNamesAfterReload(fixture: CompatibleNameRecoveryFixture) {
  if (abandonedReceiveAuthorities.has(fixture.operationId)) {
    throw new TypeError('catch-up must run after the live receiving context was destroyed')
  }
  const repository = await IndexedDbReceiveOperationRepository.open(fixture.databaseName)
  const beforeDescriptor = await readDescriptor(fixture, repository)
  const actionsBefore = await retainedActions(fixture)
  const beforeRecord = await repository.readLifecycle(fixture.operationId)
  const caught = await mutateRetained(fixture, 'catch-up')
  if (caught.kind !== 'continuation' || caught.continuation.kind !== 'direct-tree-catch-up') {
    throw new TypeError('local compatible-name catch-up was not authorized')
  }
  const operation = caught.continuation.operation
  let result: Awaited<ReturnType<typeof catchUpFileSystemAccessCompatibleNames>>
  try {
    result = await catchUpFileSystemAccessCompatibleNames({
      operation,
      signal: SIGNAL,
      openSession: value => openFileSystemAccessCompatibleNameCatchUp({
        intent: value.intent,
        operationRepository: value.repository,
        databaseName: fixture.databaseName,
        compatibleNamePreparation: { platform: 'windows', randomBits: () => 0 },
      }),
    })
  } finally {
    await operation.close()
  }
  const afterRecord = await repository.readLifecycle(fixture.operationId)
  const afterDescriptor = await readDescriptor(fixture, repository)
  const ledger = await IndexedDbCompatibleNameLedger.open(fixture.databaseName)
  const header = await ledger.readHeader(fixture.operationId)
  ledger.close()
  if (header === undefined) throw new TypeError('catch-up lost its compatible-name operation')
  const parent = await (await navigator.storage.getDirectory()).getDirectoryHandle(fixture.parentName)
  const root = await parent.getDirectoryHandle(header.root.physicalName)
  const sidecar = await decodeSidecar(await root.getFileHandle(header.pair.sidecar.physicalName))
  repository.close()
  return Object.freeze({
    restoreCommandAvailable: presentCompatibleNameRepair({
      state: result.lifecycle, summary: result.repairSummary, context: 'retained-operation',
    }).runCommand !== null,
    actionsBefore,
    actionsAfter: await retainedActions(fixture),
    lifecycle: result.lifecycle.kind,
    lifecycleUnchanged: beforeRecord?.digest === afterRecord?.digest,
    continuationBefore: beforeDescriptor.continuation,
    continuationAfter: afterDescriptor.continuation,
    footer: sidecar.footer.state,
    committedCount: sidecar.footer.committedCount,
    sidecarSync: result.repairSummary.sidecarSync,
    terminalSettlement: result.repairSummary.terminalSettlement,
    pendingOutcomePresent: header.pendingTerminalOutcome !== undefined,
    scriptName: header.pair.script.physicalName,
    sidecarName: header.pair.sidecar.physicalName,
  })
}

export async function resumeAfterActiveCompatibleNameCatchUp(fixture: CompatibleNameRecoveryFixture) {
  const resumed = await mutateRetained(fixture, 'continue')
  if (resumed.kind !== 'continuation' || resumed.continuation.kind !== 'direct-tree-receive') {
    throw new TypeError('catch-up removed ordinary receive eligibility')
  }
  const operation = resumed.continuation.operation
  const lifecycle = operation.lifecycle.kind
  const retainedFileRecovery = operation.retainedFileRecovery
  await operation.close()
  const proof = await reopenCompatibleNameRecovery(fixture, 1)
  return Object.freeze({ lifecycle, retainedFileRecovery, ...proof })
}

async function startReceiving(
  fixture: CompatibleNameRecoveryFixture,
  repository: IndexedDbReceiveOperationRepository,
) {
  const initial = initialReceiveLifecycleState({
    operationId: fixture.operationId, receiveIntentDigest: fixture.intent.digest,
  })
  const lease = await acquireBrowserReceiveOperationLease(repository, fixture.operationId, {
    acquireTransition: { lifecycle: initial },
  })
  const receiving = reduceReceiveLifecycle(initial, {
    kind: 'receive-started', expectedGeneration: initial.generation, leaseId: lease.leaseId,
  }, {
    planKind: 'direct-tree', preparationRequired: false,
    activeLeaseId: lease.leaseId, nowMilliseconds: Date.now(),
  })
  if (receiving.status !== 'applied' || receiving.state.kind !== 'receiving') {
    throw new TypeError('crash fixture failed to enter the production receiving lifecycle')
  }
  await repository.commitTransition({
    operationId: fixture.operationId,
    expectedLifecycleGeneration: initial.generation,
    expectedLeaseId: lease.leaseId,
    lifecycle: receiving.state,
  })
  return lease
}

async function mutateRetained(fixture: CompatibleNameRecoveryFixture, action: 'continue' | 'catch-up') {
  const source = await IndexedDbReceiveResumeSource.open(fixture.databaseName)
  const authority = new ReceiveOperationResumeAuthority({
    source,
    mutations: createBrowserReceiveOperationMutationPort({ checkpointDatabaseName: fixture.databaseName }),
  })
  const inventory = await authority.listResumeState()
  try {
    const reference = inventory.operations.find(value => value.descriptor.operationId === fixture.operationId)
    if (reference === undefined) throw new TypeError('retained mutation lacks its inventoried operation')
    if (action === 'catch-up') return await authority.catchUp(reference)
    const request = reference.recoverySummary === undefined
      ? undefined : { retainedFileRecovery: 'preserve' as const }
    return await authority.resume(reference, request)
  } finally {
    inventory.close()
    source.close()
  }
}

function openRetainedInventory(fixture: CompatibleNameRecoveryFixture) {
  return listBrowserRetainedOperations(window, {
    openResumeSource: () => IndexedDbReceiveResumeSource.open(fixture.databaseName),
    resumeMutations: createBrowserReceiveOperationMutationPort({ checkpointDatabaseName: fixture.databaseName }),
  }, SIGNAL)
}

export async function retainedOperationPresent(fixture: CompatibleNameRecoveryFixture) {
  const inventory = await openRetainedInventory(fixture)
  try {
    return inventory.operations.some(value => value.operationId === fixture.operationId)
  } finally {
    inventory.close()
  }
}

async function retainedActions(fixture: CompatibleNameRecoveryFixture) {
  const inventory = await openRetainedInventory(fixture)
  const ledger = await IndexedDbCompatibleNameLedger.open(fixture.databaseName)
  try {
    const operation = inventory.operations.find(value => value.operationId === fixture.operationId)
    if (operation === undefined) throw new TypeError('abandoned receiving operation disappeared from inventory')
    const summary = await ledger.readRepairSummary(fixture.operationId)
    return retainedPresentationActions(operation, summary)
  } finally {
    inventory.close()
    ledger.close()
  }
}

async function readDescriptor(fixture: CompatibleNameRecoveryFixture, repository: IndexedDbReceiveOperationRepository) {
  const record = await repository.readLifecycle(fixture.operationId)
  if (record === undefined) throw new TypeError('missing receive lifecycle')
  const descriptor = receiveOperationResumeDescriptor(decodeStoredReceiveLifecycleState(record), Date.now())
  if (descriptor === undefined) throw new TypeError('missing retained receive descriptor')
  return descriptor
}

async function decodeSidecar(handle: FileSystemFileHandle) {
  return decodeCompatibleNameSidecar(new Uint8Array(await (await handle.getFile()).arrayBuffer()))
}

function refuseSidecarWrites(sidecar: FileSystemFileHandle) {
  const prototype = FileSystemFileHandle.prototype
  const original = prototype.createWritable
  let count = 0
  let observed!: () => void
  const reached = new Promise<void>(resolve => { observed = resolve })
  prototype.createWritable = async function (options) {
    if (await this.isSameEntry(sidecar)) {
      count += 1
      observed()
      throw new DOMException('injected sidecar write failure after durable mapping', 'QuotaExceededError')
    }
    return original.call(this, options)
  }
  return { observed: reached, count: () => count, restore: () => { prototype.createWritable = original } }
}

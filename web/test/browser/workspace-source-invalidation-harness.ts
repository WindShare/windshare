import { V2RemoteRevisionError } from '../../src/content/v2-session-services'
import { createFailureIdentity } from '../../src/diagnostics/incident'
import { IndexedDbReceiveResumeSource } from '../../src/output/browser/indexeddb-resume-state'
import { ReceiveOperationResumeAuthority } from '../../src/output/resume/authority'
import { createBrowserReceiveOperationMutationPort } from '../../src/output/resume/reopen-authority'
import { normalizeV2FileTransferFailure } from '../../src/transfer/job/failures'
import { EMPTY_TRANSFER_FAILURE_SUMMARY, transferWorkerSettlement } from '../../src/transfer/outcome'
import { persistentWorkspaceInterruption } from '../../src/transfer/settlement/persistent-evidence'
import { PersistentSettlementCut } from '../../src/transfer/settlement/persistent-settlement-cut'
import { WorkspaceReceivePackaging } from '../../src/ui/browser-receive/workspace-packaging'
import { retainedOperationAuthority } from '../../src/ui/browser-receive/retained-operation-authority'
import { DURABLE_FIXTURE_CAPACITY_BYTES } from './durable-output-fixture'
import type { DurableReceiveFixture } from './durable-recovery-harness'

const SOURCE_STALE = 0x3001
const RECOVERY_TIME = 3_000

export async function invalidateRetainedOriginal(fixture: DurableReceiveFixture) {
  const source = await IndexedDbReceiveResumeSource.open(fixture.checkpointDatabaseName)
  const mutations = mutationPort(fixture)
  const authority = new ReceiveOperationResumeAuthority({ source, mutations })
  const inventory = await authority.listResumeState()
  const reference = inventory.operations[0]
  if (reference === undefined) throw new Error('Original receive is missing')
  const resumed = await authority.resume(reference)
  inventory.close()
  source.close()
  if (resumed.kind !== 'continuation' || resumed.continuation.kind !== 'workspace-receive') {
    throw new Error('Original receive did not reopen')
  }
  const operation = resumed.continuation.operation
  if (operation.intent.artifact.kind !== 'original-file') throw new Error('Expected original-file receive')
  const backend = await operation.receiveContinuation.openBackend()
  try {
    const before = await payloads(operation.namespace.root)
    let failure: unknown
    try {
      await backend.materialization.beginFile({
        materializationRelativePath: [operation.intent.artifact.suggestedName],
        openRevision: async () => { throw sourceFailure() },
      })
    } catch (error) { failure = error }
    if (failure === undefined) throw new Error('Changed source unexpectedly opened')
    const normalized = normalizeV2FileTransferFailure(failure)
    const packaging = new WorkspaceReceivePackaging({
      windowPort: window, intent: operation.intent, repository: operation.repository,
      namespace: operation.namespace, stages: operation.stages,
    })
    const interruption = persistentWorkspaceInterruption({
      reason: normalized.diagnostic,
      worker: transferWorkerSettlement('Paused', EMPTY_TRANSFER_FAILURE_SUMMARY),
      materialization: { entryCount: 0n, fileCount: 0n, directoryCount: 0n, rawBytes: 0n },
      selectionFacts: { discoveredFileCount: 1n, discoveredBytes: 5n, discovery: 'complete' },
    })
    const lifecycle = await packaging.settlement(backend, () => 'unused', () => backend).interrupt(
      interruption,
      new PersistentSettlementCut({
        kind: 'workspace-manifest', generations: [], entries: [], directorySettlements: [],
      }, () => backend.materialization.close()),
      new AbortController().signal,
    )
    return { lifecycle: lifecycle.kind, before, after: await payloads(operation.namespace.root) }
  } finally {
    await backend.close()
    await operation.close()
  }
}

export async function inspectAndDiscardInvalidatedOriginal(fixture: DurableReceiveFixture) {
  const source = await IndexedDbReceiveResumeSource.open(fixture.checkpointDatabaseName)
  const mutations = mutationPort(fixture)
  const authority = new ReceiveOperationResumeAuthority({ source, mutations })
  const inventory = await authority.listResumeState()
  try {
    const reference = inventory.operations[0]
    if (reference === undefined) throw new Error('Invalidated receive disappeared with retained data')
    const descriptor = reference.descriptor
    const actions = retainedOperationAuthority(descriptor.continuation, true, false, false).actions
    const resumed = await mutations.resume(descriptor).then(() => true, () => false)
    const cleanup = await authority.discard(reference)
    const remaining = await authority.listResumeState()
    const remainingCount = remaining.operations.length
    remaining.close()
    return { lifecycle: descriptor.lifecycle.kind, continuation: descriptor.continuation,
      actions, resumed, cleanup: cleanup.kind, remainingCount,
      retainedPayloads: await payloads(await navigator.storage.getDirectory()) }
  } finally {
    inventory.close()
    source.close()
  }
}

function mutationPort(fixture: DurableReceiveFixture) {
  return createBrowserReceiveOperationMutationPort({
    checkpointDatabaseName: fixture.checkpointDatabaseName,
    workspaceBudgetDatabaseName: fixture.admissionDatabaseName,
    clock: { now: () => RECOVERY_TIME },
    leaseOptions: { clock: { now: () => RECOVERY_TIME }, randomBytes: () => crypto.getRandomValues(new Uint8Array(16)) },
    estimateWorkspaceStorage: async () => ({ usage: 0, quota: Number(DURABLE_FIXTURE_CAPACITY_BYTES) }),
  })
}

function sourceFailure(): V2RemoteRevisionError {
  return new V2RemoteRevisionError({
    requestKind: 'open_revisions', content: { scope: 'revision', code: SOURCE_STALE, retryable: false },
    correlation: {
      protocolSessionId: createFailureIdentity('protocol_session', new Uint8Array(16).fill(1)),
      protocolOperationId: createFailureIdentity('protocol_operation', new Uint8Array(16).fill(2)),
      lane: { id: 1, epoch: 0 },
    },
  })
}

async function payloads(directory: FileSystemDirectoryHandle): Promise<readonly number[][]> {
  const files: number[][] = []
  for await (const entry of directory.values()) {
    if (entry.kind === 'directory') files.push(...await payloads(entry))
    else files.push([...new Uint8Array(await (await entry.getFile()).arrayBuffer())])
  }
  return files.sort((left, right) => JSON.stringify(left).localeCompare(JSON.stringify(right)))
}

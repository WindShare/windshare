import { FSAReceiveOperation } from '../../src/application/browser-receive/fsa/operation'
import { BrowserFolderDeliveryProgress } from '../../src/application/browser-receive/fsa/folder-delivery'
import { listRetainedOperations } from '../../src/application/browser-receive/retained/production'
import type { BoundReceiveOperation } from '../../src/application/receive/contracts'
import { SelectionPolicy } from '../../src/catalog/selection'
import { RevisionReopenedDuringRecoveryError } from '../../src/content/session-services'
import { decodeBase64Url } from '../../src/crypto/bytes'
import type { BrowserReceiveWindow } from '../../src/output/browser/environment'
import { DEFAULT_OUTPUT_DATABASE_NAME } from '../../src/output/browser/indexeddb-database'
import { IndexedDbReceiveOperationRepository } from '../../src/output/browser/indexeddb-repository'
import { acquireBrowserReceiveOperationLease } from '../../src/output/browser/session-lease'
import { IndexedDbBrowserDeliveryRepository } from '../../src/output/browser-delivery/indexeddb'
import { createBrowserSavePolicy } from '../../src/output/browser-delivery/policy'
import { createBrowserDeliveryRecord } from '../../src/output/browser-delivery/records'
import { FSAResourceOwner } from '../../src/output/file-system-access/execution/resource-owner'
import { createFileSystemAccessSettlementAuthority } from '../../src/output/file-system-access/settlement'
import type { BrowserRecoveryPreference } from '../../src/output/model/browser-delivery'
import { FILE_CHECKPOINT_MATERIALIZER_FSA_TREE, FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE } from '../../src/output/persistence/checkpoint'
import { initialReceiveLifecycleState } from '../../src/transfer/lifecycle/state'
import {
  createOperationID,
  createOutputSessionID,
  createResultRootDirectoryTreeArtifact,
  createSyntheticSelectionResultRoot,
  createTransferJobID,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import { TransferJob } from '../../src/transfer/job'
import { catalogFixture, digestIdentity, fileEntry, identity, identityText, readerFixture } from '../transfer/v2-job-fixture'
import { bindTask } from './fsa-namespace-atomicity-harness'

const SIGNAL = new AbortController().signal
const CHUNK_SIZE = 2
const PAYLOAD_BYTES = Uint8Array.of(1, 2, 3, 4, 5, 6)
const SIBLING_BYTES = Uint8Array.of(7, 8)
const PAYLOAD = fileEntry(identity(80), 'payload.bin', BigInt(PAYLOAD_BYTES.length))
const SIBLING = fileEntry(identity(81), 'sibling.bin', BigInt(SIBLING_BYTES.length))
const PAYLOAD_REVISIONS = [51, 52, 53] as const
const SIBLING_REVISION = 54
const REOPENED_SIBLING_REVISION = 55
const SOURCE_METADATA = digestIdentity(61)

interface ReopenedFolderFixture {
  readonly operationId: string
  readonly parentName: string
  readonly targetName: string
}

export async function exhaustFolderFileReadmission(parentName: string, preference: BrowserRecoveryPreference) {
  const databaseName = DEFAULT_OUTPUT_DATABASE_NAME
  const parent = await (await navigator.storage.getDirectory()).getDirectoryHandle(parentName, { create: true })
  const artifact = await createResultRootDirectoryTreeArtifact(createSyntheticSelectionResultRoot())
  const repository = await IndexedDbReceiveOperationRepository.open(databaseName)
  let runtime: FSAReceiveOperation | undefined
  let resources: FSAResourceOwner | undefined
  try {
    const session = await bindTask({ databaseName, parentName }, parent, repository, artifact, 41, false)
    const { intent } = session
    const lease = await acquireBrowserReceiveOperationLease(repository, intent.operationId).catch(async error => {
      await session.close()
      throw error
    })
    resources = new FSAResourceOwner({ repository, operationLease: lease, outputSession: session })
    const lifecycle = initialReceiveLifecycleState({ operationId: intent.operationId, receiveIntentDigest: intent.digest })
    await repository.commitTransition({ operationId: intent.operationId, expectedLeaseId: lease.leaseId, lifecycle })
    const transferJobId = createTransferJobID()
    const settlement = await createFileSystemAccessSettlementAuthority({
      intent, repository, lifecycleLeaseId: lease.leaseId, transferJobId,
    })
    if (preference === 'automatic') await pinTinyStagedPlacement(intent)
    runtime = await FSAReceiveOperation.createCommitted({
      intent, repository, lifecycle, lease, session, resources, settlement,
      transferJobId, outputSessionId: createOutputSessionID(),
      attemptIdentities: { createTransferJobId: createTransferJobID, createOutputSessionId: createOutputSessionID },
      folderDelivery: {
        storage: navigator.storage, preference, progress: new BrowserFolderDeliveryProgress(),
        storageFacts: async () => ({ opfs: 'usable', pressure: 'normal', persistence: 'not-persisted', quota: { kind: 'unknown' } }),
      },
    })
    const { result, reads, decisions } = await runFolderTransfer(runtime, 'reopened-twice')
    const fixture = { operationId: intent.operationId, parentName, targetName: session.reservation.physicalName }
    return { fixture, worker: result.worker.status, lifecycle: result.lifecycle.kind, reads, decisions,
      siblingBytes: await savedBytes(fixture, SIBLING.name) }
  } finally {
    if (runtime !== undefined) await runtime.detach()
    else if (resources !== undefined) await resources.close()
    else repository.close()
  }
}

export async function continueReopenedFolder(fixture: ReopenedFolderFixture) {
  const inventory = await listRetainedOperations(window as unknown as BrowserReceiveWindow, {}, SIGNAL)
  try {
    const retained = inventory.operations.find(operation => operation.operationId === fixture.operationId)
    if (retained === undefined) throw new Error('Missing persisted folder download')
    const continuation = await inventory.act(retained, 'continue', SIGNAL)
    if (continuation.kind !== 'receive-continuation') throw new Error('Continue did not reopen the folder download')
    try {
      const { result, opens, reads } = await runFolderTransfer(continuation.runtime, 'stable')
      return { opens, reads, lifecycle: result.lifecycle.kind, worker: result.worker.status,
        stagedBytesBefore: retained.browserDelivery?.stagedBytes.toString(),
        payloadBytes: await savedBytes(fixture, PAYLOAD.name), siblingBytes: await savedBytes(fixture, SIBLING.name) }
    } finally { await continuation.runtime.detach() }
  } finally { inventory.close() }
}

async function runFolderTransfer(runtime: BoundReceiveOperation, source: 'reopened-twice' | 'stable') {
  const { intent, plans, transferJobId } = runtime
  const shareInstance = decodeBase64Url(intent.shareInstance)!
  const syntheticRoot = decodeBase64Url(intent.syntheticRoot)!
  const { catalog } = catalogFixture([{ id: syntheticRoot, entries: [PAYLOAD, SIBLING] }], shareInstance)
  const reader = readerFixture([PAYLOAD, SIBLING]).revisions
  const initial = await reader.open(PAYLOAD.id)
  const revisions = PAYLOAD_REVISIONS.map(seed => ({ ...initial.descriptor, shareInstance,
    shareInstanceId: intent.shareInstance, fileRevision: identity(seed), fileRevisionText: identityText(seed),
    sourceMetadata: SOURCE_METADATA }))
  let payloadOpens = 0
  const opens: string[] = []
  const reads: { name: string; offset: number }[] = []
  const decisions: { decision: string; retained: string; reused: string }[] = []
  const job = new TransferJob({
    descriptor: {
      shareInstance, shareInstanceId: intent.shareInstance,
      syntheticRoot, syntheticRootId: intent.syntheticRoot, chunkSize: CHUNK_SIZE,
    } as never,
    intent, plans, transferJobId, selection: new SelectionPolicy(true), lanes: { size: 1 },
    maximumConcurrentFiles: 1,
    revisionCapacity: { generation: { waitForProtocolSessionReplacement: async () => {
      throw new Error('Unexpected revision capacity wait')
    } } },
    catalog,
    revisions: { open: async (fileId, signal) => {
      const opened = await reader.open(fileId, signal)
      if (opened.descriptor.fileIdText === PAYLOAD.idText) {
        opens.push(PAYLOAD.name)
        const descriptor = source === 'stable' ? revisions.at(-1)! : revisions[payloadOpens++]!
        return { ...opened, descriptor }
      }
      opens.push(SIBLING.name)
      const seed = source === 'stable' ? REOPENED_SIBLING_REVISION : SIBLING_REVISION
      return { ...opened, descriptor: { ...opened.descriptor, shareInstance, shareInstanceId: intent.shareInstance,
        fileRevision: identity(seed), fileRevisionText: identityText(seed), sourceMetadata: SOURCE_METADATA } }
    } },
    broker: { readRange: async function* (revision, _lease, range) {
      const payload = revision.fileIdText === PAYLOAD.idText
      const position = Number(range.start)
      reads.push({ name: payload ? PAYLOAD.name : SIBLING.name, offset: position })
      if (payload && source === 'reopened-twice') {
        const index = revisions.findIndex(candidate => candidate.fileRevisionText === revision.fileRevisionText)
        yield { offset: range.start, data: PAYLOAD_BYTES.slice(position, position + CHUNK_SIZE) }
        throw new RevisionReopenedDuringRecoveryError(revision, revisions[index + 1]!)
      }
      yield { offset: range.start, data: (payload ? PAYLOAD_BYTES : SIBLING_BYTES).slice(position, Number(range.end)) }
    } },
    trace: { current: event => {
      if ('transition' in event && event.transition === 'file_revision_readmission') {
        decisions.push({ decision: event.decision, retained: String(event.retainedBytes), reused: String(event.reusedBytes) })
      }
    } },
  })
  return { result: await job.run(), opens, reads, decisions }
}

async function pinTinyStagedPlacement(intent: ReceiveIntent) {
  if (intent.plan.kind !== 'direct-tree') throw new Error('Expected a folder plan')
  const { reservation } = intent.plan
  const policy = createBrowserSavePolicy({
    operationId: intent.operationId, receiveIntentDigest: intent.digest, preference: 'automatic',
    target: { operationId: intent.operationId, receiveIntentDigest: intent.digest,
      materializationBindingDigest: reservation.digest, materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
      authorityRef: reservation.authorityRef },
    staging: { operationId: createOperationID(), receiveIntentDigest: intent.digest,
      materializationBindingDigest: digestIdentity(62), materializerKind: FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE,
      authorityRef: digestIdentity(63) },
  })
  const journal = await IndexedDbBrowserDeliveryRepository.open()
  try {
    await journal.installPolicy(policy)
    // A pinned placement exercises real staging without transferring a threshold-sized file.
    for (const [file, seed] of [[PAYLOAD, PAYLOAD_REVISIONS[0]], [SIBLING, SIBLING_REVISION]] as const) {
      await journal.createFile(createBrowserDeliveryRecord({ policy,
        source: { fileId: file.idText, fileRevision: identityText(seed),
          sourceMetadata: SOURCE_METADATA, exactSize: file.expectedSize, canonicalPath: [file.name] },
        materializationRelativePath: [file.name], placement: 'staged', placementReason: 'retained-stage-fixture',
      }))
    }
  } finally { journal.close() }
}

async function savedBytes(fixture: ReopenedFolderFixture, name: string) {
  const parent = await (await navigator.storage.getDirectory()).getDirectoryHandle(fixture.parentName)
  const target = await parent.getDirectoryHandle(fixture.targetName)
  const file = await (await target.getFileHandle(name)).getFile()
  return [...new Uint8Array(await file.arrayBuffer())]
}

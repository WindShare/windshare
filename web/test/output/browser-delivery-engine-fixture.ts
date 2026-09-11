import { BrowserDeliveryMaterialization, type BrowserDeliveryMaterializationOptions } from '../../src/output/browser-delivery/session'
import { advanceBrowserDeliveryRecord, assertBrowserDeliveryTransition, authorizeBrowserDeliveryRestart } from '../../src/output/browser-delivery/lifecycle'
import { browserDeliveryStagingPath, validateBrowserDeliveryRecord } from '../../src/output/browser-delivery/records'
import type { BrowserDeliveryRecordV1, BrowserSavePolicyV1 } from '../../src/output/browser-delivery/model'
import type { BrowserDeliveryRepository, BrowserDeliveryFileScan } from '../../src/output/browser-delivery/repository'
import type { BrowserDeliveryReservation, BrowserDeliveryStagePort, BrowserDeliveryTargetPort } from '../../src/output/browser-delivery/ports'
import { readBrowserDeliveryCheckpoint } from '../../src/output/browser-delivery/checkpoint-reader'
import { PersistentTreeOutputSession } from '../../src/output/persistent-tree/session'
import type { PersistentFileRequest } from '../../src/output/persistent-tree/contracts'
import { acquireArtifactReader, withArtifactCleanup } from '../../src/output/origin-private/export-readers'
import { deliveryIdentity, deliveryPolicy } from './browser-delivery-fixture'
import { MemoryTree } from './persistent-tree-file-fixture'
import { MemoryCheckpointRepository } from './persistent-tree-session-fixture'

export class DeliveryMemoryRepository implements BrowserDeliveryRepository {
  readonly files = new Map<string, BrowserDeliveryRecordV1>()
  readonly transitions: string[] = []
  readonly policy: BrowserSavePolicyV1
  failTransition: string | undefined
  failCreate = false
  beforeReplace: ((next: BrowserDeliveryRecordV1) => Promise<void>) | undefined
  readFinalCheckpoint!: (recordId: string) => Promise<import('../../src/output/persistence/checkpoint').FileCheckpointV2 | undefined>
  constructor(policy: BrowserSavePolicyV1) { this.policy = policy }
  async installPolicy(policy: BrowserSavePolicyV1) {
    if (policy.digest !== this.policy.digest) throw new Error('policy mismatch')
    return policy
  }
  async readPolicy() { return this.policy }
  async createFile(record: BrowserDeliveryRecordV1) {
    if (this.failCreate) { this.failCreate = false; throw new DOMException('delivery creation failed', 'QuotaExceededError') }
    const previous = this.files.get(record.fileId)
    if (previous !== undefined) return previous
    this.files.set(record.fileId, validateBrowserDeliveryRecord(this.policy, record)); return record
  }
  async readFile(_operationId: string, fileId: string) { return this.files.get(fileId) }
  async replaceFile(previous: BrowserDeliveryRecordV1, next: BrowserDeliveryRecordV1) {
    if (this.files.get(previous.fileId)?.digest !== previous.digest) throw new Error('stale journal')
    assertBrowserDeliveryTransition(this.policy, previous, next)
    if (this.failTransition === next.state.kind) { this.failTransition = undefined; throw new Error('journal failure') }
    await this.beforeReplace?.(next)
    this.files.set(next.fileId, next)
    this.transitions.push(next.state.kind)
  }
  async authorizeRestart(previous: BrowserDeliveryRecordV1, checkpoint: import('../../src/output/persistence/checkpoint').FileCheckpointV2, authorizationId: string) {
    const next = authorizeBrowserDeliveryRestart(this.policy, previous, checkpoint, authorizationId)
    await this.replaceFile(previous, next)
    return next
  }
  async finalizeDirect(previous: BrowserDeliveryRecordV1, proof: import('../../src/output/persistence/journal').FinalFileCheckpointProof) {
    const checkpoint = await this.readFinalCheckpoint(proof.recordId)
    if (checkpoint === undefined) throw new Error('target proof missing')
    const saved = advanceBrowserDeliveryRecord(this.policy, previous, { kind: 'target-saved', target: checkpoint })
    await this.replaceFile(previous, saved)
    const cleaned = advanceBrowserDeliveryRecord(this.policy, saved, { kind: 'cleaned', target: checkpoint })
    await this.replaceFile(saved, cleaned)
    return cleaned
  }
  async scanFiles(scan: BrowserDeliveryFileScan) { return { records: [...this.files.values()].filter(record => scan.afterFileId === undefined || record.fileId > scan.afterFileId) } }
  close() {}
}

export async function deliveryEngineFixture(overrides: Partial<Pick<BrowserDeliveryMaterializationOptions, 'reserveStage' | 'reconcileReservations'>> = {}) {
  const policy = deliveryPolicy()
  const repository = new DeliveryMemoryRepository(policy)
  const events: string[] = []
  const targetTree = new MemoryTree(events, policy.target)
  const stageTree = new MemoryTree(events, policy.staging!)
  const targetCheckpoints = new MemoryCheckpointRepository(policy.target)
  repository.readFinalCheckpoint = recordId => targetCheckpoints.readCommitted(recordId)
  const stageCheckpoints = new MemoryCheckpointRepository(policy.staging!)
  await stageTree.proposeFileOwnedObjectId(['unused'], { fileId: deliveryIdentity(50, 16), fileRevision: deliveryIdentity(51, 16), exactSize: 0n })
  let targetSession: PersistentTreeOutputSession
  let stageSession: PersistentTreeOutputSession
  const holds = new Map<string, BrowserDeliveryReservation>()
  const phases = new Map<string, string>()
  let now = 10
  let deleteFailure = false
  let targetWrite: ((request: PersistentFileRequest) => Promise<void>) | undefined
  const makeTarget = (): BrowserDeliveryTargetPort => ({
    beginDirectFile: async (request, delivery) => {
      delivery.committed(await repository.createFile(delivery.currentRecord()))
      const transaction = await makeTarget().beginFile(request)
      return { ...transaction, commit: async signal => {
        const result = await transaction.commit(signal)
        delivery.committed(await repository.finalizeDirect(delivery.currentRecord(), result.checkpointProof))
        return result
      } }
    },
    beginFile: async request => {
      const transaction = await targetSession.beginFile(request)
      return { ...transactionPort(transaction), writeRange: async (...args) => {
        await targetWrite?.(request); await transaction.writeRange(...args)
      } }
    },
    ensureDirectory: path => targetSession.ensureDirectory(path),
    readCheckpoint: fileId => readBrowserDeliveryCheckpoint(targetCheckpoints, fileId),
    close: () => targetSession.close(),
  })
  const stage: BrowserDeliveryStagePort = {
    beginFile: request => stageSession.beginFile(request),
    ensureDirectory: path => stageSession.ensureDirectory(path),
    readCheckpoint: fileId => readBrowserDeliveryCheckpoint(stageCheckpoints, fileId),
    bindCapacity: async () => undefined,
    readComplete: async checkpoint => {
      const reader = await acquireArtifactReader(policy.staging!.operationId)
      const file = stageTree.file(checkpoint.canonicalPath)
      return { blob: await file.read(), release: () => reader.release() }
    },
    removeComplete: async checkpoint => withArtifactCleanup(policy.staging!.operationId, async () => {
      if (deleteFailure) { deleteFailure = false; throw new Error('delete failure') }
      events.push('delete-stage')
      if (await stageTree.openFile(checkpoint.canonicalPath, checkpoint.ownedObjectId) !== undefined) await stageTree.removeFile(checkpoint.canonicalPath, checkpoint.ownedObjectId)
    }),
    discard: async (source, checkpoint) => {
      if (checkpoint !== undefined && await stageTree.openFile(browserDeliveryStagingPath(source.fileId), checkpoint.ownedObjectId) !== undefined) {
        await stageTree.removeFile(browserDeliveryStagingPath(source.fileId), checkpoint.ownedObjectId)
      }
    },
    close: () => stageSession.close(),
  }
  const reserve = async (source: { fileId: string }, retained?: BrowserDeliveryRecordV1) => {
    if (retained !== undefined || !holds.has(source.fileId)) {
      let phase = 'queued'
      if (retained === undefined || retained.state.kind === 'receiving') phase = 'receiving'
      else if (retained.state.kind === 'target-saved' || retained.state.kind === 'cleanup-pending') phase = 'saved'
      phases.set(source.fileId, phase)
      const reservation: BrowserDeliveryReservation = {
        objectCapacity: { reserveGrowth: async () => ({ reservationId: 'test', settle: async () => undefined, release: async () => undefined }) },
        received: async () => undefined,
        queueExport: async () => { phases.set(source.fileId, 'queued') },
        beginExport: async () => { phases.set(source.fileId, 'copying'); return true },
        exportFailed: async () => { phases.set(source.fileId, 'failed') },
        targetSaved: async () => { phases.set(source.fileId, 'saved'); events.push('budget-saved') },
        releaseDeleted: async () => { phases.set(source.fileId, 'released'); events.push('budget-released') },
        releaseDiscarded: async () => { phases.set(source.fileId, 'released') },
        cancelUnused: async () => { phases.set(source.fileId, 'released') },
      }
      holds.set(source.fileId, reservation)
    }
    return holds.get(source.fileId)!
  }
  const reopen = async () => {
    targetSession = await PersistentTreeOutputSession.open({ tree: targetTree, checkpoints: targetCheckpoints, semantic: targetCheckpoints })
    stageSession = await PersistentTreeOutputSession.open({ tree: stageTree, checkpoints: stageCheckpoints })
    return BrowserDeliveryMaterialization.open({ policy, repository, target: makeTarget(), staging: stage,
      choosePlacement: async source => ({ placement: source.exactSize <= 2n ? 'direct' : 'staged', reason: 'test-authenticated-size' }),
      reserveStage: reserve, now: () => now,
      trace: event => events.push(event.transition),
      ...overrides,
    })
  }
  const reopenCleanup = async () => {
    stageSession = await PersistentTreeOutputSession.open({ tree: stageTree, checkpoints: stageCheckpoints })
    return BrowserDeliveryMaterialization.openForCleanup({ policy, repository, staging: stage,
      reserveStage: reserve, ...overrides })
  }
  const session = await reopen()
  const request = (id = 11, size = 4n, path = ['nested', `file-${id}.bin`]): PersistentFileRequest => ({
    sourceAuthenticationPath: path,
    materializationRelativePath: path,
    openRevision: async () => ({ fileId: deliveryIdentity(id, 16), fileRevision: deliveryIdentity(22, 16), exactSize: size }),
  })
  return { policy, repository, targetTree, stageTree, targetCheckpoints, stageCheckpoints, events, phases, session, request, reopen, reopenCleanup,
    setNow: (value: number) => { now = value }, failDelete: () => { deleteFailure = true },
    onTargetWrite: (callback: typeof targetWrite) => { targetWrite = callback } }
}

function transactionPort(transaction: Awaited<ReturnType<PersistentTreeOutputSession['beginFile']>>) {
  return {
    revision: transaction.revision, ownedObjectId: transaction.ownedObjectId,
    checkpointPolicy: transaction.checkpointPolicy,
    initialDurableRanges: transaction.initialDurableRanges,
    get verifiedRanges() { return transaction.verifiedRanges },
    writeRange: transaction.writeRange.bind(transaction),
    automaticCheckpoint: transaction.automaticCheckpoint.bind(transaction),
    checkpoint: transaction.checkpoint.bind(transaction),
    commit: transaction.commit.bind(transaction),
    pause: transaction.pause.bind(transaction),
    retire: transaction.retire.bind(transaction),
    close: transaction.close.bind(transaction),
  }
}

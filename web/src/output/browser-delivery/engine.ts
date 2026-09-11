import { withStagingExportAuthority } from '../staging-budget/export-authority'
import type { PersistentFileRequest, PersistentFileTransactionPort, PersistentFinalFileCommit } from '../persistent-tree/contracts'
import { fileCheckpointIsComplete, type FileCheckpointV2 } from '../persistence/checkpoint'
import type { BrowserDeliveryRecordV1, BrowserDeliveryState, BrowserSavePolicyV1 } from './model'
import { advanceBrowserDeliveryRecord, stageCheckpoint, targetCheckpoint } from './lifecycle'
import type { BrowserDeliveryRepository } from './repository'
import type { BrowserDeliveryReservation, BrowserDeliveryRuntimeTrace, BrowserDeliveryStagePort, BrowserDeliveryTargetPort } from './ports'

const LOCAL_COPY_CHUNK_BYTES = 1024 * 1024

export interface BrowserDeliveryEngineOptions {
  readonly policy: BrowserSavePolicyV1
  readonly repository: BrowserDeliveryRepository
  readonly target: BrowserDeliveryTargetPort
  readonly staging?: BrowserDeliveryStagePort
  readonly reservation: (record: BrowserDeliveryRecordV1) => Promise<BrowserDeliveryReservation>
  readonly changed: (record: BrowserDeliveryRecordV1) => void
  readonly released?: (fileId: string) => void
  readonly currentRecord?: (fileId: string) => BrowserDeliveryRecordV1 | undefined
  readonly now?: () => number
  readonly trace?: (event: BrowserDeliveryRuntimeTrace) => void
  readonly observeCopy?: (bytes: bigint, milliseconds: number) => void
}

/** Only this local reducer turns receiving proof into destination proof and then releases staging. */
export class BrowserDeliveryEngine {
  readonly #options: BrowserDeliveryEngineOptions
  readonly #pending = new Map<string, Promise<unknown>>()

  constructor(options: BrowserDeliveryEngineOptions) { this.#options = options }

  async recordCheckpoint(fileId: string, checkpoint: FileCheckpointV2): Promise<void> {
    const record = await this.#read(fileId)
    if (record.state.kind !== 'receiving') return
    const receiving = await this.#advance(record, { kind: 'receiving', checkpoint })
    this.#emit(receiving, 'receiving')
    if (record.placement === 'staged') {
      await (await this.#options.reservation(record)).received(checkpointBytes(checkpoint))
    }
  }

  async completeReceiving(fileId: string, commit: PersistentFinalFileCommit): Promise<BrowserDeliveryRecordV1> {
    const record = this.#options.currentRecord?.(fileId) ?? await this.#read(fileId)
    if (record.placement === 'direct' && record.state.kind === 'cleaned') {
      this.#emit(record, 'target-saved')
      this.#emit(record, 'cleaned')
      return record
    }
    if (record.state.kind !== 'receiving') return record
    if (record.placement === 'direct') {
      const cleaned = await this.#options.repository.finalizeDirect(record, commit.checkpointProof)
      this.#options.changed(cleaned)
      this.#emit(cleaned, 'target-saved')
      this.#emit(cleaned, 'cleaned')
      return cleaned
    }
    const port = this.#stage()
    const checkpoint = await port.readCheckpoint(fileId)
    if (checkpoint === undefined || checkpoint.recordId !== commit.recordId || !fileCheckpointIsComplete(checkpoint)) {
      throw new TypeError('Completed receiving transaction has no matching durable checkpoint')
    }
    const staged = await this.#advance(record, { kind: 'staged-complete', stage: checkpoint })
    const reservation = await this.#options.reservation(staged)
    await reservation.received(checkpoint.exactSize)
    await reservation.queueExport()
    return staged
  }

  deliver(fileId: string, request?: PersistentFileRequest, activeTarget?: PersistentFileTransactionPort, signal?: AbortSignal): Promise<PersistentFinalFileCommit> {
    return this.#serialize(fileId, () => this.#deliver(fileId, request, activeTarget, signal))
  }

  async #deliver(fileId: string, request: PersistentFileRequest | undefined, activeTarget: PersistentFileTransactionPort | undefined, signal: AbortSignal | undefined): Promise<PersistentFinalFileCommit> {
    let record = await this.reconcile(fileId)
    const saved = targetCheckpoint(record.state)
    if (saved !== undefined) {
      const target = activeTarget ?? await this.#openTarget(record, request)
      try {
        const commit = await target.commit(signal)
        if (commit.ownedObjectId !== saved.ownedObjectId) throw new TypeError('Saved target ownership changed')
        await this.#cleanup(record).catch(error => this.#emit(record, 'cleanup-failed', error))
        return commit
      } finally { await target.close() }
    }
    const stage = stageCheckpoint(record.state)
    if (stage === undefined) throw new DOMException('File has incomplete staging and still requires its source revision', 'InvalidStateError')
    return withStagingExportAuthority(async authority => {
      signal?.throwIfAborted()
      let target = activeTarget
      let reader: Awaited<ReturnType<BrowserDeliveryStagePort['readComplete']>> | undefined
      const reservation = await this.#options.reservation(record)
      // On reopen the operation lease proves the old page lost writer authority. The
      // target opener additionally truncates only a verified owned, incomplete target.
      if (record.state.kind === 'copying') {
        if (target !== undefined) { await target.retire(); target = undefined }
        record = await this.#advance(record, { kind: 'staged-complete', stage })
      }
      if (!await reservation.beginExport(authority)) throw new DOMException('Another staged file is still saving', 'InvalidStateError')
      const started = this.#now()
      let savedCommit: PersistentFinalFileCommit | undefined
      try {
        record = await this.#advance(record, { kind: 'copying', stage,
          attempt: { attemptId: crypto.randomUUID(), ...(target === undefined ? {} : { targetOwnedObjectId: target.ownedObjectId }) } })
        target ??= await this.#openTarget(record, request)
        if (record.state.kind !== 'copying') throw new TypeError('Copy attempt lost its journal state')
        if (record.state.attempt.targetOwnedObjectId === undefined) {
          record = await this.#advance(record, { ...record.state, attempt: { ...record.state.attempt, targetOwnedObjectId: target.ownedObjectId } })
        }
        this.#emit(record, 'copy-started')
        const existing = await this.#options.target.readCheckpoint(fileId)
        if (existing === undefined || !fileCheckpointIsComplete(existing)) {
          reader = await this.#stage().readComplete(stage)
          await this.#copyBytes(reader.blob, target, signal)
        }
        const commit = await target.commit(signal)
        const checkpoint = await this.#options.target.readCheckpoint(fileId)
        if (checkpoint === undefined || checkpoint.recordId !== commit.recordId || !fileCheckpointIsComplete(checkpoint)) {
          throw new TypeError('Destination close has no durable final target proof')
        }
        record = await this.#advance(record, { kind: 'target-saved', target: checkpoint, stage })
        savedCommit = commit
        const elapsed = Math.max(0, this.#now() - started)
        this.#emit(record, 'target-saved', undefined, elapsed)
        try { this.#options.observeCopy?.(record.source.exactSize, elapsed) } catch { /* Observation has no delivery authority. */ }
        await reservation.targetSaved()
      } catch (error) {
        if (savedCommit !== undefined) {
          this.#emit(record, 'cleanup-failed', error)
          return savedCommit
        }
        // A failed writable must be fully ended before a fresh attempt can create
        // an atomic replacement; otherwise the old temporary file becomes a third copy.
        await target?.retire(error)
        target = undefined
        await reservation.exportFailed()
        if (record.state.kind === 'copying') {
          record = await this.#advance(record, { kind: 'staged-complete', stage, failureReason: failureName(error) })
        }
        this.#emit(record, 'copy-failed', error)
        throw error
      } finally {
        reader?.release()
        await target?.close()
      }
      await this.#cleanup(record).catch(error => this.#emit(record, 'cleanup-failed', error))
      return savedCommit!
    }, signal)
  }

  async #copyBytes(blob: Blob, target: PersistentFileTransactionPort, signal?: AbortSignal): Promise<void> {
    for (let offset = 0; offset < blob.size; offset += LOCAL_COPY_CHUNK_BYTES) {
      signal?.throwIfAborted()
      const bytes = new Uint8Array(await blob.slice(offset, offset + LOCAL_COPY_CHUNK_BYTES).arrayBuffer())
      await target.writeRange(BigInt(offset), bytes, signal)
    }
  }

  async authorizeRestart(fileId: string): Promise<BrowserDeliveryRecordV1> {
    const record = await this.#read(fileId)
    if (record.placement !== 'direct' || record.state.kind !== 'receiving') return record
    const checkpoint = await this.#options.target.readCheckpoint(fileId)
    if (checkpoint === undefined || checkpoint.verifiedRanges.length === 0 || fileCheckpointIsComplete(checkpoint)) return record
    const authorized = await this.#options.repository.authorizeRestart(record, checkpoint, crypto.randomUUID())
    this.#options.changed(authorized)
    return authorized
  }

  async completeRestart(fileId: string): Promise<BrowserDeliveryRecordV1> {
    const record = await this.#read(fileId)
    if (record.state.kind !== 'restart-authorized') return record
    const checkpoint = await this.#options.target.readCheckpoint(fileId)
    if (checkpoint === undefined) throw new TypeError('Authorized restart lost its target checkpoint')
    return this.#advance(record, { kind: 'receiving', checkpoint })
  }

  async reconcile(fileId: string): Promise<BrowserDeliveryRecordV1> {
    let record = await this.#read(fileId)
    if (record.state.kind === 'restart-authorized') {
      const target = await this.#openTarget(record)
      try { return await this.completeRestart(fileId) }
      finally { await target.retire() }
    }
    if (record.state.kind !== 'receiving') return record
    const port = record.placement === 'staged' ? this.#stage() : this.#options.target
    const checkpoint = await port.readCheckpoint(fileId)
    if (checkpoint === undefined) return record
    if (fileCheckpointIsComplete(checkpoint)) {
      if (record.placement === 'staged') {
        record = await this.#advance(record, { kind: 'staged-complete', stage: checkpoint })
        const reservation = await this.#options.reservation(record)
        await reservation.received(checkpoint.exactSize)
        await reservation.queueExport()
      } else {
        // A complete paused checkpoint can precede the final-file ledger. Reopen
        // owned local content to establish that proof before claiming a saved target.
        const target = await this.#openTarget(record)
        try {
          const commit = await target.commit()
          record = await this.#options.repository.finalizeDirect(record, commit.checkpointProof)
          this.#options.changed(record)
        } finally { await target.close() }
      }
    } else if (record.state.checkpoint?.checkpointGeneration !== checkpoint.checkpointGeneration ||
        record.state.checkpoint?.stateGeneration !== checkpoint.stateGeneration) {
      record = await this.#advance(record, { kind: 'receiving', checkpoint })
    }
    return record
  }

  discard(fileId: string): Promise<void> {
    return this.#serialize(fileId, async () => {
      let record = await this.#read(fileId)
      if (record.state.kind === 'discarded' || record.state.kind === 'cleaned') return
      if (targetCheckpoint(record.state) !== undefined) { await this.#cleanup(record); return }
      if (record.placement !== 'staged') return
      const checkpoint = record.state.kind === 'discarding' ? record.state.checkpoint
        : await this.#stage().readCheckpoint(fileId) ?? stageCheckpoint(record.state)
      if (record.state.kind !== 'discarding') {
        record = await this.#advance(record, { kind: 'discarding', ...(checkpoint === undefined ? {} : { checkpoint }) })
      }
      await this.#stage().discard(record.source, checkpoint)
      await (await this.#options.reservation(record)).releaseDiscarded()
      this.#options.released?.(record.fileId)
      await this.#advance(record, { kind: 'discarded' })
    })
  }

  cleanup(fileId: string): Promise<void> {
    return this.#serialize(fileId, async () => { await this.#cleanup(await this.#read(fileId)) })
  }

  async #cleanup(record: BrowserDeliveryRecordV1): Promise<void> {
    if (record.state.kind === 'cleaned') return
    const target = targetCheckpoint(record.state)
    if (target === undefined) return
    const stage = stageCheckpoint(record.state)
    if (stage === undefined) { await this.#advance(record, { kind: 'cleaned', target }); return }
    if (record.state.kind === 'target-saved') record = await this.#advance(record, { kind: 'cleanup-pending', target, stage })
    const reservation = await this.#options.reservation(record)
    try {
      await this.#stage().removeComplete(stage)
      await reservation.releaseDeleted()
      this.#options.released?.(record.fileId)
      await this.#advance(record, { kind: 'cleaned', target })
      this.#emit(record, 'cleaned')
    } catch (error) {
      await this.#advance(record, { kind: 'cleanup-pending', target, stage, failureReason: failureName(error) })
      throw error
    }
  }

  #openTarget(record: BrowserDeliveryRecordV1, request?: PersistentFileRequest): Promise<PersistentFileTransactionPort> {
    return this.#options.target.beginFile({ ...request, sourceAuthenticationPath: record.source.canonicalPath, materializationRelativePath: record.materializationRelativePath,
      recovery: { pausedFile: 'restart-owned-file' },
      openRevision: async () => record.source })
  }

  async #read(fileId: string): Promise<BrowserDeliveryRecordV1> {
    const record = await this.#options.repository.readFile(this.#options.policy.operationId, fileId)
    if (record === undefined) throw new TypeError('Browser delivery record disappeared')
    return record
  }

  async #advance(previous: BrowserDeliveryRecordV1, state: BrowserDeliveryState): Promise<BrowserDeliveryRecordV1> {
    const next = advanceBrowserDeliveryRecord(this.#options.policy, previous, state)
    await this.#options.repository.replaceFile(previous, next)
    this.#options.changed(next)
    return next
  }

  #stage(): BrowserDeliveryStagePort {
    if (this.#options.staging === undefined) throw new DOMException('Retained staging is unavailable', 'InvalidStateError')
    return this.#options.staging
  }

  #serialize<T>(fileId: string, work: () => Promise<T>): Promise<T> {
    const previous = this.#pending.get(fileId) ?? Promise.resolve()
    const next = previous.catch(() => undefined).then(work)
    this.#pending.set(fileId, next)
    next.finally(() => { if (this.#pending.get(fileId) === next) this.#pending.delete(fileId) }).catch(() => undefined)
    return next
  }

  #now(): number { return this.#options.now?.() ?? performance.now() }

  #emit(record: BrowserDeliveryRecordV1, transition: BrowserDeliveryRuntimeTrace['transition'], error?: unknown, milliseconds?: number): void {
    try { this.#options.trace?.({ name: 'browser.delivery.runtime', operation_id: record.operationId, file_id: record.fileId,
      transition, placement: record.placement, placement_reason: record.placementReason,
      ...(error === undefined ? {} : { failure_name: failureName(error) }),
      ...(milliseconds === undefined ? {} : { copy_milliseconds: milliseconds }) }) } catch { /* Diagnostics cannot change ownership. */ }
  }
}

export function checkpointBytes(checkpoint: FileCheckpointV2 | undefined): bigint {
  return checkpoint?.verifiedRanges.reduce((sum, range) => sum + range.end - range.start, 0n) ?? 0n
}

function failureName(error: unknown): string { return error instanceof Error ? error.name : 'UnknownError' }

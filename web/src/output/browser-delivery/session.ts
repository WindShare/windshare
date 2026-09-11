import type { PersistentFileRequest, PersistentFileTransactionPort, PersistentMaterializationPort } from '../persistent-tree/contracts'
import { fileCheckpointIsComplete } from '../persistence/checkpoint'
import type { BrowserDeliveryRecordV1, BrowserDeliverySource } from './model'
import { browserDeliveryStagingPath, createBrowserDeliveryRecord, validateBrowserDeliveryRecord } from './records'
import { BrowserDeliveryLiveProjection, type BrowserDeliveryResumeSummary } from './retained'
import type { BrowserDeliveryPlacementDecision, BrowserDeliveryReservation, BrowserDeliveryStagePort, BrowserDeliveryTargetPort } from './ports'
import { BrowserDeliveryEngine, checkpointBytes, type BrowserDeliveryEngineOptions } from './engine'

export interface BrowserDeliveryMaterializationOptions extends Omit<BrowserDeliveryEngineOptions, 'changed' | 'reservation' | 'released' | 'currentRecord'> {
  readonly choosePlacement: (source: BrowserDeliverySource) => Promise<BrowserDeliveryPlacementDecision>
  readonly reserveStage: (source: BrowserDeliverySource, retained?: BrowserDeliveryRecordV1) => Promise<BrowserDeliveryReservation | undefined>
  readonly observeReceipt?: (newReceivedBytes: bigint) => void
  readonly observeFlush?: (bytes: bigint, durationMilliseconds: number) => void
  readonly closeResources?: () => void
  readonly reconcileReservations?: (deliveryFileIds: readonly string[]) => Promise<void>
}

export type BrowserDeliveryCleanup = Pick<BrowserDeliveryMaterialization,
  'getSummary' | 'subscribe' | 'cleanupStaging' | 'discardStaging' | 'discardIncompleteStaging' | 'close'>

/** Folders retain one target authority while each unopened authenticated file chooses receiving storage. */
export class BrowserDeliveryMaterialization implements PersistentMaterializationPort {
  readonly #options: BrowserDeliveryMaterializationOptions
  readonly #engine: BrowserDeliveryEngine
  readonly #projection: BrowserDeliveryLiveProjection
  readonly #records = new Map<string, BrowserDeliveryRecordV1>()
  readonly #reservations = new Map<string, BrowserDeliveryReservation>()
  readonly #transactions = new Set<BrowserDeliveryFileTransaction>()
  readonly #beginnings = new Set<Promise<PersistentFileTransactionPort>>()
  readonly #localWork = new Set<Promise<void>>()
  readonly #listeners = new Set<(summary: BrowserDeliveryResumeSummary) => void>()
  #newReceivedBytes = 0n
  #closed = false
  #receivingClosed = false
  #dispositionPromise: Promise<void> | undefined
  #closePromise: Promise<void> | undefined

  private constructor(options: BrowserDeliveryMaterializationOptions) {
    this.#options = options
    this.#projection = new BrowserDeliveryLiveProjection(options.policy)
    this.#engine = new BrowserDeliveryEngine({ ...options, changed: record => this.#changed(record),
      reservation: record => this.#reservation(record), released: fileId => this.#reservations.delete(fileId),
      currentRecord: fileId => this.#records.get(fileId) })
  }

  static async open(options: BrowserDeliveryMaterializationOptions): Promise<BrowserDeliveryMaterialization> {
    const result = await this.#load(options)
    for (const record of result.#records.values()) await result.#engine.reconcile(record.fileId)
    return result
  }

  static openForCleanup(options: Omit<BrowserDeliveryMaterializationOptions, 'target' | 'choosePlacement'>): Promise<BrowserDeliveryCleanup> {
    const unavailable = async (): Promise<never> => { throw new DOMException('Cleanup has no destination write authority', 'InvalidStateError') }
    return this.#load({ ...options, choosePlacement: unavailable,
      target: { beginFile: unavailable, beginDirectFile: unavailable, ensureDirectory: unavailable, readCheckpoint: unavailable,
        verifyStagedTarget: unavailable, close: async () => undefined } })
  }

  static async #load(options: BrowserDeliveryMaterializationOptions): Promise<BrowserDeliveryMaterialization> {
    const policy = await options.repository.installPolicy(options.policy)
    const result = new BrowserDeliveryMaterialization({ ...options, policy })
    let afterFileId: string | undefined
    do {
      const page = await options.repository.scanFiles({ operationId: policy.operationId, ...(afterFileId === undefined ? {} : { afterFileId }) })
      for (const record of page.records) result.#changed(validateBrowserDeliveryRecord(policy, record))
      afterFileId = page.nextFileId
    } while (afterFileId !== undefined)
    await options.reconcileReservations?.([...result.#records.keys()])
    // Retained obligations are restored before any fresh file can consume capacity.
    for (const record of result.#records.values()) {
      if (record.placement === 'staged' && record.state.kind !== 'cleaned' && record.state.kind !== 'discarded') await result.#reservation(record)
    }
    return result
  }

  get summary(): BrowserDeliveryResumeSummary { return this.getSummary() }
  getSummary(): BrowserDeliveryResumeSummary { return this.#projection.summary() }
  subscribe(listener: (summary: BrowserDeliveryResumeSummary) => void): () => void {
    this.#listeners.add(listener)
    return () => this.#listeners.delete(listener)
  }

  beginFile(request: PersistentFileRequest): Promise<PersistentFileTransactionPort> {
    if (this.#receivingClosed) return Promise.reject(new DOMException('Browser delivery receiving is closed', 'InvalidStateError'))
    const opening = this.#beginFile(request)
    this.#beginnings.add(opening)
    opening.finally(() => this.#beginnings.delete(opening)).catch(() => undefined)
    return opening
  }

  async #beginFile(request: PersistentFileRequest): Promise<PersistentFileTransactionPort> {
    if (request.sourceAuthenticationPath === undefined) throw new TypeError('Browser delivery requires the authenticated source coordinate')
    const revision = await request.openRevision()
    const source: BrowserDeliverySource = { ...revision, canonicalPath: request.sourceAuthenticationPath }
    const record = await this.#selectRecord(source, request.materializationRelativePath)
    return this.#openReceiving(record, source, request)
  }

  async #selectRecord(source: BrowserDeliverySource, materializationRelativePath: readonly string[]): Promise<BrowserDeliveryRecordV1> {
    const record = this.#records.get(source.fileId)
    if (record === undefined) return this.#createRecord(source, materializationRelativePath)
    const expected = createBrowserDeliveryRecord({ policy: this.#options.policy, source, materializationRelativePath,
      placement: record.placement, placementReason: record.placementReason })
    if (expected.source.fileRevision !== record.source.fileRevision || expected.source.exactSize !== record.source.exactSize ||
        JSON.stringify(expected.source.canonicalPath) !== JSON.stringify(record.source.canonicalPath) ||
        JSON.stringify(expected.materializationRelativePath) !== JSON.stringify(record.materializationRelativePath)) {
      throw new DOMException('Retained file requires its original authenticated source revision', 'InvalidStateError')
    }
    if (record.state.kind === 'discarding' || record.state.kind === 'discarded') {
      throw new DOMException('Discarded staging cannot be received again by the retained operation', 'InvalidStateError')
    }
    return record
  }

  async #createRecord(source: BrowserDeliverySource, materializationRelativePath: readonly string[]): Promise<BrowserDeliveryRecordV1> {
    let record: BrowserDeliveryRecordV1
    let decision = await this.#options.choosePlacement(source)
    let reservation: BrowserDeliveryReservation | undefined
    if (decision.placement === 'staged') {
      reservation = await this.#options.reserveStage(source)
      if (reservation === undefined) decision = { placement: 'direct', reason: 'staging-capacity-unavailable' }
    }
    // Direct placement joins the target's initial claim before it can create or write the file.
    if (decision.placement === 'direct') return createBrowserDeliveryRecord({ policy: this.#options.policy, source,
      materializationRelativePath, placement: decision.placement, placementReason: decision.reason })
    try {
      record = await this.#options.repository.createFile(createBrowserDeliveryRecord({ policy: this.#options.policy, source, materializationRelativePath,
        placement: decision.placement, placementReason: decision.reason }))
    } catch (error) {
      // Only a confirmed absent placement can release this unused claim. An
      // unknown IndexedDB outcome remains an obligation until lease recovery.
      if (reservation !== undefined && await this.#options.repository.readFile(this.#options.policy.operationId, source.fileId) === undefined) {
        await reservation.cancelUnused()
      }
      throw error
    }
    if (reservation !== undefined) this.#reservations.set(record.fileId, reservation)
    this.#changed(record)
    try { this.#options.trace?.({ name: 'browser.delivery.runtime', operation_id: record.operationId,
      file_id: record.fileId, transition: 'placement', placement: record.placement, placement_reason: record.placementReason }) }
    catch { /* Diagnostics cannot change file placement. */ }
    return record
  }

  async #openReceiving(record: BrowserDeliveryRecordV1, source: BrowserDeliverySource, request: PersistentFileRequest): Promise<PersistentFileTransactionPort> {
    const openedSource = record.source
    const openedRequest: PersistentFileRequest = { ...request, sourceAuthenticationPath: openedSource.canonicalPath, materializationRelativePath: record.materializationRelativePath, openRevision: async () => openedSource }
    let target: PersistentFileTransactionPort | undefined
    let receiving: PersistentFileTransactionPort | undefined
    try {
      if (record.placement === 'direct' && request.recovery?.pausedFile === 'restart-owned-file') {
        record = await this.#engine.authorizeRestart(record.fileId)
      }
      // beginFile reserves an owned empty entry; FSA does not open its writable until
      // the first write. This prebinds final identity without allocating a target copy.
      let targetRequest = openedRequest
      if (record.placement === 'staged') targetRequest = { ...openedRequest, recovery: { pausedFile: 'preserve' } }
      else if (record.state.kind === 'restart-authorized') targetRequest = { ...openedRequest, recovery: { pausedFile: 'restart-owned-file' } }
      target = record.placement === 'direct'
        ? await this.#options.target.beginDirectFile(targetRequest, {
          currentRecord: () => this.#records.get(record.fileId) ?? record,
          committed: committed => {
            this.#changed(committed)
            if (committed.generation === 1n) {
              try { this.#options.trace?.({ name: 'browser.delivery.runtime', operation_id: committed.operationId,
                file_id: committed.fileId, transition: 'placement', placement: committed.placement, placement_reason: committed.placementReason }) }
              catch { /* Diagnostics cannot change committed placement. */ }
            }
          },
        })
        : await this.#options.target.beginFile(targetRequest)
      if (record.state.kind === 'restart-authorized') record = await this.#engine.completeRestart(record.fileId)
      if (record.placement === 'staged' && record.state.kind === 'receiving') {
        const reservation = await this.#reservation(record)
        await this.#stage().bindCapacity(source, reservation.objectCapacity)
        receiving = await this.#stage().beginFile({ ...openedRequest, materializationRelativePath: browserDeliveryStagingPath(record.fileId), recovery: { pausedFile: 'preserve' } })
      } else if (record.placement === 'direct') receiving = target
      const transaction = new BrowserDeliveryFileTransaction({ record, request: openedRequest, target, ...(receiving === undefined ? {} : { receiving }),
        engine: this.#engine, targetPort: this.#options.target,
        ...(this.#options.staging === undefined ? {} : { stage: this.#options.staging }),
        release: file => this.#transactions.delete(file),
        received: bytes => { this.#newReceivedBytes += bytes; this.#options.observeReceipt?.(this.#newReceivedBytes) },
        flushed: (bytes, duration) => this.#options.observeFlush?.(bytes, duration),
        now: this.#options.now ?? (() => performance.now()) })
      this.#transactions.add(transaction)
      return transaction
    } catch (error) {
      await receiving?.retire(error)
      if (target !== receiving) await target?.retire(error)
      throw error
    }
  }

  ensureDirectory(path: readonly string[]) { return this.#options.target.ensureDirectory(path) }
  materializeDirectory: NonNullable<PersistentMaterializationPort['materializeDirectory']> = request => {
    const method = this.#options.target.materializeDirectory
    if (method === undefined) return Promise.reject(new TypeError('Target directory ledger is unavailable'))
    return method.call(this.#options.target, request)
  }
  finalizeDirectory: NonNullable<PersistentMaterializationPort['finalizeDirectory']> = (admission, outcome) => {
    const method = this.#options.target.finalizeDirectory
    if (method === undefined) return Promise.reject(new TypeError('Target directory ledger is unavailable'))
    return method.call(this.#options.target, admission, outcome)
  }

  saveStagedFiles(signal?: AbortSignal): Promise<void> {
    return this.#runLocal(() => this.#saveStagedFiles(signal))
  }
  async #saveStagedFiles(signal?: AbortSignal): Promise<void> {
    for (const record of await this.#currentRecords()) {
      signal?.throwIfAborted()
      const current = await this.#engine.reconcile(record.fileId)
      if (current.state.kind === 'staged-complete' || current.state.kind === 'copying') {
        await this.#engine.deliver(record.fileId, undefined, undefined, signal)
      } else if (current.state.kind === 'target-saved' || current.state.kind === 'cleanup-pending') {
        await this.#engine.cleanup(record.fileId)
      }
    }
  }
  cleanupStaging(signal?: AbortSignal): Promise<void> {
    return this.#runLocal(() => this.#cleanupStaging(signal))
  }
  async #cleanupStaging(signal?: AbortSignal): Promise<void> {
    for (const record of await this.#currentRecords()) {
      signal?.throwIfAborted()
      if (record.state.kind === 'discarding') await this.#engine.discard(record.fileId)
      else await this.#engine.cleanup(record.fileId)
    }
  }
  cleanupSavedFiles(signal?: AbortSignal): Promise<void> { return this.cleanupStaging(signal) }

  discardStaging(signal?: AbortSignal): Promise<void> {
    return this.#runLocal(() => this.#discardStaging(signal))
  }
  async #discardStaging(signal?: AbortSignal): Promise<void> {
    await Promise.all([...this.#transactions].map(transaction => transaction.pause()))
    for (const record of await this.#currentRecords()) {
      signal?.throwIfAborted()
      if (record.placement === 'staged') await this.#engine.discard(record.fileId)
    }
  }

  /** Explicit terminal abandonment also repairs operations stopped before automatic disposal existed. */
  discardIncompleteStaging(signal?: AbortSignal): Promise<void> {
    if (this.#closed) return Promise.reject(new DOMException('Browser delivery session is closed', 'InvalidStateError'))
    if (this.#dispositionPromise !== undefined) return this.#dispositionPromise
    this.#receivingClosed = true
    const running = (async () => {
      const failures = await this.#drain()
      if (failures.length > 0) throw new AggregateError(failures, 'Browser delivery writers could not be drained')
      await this.#discardIncompleteStaging(signal)
    })()
    this.#dispositionPromise = running
    this.#localWork.add(running)
    running.finally(() => {
      this.#localWork.delete(running)
      this.#dispositionPromise = undefined
    }).catch(() => undefined)
    return running
  }

  async #discardIncompleteStaging(signal?: AbortSignal): Promise<void> {
    const failures: unknown[] = []
    for (const record of await this.#currentRecords()) {
      signal?.throwIfAborted()
      if (record.placement !== 'staged') continue
      try { await this.#engine.discardIncomplete(record.fileId) }
      catch (error) { failures.push(error) }
    }
    if (failures.length > 0) throw new AggregateError(failures, 'Browser delivery staging cleanup remains pending')
  }

  closeForTerminalSettlement(): Promise<void> { return this.#close('terminal') }
  closeForStopSettlement(): Promise<void> { return this.#close('stop') }
  close(): Promise<void> { return this.#close('preserve') }

  #close(disposition: 'preserve' | 'terminal' | 'stop'): Promise<void> {
    this.#closed = true
    this.#receivingClosed = true
    this.#closePromise ??= (async () => {
      const failures = await this.#drain()
      if (disposition === 'stop' && failures.length === 0) {
        // The parent Stop is already durable. Its terminal authority and the child
        // journal retain failed cleanup as local work, without failing source Stop.
        await this.#discardIncompleteStaging().catch(error => {
          for (const record of this.#records.values()) {
            if (record.placement !== 'staged' || record.state.kind === 'cleaned' || record.state.kind === 'discarded') continue
            try { this.#options.trace?.({ name: 'browser.delivery.runtime', operation_id: record.operationId,
              file_id: record.fileId, transition: 'stop-cleanup-pending',
              failure_name: error instanceof Error ? error.name : 'UnknownError' }) }
            catch { /* Durable child ownership survives unavailable diagnostics. */ }
          }
        })
      }
      try { await this.#options.staging?.close() } catch (error) { failures.push(error) }
      try {
        if (disposition === 'stop' && this.#options.target.closeForStopSettlement !== undefined) await this.#options.target.closeForStopSettlement()
        else if (disposition !== 'preserve' && this.#options.target.closeForTerminalSettlement !== undefined) await this.#options.target.closeForTerminalSettlement()
        else await this.#options.target.close()
      } catch (error) { failures.push(error) }
      this.#options.closeResources?.()
      if (failures.length > 0) throw new AggregateError(failures, 'Browser delivery close failed')
    })()
    return this.#closePromise
  }

  async #drain(): Promise<unknown[]> {
    await Promise.allSettled([...this.#beginnings, ...this.#localWork])
    const paused = await Promise.allSettled([...this.#transactions].map(transaction => transaction.pause()))
    return paused.filter((result): result is PromiseRejectedResult => result.status === 'rejected').map(result => result.reason as unknown)
  }

  #runLocal(work: () => Promise<void>): Promise<void> {
    if (this.#closed || this.#dispositionPromise !== undefined) return Promise.reject(new DOMException('Browser delivery local work is closed', 'InvalidStateError'))
    const running = work()
    this.#localWork.add(running)
    running.finally(() => this.#localWork.delete(running)).catch(() => undefined)
    return running
  }

  async #reservation(record: BrowserDeliveryRecordV1): Promise<BrowserDeliveryReservation> {
    let reservation = this.#reservations.get(record.fileId)
    if (reservation === undefined) {
      reservation = await this.#options.reserveStage(record.source, record)
      if (reservation === undefined) throw new DOMException('Retained staging capacity could not be restored', 'QuotaExceededError')
      this.#reservations.set(record.fileId, reservation)
    }
    return reservation
  }

  #changed(record: BrowserDeliveryRecordV1): void {
    this.#projection.replace(record)
    this.#records.set(record.fileId, record)
    if (this.#listeners.size === 0) return
    const summary = this.getSummary()
    for (const listener of this.#listeners) { try { listener(summary) } catch { /* Presentation has no journal authority. */ } }
  }
  async #currentRecords(): Promise<readonly BrowserDeliveryRecordV1[]> {
    const records: BrowserDeliveryRecordV1[] = []
    let afterFileId: string | undefined
    do {
      const page = await this.#options.repository.scanFiles({ operationId: this.#options.policy.operationId, ...(afterFileId === undefined ? {} : { afterFileId }) })
      records.push(...page.records); afterFileId = page.nextFileId
    } while (afterFileId !== undefined)
    return records
  }
  #stage(): BrowserDeliveryStagePort {
    if (this.#options.staging === undefined) throw new DOMException('Staging authority is unavailable', 'InvalidStateError')
    return this.#options.staging
  }
}

interface BrowserDeliveryFileTransactionOptions {
  readonly record: BrowserDeliveryRecordV1
  readonly request: PersistentFileRequest
  readonly target: PersistentFileTransactionPort
  readonly receiving?: PersistentFileTransactionPort
  readonly targetPort: BrowserDeliveryTargetPort
  readonly stage?: BrowserDeliveryStagePort
  readonly engine: BrowserDeliveryEngine
  readonly release: (file: BrowserDeliveryFileTransaction) => void
  readonly received: (bytes: bigint) => void
  readonly flushed: (bytes: bigint, duration: number) => void
  readonly now: () => number
}

class BrowserDeliveryFileTransaction implements PersistentFileTransactionPort {
  readonly #options: BrowserDeliveryFileTransactionOptions
  #tail: Promise<unknown> = Promise.resolve()
  #settled = false
  #copyAttemptStarted = false
  #checkpointedBytes: bigint
  constructor(options: BrowserDeliveryFileTransactionOptions) {
    this.#options = options
    this.#checkpointedBytes = options.receiving?.initialDurableRanges.reduce((sum, range) => sum + range.end - range.start, 0n) ?? 0n
  }
  get revision() { return this.#options.target.revision }
  get ownedObjectId() { return this.#options.target.ownedObjectId }
  get checkpointObjectId() {
    return this.#options.receiving?.checkpointObjectId ?? this.#options.receiving?.ownedObjectId ??
      ('stage' in this.#options.record.state ? this.#options.record.state.stage?.ownedObjectId : undefined) ?? this.ownedObjectId
  }
  get checkpointPolicy() { return this.#options.receiving?.checkpointPolicy ?? this.#options.target.checkpointPolicy ?? { kind: 'disabled' as const } }
  get initialDurableRanges() {
    return this.#options.receiving?.initialDurableRanges ?? (this.revision.exactSize === 0n ? [] : [{ start: 0n, end: this.revision.exactSize }])
  }
  get verifiedRanges() { return this.#options.receiving?.verifiedRanges ?? this.initialDurableRanges }
  writeRange(...args: Parameters<PersistentFileTransactionPort['writeRange']>) {
    return this.#serialize(async () => {
      if (this.#options.receiving === undefined) throw new TypeError('Complete local staging does not accept source bytes')
      await this.#options.receiving.writeRange(...args)
      this.#options.received(BigInt(args[1].byteLength))
    })
  }
  automaticCheckpoint(...args: Parameters<PersistentFileTransactionPort['automaticCheckpoint']>) {
    return this.#serialize(async () => {
      const started = this.#options.now()
      const result = await this.#options.receiving!.automaticCheckpoint(...args)
      if (result.kind === 'advanced') {
        const previousBytes = this.#checkpointedBytes
        await this.#recordCheckpoint()
        this.#options.flushed(this.#checkpointedBytes - previousBytes, this.#options.now() - started)
      }
      return result
    })
  }
  checkpoint(signal?: AbortSignal) { return this.#serialize(async () => {
    const ranges = await this.#options.receiving!.checkpoint(signal); await this.#recordCheckpoint(); return ranges
  }) }
  commit(signal?: AbortSignal) { return this.#serialize(async () => {
    const options = this.#options
    if (options.receiving !== undefined) {
      const received = await options.receiving.commit(signal)
      await options.engine.completeReceiving(options.record.fileId, received)
      if (options.record.placement === 'direct') { this.#finish(); return received }
    }
    const activeTarget = this.#copyAttemptStarted ? undefined : options.target
    this.#copyAttemptStarted = true
    const saved = await options.engine.deliver(options.record.fileId, options.request, activeTarget, signal)
    this.#finish()
    return saved
  }) }
  pause(reason?: unknown) { return this.#serialize(async () => {
    if (this.#settled) return this.verifiedRanges
    let durable = this.verifiedRanges
    const failures: unknown[] = []
    try {
      if (this.#options.receiving !== undefined) {
        const port = this.#options.record.placement === 'staged' ? this.#options.stage! : this.#options.targetPort
        const checkpoint = await port.readCheckpoint(this.#options.record.fileId)
        if (checkpoint === undefined || !fileCheckpointIsComplete(checkpoint)) {
          durable = await this.#options.receiving.pause(reason)
          await this.#recordCheckpoint()
        }
      }
    } catch (error) { failures.push(error) }
    // A checkpoint read/commit failure cannot leave a native writer alive while
    // Stop classifies its storage. A complete checkpoint may still own a writer too.
    const receivingRetired = await this.#options.receiving?.retire(reason).then(() => true, error => { failures.push(error); return false }) ?? true
    const targetRetired = this.#options.receiving === this.#options.target ||
      await this.#options.target.retire(reason).then(() => true, error => { failures.push(error); return false })
    if (receivingRetired && targetRetired) this.#finish()
    if (failures.length === 1) throw failures[0]
    if (failures.length > 1) throw new AggregateError(failures, 'Browser delivery pause failed')
    return durable
  }) }
  retire(reason?: unknown) { return this.pause(reason).then(() => undefined) }
  close() { return this.retire() }
  async #recordCheckpoint(): Promise<void> {
    const options = this.#options
    const port = options.record.placement === 'staged' ? options.stage! : options.targetPort
    const checkpoint = await port.readCheckpoint(options.record.fileId)
    if (checkpoint !== undefined) {
      await options.engine.recordCheckpoint(options.record.fileId, checkpoint)
      this.#checkpointedBytes = checkpointBytes(checkpoint)
    }
  }
  #serialize<T>(work: () => Promise<T>): Promise<T> {
    const next = this.#tail.catch(() => undefined).then(work); this.#tail = next; return next
  }
  #finish(): void { this.#settled = true; this.#options.release(this) }
}

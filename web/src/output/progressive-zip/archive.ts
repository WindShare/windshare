import { SourceFaultCode } from '../../transfer/failure/fault'
import { zipMemberCoordinates } from '../zip-layout/member'
import {
  evaluatePersistedSourceAdmission,
  persistedSourceAdmissionError,
  type PersistedSourceAdmissionRejection,
} from '../persistence/source-admission'
import { receiveContentWarningFields, snapshotReceiveContentWarning, type ReceiveContentWarning } from '../../transfer/lifecycle/content-warning'
import type { NativeObjectIO } from '../origin-private/native-object/contracts'
import { writeObjectBatch, type NativeObjectWrite, type ObjectWriteBatchCapacity } from '../origin-private/native-object/write-batch'
import { inspectZipForFinalization, zipFinalizationBatches } from './finalization'
import type { ObjectCheckpointCoordinator } from '../origin-private/native-object/coordinator'
import type { TaskCheckpoint, TaskEntry, TaskObjectRef, TaskDirectoryPin } from '../origin-private/task-checkpoint/model'
import type { TaskCheckpointStore } from '../origin-private/task-checkpoint/store'
import {
  checkedZipAdd, encodeZipEndRecords, normalizeZipEntry, planZipEntry,
  requiresZip64End,
} from '../zip-layout/policy'
import { completeZipEntryCrc, insertZipCrcRange, zipBytesCrc32, zipRangeDisposition } from './crc-ranges'

const ENTRY_PAGE_SIZE = 128
const CHECKPOINT_METADATA_HEADROOM = 1024n * 1024n
const MAX_PENDING_ENTRIES = 128
const MAX_ENTRY_RANGES = 16_384
const RANGE_METADATA_BYTES = 64n
const ENTRY_METADATA_BYTES = 4096n
const ENTRY_ALLOCATION_CHECKPOINT = 'zip-entry-allocation'
const REVISION_FAILURE_CHECKPOINT = 'zip-revision-unavailable'
const ZIP_LAYOUT_FROZEN = 'ZIP entry layout is frozen for finalization'

export type ZipObjectCapacity = ObjectWriteBatchCapacity

export type TerminalSourceRevisionFaultCode =
  | typeof SourceFaultCode.RevisionChanged
  | typeof SourceFaultCode.RevisionInvalidated
  | typeof SourceFaultCode.Permanent

/** Transient availability or missing comparison evidence must not taint retained entries as unusable revisions. */
export function isTerminalSourceRevisionFaultCode(code: SourceFaultCode): code is TerminalSourceRevisionFaultCode {
  return code === SourceFaultCode.RevisionChanged ||
    code === SourceFaultCode.RevisionInvalidated ||
    code === SourceFaultCode.Permanent
}

export interface ProgressiveZipInput {
  readonly store: TaskCheckpointStore
  readonly coordinator: ObjectCheckpointCoordinator
  readonly capacity: ZipObjectCapacity
  readonly object: TaskObjectRef
  readonly selectedPaths: readonly (readonly string[])[]
  readonly trace?: (event: string, detail: Readonly<Record<string, string | bigint>>) => void
}

export interface ZipEntryAdmission {
  readonly entryId: string
  readonly path: readonly string[]
  readonly source: TaskEntry['source']
  readonly modifiedTimeMilliseconds?: bigint
}

type ZipEntryRevision = NonNullable<TaskEntry['revision']>
type ZipEntryRequest = ZipEntryAdmission & { readonly revision?: ZipEntryRevision }

type ZipEntryComparison =
  | Readonly<{ kind: 'same' }>
  | Readonly<{ kind: 'reuse'; previous: ZipEntryRevision; current: ZipEntryRevision }>
  | Readonly<{
      kind: 'rejected'
      outcome: PersistedSourceAdmissionRejection
      previous?: ZipEntryRevision
      current?: ZipEntryRevision
    }>

type ZipObservedChange = Extract<PersistedSourceAdmissionRejection, { kind: 'source-changed' }>

export interface CompleteZipEntrySpan {
  readonly entry: TaskEntry
  readonly payloadOffset: bigint
  readonly length: bigint
  readonly crc32: number
}

/** Owns one archive; persistent pages, rather than an all-entry ledger, are recovery authority. */
export class ProgressiveZipArchive {
  readonly #input: ProgressiveZipInput
  readonly #dirty = new Map<string, TaskEntry>()
  #checkpoint: TaskCheckpoint
  #physicalLength = 0n
  #headroomBytes = 0n
  readonly #headroomReservations: { release(): Promise<void> }[] = []

  private constructor(input: ProgressiveZipInput, checkpoint: TaskCheckpoint) {
    this.#input = input
    this.#checkpoint = checkpoint
  }

  static async open(input: ProgressiveZipInput): Promise<ProgressiveZipArchive> {
    let checkpoint = await input.store.readCheckpoint()
    if (checkpoint === undefined) {
      checkpoint = {
        object: input.object, generation: 1n, allocatedLength: 0n, physicalLength: 0n, entryCount: 0n,
        discoveryComplete: false, selectedPaths: input.selectedPaths, artifactState: 'receiving',
      }
      await input.store.commit({ expectedGeneration: undefined, checkpoint, entries: [] })
    }
    if (checkpoint.object.operationId !== input.object.operationId ||
        checkpoint.object.objectId !== input.object.objectId ||
        checkpoint.object.handleId !== input.object.handleId ||
        checkpoint.object.kind !== input.object.kind || input.object.kind !== 'zip-archive') {
      throw new TypeError('ZIP checkpoint belongs to another task object')
    }
    const archive = new ProgressiveZipArchive(input, checkpoint)
    const physicalLength = await input.coordinator.size()
    archive.#physicalLength = physicalLength
    for await (const entry of archive.#entries()) {
      for (const range of entry.ranges) {
        if (entry.zipLayout!.payloadOffset + range.end > physicalLength) {
          await input.coordinator.close()
          throw new Error('ZIP object is shorter than its committed payload coverage')
        }
      }
    }
    if (checkpoint.finalization !== undefined && checkpoint.finalization.nextEntry > 0n &&
        physicalLength < checkpoint.finalization.committedLength) {
      await input.coordinator.close()
      throw new Error('ZIP object is shorter than its committed central directory')
    }
    if (checkpoint.sealedLength !== undefined && physicalLength !== checkpoint.sealedLength) {
      await input.coordinator.close()
      throw new Error('ZIP sealed object length differs from its checkpoint')
    }
    if (checkpoint.artifactState !== 'sealed') {
      await archive.#ensureHeadroom(physicalLength, CHECKPOINT_METADATA_HEADROOM)
    }
    return archive
  }

  observeCapacityWindow(stage: string, activeEntries: number): void {
    this.#trace(stage, { generation: this.#checkpoint.generation, activeEntries: BigInt(activeEntries) })
  }

  get state(): TaskCheckpoint { return this.#checkpoint }
  get failed(): boolean { return this.#input.coordinator.failed }

  findCommittedEntry(entryId: string): Promise<TaskEntry | undefined> {
    return this.#input.store.readEntry(entryId)
  }

  async committedEntry(entryId: string): Promise<TaskEntry> {
    const entry = await this.findCommittedEntry(entryId)
    if (entry === undefined) throw new TypeError('ZIP entry has no committed checkpoint')
    return entry
  }

  async entry(entryId: string): Promise<TaskEntry> {
    const entry = this.#dirty.get(entryId) ?? await this.#input.store.readEntry(entryId)
    if (entry === undefined) throw new TypeError('ZIP entry has no persisted allocation')
    return entry
  }

  admitFile(input: ZipEntryAdmission & { readonly revision: ZipEntryRevision }): Promise<TaskEntry> {
    return this.#admit('file', input)
  }

  admitDirectory(input: ZipEntryAdmission): Promise<TaskEntry> {
    return this.#admit('directory', input)
  }

  async #admit(kind: TaskEntry['kind'], input: ZipEntryRequest): Promise<TaskEntry> {
    const result = await this.#input.coordinator.checkpointFromPlan<TaskEntry | ZipObservedChange>(async () => {
      if (this.#checkpoint.artifactState !== 'receiving') {
        return { kind: 'rejected', cause: new Error(ZIP_LAYOUT_FROZEN) }
      }
      const committed = await this.#input.store.readEntry(input.entryId)
      if (committed === undefined) {
        // Check allocation preconditions before flushing so a rejected entry never poisons the shared writer.
        if (this.#checkpoint.discoveryComplete) {
          return { kind: 'rejected', cause: new Error('ZIP discovery is closed') }
        }
        if (kind === 'file' && input.revision === undefined) {
          return { kind: 'rejected', cause: new TypeError('ZIP allocation requires an authenticated opened revision') }
        }
        return { kind: 'commit', reason: ENTRY_ALLOCATION_CHECKPOINT, commit: () => this.#allocate(kind, input) }
      }
      const existing = this.#dirty.get(input.entryId) ?? committed
      const comparison = compareZipEntry(existing, kind, input)
      if (comparison.kind === 'rejected') {
        if (comparison.outcome.kind === 'source-changed') {
          const changed = comparison.outcome
          return { kind: 'commit', reason: REVISION_FAILURE_CHECKPOINT, commit: async () => {
            await this.#commitRevisionFailure(existing, SourceFaultCode.RevisionChanged)
            return changed
          } }
        }
        if (comparison.outcome.kind === 'evidence-unavailable' &&
            comparison.previous !== undefined && comparison.current !== undefined) {
          this.#trace('source_comparison_retained', {
            entryId: committed.entryId, fileId: comparison.previous.fileId,
            previousRevision: comparison.previous.fileRevision, currentRevision: comparison.current.fileRevision,
            reason: comparison.outcome.comparison.reason,
            retainedBytes: committed.ranges.reduce((total, range) => total + range.end - range.start, 0n),
          })
        }
        return { kind: 'rejected', cause: persistedSourceAdmissionError(comparison.outcome) }
      }
      return { kind: 'commit', reason: ENTRY_ALLOCATION_CHECKPOINT, commit: () => this.#restoreEntry(existing, comparison) }
    })
    // A known entry refusal must leave the shared writer available for other entries.
    if (result.kind === 'source-changed') {
      throw persistedSourceAdmissionError(result, {
        cause: new Error(`ZIP source evidence changed (${result.reason})`),
      })
    }
    return result
  }

  async #restoreEntry(
    entry: TaskEntry, comparison: Extract<ZipEntryComparison, { kind: 'same' | 'reuse' }>,
  ): Promise<TaskEntry> {
    const { revisionFailure, ...retained } = entry
    if (comparison.kind === 'reuse') {
      const restored = { ...retained, revision: { ...comparison.current, reusedFrom: comparison.previous.fileRevision } }
      await this.#commit({}, [restored])
      this.#trace('source_metadata_reused', {
        entryId: entry.entryId, previousRevision: comparison.previous.fileRevision,
        currentRevision: comparison.current.fileRevision, reason: 'metadata-agreement',
        reusedBytes: entry.ranges.reduce((total, range) => total + range.end - range.start, 0n),
      })
      return restored
    }
    if (revisionFailure !== undefined) {
      await this.#commit({}, [retained])
      return retained
    }
    await this.#commit({})
    return entry
  }

  async #allocate(kind: TaskEntry['kind'], input: ZipEntryRequest): Promise<TaskEntry> {
    const normalized = normalizeZipEntry({
      kind, path: input.path, exactSize: input.revision?.exactSize ?? 0n,
      ...(input.modifiedTimeMilliseconds === undefined ? {} : {
        modifiedTimeMilliseconds: input.modifiedTimeMilliseconds,
      }),
    })
    const plan = planZipEntry(normalized, this.#checkpoint.allocatedLength)
    const { payloadOffset, descriptorOffset, endOffset } = zipMemberCoordinates(plan)
    const entry: TaskEntry = {
      entryId: input.entryId, kind, path: normalized.path, source: input.source,
      ...(input.revision === undefined ? {} : { revision: input.revision }),
      zipPlan: plan,
      zipLayout: {
        entryId: input.entryId, sequence: this.#checkpoint.entryCount,
        localHeaderOffset: plan.localHeaderOffset, payloadOffset, exactSize: plan.exactSize,
        descriptorOffset, endOffset, encodingVersion: 1,
      },
      ranges: [],
    }
    // Atomic topology validation and immutable offsets happen before any payload is admitted.
    await this.#commit({
      allocatedLength: endOffset, entryCount: this.#checkpoint.entryCount + 1n,
    }, [entry])
    this.#trace('zip_entry_allocated', { entryId: entry.entryId, endOffset })
    return entry
  }

  async writeRange(entryId: string, offset: bigint, bytes: Uint8Array): Promise<void> {
    await this.#input.coordinator.admittedMutation(async currentLength => {
      if (this.#checkpoint.artifactState !== 'receiving') throw new Error('ZIP is sealed against reception')
      const entry = await this.entry(entryId)
      const layout = entry.zipLayout!
      const end = checkedZipAdd(offset, BigInt(bytes.byteLength))
      if (entry.kind !== 'file' || end > layout.exactSize) throw new RangeError('ZIP write exceeds opened revision')
      if (zipRangeDisposition(entry.ranges, offset, end) === 'covered') return undefined
      const ranges = insertZipCrcRange(entry.ranges, { start: offset, end, crc32: zipBytesCrc32(bytes) })
      if (ranges.length > MAX_ENTRY_RANGES) throw new RangeError('ZIP fragmented range bound exceeded')
      const payloadOffset = checkedZipAdd(layout.payloadOffset, offset)
      await this.#ensureHeadroom(currentLength, this.#metadataBytes([{ ...entry, ranges }]))
      const reservation = await this.#input.capacity.reserveGrowth({
        operationId: this.#checkpoint.object.operationId, objectId: this.#checkpoint.object.objectId,
        currentLength, targetLength: payloadOffset + BigInt(bytes.byteLength),
        metadataHeadroom: 0n,
      })
      const targetLength = payloadOffset + BigInt(bytes.byteLength)
      return { entry, ranges, payloadOffset, reservation,
        occupiedBound: currentLength > targetLength ? currentLength : targetLength }
    }, async (io, admitted) => {
      if (admitted === undefined) return
      let charged = false
      try {
        await io.writeAt(admitted.payloadOffset, bytes)
        this.#physicalLength = admitted.occupiedBound
        await admitted.reservation.settle(this.#physicalLength)
        charged = true
        this.#dirty.set(entryId, { ...admitted.entry, ranges: admitted.ranges })
      } catch (error) {
        // A native failure can leave a partial extension; retain its maximum charge until recovery.
        try {
          await admitted.reservation.settle(admitted.occupiedBound)
          charged = true
        } catch { /* Keep the outstanding reservation fenced until physical recovery. */ }
        throw error
      } finally {
        if (charged) await admitted.reservation.release()
      }
    })
    if (this.#dirty.size >= MAX_PENDING_ENTRIES) await this.checkpoint('zip-pending-entry-bound')
  }

  checkpoint(reason: string): Promise<TaskCheckpoint> {
    return this.#input.coordinator.checkpointFromPlan(() => this.#dirty.size === 0
      ? { kind: 'unchanged', result: this.#checkpoint }
      : { kind: 'commit', reason, commit: () => this.#commit({}) })
  }

  async pinDirectory(directoryId: string, generation: string, sourcePath: readonly string[]): Promise<void> {
    const existing = await this.#input.store.readDirectoryPin(directoryId)
    if (existing !== undefined) {
      if (existing.generation !== generation || existing.sourcePath.join('/') !== sourcePath.join('/')) {
        throw new Error('ZIP discovery generation changed; the original selection must be retained')
      }
      return
    }
    await this.#input.coordinator.checkpoint('zip-discovery-generation', async () => {
      await this.#commit({}, [], [{ directoryId, generation, sourcePath }])
    })
  }

  async recordContentWarning(warning?: ReceiveContentWarning): Promise<void> {
    const contentWarning = warning === undefined ? undefined : snapshotReceiveContentWarning(warning)
    const checkpoint = await this.#input.coordinator.checkpointFromPlan(() => {
      if (this.#checkpoint.artifactState !== 'receiving') return { kind: 'unchanged', result: this.#checkpoint }
      if (contentWarning === undefined && this.#checkpoint.contentWarning === undefined) {
        return { kind: 'unchanged', result: this.#checkpoint }
      }
      return { kind: 'commit', reason: 'zip-content-warning',
        commit: () => this.#commit({ contentWarning: contentWarning ?? null }) }
    })
    if (checkpoint.artifactState === 'receiving' && contentWarning !== undefined) this.#traceContentWarning(contentWarning)
  }

  async markDiscoveryComplete(warning?: ReceiveContentWarning): Promise<void> {
    const contentWarning = warning === undefined ? undefined : snapshotReceiveContentWarning(warning)
    const checkpoint = await this.#input.coordinator.checkpointFromPlan(() => this.#checkpoint.artifactState === 'receiving'
      ? { kind: 'commit', reason: 'zip-discovery-complete',
          commit: () => this.#commit({ discoveryComplete: true, contentWarning: contentWarning ?? null }) }
      : { kind: 'unchanged', result: this.#checkpoint })
    if (checkpoint.artifactState === 'receiving' && contentWarning !== undefined) this.#traceContentWarning(contentWarning)
  }

  #traceContentWarning(contentWarning: ReceiveContentWarning): void {
    this.#trace('zip_selected_content_missing', {
      reason: JSON.stringify({ completed_file_count: contentWarning.completedFileCount.toString(),
        selected_file_count: contentWarning.selectedFileCount.toString(), missing_files: contentWarning.missingFiles }),
    })
  }

  markRevisionFailure(entryId: string, reason: TerminalSourceRevisionFaultCode): Promise<void> {
    return this.#input.coordinator.checkpointFromPlan(() => this.#checkpoint.artifactState === 'receiving'
      ? { kind: 'commit', reason: REVISION_FAILURE_CHECKPOINT,
          commit: async () => this.#commitRevisionFailure(await this.entry(entryId), reason) }
      : { kind: 'rejected', cause: new Error(ZIP_LAYOUT_FROZEN) })
  }

  async #commitRevisionFailure(entry: TaskEntry, reason: TerminalSourceRevisionFaultCode): Promise<void> {
    await this.#commit({}, [{ ...entry, revisionFailure: reason }])
    this.#trace('zip_revision_unavailable', { entryId: entry.entryId, path: entry.path.join('/'), reason })
  }

  async *completeEntries(): AsyncGenerator<CompleteZipEntrySpan> {
    for await (const entry of this.#entries()) {
      const crc32 = completeZipEntryCrc(entry.ranges, entry.zipLayout!.exactSize)
      if (crc32 !== undefined && entry.revisionFailure === undefined) {
        yield { entry, payloadOffset: entry.zipLayout!.payloadOffset,
          length: entry.zipLayout!.exactSize, crc32 }
      }
    }
  }

  async finalize(signal?: AbortSignal): Promise<TaskCheckpoint> {
    signal?.throwIfAborted()
    if (this.#checkpoint.artifactState === 'sealed') {
      await this.close()
      return this.#checkpoint
    }
    await this.checkpoint('zip-before-finalization')
    signal?.throwIfAborted()
    await this.#beginFinalization(signal)
    signal?.throwIfAborted()
    const start = this.#checkpoint.finalization!
    // Only the uncommitted central-directory tail is discarded after interrupted finalization.
    await this.#input.coordinator.mutate(io => this.#truncateTail(io, start.committedLength))
    let committedLength = start.committedLength
    const entries = this.#entries(start.nextEntry === 0n ? undefined : start.nextEntry - 1n)
    for await (const batch of zipFinalizationBatches(entries, committedLength)) {
      signal?.throwIfAborted()
      await this.#writeBatch(batch.writes)
      committedLength = batch.committedLength
      await this.#finalizationCheckpoint(batch.nextEntry, committedLength)
      this.#trace('zip_finalization_progress', {
        nextEntry: batch.nextEntry, entryCount: this.#checkpoint.entryCount, committedLength,
      })
    }
    signal?.throwIfAborted()
    const endLayout = { entryCount: this.#checkpoint.entryCount,
      centralDirectoryOffset: start.centralDirectoryOffset,
      centralDirectoryBytes: committedLength - start.centralDirectoryOffset }
    const ends = encodeZipEndRecords({ ...endLayout, zip64EndRequired: requiresZip64End(endLayout) })
    const endWrites: NativeObjectWrite[] = []
    for (const bytes of [ends.zip64End, ends.zip64Locator, ends.classicEnd]) {
      if (bytes === undefined) continue
      endWrites.push({ offset: committedLength, bytes })
      committedLength = checkedZipAdd(committedLength, BigInt(bytes.byteLength))
    }
    await this.#writeBatch(endWrites)
    signal?.throwIfAborted()
    const sealed = await this.#input.coordinator.checkpoint('zip-sealed', () =>
      this.#commit({ artifactState: 'sealed', sealedLength: committedLength }))
    this.#trace('zip_sealed', { sealedLength: committedLength, entryCount: sealed.entryCount })
    await this.close()
    return sealed
  }

  async close(): Promise<void> {
    try { await this.#input.coordinator.close() } finally {
      const reservations = this.#headroomReservations.splice(0)
      this.#headroomBytes = 0n
      await Promise.all(reservations.map(reservation => reservation.release()))
    }
  }

  async #beginFinalization(signal?: AbortSignal): Promise<void> {
    // Completeness inspection and the layout freeze share one coordinator turn to avoid TOCTOU races.
    await this.#input.coordinator.checkpointFromPlan(async () => {
      if (!this.#checkpoint.discoveryComplete) {
        return { kind: 'rejected', cause: new Error('ZIP still needs remote discovery') }
      }
      if (this.#checkpoint.artifactState !== 'receiving') return { kind: 'unchanged', result: this.#checkpoint }
      const result = await inspectZipForFinalization(this.#entries(), this.#checkpoint.contentWarning, signal)
      if (result.kind === 'rejected') return result
      const offset = this.#checkpoint.allocatedLength
      return { kind: 'commit', reason: 'zip-finalization-start', commit: () => this.#commit({
        artifactState: 'finalizing', contentWarning: result.contentWarning,
        finalization: { nextEntry: 0n, centralDirectoryOffset: offset, committedLength: offset },
      }) }
    })
  }

  async #finalizationCheckpoint(nextEntry: bigint, committedLength: bigint): Promise<void> {
    await this.#input.coordinator.checkpoint('zip-finalization-page', () => this.#commit({
      finalization: { nextEntry, committedLength,
        centralDirectoryOffset: this.#checkpoint.finalization!.centralDirectoryOffset },
    }))
  }

  async #truncateTail(io: NativeObjectIO, length: bigint): Promise<void> {
    const currentLength = await io.size()
    if (currentLength <= length) return
    const reservation = await this.#input.capacity.reserveGrowth({
      operationId: this.#checkpoint.object.operationId, objectId: this.#checkpoint.object.objectId,
      currentLength, targetLength: currentLength, metadataHeadroom: 0n,
    })
    let charged = false
    try {
      await io.truncate(length)
      this.#physicalLength = length
      await reservation.settle(length)
      charged = true
    } catch (error) {
      try {
        await reservation.settle(currentLength)
        charged = true
      } catch { /* Recovery must reconcile any uncertain native mutation. */ }
      throw error
    } finally {
      if (charged) await reservation.release()
    }
  }

  async #writeBatch(writes: readonly NativeObjectWrite[]): Promise<void> {
    await this.#input.coordinator.mutate(async io => {
      this.#physicalLength = await writeObjectBatch(io, {
        object: this.#checkpoint.object, capacity: this.#input.capacity, writes,
      })
    })
  }

  async #commit(
    changes: Partial<Omit<TaskCheckpoint, 'contentWarning'>> & { contentWarning?: ReceiveContentWarning | null },
    entries: readonly TaskEntry[] = [],
    directoryPins: readonly TaskDirectoryPin[] = [],
  ): Promise<TaskCheckpoint> {
    const changed = new Map(this.#dirty)
    for (const entry of entries) changed.set(entry.entryId, entry)
    const { contentWarning: previousWarning, ...previous } = this.#checkpoint
    const { contentWarning: updatedWarning, ...updates } = changes
    const warning = Object.hasOwn(changes, 'contentWarning') ? updatedWarning ?? undefined : previousWarning
    const checkpoint = { ...previous, ...updates, ...receiveContentWarningFields(warning), physicalLength: this.#physicalLength,
      generation: this.#checkpoint.generation + 1n }
    await this.#ensureHeadroom(this.#physicalLength, this.#metadataBytes(entries))
    await this.#input.store.commit({
      expectedGeneration: this.#checkpoint.generation, checkpoint,
      entries: [...changed.values()], directoryPins,
    })
    this.#checkpoint = checkpoint
    this.#dirty.clear()
    return checkpoint
  }

  #metadataBytes(entries: readonly TaskEntry[]): bigint {
    const changed = new Map(this.#dirty)
    for (const entry of entries) changed.set(entry.entryId, entry)
    let bytes = 0n
    for (const entry of changed.values()) {
      bytes += ENTRY_METADATA_BYTES + BigInt(entry.ranges.length) * RANGE_METADATA_BYTES +
        BigInt(entry.zipPlan?.nameBytes.length ?? 0) * 2n
    }
    return bytes > CHECKPOINT_METADATA_HEADROOM ? bytes : CHECKPOINT_METADATA_HEADROOM
  }

  async #ensureHeadroom(currentLength: bigint, requiredBytes: bigint): Promise<void> {
    if (requiredBytes <= this.#headroomBytes) return
    const additional = ((requiredBytes - this.#headroomBytes + CHECKPOINT_METADATA_HEADROOM - 1n) /
      CHECKPOINT_METADATA_HEADROOM) * CHECKPOINT_METADATA_HEADROOM
    // Metadata authority must survive payload settlement, otherwise another task can consume the checkpoint gap.
    const reservation = await this.#input.capacity.reserveGrowth({
      operationId: this.#checkpoint.object.operationId, objectId: this.#checkpoint.object.objectId,
      currentLength, targetLength: 0n, metadataHeadroom: additional,
    })
    this.#headroomReservations.push(reservation)
    this.#headroomBytes += additional
  }

  async *#entries(afterSequence?: bigint): AsyncGenerator<TaskEntry> {
    let cursor = afterSequence
    while (true) {
      const page = await this.#input.store.readEntries({
        ...(cursor === undefined ? {} : { afterSequence: cursor }), limit: ENTRY_PAGE_SIZE,
      })
      if (page.length === 0) return
      for (const entry of page) yield entry
      cursor = page.at(-1)!.zipLayout!.sequence
    }
  }

  #trace(event: string, detail: Readonly<Record<string, string | bigint>>): void {
    try {
      this.#input.trace?.(event, { operationId: this.#checkpoint.object.operationId,
        objectId: this.#checkpoint.object.objectId, checkpointGeneration: this.#checkpoint.generation, ...detail })
    } catch { /* Diagnostics cannot change a committed checkpoint outcome. */ }
  }
}

function compareZipEntry(entry: TaskEntry, kind: TaskEntry['kind'], input: ZipEntryRequest): ZipEntryComparison {
  const previous = entry.revision
  const current = input.revision
  if (!sameEntryContext(entry, kind, input) || previous?.fileId !== current?.fileId) {
    return { kind: 'rejected', outcome: { kind: 'source-changed', reason: 'context-changed' } }
  }
  if (previous === undefined || current === undefined) return { kind: 'same' }

  const admission = evaluatePersistedSourceAdmission({
    previous,
    current,
    allowsMetadataReuse: completeZipEntryCrc(entry.ranges, previous.exactSize) === undefined,
  })
  if (admission.kind === 'same-revision') return { kind: 'same' }
  if (admission.kind === 'metadata-reuse') return { kind: 'reuse', previous, current }
  return { kind: 'rejected', outcome: admission, previous, current }
}

function sameEntryContext(entry: TaskEntry, kind: TaskEntry['kind'], input: ZipEntryRequest): boolean {
  return entry.kind === kind && entry.path.join('/') === input.path.join('/') &&
    entry.source.shareInstance === input.source.shareInstance &&
    entry.source.directoryId === input.source.directoryId &&
    entry.source.generation === input.source.generation &&
    entry.source.sourcePath.join('/') === input.source.sourcePath.join('/')
}

import { fileCheckpointIsComplete } from '../persistence/checkpoint'
import { stageCheckpoint, targetCheckpoint } from './lifecycle'
import type { BrowserDeliveryRecordV1, BrowserSavePolicyV1 } from './model'
import { validateBrowserSavePolicy } from './policy'
import { validateBrowserDeliveryRecord } from './records'

export interface BrowserDeliveryResumeSummary {
  readonly policy: BrowserSavePolicyV1
  readonly receivingFiles: number
  readonly incompleteStagedFiles: number
  readonly stagedCompleteFiles: number
  readonly copyingFiles: number
  readonly cleanupPendingFiles: number
  readonly targetSavedFiles: number
  readonly discardedFiles: number
  readonly recoverableBytes: bigint
  readonly stagedBytes: bigint
  readonly reservedStagingBytes: bigint
  readonly targetSavedBytes: bigint
  readonly localContinuation?: 'save-staged-files' | 'retry-staging-cleanup'
}

export interface BrowserDeliveryLocalContinuation {
  readonly summary: BrowserDeliveryResumeSummary
  saveStagedFiles(signal?: AbortSignal): Promise<void>
  cleanupStaging(signal?: AbortSignal): Promise<void>
  /** Terminal receiving cannot reuse incomplete stages; completed files remain locally saveable. */
  discardIncompleteStaging(signal?: AbortSignal): Promise<void>
  /** Invoked only for an explicit user abandonment decision, after draining physical writers. */
  discardStaging(signal?: AbortSignal): Promise<void>
}

export function hasBrowserDeliveryStaging(summary: BrowserDeliveryResumeSummary): boolean {
  // Empty owned files and reservations still require disposal authority.
  return summary.incompleteStagedFiles + summary.stagedCompleteFiles + summary.copyingFiles + summary.cleanupPendingFiles > 0 ||
    summary.reservedStagingBytes > 0n
}

type DeliveryCounters = Omit<BrowserDeliveryResumeSummary, 'policy' | 'localContinuation'>
type MutableCounters = { -readonly [K in keyof DeliveryCounters]: DeliveryCounters[K] }

/** This describes retained work; only reopened storage authority can authorize a local copy or deletion. */
export function summarizeBrowserDeliveries(
  policyInput: BrowserSavePolicyV1,
  records: readonly BrowserDeliveryRecordV1[],
): BrowserDeliveryResumeSummary {
  const accumulator = new BrowserDeliveryResumeAccumulator(policyInput)
  accumulator.add(records)
  return accumulator.summary()
}

/** Inventory keeps counters and file identities while releasing each bounded checkpoint page. */
export class BrowserDeliveryResumeAccumulator {
  readonly #policy: BrowserSavePolicyV1
  readonly #seen = new Set<string>()
  readonly #counters: MutableCounters = {
    receivingFiles: 0, incompleteStagedFiles: 0, stagedCompleteFiles: 0, copyingFiles: 0, cleanupPendingFiles: 0, targetSavedFiles: 0,
    discardedFiles: 0, recoverableBytes: 0n, stagedBytes: 0n, reservedStagingBytes: 0n, targetSavedBytes: 0n,
  }

  constructor(policy: BrowserSavePolicyV1) { this.#policy = validateBrowserSavePolicy(policy) }

  add(records: readonly BrowserDeliveryRecordV1[]): void {
    for (const input of records) {
      const record = validateBrowserDeliveryRecord(this.#policy, input)
      if (this.#seen.has(record.fileId)) throw new TypeError('Browser delivery inventory repeats a file')
      this.#seen.add(record.fileId)
      addDelivery(this.#counters, record)
    }
  }

  summary(): BrowserDeliveryResumeSummary { return freezeSummary(this.#policy, this.#counters) }
}

/** Active tasks update only the changed file; retained history must not cause quadratic rehashing. */
export class BrowserDeliveryLiveProjection {
  readonly #policy: BrowserSavePolicyV1
  readonly #files = new Map<string, { generation: bigint; digest: string; counters: MutableCounters }>()
  readonly #counters = emptyCounters()

  constructor(policy: BrowserSavePolicyV1) { this.#policy = validateBrowserSavePolicy(policy) }

  replace(input: BrowserDeliveryRecordV1): void {
    const record = validateBrowserDeliveryRecord(this.#policy, input)
    const prior = this.#files.get(record.fileId)
    if (prior !== undefined && record.generation <= prior.generation) {
      if (record.digest === prior.digest) return
      throw new TypeError('Live browser delivery projection received a stale file generation')
    }
    const counters = emptyCounters()
    addDelivery(counters, record)
    updateCounterTotals(this.#counters, prior?.counters ?? emptyCounters(), counters)
    this.#files.set(record.fileId, { generation: record.generation, digest: record.digest, counters })
  }

  summary(): BrowserDeliveryResumeSummary { return freezeSummary(this.#policy, this.#counters) }
}

function freezeSummary(policy: BrowserSavePolicyV1, counters: DeliveryCounters): BrowserDeliveryResumeSummary {
  const localContinuation = continuationFor(counters)
  return Object.freeze({ policy, ...counters, ...(localContinuation === undefined ? {} : { localContinuation }) })
}

function emptyCounters(): MutableCounters {
  return {
    receivingFiles: 0, incompleteStagedFiles: 0, stagedCompleteFiles: 0, copyingFiles: 0, cleanupPendingFiles: 0, targetSavedFiles: 0,
    discardedFiles: 0, recoverableBytes: 0n, stagedBytes: 0n, reservedStagingBytes: 0n, targetSavedBytes: 0n,
  }
}

function updateCounterTotals(total: MutableCounters, previous: DeliveryCounters, next: DeliveryCounters): void {
  const fileCounters = [
    'receivingFiles', 'incompleteStagedFiles', 'stagedCompleteFiles', 'copyingFiles', 'cleanupPendingFiles', 'targetSavedFiles', 'discardedFiles',
  ] as const
  const byteCounters = ['recoverableBytes', 'stagedBytes', 'reservedStagingBytes', 'targetSavedBytes'] as const
  for (const key of fileCounters) total[key] += next[key] - previous[key]
  for (const key of byteCounters) total[key] += next[key] - previous[key]
}

function addDelivery(counters: MutableCounters, record: BrowserDeliveryRecordV1): void {
  const state = record.state
  const stage = stageCheckpoint(state)
  const target = targetCheckpoint(state)
  if (state.kind === 'receiving' || state.kind === 'discarding' || state.kind === 'restart-authorized') {
    addReceiving(counters, record, state)
  } else if (state.kind === 'staged-complete' || state.kind === 'copying') {
    counters.recoverableBytes += record.source.exactSize
  }
  if (stage !== undefined) {
    counters.stagedBytes += stage.exactSize
    counters.reservedStagingBytes += stage.exactSize
  }
  if (target !== undefined) {
    counters.targetSavedFiles += 1
    counters.targetSavedBytes += target.exactSize
    counters.recoverableBytes += target.exactSize
  }
  if (state.kind === 'discarded') counters.discardedFiles += 1
  if (state.kind === 'staged-complete') counters.stagedCompleteFiles += 1
  if (state.kind === 'copying') counters.copyingFiles += 1
  if (state.kind === 'cleanup-pending' || (state.kind === 'target-saved' && stage !== undefined)) counters.cleanupPendingFiles += 1
}

function addReceiving(
  counters: MutableCounters,
  record: BrowserDeliveryRecordV1,
  state: Extract<BrowserDeliveryRecordV1['state'], { kind: 'receiving' | 'discarding' | 'restart-authorized' }>,
): void {
  if (state.kind === 'receiving' || state.kind === 'restart-authorized') {
    if (record.placement === 'staged' && state.checkpoint !== undefined && fileCheckpointIsComplete(state.checkpoint)) {
      counters.stagedCompleteFiles += 1
    } else {
      counters.receivingFiles += 1
      if (record.placement === 'staged') counters.incompleteStagedFiles += 1
    }
  } else counters.cleanupPendingFiles += 1
  const durableBytes = state.checkpoint?.verifiedRanges.reduce((sum, range) => sum + range.end - range.start, 0n) ?? 0n
  counters.recoverableBytes += durableBytes
  if (record.placement === 'staged') {
    counters.stagedBytes += durableBytes
    counters.reservedStagingBytes += record.source.exactSize
  }
}

function continuationFor(counters: DeliveryCounters): BrowserDeliveryResumeSummary['localContinuation'] {
  if (counters.stagedCompleteFiles + counters.copyingFiles > 0) return 'save-staged-files'
  if (counters.cleanupPendingFiles > 0) return 'retry-staging-cleanup'
  return undefined
}

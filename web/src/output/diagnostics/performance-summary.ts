import {
  projectPerformancePhasePayloadV1,
  projectPerformanceSummaryPayloadV1,
} from '../../diagnostics/export/projector'
import type {
  TraceEventObservationV1,
} from '../../diagnostics/trace/model'
import type { DomainTraceSource, TraceClock } from '../../diagnostics/trace/ports'
import {
  PERFORMANCE_MILESTONES_V1,
  type PerformanceFilePipelineStageV1,
  type PerformanceCorrelationProjectionInput,
  type PerformanceMilestoneV1,
  type PerformanceNamespaceKindV1,
  type PerformanceSummaryPayloadV1,
  type PerformanceSummaryProjectionInput,
} from '../../diagnostics/trace/transfer-payload'
import {
  BoundedMillisecondsHistogram,
  PERFORMANCE_UINT64_MAX,
  requirePerformanceMilliseconds,
} from './performance-histogram'
import { BoundedPerformanceStarvationSummary } from './performance-starvation-summary'
import type { PerformanceClaimPhaseSamples } from './claim-batch-performance'
import type { PerformanceClaimInspectorSample } from './claim-inspector-performance'

const UINT32_MAX = 0xffff_ffff

export type PerformanceQueueLane = 'writer' | 'namespace'
export type PerformanceAuthorityKind = 'directory' | 'file'
export type PerformanceByteState = 'pending' | 'durable' | 'final'
export type PerformanceCheckpointTrigger = 'automatic' | 'forced_pause'
export type PerformanceCheckpointDecision = 'advanced' | 'declined'
export type PerformanceCheckpointCost =
  | 'constant'
  | 'prefix_copy'
  | 'space_preflight'
export type PerformanceLedgerTransition =
  | 'entry'
  | 'page'
  | 'seal'
  | 'recovery_scan_fallback'

export type PerformanceTraceEvent = Extract<
  TraceEventObservationV1,
  { readonly eventName: 'performance_phase' | 'performance_summary' }
>

export interface PerformanceSummarySink {
  observeQueueRun(
    lane: PerformanceQueueLane,
    waitMilliseconds: number,
    runMilliseconds: number,
    namespaceKind?: PerformanceNamespaceKindV1,
  ): void
  observeConcurrency(input: Readonly<{
    activeWriters: number
    queuedWriters: number
    activeNamespace: number
    queuedNamespace: number
  }>): void
  observeAuthorityLookup(input: Readonly<{
    kind: PerformanceAuthorityKind
    result: 'hit' | 'miss'
    walkDepth?: number
  }>): void
  observeAuthorityInvalidation(): void
  observeAuthorityEviction(): void
  observeByteTransition(state: PerformanceByteState, bytes: bigint): void
  observeCheckpoint(input: Readonly<{
    trigger: PerformanceCheckpointTrigger
    decision: PerformanceCheckpointDecision
    cost: PerformanceCheckpointCost
    elapsedMilliseconds: number
    estimatedCopyBytes?: bigint
  }>): void
  observeFinalTransaction(elapsedMilliseconds: number): void
  observeLedger(input: Readonly<{
    transition: PerformanceLedgerTransition
    elapsedMilliseconds?: number
  }>): void
  observeFilePipelineTransition(input: Readonly<{
    from?: PerformanceFilePipelineStageV1
    to?: PerformanceFilePipelineStageV1
    atMilliseconds: number
  }>): void
  observeRevisionOpenStarted(atMilliseconds: number): void
  observeRevisionOpenFinished(input: Readonly<{
    atMilliseconds: number
    waitMilliseconds: number
    runMilliseconds: number
    succeeded: boolean
  }>): void
  observeClaimBatch(input: Readonly<{
    size: number
    oldestWaitMilliseconds: number
    newestWaitMilliseconds: number
    runMilliseconds: number
    phases: PerformanceClaimPhaseSamples
  }>): void
  observeClaimInspector(input: PerformanceClaimInspectorSample): void
  observeOutputResource(input: Readonly<{
    resource: 'active_files' | 'write_bytes' | 'buffered_bytes'
    waitMilliseconds: number
    peak: number | bigint
  }>): void
  markMilestone(milestone: PerformanceMilestoneV1): boolean
  snapshot(): PerformanceSummaryProjectionInput
  complete(): PerformanceSummaryPayloadV1
}

export interface BoundedPerformanceSummaryOptions {
  readonly correlation: PerformanceCorrelationProjectionInput
  readonly clock: TraceClock
  readonly trace?: DomainTraceSource<PerformanceTraceEvent>
}

/**
 * Runtime call sites share the summary's clock so queue and repository durations
 * remain comparable to phase timestamps without letting diagnostics own policy.
 */
export interface PerformanceSummaryObservations {
  readonly summary: PerformanceSummarySink
  readonly clock: TraceClock
}

type MutableHistogram = BoundedMillisecondsHistogram

export class BoundedPerformanceSummary implements PerformanceSummarySink {
  readonly #correlation: PerformanceCorrelationProjectionInput
  readonly #clock: TraceClock
  readonly #trace: DomainTraceSource<PerformanceTraceEvent> | undefined
  readonly #startedAtMilliseconds: number
  #lastNowMilliseconds: number

  readonly #queue: Readonly<{
    writer: Readonly<{ wait: MutableHistogram; run: MutableHistogram }>
    namespace: Readonly<{ wait: MutableHistogram; run: MutableHistogram }>
  }>
  readonly #peaks = {
    activeWriters: 0,
    queuedWriters: 0,
    activeNamespace: 0,
    queuedNamespace: 0,
  }
  readonly #authorityCache = {
    directoryHits: 0n,
    directoryMisses: 0n,
    fileHits: 0n,
    fileMisses: 0n,
    invalidations: 0n,
    evictions: 0n,
    walkedSegments: 0n,
    maximumWalkDepth: 0,
  }
  readonly #bytes = {
    pending: 0n,
    durable: 0n,
    final: 0n,
  }
  readonly #checkpoints: {
    automaticTriggers: bigint
    forcedPauseTriggers: bigint
    advanced: bigint
    declined: bigint
    constantCost: bigint
    prefixCopyCost: bigint
    spacePreflightCost: bigint
    estimatedCopyBytes: bigint
    elapsed: MutableHistogram
  }
  readonly #finalTransactions: {
    count: bigint
    elapsed: MutableHistogram
  }
  readonly #ledger: {
    entries: bigint
    pages: bigint
    seals: bigint
    recoveryScanFallbacks: bigint
    sealElapsed: MutableHistogram
  }
  readonly #starvation: BoundedPerformanceStarvationSummary
  readonly #milestones: Partial<Record<PerformanceMilestoneV1, bigint>> = {}
  #latestMilestoneIndex = -1
  #counterOverflowed = false
  #completed: PerformanceSummaryPayloadV1 | undefined

  constructor(options: BoundedPerformanceSummaryOptions) {
    this.#correlation = snapshotCorrelation(options.correlation)
    this.#clock = options.clock
    this.#trace = options.trace
    this.#startedAtMilliseconds = requireClockMilliseconds(
      this.#clock.nowMilliseconds(),
      'performance observer start',
    )
    this.#lastNowMilliseconds = this.#startedAtMilliseconds
    const histogram = (): MutableHistogram =>
      new BoundedMillisecondsHistogram(() => {
        this.#counterOverflowed = true
      })
    this.#queue = {
      writer: { wait: histogram(), run: histogram() },
      namespace: { wait: histogram(), run: histogram() },
    }
    this.#checkpoints = {
      automaticTriggers: 0n,
      forcedPauseTriggers: 0n,
      advanced: 0n,
      declined: 0n,
      constantCost: 0n,
      prefixCopyCost: 0n,
      spacePreflightCost: 0n,
      estimatedCopyBytes: 0n,
      elapsed: histogram(),
    }
    this.#finalTransactions = {
      count: 0n,
      elapsed: histogram(),
    }
    this.#ledger = {
      entries: 0n,
      pages: 0n,
      seals: 0n,
      recoveryScanFallbacks: 0n,
      sealElapsed: histogram(),
    }
    this.#starvation = new BoundedPerformanceStarvationSummary(
      this.#startedAtMilliseconds,
      () => { this.#counterOverflowed = true },
    )
  }

  observeQueueRun(
    lane: PerformanceQueueLane,
    waitMilliseconds: number,
    runMilliseconds: number,
    namespaceKind?: PerformanceNamespaceKindV1,
  ): void {
    if (this.#completed !== undefined) return
    const summary = this.#queue[lane]
    if (summary === undefined) throw new TypeError('performance queue lane is invalid')
    summary.wait.observe(waitMilliseconds)
    summary.run.observe(runMilliseconds)
    if (lane === 'namespace') {
      if (namespaceKind === undefined) {
        throw new TypeError('performance namespace queue sample requires a kind')
      }
      this.#starvation.observeNamespace(namespaceKind, waitMilliseconds, runMilliseconds)
    } else if (namespaceKind !== undefined) {
      throw new TypeError('performance writer queue sample cannot carry a namespace kind')
    }
  }

  observeConcurrency(input: Readonly<{
    activeWriters: number
    queuedWriters: number
    activeNamespace: number
    queuedNamespace: number
  }>): void {
    if (this.#completed !== undefined) return
    this.#peaks.activeWriters = Math.max(
      this.#peaks.activeWriters,
      requireUint32(input.activeWriters, 'active writers'),
    )
    this.#peaks.queuedWriters = Math.max(
      this.#peaks.queuedWriters,
      requireUint32(input.queuedWriters, 'queued writers'),
    )
    this.#peaks.activeNamespace = Math.max(
      this.#peaks.activeNamespace,
      requireUint32(input.activeNamespace, 'active namespace work'),
    )
    this.#peaks.queuedNamespace = Math.max(
      this.#peaks.queuedNamespace,
      requireUint32(input.queuedNamespace, 'queued namespace work'),
    )
  }

  observeAuthorityLookup(input: Readonly<{
    kind: PerformanceAuthorityKind
    result: 'hit' | 'miss'
    walkDepth?: number
  }>): void {
    if (this.#completed !== undefined) return
    if (input.kind !== 'directory' && input.kind !== 'file') {
      throw new TypeError('performance authority kind is invalid')
    }
    if (input.result !== 'hit' && input.result !== 'miss') {
      throw new TypeError('performance authority result is invalid')
    }
    const counter = `${input.kind}${input.result === 'hit' ? 'Hits' : 'Misses'}` as
      | 'directoryHits'
      | 'directoryMisses'
      | 'fileHits'
      | 'fileMisses'
    this.#authorityCache[counter] = this.#increment(this.#authorityCache[counter])
    if (input.walkDepth !== undefined) {
      const walkDepth = requireUint32(input.walkDepth, 'authority walk depth')
      this.#authorityCache.walkedSegments = this.#add(
        this.#authorityCache.walkedSegments,
        BigInt(walkDepth),
      )
      this.#authorityCache.maximumWalkDepth = Math.max(
        this.#authorityCache.maximumWalkDepth,
        walkDepth,
      )
    }
  }

  observeAuthorityInvalidation(): void {
    if (this.#completed !== undefined) return
    this.#authorityCache.invalidations = this.#increment(
      this.#authorityCache.invalidations,
    )
  }

  observeAuthorityEviction(): void {
    if (this.#completed !== undefined) return
    this.#authorityCache.evictions = this.#increment(this.#authorityCache.evictions)
  }

  observeByteTransition(state: PerformanceByteState, bytes: bigint): void {
    if (this.#completed !== undefined) return
    if (state !== 'pending' && state !== 'durable' && state !== 'final') {
      throw new TypeError('performance byte state is invalid')
    }
    this.#bytes[state] = this.#add(this.#bytes[state], requireNonNegativeBigint(
      bytes,
      `${state} bytes`,
    ))
  }

  observeCheckpoint(input: Readonly<{
    trigger: PerformanceCheckpointTrigger
    decision: PerformanceCheckpointDecision
    cost: PerformanceCheckpointCost
    elapsedMilliseconds: number
    estimatedCopyBytes?: bigint
  }>): void {
    if (this.#completed !== undefined) return
    const trigger = checkpointCounter(input.trigger)
    const decision = checkpointCounter(input.decision)
    const cost = checkpointCounter(input.cost)
    this.#checkpoints[trigger] = this.#increment(this.#checkpoints[trigger])
    this.#checkpoints[decision] = this.#increment(this.#checkpoints[decision])
    this.#checkpoints[cost] = this.#increment(this.#checkpoints[cost])
    this.#checkpoints.estimatedCopyBytes = this.#add(
      this.#checkpoints.estimatedCopyBytes,
      requireNonNegativeBigint(
        input.estimatedCopyBytes ?? 0n,
        'checkpoint estimated copy bytes',
      ),
    )
    this.#checkpoints.elapsed.observe(input.elapsedMilliseconds)
  }

  observeFinalTransaction(elapsedMilliseconds: number): void {
    if (this.#completed !== undefined) return
    this.#finalTransactions.count = this.#increment(this.#finalTransactions.count)
    this.#finalTransactions.elapsed.observe(elapsedMilliseconds)
  }

  observeLedger(input: Readonly<{
    transition: PerformanceLedgerTransition
    elapsedMilliseconds?: number
  }>): void {
    if (this.#completed !== undefined) return
    switch (input.transition) {
      case 'entry':
        requireAbsentElapsed(input.elapsedMilliseconds, 'ledger entry')
        this.#ledger.entries = this.#increment(this.#ledger.entries)
        return
      case 'page':
        requireAbsentElapsed(input.elapsedMilliseconds, 'ledger page')
        this.#ledger.pages = this.#increment(this.#ledger.pages)
        return
      case 'seal':
        if (input.elapsedMilliseconds === undefined) {
          throw new TypeError('ledger seal requires elapsed milliseconds')
        }
        this.#ledger.seals = this.#increment(this.#ledger.seals)
        this.#ledger.sealElapsed.observe(input.elapsedMilliseconds)
        return
      case 'recovery_scan_fallback':
        requireAbsentElapsed(input.elapsedMilliseconds, 'ledger recovery scan fallback')
        this.#ledger.recoveryScanFallbacks = this.#increment(
          this.#ledger.recoveryScanFallbacks,
        )
        return
      default:
        throw new TypeError('performance ledger transition is invalid')
    }
  }

  observeFilePipelineTransition(input: Readonly<{
    from?: PerformanceFilePipelineStageV1
    to?: PerformanceFilePipelineStageV1
    atMilliseconds: number
  }>): void {
    if (this.#completed !== undefined) return
    this.#starvation.transitionPipeline(input)
  }

  observeRevisionOpenStarted(atMilliseconds: number): void {
    if (this.#completed !== undefined) return
    this.#starvation.revisionStarted(atMilliseconds)
  }

  observeRevisionOpenFinished(input: Readonly<{
    atMilliseconds: number
    waitMilliseconds: number
    runMilliseconds: number
    succeeded: boolean
  }>): void {
    if (this.#completed !== undefined) return
    this.#starvation.revisionFinished(input)
  }

  observeClaimBatch(input: Readonly<{
    size: number
    oldestWaitMilliseconds: number
    newestWaitMilliseconds: number
    runMilliseconds: number
    phases: PerformanceClaimPhaseSamples
  }>): void {
    if (this.#completed !== undefined) return
    this.#starvation.observeClaimBatch(input)
  }

  observeClaimInspector(input: PerformanceClaimInspectorSample): void {
    if (this.#completed !== undefined) return
    this.#starvation.observeClaimInspector(input)
  }

  observeOutputResource(input: Readonly<{
    resource: 'active_files' | 'write_bytes' | 'buffered_bytes'
    waitMilliseconds: number
    peak: number | bigint
  }>): void {
    if (this.#completed !== undefined) return
    this.#starvation.observeOutputResource(input)
  }

  markMilestone(milestone: PerformanceMilestoneV1): boolean {
    if (this.#completed !== undefined || this.#milestones[milestone] !== undefined) {
      return false
    }
    const milestoneIndex = PERFORMANCE_MILESTONES_V1.indexOf(milestone)
    if (milestoneIndex < 0) throw new TypeError('performance milestone is invalid')
    if (milestoneIndex < this.#latestMilestoneIndex) {
      throw new TypeError('performance milestone order regressed')
    }
    const elapsed = BigInt(this.#readNow() - this.#startedAtMilliseconds)
    this.#milestones[milestone] = elapsed
    this.#latestMilestoneIndex = milestoneIndex
    this.#emit(Object.freeze({
      eventName: 'performance_phase',
      payload: projectPerformancePhasePayloadV1({
        correlation: this.#correlation,
        milestone,
        observerElapsedMilliseconds: elapsed,
      }),
    }))
    if (milestone === 'published') this.complete()
    return true
  }

  snapshot(): PerformanceSummaryProjectionInput {
    const starvation = this.#starvation.snapshot()
    return Object.freeze({
      correlation: this.#correlation,
      queue: Object.freeze({
        writer: Object.freeze({
          wait: this.#queue.writer.wait.snapshot(),
          run: this.#queue.writer.run.snapshot(),
        }),
        namespace: Object.freeze({
          wait: this.#queue.namespace.wait.snapshot(),
          run: this.#queue.namespace.run.snapshot(),
        }),
      }),
      namespaceByKind: starvation.namespaceByKind,
      filePipeline: starvation.filePipeline,
      peaks: Object.freeze({ ...this.#peaks }),
      authorityCache: Object.freeze({ ...this.#authorityCache }),
      bytes: Object.freeze({ ...this.#bytes }),
      checkpoints: Object.freeze({
        automaticTriggers: this.#checkpoints.automaticTriggers,
        forcedPauseTriggers: this.#checkpoints.forcedPauseTriggers,
        advanced: this.#checkpoints.advanced,
        declined: this.#checkpoints.declined,
        constantCost: this.#checkpoints.constantCost,
        prefixCopyCost: this.#checkpoints.prefixCopyCost,
        spacePreflightCost: this.#checkpoints.spacePreflightCost,
        estimatedCopyBytes: this.#checkpoints.estimatedCopyBytes,
        elapsed: this.#checkpoints.elapsed.snapshot(),
      }),
      finalTransactions: Object.freeze({
        count: this.#finalTransactions.count,
        elapsed: this.#finalTransactions.elapsed.snapshot(),
      }),
      ledger: Object.freeze({
        entries: this.#ledger.entries,
        pages: this.#ledger.pages,
        seals: this.#ledger.seals,
        recoveryScanFallbacks: this.#ledger.recoveryScanFallbacks,
        sealElapsed: this.#ledger.sealElapsed.snapshot(),
      }),
      revisionOpens: starvation.revisionOpens,
      claimBatches: starvation.claimBatches,
      outputResources: starvation.outputResources,
      milestones: Object.freeze({ ...this.#milestones }),
      counterOverflowed: this.#counterOverflowed,
    })
  }

  complete(): PerformanceSummaryPayloadV1 {
    if (this.#completed !== undefined) return this.#completed
    this.#starvation.complete(this.#readNow())
    const completed = projectPerformanceSummaryPayloadV1(this.snapshot())
    this.#completed = completed
    this.#emit(Object.freeze({
      eventName: 'performance_summary',
      payload: completed,
    }))
    return completed
  }

  #increment(value: bigint): bigint {
    return this.#add(value, 1n)
  }

  #add(value: bigint, delta: bigint): bigint {
    if (value > PERFORMANCE_UINT64_MAX - delta) {
      this.#counterOverflowed = true
      return PERFORMANCE_UINT64_MAX
    }
    return value + delta
  }

  #readNow(): number {
    const observed = requireClockMilliseconds(
      this.#clock.nowMilliseconds(),
      'performance observer clock',
    )
    this.#lastNowMilliseconds = Math.max(this.#lastNowMilliseconds, observed)
    return this.#lastNowMilliseconds
  }

  #emit(event: PerformanceTraceEvent): void {
    const observer = this.#trace?.current
    if (observer === undefined) return
    try {
      observer(event)
    } catch {
      // Diagnostics are observational and cannot acquire transfer/output authority.
    }
  }
}

export function createBoundedPerformanceSummary(
  options: BoundedPerformanceSummaryOptions,
): BoundedPerformanceSummary {
  return new BoundedPerformanceSummary(options)
}

export function createPerformanceSummaryObservations(
  options: BoundedPerformanceSummaryOptions,
): PerformanceSummaryObservations {
  return Object.freeze({
    summary: createBoundedPerformanceSummary(options),
    clock: options.clock,
  })
}

export function performanceNowMilliseconds(
  observations: PerformanceSummaryObservations | undefined,
): number | undefined {
  if (observations === undefined) return undefined
  try {
    return requireClockMilliseconds(
      observations.clock.nowMilliseconds(),
      'performance observation clock',
    )
  } catch {
    return undefined
  }
}

export function performanceElapsedMilliseconds(
  startedAtMilliseconds: number | undefined,
  completedAtMilliseconds: number | undefined,
): number | undefined {
  if (startedAtMilliseconds === undefined || completedAtMilliseconds === undefined) return undefined
  return Math.max(0, completedAtMilliseconds - startedAtMilliseconds)
}

export function observePerformance(
  observations: PerformanceSummaryObservations | undefined,
  observe: (summary: PerformanceSummarySink) => void,
): void {
  if (observations === undefined) return
  try {
    observe(observations.summary)
  } catch {
    // Performance telemetry cannot acquire transfer, output, or repository authority.
  }
}

function snapshotCorrelation(
  input: PerformanceCorrelationProjectionInput,
): PerformanceCorrelationProjectionInput {
  return Object.freeze({
    receiveOperationId: input.receiveOperationId,
    transferJobId: input.transferJobId,
    outputSessionId: input.outputSessionId,
    ...(input.protocolSessionId === undefined
      ? {}
      : { protocolSessionId: input.protocolSessionId }),
    ...(input.protocolGeneration === undefined
      ? {}
      : { protocolGeneration: input.protocolGeneration }),
  })
}

function checkpointCounter(
  value:
    | PerformanceCheckpointTrigger
    | PerformanceCheckpointDecision
    | PerformanceCheckpointCost,
):
  | 'automaticTriggers'
  | 'forcedPauseTriggers'
  | 'advanced'
  | 'declined'
  | 'constantCost'
  | 'prefixCopyCost'
  | 'spacePreflightCost' {
  switch (value) {
    case 'automatic': return 'automaticTriggers'
    case 'forced_pause': return 'forcedPauseTriggers'
    case 'advanced': return 'advanced'
    case 'declined': return 'declined'
    case 'constant': return 'constantCost'
    case 'prefix_copy': return 'prefixCopyCost'
    case 'space_preflight': return 'spacePreflightCost'
  }
}

function requireClockMilliseconds(value: number, field: string): number {
  return requirePerformanceMilliseconds(value, field)
}

function requireUint32(value: number, field: string): number {
  if (!Number.isInteger(value) || value < 0 || value > UINT32_MAX) {
    throw new RangeError(`${field} must be a uint32`)
  }
  return value
}

function requireNonNegativeBigint(value: bigint, field: string): bigint {
  if (typeof value !== 'bigint' || value < 0n) {
    throw new RangeError(`${field} must be a non-negative bigint`)
  }
  return value
}

function requireAbsentElapsed(value: number | undefined, field: string): void {
  if (value !== undefined) {
    throw new TypeError(`${field} does not accept elapsed milliseconds`)
  }
}

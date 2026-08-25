import {
  PERFORMANCE_CLAIM_INSPECTOR_REASONS_V1,
  PERFORMANCE_CLAIM_PHASES_V1,
  PERFORMANCE_FILE_PIPELINE_STAGES_V1,
  PERFORMANCE_NAMESPACE_KINDS_V1,
  type PerformanceClaimPhaseV1,
  type PerformanceClaimInspectorReasonV1,
  type PerformanceFilePipelineStageV1,
  type PerformanceNamespaceKindV1,
  type PerformanceSummaryProjectionInput,
} from '../../diagnostics/trace/transfer-payload'
import {
  BoundedMillisecondsHistogram,
  requirePerformanceMilliseconds,
  saturatingPerformanceAdd,
} from './performance-histogram'
import type { PerformanceClaimPhaseSamples } from './claim-batch-performance'
import type { PerformanceClaimInspectorSample } from './claim-inspector-performance'

const UINT32_MAX = 0xffff_ffff

type StarvationSnapshot = Pick<
  PerformanceSummaryProjectionInput,
  'namespaceByKind' | 'filePipeline' | 'revisionOpens' | 'claimBatches' | 'outputResources'
>

type Histogram = BoundedMillisecondsHistogram

interface PipelineStageState {
  entries: bigint
  occupancyMilliseconds: bigint
  active: number
  peakActive: number
}

interface ClaimPhaseState {
  readonly queue: Histogram
  readonly run: Histogram
  batchCount: bigint
  memberCount: bigint
  activeMilliseconds: bigint
  overlapMilliseconds: bigint
  maximumActive: number
  activeAtCompletion: number
}

interface ClaimInspectorState {
  drains: bigint
  wallMilliseconds: bigint
  maximumWidth: number
  atCapacityMilliseconds: bigint
  activeMilliseconds: bigint
  queuedMemberMilliseconds: bigint
  pendingMemberMilliseconds: bigint
  residentContextMilliseconds: bigint
  unfinishedInspectionContextMilliseconds: bigint
  orderedSettlementContextMilliseconds: bigint
  maximumActive: number
  maximumQueuedMembers: number
  maximumPendingMembers: number
  maximumResidentContexts: number
  maximumUnfinishedInspectionContexts: number
  maximumOrderedSettlementContexts: number
  activeAtCompletion: number
  queuedMembersAtCompletion: number
  pendingMembersAtCompletion: number
  residentContextsAtCompletion: number
  unfinishedInspectionContextsAtCompletion: number
  orderedSettlementContextsAtCompletion: number
  underCapacity: Record<PerformanceClaimInspectorReasonV1, {
    wallMilliseconds: bigint
    idleSlotMilliseconds: bigint
  }>
}

export class BoundedPerformanceStarvationSummary {
  readonly #onOverflow: () => void
  readonly #namespaceByKind: Record<PerformanceNamespaceKindV1, Readonly<{
    wait: Histogram
    run: Histogram
  }>>
  readonly #pipeline: Record<PerformanceFilePipelineStageV1, PipelineStageState>
  readonly #revision: {
    wait: Histogram
    run: Histogram
    attempts: bigint
    count: bigint
    overlapMilliseconds: bigint
    active: number
    maximumActive: number
  }
  readonly #claimBatches: {
    oldestWait: Histogram
    newestWait: Histogram
    run: Histogram
    count: bigint
    members: bigint
    maximumSize: number
    phases: Record<PerformanceClaimPhaseV1, ClaimPhaseState>
    inspector: ClaimInspectorState
  }
  readonly #outputResources: {
    activeFiles: { wait: Histogram; peak: number }
    writeBytes: { wait: Histogram; peak: bigint }
    bufferedBytes: { wait: Histogram; peak: bigint }
  }
  #pipelineLastAtMilliseconds: number
  #pipelineWorkerStarts = 0n
  #pipelineWorkerStops = 0n
  #pipelineWorkerMilliseconds = 0n
  #revisionLastAtMilliseconds: number

  constructor(startedAtMilliseconds: number, onOverflow: () => void) {
    this.#pipelineLastAtMilliseconds = startedAtMilliseconds
    this.#revisionLastAtMilliseconds = startedAtMilliseconds
    this.#onOverflow = onOverflow
    const histogram = () => new BoundedMillisecondsHistogram(onOverflow)
    this.#namespaceByKind = Object.fromEntries(PERFORMANCE_NAMESPACE_KINDS_V1.map(kind => [
      kind,
      Object.freeze({ wait: histogram(), run: histogram() }),
    ])) as Record<PerformanceNamespaceKindV1, Readonly<{ wait: Histogram; run: Histogram }>>
    this.#pipeline = Object.fromEntries(PERFORMANCE_FILE_PIPELINE_STAGES_V1.map(stage => [
      stage,
      { entries: 0n, occupancyMilliseconds: 0n, active: 0, peakActive: 0 },
    ])) as Record<PerformanceFilePipelineStageV1, PipelineStageState>
    this.#revision = {
      wait: histogram(),
      run: histogram(),
      attempts: 0n,
      count: 0n,
      overlapMilliseconds: 0n,
      active: 0,
      maximumActive: 0,
    }
    this.#claimBatches = {
      oldestWait: histogram(),
      newestWait: histogram(),
      run: histogram(),
      count: 0n,
      members: 0n,
      maximumSize: 0,
      phases: Object.fromEntries(PERFORMANCE_CLAIM_PHASES_V1.map(phase => [phase, {
        queue: histogram(),
        run: histogram(),
        batchCount: 0n,
        memberCount: 0n,
        activeMilliseconds: 0n,
        overlapMilliseconds: 0n,
        maximumActive: 0,
        activeAtCompletion: 0,
      }])) as Record<PerformanceClaimPhaseV1, ClaimPhaseState>,
      inspector: {
        drains: 0n,
        wallMilliseconds: 0n,
        maximumWidth: 0,
        atCapacityMilliseconds: 0n,
        activeMilliseconds: 0n,
        queuedMemberMilliseconds: 0n,
        pendingMemberMilliseconds: 0n,
        residentContextMilliseconds: 0n,
        unfinishedInspectionContextMilliseconds: 0n,
        orderedSettlementContextMilliseconds: 0n,
        maximumActive: 0,
        maximumQueuedMembers: 0,
        maximumPendingMembers: 0,
        maximumResidentContexts: 0,
        maximumUnfinishedInspectionContexts: 0,
        maximumOrderedSettlementContexts: 0,
        activeAtCompletion: 0,
        queuedMembersAtCompletion: 0,
        pendingMembersAtCompletion: 0,
        residentContextsAtCompletion: 0,
        unfinishedInspectionContextsAtCompletion: 0,
        orderedSettlementContextsAtCompletion: 0,
        underCapacity: Object.fromEntries(PERFORMANCE_CLAIM_INSPECTOR_REASONS_V1.map(reason => [
          reason,
          { wallMilliseconds: 0n, idleSlotMilliseconds: 0n },
        ])) as ClaimInspectorState['underCapacity'],
      },
    }
    this.#outputResources = {
      activeFiles: { wait: histogram(), peak: 0 },
      writeBytes: { wait: histogram(), peak: 0n },
      bufferedBytes: { wait: histogram(), peak: 0n },
    }
  }

  observeNamespace(
    kind: PerformanceNamespaceKindV1,
    waitMilliseconds: number,
    runMilliseconds: number,
  ): void {
    const summary = this.#namespaceByKind[kind]
    if (summary === undefined) throw new TypeError('performance namespace kind is invalid')
    summary.wait.observe(waitMilliseconds)
    summary.run.observe(runMilliseconds)
  }

  transitionPipeline(input: Readonly<{
    from?: PerformanceFilePipelineStageV1
    to?: PerformanceFilePipelineStageV1
    atMilliseconds: number
  }>): void {
    if (input.from === undefined && input.to === undefined) {
      throw new TypeError('performance pipeline transition is empty')
    }
    this.#advancePipeline(input.atMilliseconds)
    if (input.from === undefined) {
      this.#pipelineWorkerStarts = this.#increment(this.#pipelineWorkerStarts)
    } else {
      const previous = this.#pipeline[input.from]
      if (previous === undefined || previous.active === 0) {
        throw new TypeError('performance pipeline stage transition underflowed')
      }
      previous.active -= 1
    }
    if (input.to === undefined) {
      this.#pipelineWorkerStops = this.#increment(this.#pipelineWorkerStops)
      return
    }
    const next = this.#pipeline[input.to]
    if (next === undefined) throw new TypeError('performance pipeline stage is invalid')
    next.entries = this.#increment(next.entries)
    next.active = requireUint32(next.active + 1, 'active pipeline stage')
    next.peakActive = Math.max(next.peakActive, next.active)
  }

  revisionStarted(atMilliseconds: number): void {
    this.#advanceRevision(atMilliseconds)
    this.#revision.attempts = this.#increment(this.#revision.attempts)
    this.#revision.active = requireUint32(this.#revision.active + 1, 'active revision opens')
    this.#revision.maximumActive = Math.max(
      this.#revision.maximumActive,
      this.#revision.active,
    )
  }

  revisionFinished(input: Readonly<{
    atMilliseconds: number
    waitMilliseconds: number
    runMilliseconds: number
    succeeded: boolean
  }>): void {
    this.#advanceRevision(input.atMilliseconds)
    if (this.#revision.active === 0) {
      throw new TypeError('performance revision-open concurrency underflowed')
    }
    this.#revision.active -= 1
    if (!input.succeeded) return
    this.#revision.count = this.#increment(this.#revision.count)
    this.#revision.wait.observe(input.waitMilliseconds)
    this.#revision.run.observe(input.runMilliseconds)
  }

  observeClaimBatch(input: Readonly<{
    size: number
    oldestWaitMilliseconds: number
    newestWaitMilliseconds: number
    runMilliseconds: number
    phases: PerformanceClaimPhaseSamples
  }>): void {
    const size = requireUint32(input.size, 'claim batch size')
    if (size === 0) throw new RangeError('claim batch size must be positive')
    this.#claimBatches.count = this.#increment(this.#claimBatches.count)
    this.#claimBatches.members = this.#add(this.#claimBatches.members, BigInt(size))
    this.#claimBatches.maximumSize = Math.max(this.#claimBatches.maximumSize, size)
    this.#claimBatches.oldestWait.observe(input.oldestWaitMilliseconds)
    this.#claimBatches.newestWait.observe(input.newestWaitMilliseconds)
    this.#claimBatches.run.observe(input.runMilliseconds)
    for (const phase of PERFORMANCE_CLAIM_PHASES_V1) {
      const sample = input.phases[phase]
      const summary = this.#claimBatches.phases[phase]
      summary.batchCount = this.#increment(summary.batchCount)
      summary.memberCount = this.#add(summary.memberCount, BigInt(sample.memberCount))
      summary.queue.observe(sample.queueMilliseconds)
      summary.run.observe(sample.runMilliseconds)
      summary.activeMilliseconds = this.#add(
        summary.activeMilliseconds,
        sample.activeMilliseconds,
      )
      summary.overlapMilliseconds = this.#add(
        summary.overlapMilliseconds,
        sample.overlapMilliseconds,
      )
      summary.maximumActive = Math.max(summary.maximumActive, sample.maximumActive)
      summary.activeAtCompletion = requireUint32(
        summary.activeAtCompletion + sample.activeAtCompletion,
        `active ${phase} claim work`,
      )
    }
  }

  observeClaimInspector(input: PerformanceClaimInspectorSample): void {
    const state = this.#claimBatches.inspector
    const wallMilliseconds = BigInt(requirePerformanceMilliseconds(
      input.drainMilliseconds,
      'claim inspector drain',
    ))
    if (state.drains !== 0n && state.maximumWidth !== input.maximumWidth) {
      throw new TypeError('claim inspector width changed within one performance summary')
    }
    state.drains = this.#increment(state.drains)
    state.wallMilliseconds = this.#add(state.wallMilliseconds, wallMilliseconds)
    state.maximumWidth = requireUint32(input.maximumWidth, 'claim inspector maximum width')
    for (const field of [
      'atCapacityMilliseconds',
      'activeMilliseconds',
      'queuedMemberMilliseconds',
      'pendingMemberMilliseconds',
      'residentContextMilliseconds',
      'unfinishedInspectionContextMilliseconds',
      'orderedSettlementContextMilliseconds',
    ] as const) state[field] = this.#add(state[field], input[field])
    for (const field of [
      'maximumActive',
      'maximumQueuedMembers',
      'maximumPendingMembers',
      'maximumResidentContexts',
      'maximumUnfinishedInspectionContexts',
      'maximumOrderedSettlementContexts',
    ] as const) state[field] = Math.max(state[field], requireUint32(input[field], field))
    for (const field of [
      'activeAtCompletion',
      'queuedMembersAtCompletion',
      'pendingMembersAtCompletion',
      'residentContextsAtCompletion',
      'unfinishedInspectionContextsAtCompletion',
      'orderedSettlementContextsAtCompletion',
    ] as const) state[field] = requireUint32(state[field] + input[field], field)
    for (const reason of PERFORMANCE_CLAIM_INSPECTOR_REASONS_V1) {
      state.underCapacity[reason].wallMilliseconds = this.#add(
        state.underCapacity[reason].wallMilliseconds,
        input.underCapacity[reason].wallMilliseconds,
      )
      state.underCapacity[reason].idleSlotMilliseconds = this.#add(
        state.underCapacity[reason].idleSlotMilliseconds,
        input.underCapacity[reason].idleSlotMilliseconds,
      )
    }
  }

  observeOutputResource(input: Readonly<{
    resource: 'active_files' | 'write_bytes' | 'buffered_bytes'
    waitMilliseconds: number
    peak: number | bigint
  }>): void {
    switch (input.resource) {
      case 'active_files': {
        if (typeof input.peak !== 'number') throw new TypeError('active-file peak is invalid')
        const peak = requireUint32(input.peak, 'peak active output files')
        this.#outputResources.activeFiles.wait.observe(input.waitMilliseconds)
        this.#outputResources.activeFiles.peak = Math.max(
          this.#outputResources.activeFiles.peak,
          peak,
        )
        return
      }
      case 'write_bytes':
      case 'buffered_bytes': {
        if (typeof input.peak !== 'bigint' || input.peak < 0n) {
          throw new TypeError('output byte-resource peak is invalid')
        }
        const resource = input.resource === 'write_bytes'
          ? this.#outputResources.writeBytes
          : this.#outputResources.bufferedBytes
        resource.wait.observe(input.waitMilliseconds)
        resource.peak = resource.peak > input.peak ? resource.peak : input.peak
        return
      }
    }
  }

  complete(atMilliseconds: number): void {
    this.#advancePipeline(atMilliseconds)
    this.#advanceRevision(atMilliseconds)
  }

  snapshot(): StarvationSnapshot {
    return Object.freeze({
      namespaceByKind: Object.freeze(Object.fromEntries(
        PERFORMANCE_NAMESPACE_KINDS_V1.map(kind => [kind, Object.freeze({
          wait: this.#namespaceByKind[kind].wait.snapshot(),
          run: this.#namespaceByKind[kind].run.snapshot(),
        })]),
      )) as StarvationSnapshot['namespaceByKind'],
      filePipeline: Object.freeze({
        workerStarts: this.#pipelineWorkerStarts,
        workerStops: this.#pipelineWorkerStops,
        workerMilliseconds: this.#pipelineWorkerMilliseconds,
        stages: Object.freeze(Object.fromEntries(
          PERFORMANCE_FILE_PIPELINE_STAGES_V1.map(stage => [stage, Object.freeze({
            entries: this.#pipeline[stage].entries,
            occupancyMilliseconds: this.#pipeline[stage].occupancyMilliseconds,
            peakActive: this.#pipeline[stage].peakActive,
            activeAtCompletion: this.#pipeline[stage].active,
          })]),
        )) as StarvationSnapshot['filePipeline']['stages'],
      }),
      revisionOpens: Object.freeze({
        attempts: this.#revision.attempts,
        count: this.#revision.count,
        wait: this.#revision.wait.snapshot(),
        run: this.#revision.run.snapshot(),
        overlapMilliseconds: this.#revision.overlapMilliseconds,
        maximumActive: this.#revision.maximumActive,
        activeAtCompletion: this.#revision.active,
      }),
      claimBatches: Object.freeze({
        count: this.#claimBatches.count,
        members: this.#claimBatches.members,
        maximumSize: this.#claimBatches.maximumSize,
        oldestWait: this.#claimBatches.oldestWait.snapshot(),
        newestWait: this.#claimBatches.newestWait.snapshot(),
        run: this.#claimBatches.run.snapshot(),
        phases: Object.freeze(Object.fromEntries(PERFORMANCE_CLAIM_PHASES_V1.map(phase => [
          phase,
          Object.freeze({
            batchCount: this.#claimBatches.phases[phase].batchCount,
            memberCount: this.#claimBatches.phases[phase].memberCount,
            queue: this.#claimBatches.phases[phase].queue.snapshot(),
            run: this.#claimBatches.phases[phase].run.snapshot(),
            activeMilliseconds: this.#claimBatches.phases[phase].activeMilliseconds,
            overlapMilliseconds: this.#claimBatches.phases[phase].overlapMilliseconds,
            maximumActive: this.#claimBatches.phases[phase].maximumActive,
            activeAtCompletion: this.#claimBatches.phases[phase].activeAtCompletion,
          }),
        ]))) as StarvationSnapshot['claimBatches']['phases'],
        inspector: snapshotClaimInspector(this.#claimBatches.inspector),
      }),
      outputResources: Object.freeze({
        activeFiles: Object.freeze({
          wait: this.#outputResources.activeFiles.wait.snapshot(),
          peak: this.#outputResources.activeFiles.peak,
        }),
        writeBytes: Object.freeze({
          wait: this.#outputResources.writeBytes.wait.snapshot(),
          peak: this.#outputResources.writeBytes.peak,
        }),
        bufferedBytes: Object.freeze({
          wait: this.#outputResources.bufferedBytes.wait.snapshot(),
          peak: this.#outputResources.bufferedBytes.peak,
        }),
      }),
    })
  }

  #advancePipeline(atMilliseconds: number): void {
    const at = requirePerformanceMilliseconds(atMilliseconds, 'pipeline observation clock')
    const next = Math.max(this.#pipelineLastAtMilliseconds, at)
    const elapsed = BigInt(next - this.#pipelineLastAtMilliseconds)
    let activeWorkers = 0
    for (const stage of PERFORMANCE_FILE_PIPELINE_STAGES_V1) {
      const state = this.#pipeline[stage]
      activeWorkers += state.active
      state.occupancyMilliseconds = this.#add(
        state.occupancyMilliseconds,
        BigInt(state.active) * elapsed,
      )
    }
    this.#pipelineWorkerMilliseconds = this.#add(
      this.#pipelineWorkerMilliseconds,
      BigInt(activeWorkers) * elapsed,
    )
    this.#pipelineLastAtMilliseconds = next
  }

  #advanceRevision(atMilliseconds: number): void {
    const at = requirePerformanceMilliseconds(atMilliseconds, 'revision observation clock')
    const next = Math.max(this.#revisionLastAtMilliseconds, at)
    if (this.#revision.active >= 2) {
      this.#revision.overlapMilliseconds = this.#add(
        this.#revision.overlapMilliseconds,
        BigInt(next - this.#revisionLastAtMilliseconds),
      )
    }
    this.#revisionLastAtMilliseconds = next
  }

  #increment(value: bigint): bigint {
    return this.#add(value, 1n)
  }

  #add(value: bigint, delta: bigint): bigint {
    return saturatingPerformanceAdd(value, delta, this.#onOverflow)
  }
}

function snapshotClaimInspector(
  state: ClaimInspectorState,
): StarvationSnapshot['claimBatches']['inspector'] {
  return Object.freeze({
    drains: state.drains,
    wallMilliseconds: state.wallMilliseconds,
    maximumWidth: state.maximumWidth,
    atCapacityMilliseconds: state.atCapacityMilliseconds,
    activeMilliseconds: state.activeMilliseconds,
    queuedMemberMilliseconds: state.queuedMemberMilliseconds,
    pendingMemberMilliseconds: state.pendingMemberMilliseconds,
    contextMilliseconds: Object.freeze({
      resident: state.residentContextMilliseconds,
      unfinishedInspection: state.unfinishedInspectionContextMilliseconds,
      orderedSettlement: state.orderedSettlementContextMilliseconds,
    }),
    maximum: Object.freeze({
      active: state.maximumActive,
      queuedMembers: state.maximumQueuedMembers,
      pendingMembers: state.maximumPendingMembers,
      residentContexts: state.maximumResidentContexts,
      unfinishedInspectionContexts: state.maximumUnfinishedInspectionContexts,
      orderedSettlementContexts: state.maximumOrderedSettlementContexts,
    }),
    atCompletion: Object.freeze({
      active: state.activeAtCompletion,
      queuedMembers: state.queuedMembersAtCompletion,
      pendingMembers: state.pendingMembersAtCompletion,
      residentContexts: state.residentContextsAtCompletion,
      unfinishedInspectionContexts: state.unfinishedInspectionContextsAtCompletion,
      orderedSettlementContexts: state.orderedSettlementContextsAtCompletion,
    }),
    underCapacity: Object.freeze(Object.fromEntries(
      PERFORMANCE_CLAIM_INSPECTOR_REASONS_V1.map(reason => [
        reason,
        Object.freeze({ ...state.underCapacity[reason] }),
      ]),
    ) as StarvationSnapshot['claimBatches']['inspector']['underCapacity']),
  })
}

function requireUint32(value: number, field: string): number {
  if (!Number.isInteger(value) || value < 0 || value > UINT32_MAX) {
    throw new RangeError(`${field} must be a uint32`)
  }
  return value
}

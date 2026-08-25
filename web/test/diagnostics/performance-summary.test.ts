import { describe, expect, it } from 'vitest'

import {
  snapshotTraceEventObservationV1,
  traceEventObservationBytesV1,
  traceEventObservationNameV1,
} from '../../src/diagnostics/export/trace-event-v1'
import {
  type TraceEventObservationV1,
} from '../../src/diagnostics/trace/model'
import { BoundedTraceRecorder } from '../../src/diagnostics/trace/recorder'
import {
  PERFORMANCE_CLAIM_INSPECTOR_REASONS_V1,
  PERFORMANCE_CLAIM_PHASES_V1,
  PERFORMANCE_FILE_PIPELINE_STAGES_V1,
  PERFORMANCE_NAMESPACE_KINDS_V1,
} from '../../src/diagnostics/trace/transfer-payload'
import {
  createBoundedPerformanceSummary,
  createPerformanceSummaryObservations,
} from '../../src/output/diagnostics/performance-summary'
import { createPerformanceClaimBatchTimeline } from '../../src/output/diagnostics/claim-batch-performance'
import { createPerformanceClaimInspectorObservation } from '../../src/output/diagnostics/claim-inspector-performance'

const RECEIVE_OPERATION_ID = 'AQAAAAAAAAAAAAAAAAAAAA'
const TRANSFER_JOB_ID = 'AgAAAAAAAAAAAAAAAAAAAA'
const OUTPUT_SESSION_ID = 'AwAAAAAAAAAAAAAAAAAAAA'
const PROTOCOL_SESSION_ID = 'BAAAAAAAAAAAAAAAAAAAAA'
const UINT64_MAX = 0xffff_ffff_ffff_ffffn

const CORRELATION = Object.freeze({
  receiveOperationId: RECEIVE_OPERATION_ID,
  transferJobId: TRANSFER_JOB_ID,
  outputSessionId: OUTPUT_SESSION_ID,
  protocolSessionId: PROTOCOL_SESSION_ID,
  protocolGeneration: 3,
})

describe('bounded performance summary', () => {
  it('projects exact counters, fixed histograms, peaks, and monotonic phase timestamps', () => {
    let now = 1_000
    const events: TraceEventObservationV1[] = []
    const summary = createBoundedPerformanceSummary({
      correlation: CORRELATION,
      clock: { nowMilliseconds: () => now },
      trace: { current: (event) => events.push(event) },
    })

    now = 1_010
    expect(summary.markMilestone('authority_acquired')).toBe(true)
    expect(summary.markMilestone('authority_acquired')).toBe(false)
    summary.observeQueueRun('writer', 0, 1)
    summary.observeQueueRun('writer', 4, 4_097)
    summary.observeQueueRun('namespace', 16, 64, 'create_file')
    summary.observeConcurrency({
      activeWriters: 2,
      queuedWriters: 5,
      activeNamespace: 1,
      queuedNamespace: 3,
    })
    summary.observeConcurrency({
      activeWriters: 8,
      queuedWriters: 1,
      activeNamespace: 2,
      queuedNamespace: 2,
    })
    summary.observeAuthorityLookup({ kind: 'directory', result: 'miss', walkDepth: 7 })
    summary.observeAuthorityLookup({ kind: 'directory', result: 'hit' })
    summary.observeAuthorityLookup({ kind: 'file', result: 'miss', walkDepth: 1 })
    summary.observeAuthorityLookup({ kind: 'file', result: 'hit' })
    summary.observeAuthorityInvalidation()
    summary.observeAuthorityEviction()
    summary.observeByteTransition('pending', 10n)
    summary.observeByteTransition('durable', 4n)
    summary.observeByteTransition('final', 10n)
    summary.observeCheckpoint({
      trigger: 'automatic',
      decision: 'declined',
      cost: 'prefix_copy',
      elapsedMilliseconds: 17,
      estimatedCopyBytes: 10n,
    })
    summary.observeCheckpoint({
      trigger: 'forced_pause',
      decision: 'advanced',
      cost: 'space_preflight',
      elapsedMilliseconds: 257,
    })
    summary.observeFinalTransaction(65)
    summary.observeLedger({ transition: 'entry' })
    summary.observeLedger({ transition: 'page' })
    summary.observeLedger({ transition: 'seal', elapsedMilliseconds: 5 })
    summary.observeLedger({ transition: 'recovery_scan_fallback' })
    summary.observeFilePipelineTransition({ to: 'idle_no_ready_file', atMilliseconds: 1_010 })
    summary.observeFilePipelineTransition({
      from: 'idle_no_ready_file',
      to: 'revision_open',
      atMilliseconds: 1_012,
    })
    summary.observeFilePipelineTransition({
      from: 'revision_open',
      to: 'writer_lifecycle',
      atMilliseconds: 1_016,
    })
    summary.observeFilePipelineTransition({
      from: 'writer_lifecycle',
      to: 'idle_no_ready_file',
      atMilliseconds: 1_020,
    })
    summary.observeFilePipelineTransition({
      from: 'idle_no_ready_file',
      atMilliseconds: 1_021,
    })
    summary.observeRevisionOpenStarted(1_010)
    summary.observeRevisionOpenStarted(1_011)
    summary.observeRevisionOpenFinished({
      atMilliseconds: 1_014,
      waitMilliseconds: 2,
      runMilliseconds: 4,
      succeeded: true,
    })
    summary.observeRevisionOpenFinished({
      atMilliseconds: 1_016,
      waitMilliseconds: 3,
      runMilliseconds: 5,
      succeeded: true,
    })
    summary.observeClaimBatch({
      size: 4,
      oldestWaitMilliseconds: 6,
      newestWaitMilliseconds: 1,
      runMilliseconds: 8,
      phases: {
        classification: claimPhaseSample(4, 1, 1, 1n, 0n, 1),
        inspection_union: claimPhaseSample(4, 0, 3, 5n, 1n, 2),
        reclassification: claimPhaseSample(1, 1, 0, 0n, 0n, 0),
        installation: claimPhaseSample(3, 0, 2, 2n, 0n, 1),
      },
    })
    summary.observeOutputResource({ resource: 'active_files', waitMilliseconds: 2, peak: 2 })
    summary.observeOutputResource({ resource: 'write_bytes', waitMilliseconds: 3, peak: 10n })
    summary.observeOutputResource({ resource: 'buffered_bytes', waitMilliseconds: 3, peak: 10n })

    now = 1_020
    summary.markMilestone('first_content_request')
    now = 1_025
    summary.markMilestone('first_write')
    now = 1_040
    summary.markMilestone('last_byte')
    now = 1_050
    summary.markMilestone('last_final')
    now = 1_060
    summary.markMilestone('settlement_started')
    now = 1_075
    summary.markMilestone('published')

    const payload = summary.complete()
    expect(payload).toEqual({
      correlation: {
        receive_operation_id: RECEIVE_OPERATION_ID,
        transfer_job_id: TRANSFER_JOB_ID,
        output_session_id: OUTPUT_SESSION_ID,
        protocol_session_id: PROTOCOL_SESSION_ID,
        protocol_generation: 3,
      },
      queue: {
        writer: {
          wait_ms: histogram(['1', '1', '0', '0', '0', '0', '0', '0'], '2', '4', '4'),
          run_ms: histogram(['1', '0', '0', '0', '0', '0', '0', '1'], '2', '4098', '4097'),
        },
        namespace: {
          wait_ms: histogram(['0', '0', '1', '0', '0', '0', '0', '0'], '1', '16', '16'),
          run_ms: histogram(['0', '0', '0', '1', '0', '0', '0', '0'], '1', '64', '64'),
        },
      },
      namespace_by_kind: namespaceKindSummaries({
        create_file: {
          wait_ms: histogram(['0', '0', '1', '0', '0', '0', '0', '0'], '1', '16', '16'),
          run_ms: histogram(['0', '0', '0', '1', '0', '0', '0', '0'], '1', '64', '64'),
        },
      }),
      file_pipeline: {
        worker_starts: '1',
        worker_stops: '1',
        worker_ms: '11',
        stages: pipelineStages({
          revision_open: pipelineStage('1', '4', 1),
          writer_lifecycle: pipelineStage('1', '4', 1),
          idle_no_ready_file: pipelineStage('2', '3', 1),
        }),
      },
      peaks: {
        active_writers: 8,
        queued_writers: 5,
        active_namespace: 2,
        queued_namespace: 3,
      },
      authority_cache: {
        directory_hits: '1',
        directory_misses: '1',
        file_hits: '1',
        file_misses: '1',
        invalidations: '1',
        evictions: '1',
        walked_segments: '8',
        maximum_walk_depth: 7,
      },
      bytes: { pending: '10', durable: '4', final: '10' },
      checkpoints: {
        automatic_triggers: '1',
        forced_pause_triggers: '1',
        advanced: '1',
        declined: '1',
        constant_cost: '0',
        prefix_copy_cost: '1',
        space_preflight_cost: '1',
        estimated_copy_bytes: '10',
        elapsed_ms: histogram(
          ['0', '0', '0', '1', '0', '1', '0', '0'],
          '2',
          '274',
          '257',
        ),
      },
      final_transactions: {
        count: '1',
        elapsed_ms: histogram(['0', '0', '0', '0', '1', '0', '0', '0'], '1', '65', '65'),
      },
      ledger: {
        entries: '1',
        pages: '1',
        seals: '1',
        recovery_scan_fallbacks: '1',
        seal_elapsed_ms: histogram(['0', '0', '1', '0', '0', '0', '0', '0'], '1', '5', '5'),
      },
      revision_opens: {
        attempts: '2',
        count: '2',
        wait_ms: histogram(['0', '2', '0', '0', '0', '0', '0', '0'], '2', '5', '3'),
        run_ms: histogram(['0', '1', '1', '0', '0', '0', '0', '0'], '2', '9', '5'),
        overlap_ms: '3',
        maximum_active: 2,
        active_at_completion: 0,
      },
      claim_batches: {
        count: '1',
        members: '4',
        maximum_size: 4,
        oldest_wait_ms: histogram(['0', '0', '1', '0', '0', '0', '0', '0'], '1', '6', '6'),
        newest_wait_ms: histogram(['1', '0', '0', '0', '0', '0', '0', '0'], '1', '1', '1'),
        run_ms: histogram(['0', '0', '1', '0', '0', '0', '0', '0'], '1', '8', '8'),
        phases: claimPhaseSummaries({
          classification: claimPhaseSummary(4, 1, 1, '1', '0', 1),
          inspection_union: claimPhaseSummary(4, 0, 3, '5', '1', 2),
          reclassification: claimPhaseSummary(1, 1, 0, '0', '0', 0),
          installation: claimPhaseSummary(3, 0, 2, '2', '0', 1),
        }),
        inspector: emptyClaimInspectorPayload(),
      },
      output_resources: {
        active_files: {
          wait_ms: histogram(['0', '1', '0', '0', '0', '0', '0', '0'], '1', '2', '2'),
          peak: 2,
        },
        write_bytes: {
          wait_ms: histogram(['0', '1', '0', '0', '0', '0', '0', '0'], '1', '3', '3'),
          peak: '10',
        },
        buffered_bytes: {
          wait_ms: histogram(['0', '1', '0', '0', '0', '0', '0', '0'], '1', '3', '3'),
          peak: '10',
        },
      },
      milestones: {
        authority_acquired: '10',
        first_content_request: '20',
        first_write: '25',
        last_byte: '40',
        last_final: '50',
        settlement_started: '60',
        published: '75',
      },
      counter_overflowed: false,
    })
    expect(events).toHaveLength(8)
    expect(events.map((event) => event.eventName)).toEqual([
      'performance_phase',
      'performance_phase',
      'performance_phase',
      'performance_phase',
      'performance_phase',
      'performance_phase',
      'performance_phase',
      'performance_summary',
    ])
    expect(events.at(-1)?.payload).toBe(payload)
    expect(() => summary.observeRevisionOpenStarted(1_076)).not.toThrow()
    expect(summary.complete()).toBe(payload)
  })
})

describe('bounded performance summary limits', () => {
  it('conserves a phased claim drain and its inspection overlap integral', () => {
    let now = 100
    const observations = createPerformanceSummaryObservations({
      correlation: CORRELATION,
      clock: { nowMilliseconds: () => now },
    })
    const timeline = createPerformanceClaimBatchTimeline(observations, now)
    if (timeline === undefined) throw new Error('claim timeline was not created')

    now = 101
    const classification = timeline.beginPhase('classification', 4)
    now = 102
    const classificationActive = classification?.beginActive()
    now = 104
    classificationActive?.finish()
    now = 105
    classification?.finish()

    now = 106
    const inspection = timeline.beginPhase('inspection_union', 4)
    now = 107
    const firstInspection = inspection?.beginActive()
    now = 108
    const secondInspection = inspection?.beginActive()
    now = 111
    firstInspection?.finish()
    now = 113
    secondInspection?.finish()
    now = 114
    inspection?.finish()

    now = 115
    const reclassification = timeline.beginPhase('reclassification', 0)
    now = 116
    reclassification?.finish()
    now = 117
    const installation = timeline.beginPhase('installation', 4)
    now = 118
    const installationActive = installation?.beginActive()
    now = 120
    installationActive?.finish()
    now = 121

    const result = timeline.complete()
    if (result === undefined) throw new Error('claim timeline did not close')
    expect(result.completedAtMilliseconds).toBe(121)
    expect(result.phases.inspection_union).toEqual({
      memberCount: 4,
      queueMilliseconds: 1,
      runMilliseconds: 8,
      activeMilliseconds: 9n,
      overlapMilliseconds: 3n,
      maximumActive: 2,
      activeAtCompletion: 0,
    })
    expect(PERFORMANCE_CLAIM_PHASES_V1.reduce(
      (total, phase) => total + result.phases[phase].queueMilliseconds +
        result.phases[phase].runMilliseconds,
      0,
    )).toBe(21)
  })

  it('conserves global inspector capacity and assigns one reason to every idle slot', () => {
    let now = 100
    const observations = createPerformanceSummaryObservations({
      correlation: CORRELATION,
      clock: { nowMilliseconds: () => now },
    })
    const inspector = createPerformanceClaimInspectorObservation(observations, 3, now)
    if (inspector === undefined) throw new Error('claim inspector observation was not created')

    now = 102
    inspector.observePoolState({ active: 3, queuedMembers: 2 })
    now = 104
    inspector.observePoolState({ active: 1, queuedMembers: 2 })
    inspector.setPendingMembers(3)
    const first = inspector.beginContext()
    first.inspectionStarted()
    now = 107
    first.inspectionFinished()
    inspector.setPendingMembers(0)
    now = 110
    first.settlementStarted()
    now = 112
    inspector.observePoolState({ active: 0, queuedMembers: 0 })
    first.finish()

    const sample = inspector.complete()
    if (sample === undefined) throw new Error('claim inspector observation did not close')
    expect(sample).toEqual({
      drainMilliseconds: 12,
      maximumWidth: 3,
      atCapacityMilliseconds: 2n,
      activeMilliseconds: 14n,
      queuedMemberMilliseconds: 20n,
      pendingMemberMilliseconds: 9n,
      residentContextMilliseconds: 8n,
      unfinishedInspectionContextMilliseconds: 3n,
      orderedSettlementContextMilliseconds: 3n,
      maximumActive: 3,
      maximumQueuedMembers: 2,
      maximumPendingMembers: 3,
      maximumResidentContexts: 1,
      maximumUnfinishedInspectionContexts: 1,
      maximumOrderedSettlementContexts: 1,
      activeAtCompletion: 0,
      queuedMembersAtCompletion: 0,
      pendingMembersAtCompletion: 0,
      residentContextsAtCompletion: 0,
      unfinishedInspectionContextsAtCompletion: 0,
      orderedSettlementContextsAtCompletion: 0,
      underCapacity: {
        no_pending_arrival: { wallMilliseconds: 4n, idleSlotMilliseconds: 10n },
        batch_serialization: { wallMilliseconds: 3n, idleSlotMilliseconds: 6n },
        ordered_settlement: { wallMilliseconds: 3n, idleSlotMilliseconds: 6n },
      },
    })
    observations.summary.observeClaimInspector(sample)
    const payload = observations.summary.complete().claim_batches.inspector
    expect(BigInt(payload.at_capacity_ms) + PERFORMANCE_CLAIM_INSPECTOR_REASONS_V1.reduce(
      (total, reason) => total + BigInt(payload.under_capacity[reason].wall_ms),
      0n,
    )).toBe(BigInt(payload.wall_ms))
    expect(BigInt(payload.active_ms) + PERFORMANCE_CLAIM_INSPECTOR_REASONS_V1.reduce(
      (total, reason) => total + BigInt(payload.under_capacity[reason].idle_slot_ms),
      0n,
    )).toBe(BigInt(payload.maximum_width) * BigInt(payload.wall_ms))
    expect(Object.values(payload.at_completion).every(value => value === 0)).toBe(true)
  })

  it('saturates aggregate counters and exposes the loss without growing the payload', () => {
    const summary = createBoundedPerformanceSummary({
      correlation: CORRELATION,
      clock: { nowMilliseconds: () => 0 },
    })
    summary.observeByteTransition('pending', UINT64_MAX)
    summary.observeByteTransition('pending', 1n)

    const payload = summary.complete()
    expect(payload.bytes.pending).toBe(UINT64_MAX.toString())
    expect(payload.counter_overflowed).toBe(true)
    expect(JSON.stringify(payload).length).toBeLessThan(16_384)
  })

  it('keeps a 582-file success within normal trace capacity without overwrite', () => {
    let now = 0
    const recorder = new BoundedTraceRecorder<TraceEventObservationV1, never, never>({
      captureGeneration: 1n,
      clock: { nowMilliseconds: () => now },
      scheduler: {
        schedule: () => Object.freeze({ cancel: () => undefined }),
      },
      eventName: traceEventObservationNameV1,
      snapshotEvent: snapshotTraceEventObservationV1,
      eventBytes: traceEventObservationBytesV1,
      snapshotIncident: (incident) => incident,
      incidentMarkerBytes: () => 1,
      incidentScope: (incident) => incident,
      sameScope: () => true,
    })
    const summary = createBoundedPerformanceSummary({
      correlation: CORRELATION,
      clock: { nowMilliseconds: () => now },
      trace: { current: (event) => recorder.record(event) },
    })

    summary.markMilestone('authority_acquired')
    now += 1
    summary.markMilestone('first_content_request')
    summary.markMilestone('first_write')
    for (let file = 0; file < 582; file += 1) {
      now += 1
      summary.observeQueueRun('writer', file % 17, file % 71)
      summary.observeConcurrency({
        activeWriters: Math.min(8, file + 1),
        queuedWriters: Math.max(0, 582 - file - 8),
        activeNamespace: 1,
        queuedNamespace: 1,
      })
      summary.observeAuthorityLookup({ kind: 'file', result: 'hit' })
      summary.observeByteTransition('pending', 1n)
      summary.observeByteTransition('final', 1n)
      summary.observeFinalTransaction(file % 37)
      summary.observeLedger({ transition: 'entry' })
      summary.observeRevisionOpenStarted(now)
      summary.observeRevisionOpenFinished({
        atMilliseconds: now + (file % 251),
        waitMilliseconds: file % 13,
        runMilliseconds: file % 251,
        succeeded: true,
      })
    }
    summary.markMilestone('last_byte')
    summary.markMilestone('last_final')
    summary.markMilestone('settlement_started')
    summary.observeLedger({ transition: 'page' })
    summary.observeLedger({ transition: 'seal', elapsedMilliseconds: 3 })
    summary.markMilestone('published')

    const capture = recorder.snapshot()
    expect(capture.events).toHaveLength(8)
    expect(capture.health).toEqual({
      droppedCount: 0n,
      overwrittenCount: 0n,
      sampledCount: 0n,
      coalescedCount: 0n,
    })
    expect(capture.retainedEventCount).toBe(8n)
    const terminal = capture.events.at(-1)
    expect(terminal?.value.kind).toBe('event')
    if (terminal?.value.kind !== 'event') throw new Error('summary trace event is missing')
    expect(terminal.value.event.eventName).toBe('performance_summary')
    if (terminal.value.event.eventName !== 'performance_summary') {
      throw new Error('terminal trace event is not a performance summary')
    }
    expect(terminal.value.event.payload.revision_opens.count).toBe('582')
    expect(terminal.value.event.payload.final_transactions.count).toBe('582')
    expect(terminal.value.event.payload.ledger.entries).toBe('582')
    expect(terminal.value.event.payload.peaks.active_writers).toBe(8)
    expect(capture.events.every((event) => event.encodedBytes <= 16_384)).toBe(true)
  })

  it('rejects invalid samples and semantic milestone regression', () => {
    let now = 0
    const summary = createBoundedPerformanceSummary({
      correlation: CORRELATION,
      clock: { nowMilliseconds: () => now },
    })
    expect(() => summary.observeQueueRun('writer', -1, 0)).toThrow(RangeError)
    expect(() => summary.observeConcurrency({
      activeWriters: -1,
      queuedWriters: 0,
      activeNamespace: 0,
      queuedNamespace: 0,
    })).toThrow(RangeError)
    summary.markMilestone('first_write')
    now += 1
    expect(() => summary.markMilestone('first_content_request')).toThrow(
      /milestone order regressed/,
    )
  })
})

function histogram(
  bucketCounts: readonly string[],
  sampleCount: string,
  totalMilliseconds: string,
  maximumMilliseconds: string,
) {
  return {
    upper_bounds_ms: [1, 4, 16, 64, 256, 1_024, 4_096],
    bucket_counts: bucketCounts,
    sample_count: sampleCount,
    total_ms: totalMilliseconds,
    maximum_ms: maximumMilliseconds,
  }
}

function namespaceKindSummaries(
  overrides: Readonly<Record<string, unknown>> = {},
): Readonly<Record<string, unknown>> {
  return Object.fromEntries(PERFORMANCE_NAMESPACE_KINDS_V1.map(kind => [
    kind,
    overrides[kind] ?? { wait_ms: emptyHistogram(), run_ms: emptyHistogram() },
  ]))
}

function pipelineStages(
  overrides: Readonly<Record<string, unknown>> = {},
): Readonly<Record<string, unknown>> {
  return Object.fromEntries(PERFORMANCE_FILE_PIPELINE_STAGES_V1.map(stage => [
    stage,
    overrides[stage] ?? pipelineStage('0', '0', 0),
  ]))
}

function pipelineStage(entries: string, occupancyMilliseconds: string, peakActive: number) {
  return {
    entries,
    occupancy_ms: occupancyMilliseconds,
    peak_active: peakActive,
    active_at_completion: 0,
  }
}

function claimPhaseSample(
  memberCount: number,
  queueMilliseconds: number,
  runMilliseconds: number,
  activeMilliseconds: bigint,
  overlapMilliseconds: bigint,
  maximumActive: number,
) {
  return {
    memberCount,
    queueMilliseconds,
    runMilliseconds,
    activeMilliseconds,
    overlapMilliseconds,
    maximumActive,
    activeAtCompletion: 0,
  }
}

function claimPhaseSummaries(
  summaries: Readonly<Record<string, unknown>>,
): Readonly<Record<string, unknown>> {
  return Object.fromEntries(PERFORMANCE_CLAIM_PHASES_V1.map(phase => [phase, summaries[phase]]))
}

function claimPhaseSummary(
  memberCount: number,
  queueMilliseconds: number,
  runMilliseconds: number,
  activeMilliseconds: string,
  overlapMilliseconds: string,
  maximumActive: number,
) {
  return {
    batch_count: '1',
    member_count: memberCount.toString(),
    queue_ms: sampleHistogram(queueMilliseconds),
    run_ms: sampleHistogram(runMilliseconds),
    active_ms: activeMilliseconds,
    overlap_ms: overlapMilliseconds,
    maximum_active: maximumActive,
    active_at_completion: 0,
  }
}

function sampleHistogram(milliseconds: number) {
  const buckets = ['0', '0', '0', '0', '0', '0', '0', '0']
  const bucket = [1, 4, 16, 64, 256, 1_024, 4_096].findIndex(bound => milliseconds <= bound)
  buckets[bucket < 0 ? buckets.length - 1 : bucket] = '1'
  return histogram(buckets, '1', milliseconds.toString(), milliseconds.toString())
}

function emptyHistogram() {
  return histogram(['0', '0', '0', '0', '0', '0', '0', '0'], '0', '0', '0')
}

function emptyClaimInspectorPayload() {
  return {
    drains: '0',
    wall_ms: '0',
    maximum_width: 0,
    at_capacity_ms: '0',
    active_ms: '0',
    queued_member_ms: '0',
    pending_member_ms: '0',
    context_ms: {
      resident: '0',
      unfinished_inspection: '0',
      ordered_settlement: '0',
    },
    maximum: {
      active: 0,
      queued_members: 0,
      pending_members: 0,
      resident_contexts: 0,
      unfinished_inspection_contexts: 0,
      ordered_settlement_contexts: 0,
    },
    at_completion: {
      active: 0,
      queued_members: 0,
      pending_members: 0,
      resident_contexts: 0,
      unfinished_inspection_contexts: 0,
      ordered_settlement_contexts: 0,
    },
    under_capacity: Object.fromEntries(PERFORMANCE_CLAIM_INSPECTOR_REASONS_V1.map(reason => [
      reason,
      { wall_ms: '0', idle_slot_ms: '0' },
    ])),
  }
}

import { describe, expect, it } from 'vitest'

import {
  snapshotTraceEventObservationV1,
  traceEventObservationBytesV1,
  traceEventObservationNameV1,
} from '../../src/diagnostics/export/trace-event-v1'
import type { TraceEventObservationV1 } from '../../src/diagnostics/trace/model'
import { BoundedTraceRecorder } from '../../src/diagnostics/trace/recorder'
import {
  PERFORMANCE_CLAIM_INSPECTOR_REASONS_V1,
  PERFORMANCE_CLAIM_PHASES_V1,
  PERFORMANCE_FILE_PIPELINE_STAGES_V1,
  PERFORMANCE_NAMESPACE_KINDS_V1,
} from '../../src/diagnostics/trace/transfer-payload'
import {
  bindOutputPerformanceSummary,
  observePerformance,
  type OutputDiagnosticsPorts,
  type OutputTraceEvent,
} from '../../src/output/diagnostics'
import { MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT } from '../../src/output/materialization-ledger/model'
import { identityText } from '../transfer/v2-job-fixture'
import { runFSAProductionChain } from '../output/fsa-direct-tree-production-chain-fixture'

const FILE_COUNT = 582
const MAXIMUM_CONCURRENT_FILE_PIPELINES = 15
const MAXIMUM_ACTIVE_NATIVE_WRITERS = 8
const SYNTHETIC_ROOT_SCENARIO_ORDINAL = 2
const OPERATION_SEED = 120
const TRANSFER_JOB_SEED = 124
const OUTPUT_SESSION_SEED = 125

describe('FSA performance production observations', () => {
  it('emits one bounded exact summary for the deterministic 582-file production chain', async () => {
    let now = 1_000
    const clock = {
      nowMilliseconds: () => {
        const observed = now
        now += 1
        return observed
      },
    }
    const recorder = new BoundedTraceRecorder<TraceEventObservationV1, never, never>({
      captureGeneration: 1n,
      clock,
      scheduler: {
        schedule: () => Object.freeze({ cancel: () => undefined }),
      },
      eventName: traceEventObservationNameV1,
      snapshotEvent: snapshotTraceEventObservationV1,
      eventBytes: traceEventObservationBytesV1,
      snapshotIncident: incident => incident,
      incidentMarkerBytes: () => 1,
      incidentScope: incident => incident,
      sameScope: () => true,
    })
    const trace = Object.freeze({
      current: (event: OutputTraceEvent) => {
        if (event.eventName === 'performance_phase' || event.eventName === 'performance_summary') {
          recorder.record(event)
        }
      },
    })
    const receiveOperationId = identityText(OPERATION_SEED + SYNTHETIC_ROOT_SCENARIO_ORDINAL)
    const transferJobId = identityText(TRANSFER_JOB_SEED + SYNTHETIC_ROOT_SCENARIO_ORDINAL)
    const outputSessionId = identityText(OUTPUT_SESSION_SEED + SYNTHETIC_ROOT_SCENARIO_ORDINAL)
    const baseDiagnostics: OutputDiagnosticsPorts = Object.freeze({
      backend: 'file_system_access',
      trace,
    })
    const diagnostics = bindOutputPerformanceSummary(
      baseDiagnostics,
      { receiveOperationId, transferJobId, outputSessionId },
      clock,
    )
    if (diagnostics?.performance === undefined) {
      throw new Error('performance observations were not bound')
    }
    observePerformance(diagnostics.performance, summary =>
      summary.markMilestone('authority_acquired'))

    const observed = await runFSAProductionChain('synthetic-root', {
      diagnostics,
      syntheticRootFileCount: FILE_COUNT,
      maximumConcurrentFilePipelines: MAXIMUM_CONCURRENT_FILE_PIPELINES,
      maximumActiveNativeWriters: MAXIMUM_ACTIVE_NATIVE_WRITERS,
    })

    expect(observed.settlementFailure).toBeUndefined()
    expect(observed.lifecycleKind).toBe('published')
    expect(observed.workerStatus).toBe('Succeeded')
    expect(observed.physicalEntries).toHaveLength(
      FILE_COUNT + observed.directoryAdmissions.length,
    )

    const capture = recorder.snapshot()
    expect(capture.health).toEqual({
      droppedCount: 0n,
      overwrittenCount: 0n,
      sampledCount: 0n,
      coalescedCount: 0n,
    })
    expect(capture.events).toHaveLength(8)
    expect(capture.retainedEventCount).toBe(8n)
    expect(capture.events.every(event => event.encodedBytes <= 16_384)).toBe(true)
    const terminal = capture.events.at(-1)
    if (terminal?.value.kind !== 'event' ||
        terminal.value.event.eventName !== 'performance_summary') {
      throw new Error('production performance summary trace event is missing')
    }
    const summary = terminal.value.event.payload
    const expectedLedgerEntries = BigInt(
      FILE_COUNT + (2 * observed.directoryAdmissions.length),
    )
    const expectedLedgerPages = (
      expectedLedgerEntries + BigInt(MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT) - 1n
    ) / BigInt(MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT)
    const expectedNamespaceRuns = BigInt(
      1 + observed.directoryAdmissions.length + (2 * FILE_COUNT),
    )

    expect(summary.correlation).toEqual({
      receive_operation_id: receiveOperationId,
      transfer_job_id: transferJobId,
      output_session_id: outputSessionId,
    })
    expect(summary.bytes).toEqual({
      pending: FILE_COUNT.toString(),
      durable: FILE_COUNT.toString(),
      final: FILE_COUNT.toString(),
    })
    expect(summary.revision_opens.count).toBe(FILE_COUNT.toString())
    expect(summary.revision_opens.run_ms.sample_count).toBe(FILE_COUNT.toString())
    expect(summary.revision_opens.wait_ms.sample_count).toBe(FILE_COUNT.toString())
    expect(summary.final_transactions.count).toBe(FILE_COUNT.toString())
    expect(summary.final_transactions.elapsed_ms.sample_count).toBe(FILE_COUNT.toString())
    expect(summary.queue.writer.wait_ms.sample_count).toBe(FILE_COUNT.toString())
    expect(summary.queue.writer.run_ms.sample_count).toBe(FILE_COUNT.toString())
    expect(summary.queue.namespace.wait_ms.sample_count).toBe(expectedNamespaceRuns.toString())
    expect(summary.queue.namespace.run_ms.sample_count).toBe(expectedNamespaceRuns.toString())
    const namespaceKinds = PERFORMANCE_NAMESPACE_KINDS_V1.map(
      kind => summary.namespace_by_kind[kind],
    )
    expect(namespaceKinds.reduce(
      (total, kind) => total + BigInt(kind.wait_ms.sample_count),
      0n,
    )).toBe(expectedNamespaceRuns)
    expect(namespaceKinds.reduce(
      (total, kind) => total + BigInt(kind.wait_ms.total_ms),
      0n,
    )).toBe(BigInt(summary.queue.namespace.wait_ms.total_ms))
    expect(namespaceKinds.reduce(
      (total, kind) => total + BigInt(kind.run_ms.sample_count),
      0n,
    )).toBe(expectedNamespaceRuns)
    expect(namespaceKinds.reduce(
      (total, kind) => total + BigInt(kind.run_ms.total_ms),
      0n,
    )).toBe(BigInt(summary.queue.namespace.run_ms.total_ms))
    expect(summary.namespace_by_kind.reserve_name.wait_ms.sample_count).toBe('1')
    expect(summary.namespace_by_kind.create_directory.wait_ms.sample_count).toBe(
      observed.directoryAdmissions.length.toString(),
    )
    expect(summary.namespace_by_kind.create_file.wait_ms.sample_count).toBe(
      (2 * FILE_COUNT).toString(),
    )
    expect(summary.file_pipeline.worker_starts).toBe(
      MAXIMUM_CONCURRENT_FILE_PIPELINES.toString(),
    )
    expect(summary.file_pipeline.worker_stops).toBe(
      MAXIMUM_CONCURRENT_FILE_PIPELINES.toString(),
    )
    expect(PERFORMANCE_FILE_PIPELINE_STAGES_V1.reduce(
      (total, stage) => total + BigInt(summary.file_pipeline.stages[stage].occupancy_ms),
      0n,
    )).toBe(BigInt(summary.file_pipeline.worker_ms))
    expect(PERFORMANCE_FILE_PIPELINE_STAGES_V1.every(
      stage => summary.file_pipeline.stages[stage].active_at_completion === 0,
    )).toBe(true)
    expect(summary.file_pipeline.stages.revision_open.entries).toBe(FILE_COUNT.toString())
    expect(summary.file_pipeline.stages.namespace_inspection.entries).toBe(FILE_COUNT.toString())
    expect(summary.file_pipeline.stages.namespace_creation.entries).toBe(FILE_COUNT.toString())
    expect(summary.file_pipeline.stages.writer_lifecycle.entries).toBe(FILE_COUNT.toString())
    expect(summary.file_pipeline.stages.final_transaction.entries).toBe(FILE_COUNT.toString())
    expect(summary.revision_opens.attempts).toBe(FILE_COUNT.toString())
    expect(summary.revision_opens.active_at_completion).toBe(0)
    expect(summary.claim_batches.members).toBe(FILE_COUNT.toString())
    expect(BigInt(summary.claim_batches.count)).toBeGreaterThan(0n)
    expect(BigInt(summary.claim_batches.count)).toBeLessThanOrEqual(BigInt(FILE_COUNT))
    expect(summary.claim_batches.oldest_wait_ms.sample_count).toBe(summary.claim_batches.count)
    expect(summary.claim_batches.newest_wait_ms.sample_count).toBe(summary.claim_batches.count)
    expect(summary.claim_batches.run_ms.sample_count).toBe(summary.claim_batches.count)
    expect(summary.claim_batches.maximum_size).toBeGreaterThan(0)
    expect(summary.claim_batches.maximum_size).toBeLessThanOrEqual(FILE_COUNT)
    const claimPhases = PERFORMANCE_CLAIM_PHASES_V1.map(
      phase => summary.claim_batches.phases[phase],
    )
    expect(claimPhases.every(
      phase => phase.batch_count === summary.claim_batches.count &&
        phase.queue_ms.sample_count === phase.batch_count &&
        phase.run_ms.sample_count === phase.batch_count &&
        phase.active_at_completion === 0,
    )).toBe(true)
    expect(claimPhases.reduce(
      (total, phase) => total + BigInt(phase.queue_ms.total_ms) + BigInt(phase.run_ms.total_ms),
      0n,
    )).toBe(BigInt(summary.claim_batches.run_ms.total_ms))
    expect(summary.claim_batches.phases.classification.member_count).toBe(FILE_COUNT.toString())
    expect(summary.claim_batches.phases.inspection_union.member_count).toBe(FILE_COUNT.toString())
    expect(
      BigInt(summary.claim_batches.phases.reclassification.member_count) +
      BigInt(summary.claim_batches.phases.installation.member_count),
    ).toBe(BigInt(summary.claim_batches.phases.inspection_union.member_count))
    expect(summary.claim_batches.phases.inspection_union.maximum_active).toBeGreaterThan(0)
    for (const phase of claimPhases) {
      expect(BigInt(phase.overlap_ms)).toBeLessThanOrEqual(BigInt(phase.run_ms.total_ms))
      expect(BigInt(phase.active_ms)).toBeLessThanOrEqual(
        BigInt(phase.maximum_active) * BigInt(phase.run_ms.total_ms),
      )
    }
    const inspector = summary.claim_batches.inspector
    expect(BigInt(inspector.drains)).toBeGreaterThan(0n)
    expect(inspector.maximum_width).toBeGreaterThan(0)
    expect(inspector.maximum.active).toBeLessThanOrEqual(inspector.maximum_width)
    expect(BigInt(inspector.at_capacity_ms) + PERFORMANCE_CLAIM_INSPECTOR_REASONS_V1.reduce(
      (total, reason) => total + BigInt(inspector.under_capacity[reason].wall_ms),
      0n,
    )).toBe(BigInt(inspector.wall_ms))
    expect(BigInt(inspector.active_ms) + PERFORMANCE_CLAIM_INSPECTOR_REASONS_V1.reduce(
      (total, reason) => total + BigInt(inspector.under_capacity[reason].idle_slot_ms),
      0n,
    )).toBe(BigInt(inspector.maximum_width) * BigInt(inspector.wall_ms))
    expect(Object.values(inspector.at_completion).every(value => value === 0)).toBe(true)
    expect(summary.output_resources.active_files.wait_ms.sample_count).toBe(FILE_COUNT.toString())
    expect(summary.output_resources.write_bytes.wait_ms.sample_count).toBe(FILE_COUNT.toString())
    expect(summary.output_resources.buffered_bytes.wait_ms.sample_count).toBe(FILE_COUNT.toString())
    expect(summary.output_resources.active_files.peak).toBe(MAXIMUM_CONCURRENT_FILE_PIPELINES)
    expect(BigInt(summary.output_resources.write_bytes.peak)).toBeGreaterThan(0n)
    expect(BigInt(summary.output_resources.buffered_bytes.peak)).toBeGreaterThan(0n)
    expect(summary.checkpoints).toMatchObject({
      automatic_triggers: '0',
      forced_pause_triggers: '0',
      advanced: '0',
      declined: '0',
      estimated_copy_bytes: '0',
    })
    expect(summary.checkpoints.elapsed_ms.sample_count).toBe('0')
    expect(summary.ledger).toMatchObject({
      entries: expectedLedgerEntries.toString(),
      pages: expectedLedgerPages.toString(),
      seals: '1',
      recovery_scan_fallbacks: '0',
    })
    expect(summary.ledger.seal_elapsed_ms.sample_count).toBe('1')
    expect(summary.peaks.active_writers).toBeGreaterThan(0)
    expect(summary.peaks.active_writers).toBeLessThanOrEqual(MAXIMUM_ACTIVE_NATIVE_WRITERS)
    expect(summary.peaks.active_namespace).toBeGreaterThan(0)
    expect(summary.counter_overflowed).toBe(false)
    expect(Object.keys(summary.milestones)).toEqual([
      'authority_acquired',
      'first_content_request',
      'first_write',
      'last_byte',
      'last_final',
      'settlement_started',
      'published',
    ])
    const milestones = Object.values(summary.milestones).map(value => {
      if (value === null) throw new Error('production performance milestone is missing')
      return BigInt(value)
    })
    expect(milestones.every((value, index) => index === 0 || value >= milestones[index - 1]!))
      .toBe(true)
  }, 30_000)
})

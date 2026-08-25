import { describe, expect, it } from 'vitest'

import {
  TRACE_EVENT_NAMES_V1,
  type TraceEventNameV1,
  type TraceEventPayloadByNameV1,
} from '../../../src/diagnostics/trace/model'
import {
  PERFORMANCE_CLAIM_INSPECTOR_REASONS_V1,
  PERFORMANCE_FILE_PIPELINE_STAGES_V1,
  PERFORMANCE_NAMESPACE_KINDS_V1,
} from '../../../src/diagnostics/trace/transfer-payload'

const PERFORMANCE_CORRELATION = {
  receive_operation_id: 'AQAAAAAAAAAAAAAAAAAAAA',
  transfer_job_id: 'AgAAAAAAAAAAAAAAAAAAAA',
  output_session_id: 'AwAAAAAAAAAAAAAAAAAAAA',
} as const

const EMPTY_PERFORMANCE_HISTOGRAM = {
  upper_bounds_ms: [1, 4, 16, 64, 256, 1_024, 4_096],
  bucket_counts: ['0', '0', '0', '0', '0', '0', '0', '0'],
  sample_count: '0',
  total_ms: '0',
  maximum_ms: '0',
} as const

const EMPTY_NAMESPACE_BY_KIND = Object.fromEntries(PERFORMANCE_NAMESPACE_KINDS_V1.map(kind => [
  kind,
  { wait_ms: EMPTY_PERFORMANCE_HISTOGRAM, run_ms: EMPTY_PERFORMANCE_HISTOGRAM },
])) as Record<(typeof PERFORMANCE_NAMESPACE_KINDS_V1)[number], {
  wait_ms: typeof EMPTY_PERFORMANCE_HISTOGRAM
  run_ms: typeof EMPTY_PERFORMANCE_HISTOGRAM
}>

const EMPTY_FILE_PIPELINE_STAGES = Object.fromEntries(
  PERFORMANCE_FILE_PIPELINE_STAGES_V1.map(stage => [
    stage,
    { entries: '0', occupancy_ms: '0', peak_active: 0, active_at_completion: 0 },
  ]),
) as Record<(typeof PERFORMANCE_FILE_PIPELINE_STAGES_V1)[number], {
  entries: string
  occupancy_ms: string
  peak_active: number
  active_at_completion: number
}>

const PAYLOAD_FOR_EVERY_EVENT = {
  join_transition: { transition: 'started' },
  browse_transition: { transition: 'started' },
  preview_transition: { attempt: 'open', transition: 'started' },
  projection_transition: { transition: 'started', projection_epoch: '1' },
  authority_transition: {
    transition: 'activation_started',
    activation_id: 'BQAAAAAAAAAAAAAAAAAAAA',
    authenticated_share_instance_id: 'BgAAAAAAAAAAAAAAAAAAAA',
    selection_digest: 'BwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA',
    observed_protocol_session_id: 'AQAAAAAAAAAAAAAAAAAAAA',
    projection_epoch: '1',
    observation_revision: '1',
    artifact_kind: 'directory_tree',
    plan_kind: 'direct_tree',
  },
  protocol_operation: { transition: 'request_sent', request_kind: 'list_children' },
  peer_attempt: {
    stage: 'started',
    wave_ordinal: '1',
    wave_attempt_ordinal: '1',
    session_attempt_ordinal: '1',
  },
  peer_recovery: { stage: 'peer_detached' },
  lane_transition: { transition: 'attached' },
  receive_transition: { transition: 'materialization_started', plan_kind: 'direct_tree' },
  lifecycle_action_transition: { transition: 'started', action: 'pause' },
  transfer_progress: {
    discovered_files: '0',
    discovered_bytes: '0',
    written_bytes: '0',
    completed_files: '0',
    completed_bytes: '0',
    file_errors: '0',
    selection_errors: '0',
    failed_directories: '0',
    content_lanes: 0,
    capacity_waiting_files: '0',
    capacity_accumulated_wait_ms: '0',
    capacity_wait_attempts: '0',
    capacity_wait_visible: false,
    discovery: 'open',
    partial: false,
  },
  performance_phase: {
    correlation: PERFORMANCE_CORRELATION,
    milestone: 'authority_acquired',
    observer_elapsed_ms: '0',
  },
  performance_summary: {
    correlation: PERFORMANCE_CORRELATION,
    queue: {
      writer: {
        wait_ms: EMPTY_PERFORMANCE_HISTOGRAM,
        run_ms: EMPTY_PERFORMANCE_HISTOGRAM,
      },
      namespace: {
        wait_ms: EMPTY_PERFORMANCE_HISTOGRAM,
        run_ms: EMPTY_PERFORMANCE_HISTOGRAM,
      },
    },
    namespace_by_kind: EMPTY_NAMESPACE_BY_KIND,
    file_pipeline: {
      worker_starts: '0',
      worker_stops: '0',
      worker_ms: '0',
      stages: EMPTY_FILE_PIPELINE_STAGES,
    },
    peaks: {
      active_writers: 0,
      queued_writers: 0,
      active_namespace: 0,
      queued_namespace: 0,
    },
    authority_cache: {
      directory_hits: '0',
      directory_misses: '0',
      file_hits: '0',
      file_misses: '0',
      invalidations: '0',
      evictions: '0',
      walked_segments: '0',
      maximum_walk_depth: 0,
    },
    bytes: { pending: '0', durable: '0', final: '0' },
    checkpoints: {
      automatic_triggers: '0',
      forced_pause_triggers: '0',
      advanced: '0',
      declined: '0',
      constant_cost: '0',
      prefix_copy_cost: '0',
      space_preflight_cost: '0',
      estimated_copy_bytes: '0',
      elapsed_ms: EMPTY_PERFORMANCE_HISTOGRAM,
    },
    final_transactions: {
      count: '0',
      elapsed_ms: EMPTY_PERFORMANCE_HISTOGRAM,
    },
    ledger: {
      entries: '0',
      pages: '0',
      seals: '0',
      recovery_scan_fallbacks: '0',
      seal_elapsed_ms: EMPTY_PERFORMANCE_HISTOGRAM,
    },
    revision_opens: {
      attempts: '0',
      count: '0',
      wait_ms: EMPTY_PERFORMANCE_HISTOGRAM,
      run_ms: EMPTY_PERFORMANCE_HISTOGRAM,
      overlap_ms: '0',
      maximum_active: 0,
      active_at_completion: 0,
    },
    claim_batches: {
      count: '0',
      members: '0',
      maximum_size: 0,
      oldest_wait_ms: EMPTY_PERFORMANCE_HISTOGRAM,
      newest_wait_ms: EMPTY_PERFORMANCE_HISTOGRAM,
      run_ms: EMPTY_PERFORMANCE_HISTOGRAM,
      phases: {
        classification: emptyClaimPhase(),
        inspection_union: emptyClaimPhase(),
        reclassification: emptyClaimPhase(),
        installation: emptyClaimPhase(),
      },
      inspector: emptyClaimInspector(),
    },
    output_resources: {
      active_files: { wait_ms: EMPTY_PERFORMANCE_HISTOGRAM, peak: 0 },
      write_bytes: { wait_ms: EMPTY_PERFORMANCE_HISTOGRAM, peak: '0' },
      buffered_bytes: { wait_ms: EMPTY_PERFORMANCE_HISTOGRAM, peak: '0' },
    },
    milestones: {
      authority_acquired: null,
      first_content_request: null,
      first_write: null,
      last_byte: null,
      last_final: null,
      settlement_started: null,
      published: null,
    },
    counter_overflowed: false,
  },
  output_reservation: { backend: 'file_system_access', transition: 'started' },
  output_write: { backend: 'file_system_access', transition: 'transaction_started' },
  checkpoint: { backend: 'origin_private', transition: 'persisted' },
  settlement: { backend: 'portable', transition: 'completed', outcome: 'published' },
  publication: { backend: 'file_system_access', transition: 'committed' },
  continuation: { backend: 'origin_private', transition: 'paused' },
  reopen: { backend: 'origin_private', transition: 'authorized' },
  cleanup: { backend: 'portable', transition: 'completed' },
  direct_zip_milestone: {
    operation_id: 'AQAAAAAAAAAAAAAAAAAAAA',
    session_id: 'AgAAAAAAAAAAAAAAAAAAAA',
    plan_kind: 'direct_resumable_zip',
    milestone: 'checkpoint_promoted',
    checkpoint_phase: 'between_members',
    epoch_offset_class: 'member_descriptor',
    prefix_copy_decision: 'admit',
    peak_space_decision: 'within_budget',
    permission_decision: 'granted',
    identity_decision: 'verified',
    space_decision: 'admitted',
    cleanup_decision: 'not_requested',
  },
  retained_inventory: { transition: 'load_completed', operation_count: '0' },
  retained_action: {
    transition: 'started',
    action: 'continue',
    continuation: 'resume_receive',
  },
  incident_marker: {
    incident_sequence: '1',
    scope: { scope_kind: 'receive', scope_sequence: '2' },
  },
} as const satisfies {
  readonly [Name in TraceEventNameV1]: TraceEventPayloadByNameV1[Name]
}

function emptyClaimPhase() {
  return {
    batch_count: '0',
    member_count: '0',
    queue_ms: EMPTY_PERFORMANCE_HISTOGRAM,
    run_ms: EMPTY_PERFORMANCE_HISTOGRAM,
    active_ms: '0',
    overlap_ms: '0',
    maximum_active: 0,
    active_at_completion: 0,
  } as const
}

function emptyClaimInspector() {
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
    ])) as Record<(typeof PERFORMANCE_CLAIM_INSPECTOR_REASONS_V1)[number], {
      wall_ms: string
      idle_slot_ms: string
    }>,
  } as const
}

const CLOSED_AUTHORITY_OWNERSHIP_PAYLOADS = Object.freeze([
  Object.freeze({
    ...PAYLOAD_FOR_EVERY_EVENT.authority_transition,
    transition: 'commit_pre_cut_retry' as const,
    receiver_operation_id: 'AgAAAAAAAAAAAAAAAAAAAA',
  }),
  Object.freeze({
    ...PAYLOAD_FOR_EVERY_EVENT.authority_transition,
    transition: 'cleanup_failed' as const,
    receiver_operation_id: 'AgAAAAAAAAAAAAAAAAAAAA',
    failed_stage: 'settlement' as const,
  }),
]) satisfies readonly TraceEventPayloadByNameV1['authority_transition'][]

describe('TraceEventPayloadV1 integration union', () => {
  it('owns one closed JSON payload for every frozen event name', () => {
    expect(Object.keys(PAYLOAD_FOR_EVERY_EVENT)).toEqual(TRACE_EVENT_NAMES_V1)
    expect(() => JSON.stringify(PAYLOAD_FOR_EVERY_EVENT)).not.toThrow()
    expect(CLOSED_AUTHORITY_OWNERSHIP_PAYLOADS.map(({ transition }) => transition)).toEqual([
      'commit_pre_cut_retry',
      'cleanup_failed',
    ])
  })
})

import { describe, expect, it } from 'vitest'

import {
  TRACE_EVENT_NAMES_V1,
  type TraceEventNameV1,
  type TraceEventPayloadByNameV1,
} from '../../../src/diagnostics/trace/model'

const PAYLOAD_FOR_EVERY_EVENT = {
  join_transition: { transition: 'started' },
  browse_transition: { transition: 'started' },
  preview_transition: { attempt: 'open', transition: 'started' },
  projection_transition: { transition: 'started', projection_epoch: '1' },
  authority_transition: { transition: 'activation_started' },
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
    discovery: 'open',
    partial: false,
  },
  output_reservation: { backend: 'file_system_access', transition: 'started' },
  output_write: { backend: 'file_system_access', transition: 'transaction_started' },
  checkpoint: { backend: 'origin_private', transition: 'persisted' },
  settlement: { backend: 'portable', transition: 'completed', outcome: 'published' },
  publication: { backend: 'file_system_access', transition: 'committed' },
  continuation: { backend: 'origin_private', transition: 'paused' },
  reopen: { backend: 'origin_private', transition: 'authorized' },
  cleanup: { backend: 'portable', transition: 'completed' },
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

describe('TraceEventPayloadV1 integration union', () => {
  it('owns one closed JSON payload for every frozen event name', () => {
    expect(Object.keys(PAYLOAD_FOR_EVERY_EVENT)).toEqual(TRACE_EVENT_NAMES_V1)
    expect(() => JSON.stringify(PAYLOAD_FOR_EVERY_EVENT)).not.toThrow()
  })
})

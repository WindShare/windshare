import { describe, expect, it } from 'vitest'

import {
  DEFAULT_TRACE_CAPACITY_POLICY,
  DEFAULT_TRACE_CAPTURE_EXPIRY_MS,
  MAX_TRACE_EVENT_BYTES,
  MAX_TRACE_EVENT_COUNT,
  MAX_TRACE_INCIDENT_MARKERS,
  MAX_TRACE_POST_FAILURE_BYTES,
  MAX_TRACE_POST_FAILURE_EVENT_COUNT,
  MAX_TRACE_POST_FAILURE_MS,
  MAX_TRACE_PRE_FAILURE_BYTES,
  MAX_TRACE_PRE_FAILURE_EVENT_COUNT,
  MAX_TRACE_TOTAL_BYTES,
  TRACE_CHECKPOINT_COALESCE_INTERVAL_MS,
  TRACE_POST_FAILURE_SILENCE_MS,
  TRACE_PROGRESS_SAMPLE_INTERVAL_MS,
  createTraceCapacityPolicy,
} from '../../../src/diagnostics/trace/capacity'
import {
  TRACE_CAPTURE_STATES,
  TRACE_EVENT_NAMES_V1,
  TRACE_SEAL_REASONS,
} from '../../../src/diagnostics/trace/model'

describe('trace frozen contract', () => {
  it('publishes the exact named capacity defaults', () => {
    expect(DEFAULT_TRACE_CAPACITY_POLICY).toEqual({
      captureExpiryMs: DEFAULT_TRACE_CAPTURE_EXPIRY_MS,
      maxEventCount: MAX_TRACE_EVENT_COUNT,
      maxEventBytes: MAX_TRACE_EVENT_BYTES,
      maxTotalBytes: MAX_TRACE_TOTAL_BYTES,
      maxPreFailureEventCount: MAX_TRACE_PRE_FAILURE_EVENT_COUNT,
      maxPreFailureBytes: MAX_TRACE_PRE_FAILURE_BYTES,
      maxPostFailureEventCount: MAX_TRACE_POST_FAILURE_EVENT_COUNT,
      maxPostFailureBytes: MAX_TRACE_POST_FAILURE_BYTES,
      maxIncidentMarkers: MAX_TRACE_INCIDENT_MARKERS,
      maxPostFailureMs: MAX_TRACE_POST_FAILURE_MS,
      postFailureSilenceMs: TRACE_POST_FAILURE_SILENCE_MS,
      progressSampleIntervalMs: TRACE_PROGRESS_SAMPLE_INTERVAL_MS,
      checkpointCoalesceIntervalMs: TRACE_CHECKPOINT_COALESCE_INTERVAL_MS,
    })
    expect(DEFAULT_TRACE_CAPACITY_POLICY).toEqual({
      captureExpiryMs: 1_800_000,
      maxEventCount: 4_096,
      maxEventBytes: 16_384,
      maxTotalBytes: 4_194_304,
      maxPreFailureEventCount: 2_048,
      maxPreFailureBytes: 2_097_152,
      maxPostFailureEventCount: 2_048,
      maxPostFailureBytes: 2_097_152,
      maxIncidentMarkers: 32,
      maxPostFailureMs: 30_000,
      postFailureSilenceMs: 5_000,
      progressSampleIntervalMs: 1_000,
      checkpointCoalesceIntervalMs: 1_000,
    })
    expect(Object.isFrozen(DEFAULT_TRACE_CAPACITY_POLICY)).toBe(true)
  })

  it('keeps state, seal reason, and event names closed and ordered', () => {
    expect(TRACE_CAPTURE_STATES).toEqual([
      'idle',
      'recording_pre_failure',
      'recording_post_failure',
      'sealed',
    ])
    expect(TRACE_SEAL_REASONS).toEqual([
      'manual_disable',
      'expired',
      'scope_terminal',
      'post_failure_silence',
      'capacity_exhausted',
    ])
    expect(TRACE_EVENT_NAMES_V1).toEqual([
      'join_transition',
      'browse_transition',
      'preview_transition',
      'projection_transition',
      'authority_transition',
      'protocol_operation',
      'peer_attempt',
      'peer_recovery',
      'lane_transition',
      'receive_transition',
      'lifecycle_action_transition',
      'transfer_progress',
      'output_reservation',
      'output_write',
      'checkpoint',
      'settlement',
      'publication',
      'continuation',
      'reopen',
      'cleanup',
      'retained_inventory',
      'retained_action',
      'incident_marker',
    ])
    expect(Object.isFrozen(TRACE_CAPTURE_STATES)).toBe(true)
    expect(Object.isFrozen(TRACE_SEAL_REASONS)).toBe(true)
    expect(Object.isFrozen(TRACE_EVENT_NAMES_V1)).toBe(true)
  })

  it('rejects policies whose local or relational bounds are invalid', () => {
    expect(() => createTraceCapacityPolicy({ maxEventCount: 0 })).toThrow(RangeError)
    expect(() => createTraceCapacityPolicy({
      maxEventBytes: DEFAULT_TRACE_CAPACITY_POLICY.maxPreFailureBytes + 1,
    })).toThrow(/fit every capture byte budget/)
    expect(() => createTraceCapacityPolicy({
      maxIncidentMarkers: DEFAULT_TRACE_CAPACITY_POLICY.maxPostFailureEventCount + 1,
    })).toThrow(/markers/)
    expect(() => createTraceCapacityPolicy({
      postFailureSilenceMs: DEFAULT_TRACE_CAPACITY_POLICY.maxPostFailureMs + 1,
    })).toThrow(/silence/)
  })
})

export const DEFAULT_TRACE_CAPTURE_EXPIRY_MS = 1_800_000
export const MAX_TRACE_EVENT_COUNT = 4_096
export const MAX_TRACE_EVENT_BYTES = 16_384
export const MAX_TRACE_TOTAL_BYTES = 4_194_304
export const MAX_TRACE_INCIDENT_MARKERS = 32
export const TRACE_PROGRESS_SAMPLE_INTERVAL_MS = 1_000
export const TRACE_CHECKPOINT_COALESCE_INTERVAL_MS = 1_000

export interface TraceCapacityPolicy {
  readonly captureExpiryMs: number
  readonly maxEventCount: number
  readonly maxEventBytes: number
  readonly maxTotalBytes: number
  readonly maxIncidentMarkers: number
  readonly progressSampleIntervalMs: number
  readonly checkpointCoalesceIntervalMs: number
}

export const DEFAULT_TRACE_CAPACITY_POLICY: TraceCapacityPolicy = Object.freeze({
  captureExpiryMs: DEFAULT_TRACE_CAPTURE_EXPIRY_MS,
  maxEventCount: MAX_TRACE_EVENT_COUNT,
  maxEventBytes: MAX_TRACE_EVENT_BYTES,
  maxTotalBytes: MAX_TRACE_TOTAL_BYTES,
  maxIncidentMarkers: MAX_TRACE_INCIDENT_MARKERS,
  progressSampleIntervalMs: TRACE_PROGRESS_SAMPLE_INTERVAL_MS,
  checkpointCoalesceIntervalMs: TRACE_CHECKPOINT_COALESCE_INTERVAL_MS,
})

export function createTraceCapacityPolicy(
  overrides: Partial<TraceCapacityPolicy> = {},
): TraceCapacityPolicy {
  const policy: TraceCapacityPolicy = Object.freeze({ ...DEFAULT_TRACE_CAPACITY_POLICY, ...overrides })
  for (const [name, value] of Object.entries(policy)) {
    if (!Number.isSafeInteger(value) || value <= 0) {
      throw new RangeError(`${name} must be a positive safe integer`)
    }
  }
  if (policy.maxEventBytes > policy.maxTotalBytes) {
    throw new RangeError('trace event bytes must fit the capture byte budget')
  }
  if (policy.maxIncidentMarkers > policy.maxEventCount) {
    throw new RangeError('trace incident markers must fit the capture event budget')
  }
  return policy
}

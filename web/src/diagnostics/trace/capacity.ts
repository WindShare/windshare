export const DEFAULT_TRACE_CAPTURE_EXPIRY_MS = 1_800_000
export const MAX_TRACE_EVENT_COUNT = 4_096
export const MAX_TRACE_EVENT_BYTES = 16_384
export const MAX_TRACE_TOTAL_BYTES = 4_194_304
export const MAX_TRACE_PRE_FAILURE_EVENT_COUNT = 2_048
export const MAX_TRACE_PRE_FAILURE_BYTES = 2_097_152
export const MAX_TRACE_POST_FAILURE_EVENT_COUNT = 2_048
export const MAX_TRACE_POST_FAILURE_BYTES = 2_097_152
export const MAX_TRACE_INCIDENT_MARKERS = 32
export const MAX_TRACE_POST_FAILURE_MS = 30_000
export const TRACE_POST_FAILURE_SILENCE_MS = 5_000
export const TRACE_PROGRESS_SAMPLE_INTERVAL_MS = 1_000
export const TRACE_CHECKPOINT_COALESCE_INTERVAL_MS = 1_000

export interface TraceCapacityPolicy {
  readonly captureExpiryMs: number
  readonly maxEventCount: number
  readonly maxEventBytes: number
  readonly maxTotalBytes: number
  readonly maxPreFailureEventCount: number
  readonly maxPreFailureBytes: number
  readonly maxPostFailureEventCount: number
  readonly maxPostFailureBytes: number
  readonly maxIncidentMarkers: number
  readonly maxPostFailureMs: number
  readonly postFailureSilenceMs: number
  readonly progressSampleIntervalMs: number
  readonly checkpointCoalesceIntervalMs: number
}

export const DEFAULT_TRACE_CAPACITY_POLICY: TraceCapacityPolicy = Object.freeze({
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

export function createTraceCapacityPolicy(
  overrides: Partial<TraceCapacityPolicy> = {},
): TraceCapacityPolicy {
  const policy: TraceCapacityPolicy = Object.freeze({
    ...DEFAULT_TRACE_CAPACITY_POLICY,
    ...overrides,
  })
  validatePositiveSafeIntegers(policy)
  if (policy.maxEventBytes > policy.maxPreFailureBytes ||
      policy.maxEventBytes > policy.maxPostFailureBytes ||
      policy.maxEventBytes > policy.maxTotalBytes) {
    throw new RangeError('trace event bytes must fit every capture byte budget')
  }
  if (policy.maxPreFailureEventCount > policy.maxEventCount ||
      policy.maxPostFailureEventCount > policy.maxEventCount) {
    throw new RangeError('trace phase event count must fit the capture event budget')
  }
  if (policy.maxPreFailureBytes > policy.maxTotalBytes ||
      policy.maxPostFailureBytes > policy.maxTotalBytes) {
    throw new RangeError('trace phase bytes must fit the capture byte budget')
  }
  if (policy.maxIncidentMarkers > policy.maxPostFailureEventCount) {
    throw new RangeError('trace incident markers must fit the post-failure event budget')
  }
  if (policy.postFailureSilenceMs > policy.maxPostFailureMs) {
    throw new RangeError('trace silence deadline must not exceed the post-failure deadline')
  }
  return policy
}

function validatePositiveSafeIntegers(policy: TraceCapacityPolicy): void {
  for (const [name, value] of Object.entries(policy)) {
    if (!Number.isSafeInteger(value) || value <= 0) {
      throw new RangeError(`${name} must be a positive safe integer`)
    }
  }
}

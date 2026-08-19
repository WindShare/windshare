import {
  snapshotTraceEventObservationV1,
  traceEventObservationBytesV1,
  traceEventObservationNameV1,
} from '../../src/diagnostics/export/trace-event-v1'
import type { TraceEventObservationV1 } from '../../src/diagnostics/trace/model'
import {
  SYSTEM_TRACE_CLOCK,
  SYSTEM_TRACE_SCHEDULER,
  type DomainTraceSource,
} from '../../src/diagnostics/trace/ports'
import { TraceSwitch } from '../../src/diagnostics/trace/switch'

const DIRECTORY_FANOUT = 32
const LANE_COUNT = 4
const MINIMUM_FILE_BYTES = 512
const FILE_BYTE_SPAN = 1_048_576
const HEARTBEAT_INTERVAL_MS = 8
const HEXADECIMAL_RADIX = 16
const DIGEST_HEX_WIDTH = 8
const FNV_OFFSET_BASIS = 0x811c9dc5
const FNV_PRIME = 0x01000193

interface EvidenceOptions {
  readonly entryCount: number
  readonly pageSize: number
  readonly traceEnabled: boolean
}

interface EvidenceScope {
  readonly kind: 'receive'
  readonly sequence: bigint
}

interface EvidenceIncident {
  readonly sequence: bigint
  readonly scope: EvidenceScope
}

interface HeapMeasurement {
  readonly source: 'chromium_precise_performance_memory'
  readonly beforeBytes: number
  readonly peakBytes: number
  readonly afterBytes: number
}

interface WorkloadCounters {
  fileCount: number
  directoryCount: number
  completedBytes: number
  decisionDigest: number
  payloadConstructionCount: number
  readonly assignedFilesByLane: number[]
  readonly assignedBytesByLane: number[]
}

interface EvidenceTraceResult {
  readonly state: string
  readonly sealReason?: string
  readonly retainedIncidentCount: '0'
  readonly retainedIncidentBytes: '0'
  readonly retainedEventCount: string
  readonly retainedEventBytes: string
  readonly droppedCount: string
  readonly overwrittenCount: string
  readonly sampledCount: string
  readonly coalescedCount: string
  readonly payloadConstructionCount: number
}

export interface EvidenceResult {
  readonly mode: 'trace_off' | 'trace_on'
  readonly wallMilliseconds: number
  readonly heartbeatDelayMilliseconds: readonly number[]
  readonly heap: HeapMeasurement | null
  readonly usefulWork: Readonly<{
    entries: number
    bytes: string
  }>
  readonly decisions: Readonly<{
    product: Readonly<{
      artifact: 'directory_tree'
      plan: 'direct_tree'
      fileCount: string
      directoryCount: string
      byteCount: string
    }>
    lane: Readonly<{
      policy: 'least_queued_then_lane_id'
      laneCount: number
      assignedFilesByLane: readonly string[]
      assignedBytesByLane: readonly string[]
      assignmentDigest: string
    }>
    settlement: Readonly<{
      outcome: 'published'
      successCount: string
      failureCount: '0'
      completedBytes: string
      decisionDigest: string
    }>
  }>
  readonly trace: EvidenceTraceResult
}

declare global {
  interface Window {
    runWindShareDiagnosticsEvidence(options: EvidenceOptions): Promise<EvidenceResult>
  }
}

window.runWindShareDiagnosticsEvidence = runEvidence

async function runEvidence(options: EvidenceOptions): Promise<EvidenceResult> {
  validateOptions(options)
  const trace = createEvidenceTraceSwitch()
  if (options.traceEnabled) trace.enable()
  const counters = createWorkloadCounters()
  const heartbeatDelays: number[] = []
  const heapBefore = readHeapBytes()
  let heapPeak = heapBefore
  const stopHeartbeat = startHeartbeat(heartbeatDelays)
  const startedAt = performance.now()
  try {
    emitStartEvents(trace, counters)
    for (let start = 0; start < options.entryCount; start += options.pageSize) {
      const end = Math.min(options.entryCount, start + options.pageSize)
      processPage(start, end, counters)
      emitPageEvents(trace, counters, end === options.entryCount)
      heapPeak = maximumAvailable(heapPeak, readHeapBytes())
      await yieldToBrowser()
    }
    emitCompletionEvents(trace, counters)
    await yieldToBrowser()
  } finally {
    stopHeartbeat()
  }
  const wallMilliseconds = performance.now() - startedAt
  const heapAfter = readHeapBytes()
  heapPeak = maximumAvailable(heapPeak, heapAfter)
  if (options.traceEnabled) trace.disable()
  const status = trace.status()
  return Object.freeze({
    mode: options.traceEnabled ? 'trace_on' : 'trace_off',
    wallMilliseconds,
    heartbeatDelayMilliseconds: Object.freeze(heartbeatDelays),
    heap: heapMeasurement(heapBefore, heapPeak, heapAfter),
    usefulWork: Object.freeze({
      entries: counters.fileCount,
      bytes: String(counters.completedBytes),
    }),
    decisions: projectDecisions(counters),
    trace: Object.freeze({
      state: status.state,
      ...(status.sealReason === undefined ? {} : { sealReason: status.sealReason }),
      retainedIncidentCount: '0',
      retainedIncidentBytes: '0',
      retainedEventCount: String(status.retainedEventCount),
      retainedEventBytes: String(status.retainedEventBytes),
      droppedCount: String(status.health.droppedCount),
      overwrittenCount: String(status.health.overwrittenCount),
      sampledCount: String(status.health.sampledCount),
      coalescedCount: String(status.health.coalescedCount),
      payloadConstructionCount: counters.payloadConstructionCount,
    }),
  })
}

function createEvidenceTraceSwitch(): TraceSwitch<
  TraceEventObservationV1,
  EvidenceIncident,
  EvidenceScope
> {
  return new TraceSwitch({
    clock: SYSTEM_TRACE_CLOCK,
    scheduler: SYSTEM_TRACE_SCHEDULER,
    eventName: traceEventObservationNameV1,
    snapshotEvent: snapshotTraceEventObservationV1,
    eventBytes: traceEventObservationBytesV1,
    snapshotIncident: (incident) => Object.freeze({
      sequence: incident.sequence,
      scope: Object.freeze({ ...incident.scope }),
    }),
    incidentMarkerBytes: () => 1,
    incidentScope: (incident) => incident.scope,
    sameScope: (left, right) => left.kind === right.kind && left.sequence === right.sequence,
  })
}

function createWorkloadCounters(): WorkloadCounters {
  return {
    fileCount: 0,
    directoryCount: 0,
    completedBytes: 0,
    decisionDigest: FNV_OFFSET_BASIS,
    payloadConstructionCount: 0,
    assignedFilesByLane: Array.from({ length: LANE_COUNT }, () => 0),
    assignedBytesByLane: Array.from({ length: LANE_COUNT }, () => 0),
  }
}

function processPage(start: number, end: number, counters: WorkloadCounters): void {
  for (let ordinal = start; ordinal < end; ordinal += 1) {
    const entryToken = mix32(ordinal + 1)
    const fileBytes = MINIMUM_FILE_BYTES + (entryToken % FILE_BYTE_SPAN)
    const lane = chooseLane(counters.assignedBytesByLane, entryToken)
    counters.fileCount += 1
    counters.completedBytes += fileBytes
    counters.assignedFilesByLane[lane] = counters.assignedFilesByLane[lane]! + 1
    counters.assignedBytesByLane[lane] = counters.assignedBytesByLane[lane]! + fileBytes
    counters.decisionDigest = digestDecision(counters.decisionDigest, ordinal, fileBytes, lane)
  }
  counters.directoryCount = Math.ceil(counters.fileCount / DIRECTORY_FANOUT)
}

function chooseLane(assignedBytes: readonly number[], entryToken: number): number {
  let chosen = entryToken % LANE_COUNT
  for (let lane = 0; lane < assignedBytes.length; lane += 1) {
    if (assignedBytes[lane]! < assignedBytes[chosen]!) chosen = lane
  }
  return chosen
}

function emitStartEvents(
  trace: DomainTraceSource<TraceEventObservationV1>,
  counters: WorkloadCounters,
): void {
  emitTrace(trace, counters, () => ({
    eventName: 'receive_transition',
    payload: {
      transition: 'intent_frozen',
      artifact_kind: 'directory_tree',
      layout_class: 'directory_tree_result_root',
      plan_kind: 'direct_tree',
    },
  }))
  for (let lane = 0; lane < LANE_COUNT; lane += 1) {
    emitTrace(trace, counters, () => ({
      eventName: 'lane_transition',
      correlation: { lane_id: lane, lane_epoch: 0 },
      payload: { transition: 'installed' },
    }))
  }
}

function emitPageEvents(
  trace: DomainTraceSource<TraceEventObservationV1>,
  counters: WorkloadCounters,
  complete: boolean,
): void {
  emitTrace(trace, counters, () => ({
    eventName: 'browse_transition',
    payload: { transition: 'page_loaded', entry_count: String(counters.fileCount) },
  }))
  emitTrace(trace, counters, () => ({
    eventName: 'projection_transition',
    payload: {
      transition: 'refined',
      projection_epoch: '1',
      shape_proof: 'tree',
      discovery_state: complete ? 'complete' : 'discovering',
      file_count_lower_bound: String(counters.fileCount),
      directory_count_lower_bound: String(counters.directoryCount),
      byte_count_lower_bound: String(counters.completedBytes),
      unsettled_target_count: complete ? '0' : '1',
    },
  }))
  emitTrace(trace, counters, () => ({
    eventName: 'transfer_progress',
    payload: {
      discovered_files: String(counters.fileCount),
      discovered_bytes: String(counters.completedBytes),
      written_bytes: String(counters.completedBytes),
      completed_files: String(counters.fileCount),
      completed_bytes: String(counters.completedBytes),
      file_errors: '0',
      selection_errors: '0',
      failed_directories: '0',
      content_lanes: LANE_COUNT,
      discovery: complete ? 'complete' : 'open',
      partial: false,
    },
  }))
  emitTrace(trace, counters, () => ({
    eventName: 'checkpoint',
    payload: { backend: 'origin_private', transition: 'persisted' },
  }))
}

function emitCompletionEvents(
  trace: DomainTraceSource<TraceEventObservationV1>,
  counters: WorkloadCounters,
): void {
  emitTrace(trace, counters, () => ({
    eventName: 'receive_transition',
    payload: {
      transition: 'materialization_completed',
      entry_count: String(counters.fileCount + counters.directoryCount),
      file_count: String(counters.fileCount),
      directory_count: String(counters.directoryCount),
      raw_bytes: String(counters.completedBytes),
    },
  }))
  emitTrace(trace, counters, () => ({
    eventName: 'receive_transition',
    payload: {
      transition: 'tree_finalized',
      outcome: 'published',
      success_count: String(counters.fileCount),
      failure_count: '0',
    },
  }))
  emitTrace(trace, counters, () => ({
    eventName: 'settlement',
    payload: {
      backend: 'origin_private',
      transition: 'completed',
      outcome: 'published',
    },
  }))
}

function emitTrace(
  trace: DomainTraceSource<TraceEventObservationV1>,
  counters: WorkloadCounters,
  construct: () => TraceEventObservationV1,
): void {
  const observer = trace.current
  if (observer === undefined) return
  counters.payloadConstructionCount += 1
  observer(construct())
}

function projectDecisions(counters: WorkloadCounters): EvidenceResult['decisions'] {
  const digest = hexadecimalDigest(counters.decisionDigest)
  return Object.freeze({
    product: Object.freeze({
      artifact: 'directory_tree' as const,
      plan: 'direct_tree' as const,
      fileCount: String(counters.fileCount),
      directoryCount: String(counters.directoryCount),
      byteCount: String(counters.completedBytes),
    }),
    lane: Object.freeze({
      policy: 'least_queued_then_lane_id' as const,
      laneCount: LANE_COUNT,
      assignedFilesByLane: Object.freeze(counters.assignedFilesByLane.map(String)),
      assignedBytesByLane: Object.freeze(counters.assignedBytesByLane.map(String)),
      assignmentDigest: digest,
    }),
    settlement: Object.freeze({
      outcome: 'published' as const,
      successCount: String(counters.fileCount),
      failureCount: '0' as const,
      completedBytes: String(counters.completedBytes),
      decisionDigest: digest,
    }),
  })
}

function startHeartbeat(delays: number[]): () => void {
  let expectedAt = performance.now() + HEARTBEAT_INTERVAL_MS
  const timer = window.setInterval(() => {
    const observedAt = performance.now()
    delays.push(Math.max(0, observedAt - expectedAt))
    expectedAt = observedAt + HEARTBEAT_INTERVAL_MS
  }, HEARTBEAT_INTERVAL_MS)
  return () => window.clearInterval(timer)
}

function yieldToBrowser(): Promise<void> {
  return new Promise((resolve) => window.setTimeout(resolve, 0))
}

function readHeapBytes(): number | null {
  const memory = (performance as Performance & {
    readonly memory?: Readonly<{ usedJSHeapSize?: number }>
  }).memory
  const bytes = memory?.usedJSHeapSize
  return typeof bytes === 'number' && Number.isSafeInteger(bytes) && bytes >= 0 ? bytes : null
}

function heapMeasurement(
  beforeBytes: number | null,
  peakBytes: number | null,
  afterBytes: number | null,
): HeapMeasurement | null {
  if (beforeBytes === null || peakBytes === null || afterBytes === null) return null
  return Object.freeze({
    source: 'chromium_precise_performance_memory',
    beforeBytes,
    peakBytes,
    afterBytes,
  })
}

function maximumAvailable(left: number | null, right: number | null): number | null {
  if (left === null) return right
  if (right === null) return left
  return Math.max(left, right)
}

function digestDecision(seed: number, ordinal: number, bytes: number, lane: number): number {
  let digest = digestWord(seed, ordinal)
  digest = digestWord(digest, bytes)
  return digestWord(digest, lane)
}

function digestWord(seed: number, value: number): number {
  let digest = seed
  for (let shift = 0; shift < 32; shift += 8) {
    digest ^= (value >>> shift) & 0xff
    digest = Math.imul(digest, FNV_PRIME) >>> 0
  }
  return digest
}

function hexadecimalDigest(value: number): string {
  return value.toString(HEXADECIMAL_RADIX).padStart(DIGEST_HEX_WIDTH, '0')
}

function mix32(value: number): number {
  let mixed = value >>> 0
  mixed = Math.imul(mixed ^ (mixed >>> 16), 0x21f0aaad)
  mixed = Math.imul(mixed ^ (mixed >>> 15), 0x735a2d97)
  return (mixed ^ (mixed >>> 15)) >>> 0
}

function validateOptions(options: EvidenceOptions): void {
  if (!Number.isSafeInteger(options.entryCount) || options.entryCount <= 0) {
    throw new RangeError('entryCount must be a positive safe integer')
  }
  if (!Number.isSafeInteger(options.pageSize) || options.pageSize <= 0) {
    throw new RangeError('pageSize must be a positive safe integer')
  }
  if (typeof options.traceEnabled !== 'boolean') {
    throw new TypeError('traceEnabled must be a boolean')
  }
}

import {
  PERFORMANCE_CLAIM_INSPECTOR_REASONS_V1,
  PERFORMANCE_CLAIM_PHASES_V1,
  PERFORMANCE_FILE_PIPELINE_STAGES_V1,
  PERFORMANCE_HISTOGRAM_UPPER_BOUNDS_MS,
  PERFORMANCE_MILESTONES_V1,
  PERFORMANCE_NAMESPACE_KINDS_V1,
} from '../trace/transfer-payload'
import {
  booleanValue,
  decimalFields,
  decimalUint64,
  exactKeys,
  member,
  recordValue,
  uint32,
  type UnknownRecord,
} from './trace-payload-validation'

const CANONICAL_IDENTITY = /^[A-Za-z0-9_-]{21}[AQgw]$/
const ZERO_IDENTITY = 'A'.repeat(22)

export function validatePerformancePhase(payload: UnknownRecord): void {
  exactKeys(payload, ['correlation', 'milestone', 'observer_elapsed_ms'], [],
    'performance phase payload')
  validatePerformanceCorrelation(recordValue(payload.correlation, 'performance correlation'))
  member(payload.milestone, PERFORMANCE_MILESTONES_V1, 'performance milestone')
  decimalUint64(payload.observer_elapsed_ms, 'performance phase elapsed milliseconds')
}

export function validatePerformanceSummary(payload: UnknownRecord): void {
  exactKeys(payload, [
    'correlation',
    'queue',
    'namespace_by_kind',
    'file_pipeline',
    'peaks',
    'authority_cache',
    'bytes',
    'checkpoints',
    'final_transactions',
    'ledger',
    'revision_opens',
    'claim_batches',
    'output_resources',
    'milestones',
    'counter_overflowed',
  ], [], 'performance summary payload')
  validatePerformanceCorrelation(recordValue(payload.correlation, 'performance correlation'))
  booleanValue(payload.counter_overflowed, 'performance counter overflow marker')
  validatePerformanceQueues(payload)
  validatePerformanceFilePipeline(payload)
  validatePerformanceStorage(payload)
  validateRevisionOpens(recordValue(payload.revision_opens, 'performance revision opens'))
  validateClaimBatches(
    recordValue(payload.claim_batches, 'performance claim batches'),
    payload.counter_overflowed,
  )
  validateOutputResources(recordValue(payload.output_resources, 'performance output resources'))
  validatePerformanceMilestones(recordValue(payload.milestones, 'performance milestones'))
}

function validatePerformanceQueues(payload: UnknownRecord): void {
  const queue = recordValue(payload.queue, 'performance queue')
  exactKeys(queue, ['writer', 'namespace'], [], 'performance queue')
  for (const lane of ['writer', 'namespace'] as const) {
    validateQueueLane(recordValue(queue[lane], `performance ${lane} queue`), lane)
  }
  const namespaceByKind = recordValue(
    payload.namespace_by_kind,
    'performance namespace by kind',
  )
  exactKeys(namespaceByKind, PERFORMANCE_NAMESPACE_KINDS_V1, [],
    'performance namespace by kind')
  let namespaceWaitSamples = 0n
  let namespaceWaitTotal = 0n
  let namespaceRunSamples = 0n
  let namespaceRunTotal = 0n
  for (const kind of PERFORMANCE_NAMESPACE_KINDS_V1) {
    const kindSummary = recordValue(namespaceByKind[kind], `performance namespace ${kind}`)
    exactKeys(kindSummary, ['wait_ms', 'run_ms'], [], `performance namespace ${kind}`)
    const wait = recordValue(kindSummary.wait_ms, `performance namespace ${kind} wait`)
    const run = recordValue(kindSummary.run_ms, `performance namespace ${kind} run`)
    validatePerformanceHistogram(wait, `performance namespace ${kind} wait`)
    validatePerformanceHistogram(run, `performance namespace ${kind} run`)
    equalDecimal(wait.sample_count, run.sample_count, `performance namespace ${kind} count`)
    namespaceWaitSamples += BigInt(wait.sample_count as string)
    namespaceWaitTotal += BigInt(wait.total_ms as string)
    namespaceRunSamples += BigInt(run.sample_count as string)
    namespaceRunTotal += BigInt(run.total_ms as string)
  }
  const namespaceAggregate = recordValue(queue.namespace, 'performance namespace aggregate')
  if (!payload.counter_overflowed) {
    equalBigint(namespaceWaitSamples, histogramDecimal(namespaceAggregate, 'wait_ms', 'sample_count'),
      'performance namespace wait sample conservation')
    equalBigint(namespaceWaitTotal, histogramDecimal(namespaceAggregate, 'wait_ms', 'total_ms'),
      'performance namespace wait duration conservation')
    equalBigint(namespaceRunSamples, histogramDecimal(namespaceAggregate, 'run_ms', 'sample_count'),
      'performance namespace run sample conservation')
    equalBigint(namespaceRunTotal, histogramDecimal(namespaceAggregate, 'run_ms', 'total_ms'),
      'performance namespace run duration conservation')
  }
}

function validateQueueLane(laneSummary: UnknownRecord, lane: 'writer' | 'namespace'): void {
  exactKeys(laneSummary, ['wait_ms', 'run_ms'], [], `performance ${lane} queue`)
  const wait = recordValue(laneSummary.wait_ms, `performance ${lane} wait histogram`)
  const run = recordValue(laneSummary.run_ms, `performance ${lane} run histogram`)
  validatePerformanceHistogram(wait, `performance ${lane} wait`)
  validatePerformanceHistogram(run, `performance ${lane} run`)
  equalDecimal(wait.sample_count, run.sample_count, `performance ${lane} queue sample count`)
}

function validatePerformanceFilePipeline(payload: UnknownRecord): void {
  const filePipeline = recordValue(payload.file_pipeline, 'performance file pipeline')
  exactKeys(filePipeline, ['worker_starts', 'worker_stops', 'worker_ms', 'stages'], [],
    'performance file pipeline')
  decimalFields(filePipeline, ['worker_starts', 'worker_stops', 'worker_ms'],
    'performance file pipeline')
  const pipelineStages = recordValue(filePipeline.stages, 'performance file-pipeline stages')
  exactKeys(pipelineStages, PERFORMANCE_FILE_PIPELINE_STAGES_V1, [],
    'performance file-pipeline stages')
  let pipelineActive = 0n
  let pipelineOccupancy = 0n
  for (const stage of PERFORMANCE_FILE_PIPELINE_STAGES_V1) {
    const stageSummary = recordValue(pipelineStages[stage], `performance pipeline ${stage}`)
    exactKeys(stageSummary, [
      'entries', 'occupancy_ms', 'peak_active', 'active_at_completion',
    ], [], `performance pipeline ${stage}`)
    decimalFields(stageSummary, ['entries', 'occupancy_ms'], `performance pipeline ${stage}`)
    const peak = uint32(stageSummary.peak_active, `performance pipeline ${stage} peak active`)
    const active = uint32(
      stageSummary.active_at_completion,
      `performance pipeline ${stage} active at completion`,
    )
    if (active > peak) throw new TypeError(`performance pipeline ${stage} active exceeds its peak`)
    pipelineActive += BigInt(active)
    pipelineOccupancy += BigInt(stageSummary.occupancy_ms as string)
  }
  if (!payload.counter_overflowed) {
    if (BigInt(filePipeline.worker_starts as string) !==
        BigInt(filePipeline.worker_stops as string) + pipelineActive) {
      throw new TypeError('performance file-pipeline worker lifecycle is not conserved')
    }
    if (BigInt(filePipeline.worker_ms as string) !== pipelineOccupancy) {
      throw new TypeError('performance file-pipeline duration is not conserved')
    }
  }
}

function validatePerformanceStorage(payload: UnknownRecord): void {
  const peaks = recordValue(payload.peaks, 'performance peaks')
  exactKeys(peaks, [
    'active_writers', 'queued_writers', 'active_namespace', 'queued_namespace',
  ], [], 'performance peaks')
  for (const key of Object.keys(peaks)) uint32(peaks[key], `performance peak ${key}`)

  const authorityCache = recordValue(payload.authority_cache, 'performance authority cache')
  const authorityCounterKeys = [
    'directory_hits',
    'directory_misses',
    'file_hits',
    'file_misses',
    'invalidations',
    'evictions',
    'walked_segments',
  ] as const
  exactKeys(authorityCache, [...authorityCounterKeys, 'maximum_walk_depth'], [],
    'performance authority cache')
  decimalFields(authorityCache, authorityCounterKeys, 'performance authority cache')
  uint32(authorityCache.maximum_walk_depth, 'performance authority maximum walk depth')

  const bytes = recordValue(payload.bytes, 'performance bytes')
  exactKeys(bytes, ['pending', 'durable', 'final'], [], 'performance bytes')
  decimalFields(bytes, ['pending', 'durable', 'final'], 'performance bytes')

  const checkpoints = recordValue(payload.checkpoints, 'performance checkpoints')
  const checkpointCounterKeys = [
    'automatic_triggers',
    'forced_pause_triggers',
    'advanced',
    'declined',
    'constant_cost',
    'prefix_copy_cost',
    'space_preflight_cost',
    'estimated_copy_bytes',
  ] as const
  exactKeys(checkpoints, [...checkpointCounterKeys, 'elapsed_ms'], [],
    'performance checkpoints')
  decimalFields(checkpoints, checkpointCounterKeys, 'performance checkpoints')
  validatePerformanceHistogram(
    recordValue(checkpoints.elapsed_ms, 'performance checkpoint elapsed histogram'),
    'performance checkpoint elapsed',
  )
  if (
    BigInt(checkpoints.automatic_triggers as string) +
      BigInt(checkpoints.forced_pause_triggers as string) !==
    BigInt(checkpoints.advanced as string) + BigInt(checkpoints.declined as string)
  ) {
    throw new TypeError('performance checkpoint triggers contradict their decisions')
  }

  const finalTransactions = recordValue(
    payload.final_transactions,
    'performance final transactions',
  )
  exactKeys(finalTransactions, ['count', 'elapsed_ms'], [], 'performance final transactions')
  decimalUint64(finalTransactions.count, 'performance final transaction count')
  const finalHistogram = recordValue(
    finalTransactions.elapsed_ms,
    'performance final transaction histogram',
  )
  validatePerformanceHistogram(finalHistogram, 'performance final transaction elapsed')
  equalDecimal(
    finalTransactions.count,
    finalHistogram.sample_count,
    'performance final transaction count',
  )

  const ledger = recordValue(payload.ledger, 'performance ledger')
  exactKeys(ledger, [
    'entries', 'pages', 'seals', 'recovery_scan_fallbacks', 'seal_elapsed_ms',
  ], [], 'performance ledger')
  decimalFields(ledger, ['entries', 'pages', 'seals', 'recovery_scan_fallbacks'],
    'performance ledger')
  const sealHistogram = recordValue(
    ledger.seal_elapsed_ms,
    'performance ledger seal histogram',
  )
  validatePerformanceHistogram(sealHistogram, 'performance ledger seal elapsed')
  equalDecimal(ledger.seals, sealHistogram.sample_count, 'performance ledger seal count')
}

function validateRevisionOpens(revisionOpens: UnknownRecord): void {
  exactKeys(revisionOpens, [
    'attempts',
    'count',
    'wait_ms',
    'run_ms',
    'overlap_ms',
    'maximum_active',
    'active_at_completion',
  ], [], 'performance revision opens')
  decimalUint64(revisionOpens.attempts, 'performance revision-open attempts')
  decimalUint64(revisionOpens.count, 'performance revision-open count')
  decimalUint64(revisionOpens.overlap_ms, 'performance revision-open overlap')
  const revisionWait = recordValue(revisionOpens.wait_ms, 'performance revision-open wait')
  const revisionRun = recordValue(revisionOpens.run_ms, 'performance revision-open run')
  validatePerformanceHistogram(revisionWait, 'performance revision-open wait')
  validatePerformanceHistogram(revisionRun, 'performance revision-open run')
  equalDecimal(
    revisionOpens.count,
    revisionWait.sample_count,
    'performance revision-open wait count',
  )
  equalDecimal(revisionOpens.count, revisionRun.sample_count, 'performance revision-open run count')
  if (BigInt(revisionOpens.count as string) > BigInt(revisionOpens.attempts as string)) {
    throw new TypeError('performance successful revision opens exceed attempts')
  }
  const revisionMaximumActive = uint32(
    revisionOpens.maximum_active,
    'performance maximum active revision opens',
  )
  const revisionActive = uint32(
    revisionOpens.active_at_completion,
    'performance active revision opens at completion',
  )
  if (revisionActive > revisionMaximumActive) {
    throw new TypeError('performance active revision opens exceed their peak')
  }
}

function validateClaimBatches(claimBatches: UnknownRecord, counterOverflowed: boolean): void {
  exactKeys(claimBatches, [
    'count', 'members', 'maximum_size', 'oldest_wait_ms', 'newest_wait_ms', 'run_ms', 'phases',
    'inspector',
  ], [], 'performance claim batches')
  decimalFields(claimBatches, ['count', 'members'], 'performance claim batches')
  const maximumClaimSize = uint32(claimBatches.maximum_size, 'performance maximum claim batch size')
  for (const [key, label] of [
    ['oldest_wait_ms', 'oldest-member wait'],
    ['newest_wait_ms', 'newest-member wait'],
    ['run_ms', 'run'],
  ] as const) {
    const histogram = recordValue(claimBatches[key], `performance claim batch ${label}`)
    validatePerformanceHistogram(histogram, `performance claim batch ${label}`)
    equalDecimal(claimBatches.count, histogram.sample_count, `performance claim batch ${label} count`)
  }
  const claimCount = BigInt(claimBatches.count as string)
  const claimMembers = BigInt(claimBatches.members as string)
  if ((claimCount === 0n && (claimMembers !== 0n || maximumClaimSize !== 0)) ||
      (claimCount > 0n && (claimMembers < claimCount || maximumClaimSize === 0 ||
        BigInt(maximumClaimSize) > claimMembers))) {
    throw new TypeError('performance claim batch sizes contradict their counts')
  }
  validateClaimPhases(
    recordValue(claimBatches.phases, 'performance claim phases'),
    claimCount,
    claimMembers,
    histogramDecimal(claimBatches, 'run_ms', 'total_ms'),
    counterOverflowed,
  )
  validateClaimInspector(
    recordValue(claimBatches.inspector, 'performance claim inspector'),
    counterOverflowed,
  )
}

function validateClaimPhases(
  phases: UnknownRecord,
  claimCount: bigint,
  claimMembers: bigint,
  claimRunMilliseconds: bigint,
  counterOverflowed: boolean,
): void {
  exactKeys(phases, PERFORMANCE_CLAIM_PHASES_V1, [], 'performance claim phases')
  let conservedMilliseconds = 0n
  const members = new Map<string, bigint>()
  for (const phase of PERFORMANCE_CLAIM_PHASES_V1) {
    const summary = recordValue(phases[phase], `performance claim ${phase}`)
    exactKeys(summary, [
      'batch_count', 'member_count', 'queue_ms', 'run_ms', 'active_ms', 'overlap_ms',
      'maximum_active', 'active_at_completion',
    ], [], `performance claim ${phase}`)
    decimalFields(summary, ['batch_count', 'member_count', 'active_ms', 'overlap_ms'],
      `performance claim ${phase}`)
    const queue = recordValue(summary.queue_ms, `performance claim ${phase} queue`)
    const run = recordValue(summary.run_ms, `performance claim ${phase} run`)
    validatePerformanceHistogram(queue, `performance claim ${phase} queue`)
    validatePerformanceHistogram(run, `performance claim ${phase} run`)
    equalDecimal(summary.batch_count, queue.sample_count, `performance claim ${phase} queue count`)
    equalDecimal(summary.batch_count, run.sample_count, `performance claim ${phase} run count`)
    const maximumActive = uint32(summary.maximum_active, `performance claim ${phase} maximum active`)
    const activeAtCompletion = uint32(
      summary.active_at_completion,
      `performance claim ${phase} active at completion`,
    )
    if (activeAtCompletion > maximumActive || activeAtCompletion !== 0) {
      throw new TypeError(`performance claim ${phase} active work did not close`)
    }
    const runMilliseconds = BigInt(run.total_ms as string)
    const activeMilliseconds = BigInt(summary.active_ms as string)
    const overlapMilliseconds = BigInt(summary.overlap_ms as string)
    if (!counterOverflowed && (BigInt(summary.batch_count as string) !== claimCount ||
        overlapMilliseconds > runMilliseconds ||
        activeMilliseconds > BigInt(maximumActive) * runMilliseconds)) {
      throw new TypeError(`performance claim ${phase} integrals contradict its run`)
    }
    conservedMilliseconds += BigInt(queue.total_ms as string) + runMilliseconds
    members.set(phase, BigInt(summary.member_count as string))
  }
  if (!counterOverflowed && conservedMilliseconds !== claimRunMilliseconds) {
    throw new TypeError('performance claim phase durations do not conserve claim-batch run time')
  }
  if (!counterOverflowed && (members.get('classification')! > claimMembers ||
      members.get('inspection_union')! > members.get('classification')! ||
      members.get('reclassification')! + members.get('installation')! !==
        members.get('inspection_union')!)) {
    throw new TypeError('performance claim phase members contradict the batch membership')
  }
}

function validateClaimInspector(inspector: UnknownRecord, counterOverflowed: boolean): void {
  exactKeys(inspector, [
    'drains', 'wall_ms', 'maximum_width', 'at_capacity_ms', 'active_ms',
    'queued_member_ms', 'pending_member_ms', 'context_ms', 'maximum', 'at_completion',
    'under_capacity',
  ], [], 'performance claim inspector')
  decimalFields(inspector, [
    'drains', 'wall_ms', 'at_capacity_ms', 'active_ms', 'queued_member_ms',
    'pending_member_ms',
  ], 'performance claim inspector')
  const drains = BigInt(inspector.drains as string)
  const wall = BigInt(inspector.wall_ms as string)
  const atCapacity = BigInt(inspector.at_capacity_ms as string)
  const active = BigInt(inspector.active_ms as string)
  const queued = BigInt(inspector.queued_member_ms as string)
  const pending = BigInt(inspector.pending_member_ms as string)
  const maximumWidth = uint32(inspector.maximum_width, 'performance claim inspector maximum width')

  const context = recordValue(inspector.context_ms, 'performance claim inspector context integrals')
  exactKeys(context, ['resident', 'unfinished_inspection', 'ordered_settlement'], [],
    'performance claim inspector context integrals')
  decimalFields(context, ['resident', 'unfinished_inspection', 'ordered_settlement'],
    'performance claim inspector context integrals')

  const maximum = recordValue(inspector.maximum, 'performance claim inspector maxima')
  const completion = recordValue(inspector.at_completion, 'performance claim inspector completion')
  const stateFields = [
    'active', 'queued_members', 'pending_members', 'resident_contexts',
    'unfinished_inspection_contexts', 'ordered_settlement_contexts',
  ] as const
  exactKeys(maximum, stateFields, [], 'performance claim inspector maxima')
  exactKeys(completion, stateFields, [], 'performance claim inspector completion')
  const maxima = new Map<string, number>()
  for (const field of stateFields) {
    maxima.set(field, uint32(maximum[field], `performance claim inspector maximum ${field}`))
    const closed = uint32(completion[field], `performance claim inspector ${field} at completion`)
    if (closed !== 0 || closed > maxima.get(field)!) {
      throw new TypeError('performance claim inspector state did not close')
    }
  }
  if (maxima.get('active')! > maximumWidth) {
    throw new TypeError('performance claim inspector active peak exceeds its width')
  }

  const underCapacity = recordValue(
    inspector.under_capacity,
    'performance claim inspector under-capacity reasons',
  )
  exactKeys(underCapacity, PERFORMANCE_CLAIM_INSPECTOR_REASONS_V1, [],
    'performance claim inspector under-capacity reasons')
  let reasonWall = 0n
  let idleSlots = 0n
  for (const reason of PERFORMANCE_CLAIM_INSPECTOR_REASONS_V1) {
    const summary = recordValue(
      underCapacity[reason],
      `performance claim inspector ${reason}`,
    )
    exactKeys(summary, ['wall_ms', 'idle_slot_ms'], [],
      `performance claim inspector ${reason}`)
    decimalFields(summary, ['wall_ms', 'idle_slot_ms'],
      `performance claim inspector ${reason}`)
    const wallMilliseconds = BigInt(summary.wall_ms as string)
    const idleSlotMilliseconds = BigInt(summary.idle_slot_ms as string)
    if (!counterOverflowed && (idleSlotMilliseconds < wallMilliseconds ||
        idleSlotMilliseconds > BigInt(maximumWidth) * wallMilliseconds)) {
      throw new TypeError('performance claim inspector reason has impossible idle-slot time')
    }
    reasonWall += wallMilliseconds
    idleSlots += idleSlotMilliseconds
  }
  const resident = BigInt(context.resident as string)
  const unfinished = BigInt(context.unfinished_inspection as string)
  const ordered = BigInt(context.ordered_settlement as string)
  if (!counterOverflowed && (reasonWall + atCapacity !== wall ||
      active + idleSlots !== BigInt(maximumWidth) * wall ||
      unfinished + ordered > resident)) {
    throw new TypeError('performance claim inspector integrals do not conserve drain time')
  }
  const maximumTotal = [...maxima.values()].reduce((total, value) => total + value, 0)
  if ((drains === 0n && (maximumWidth !== 0 || wall !== 0n || active !== 0n ||
      queued !== 0n || pending !== 0n || resident !== 0n || unfinished !== 0n ||
      ordered !== 0n || maximumTotal !== 0)) ||
      (drains > 0n && maximumWidth === 0)) {
    throw new TypeError('performance claim inspector drain count contradicts its width')
  }
}

function validateOutputResources(outputResources: UnknownRecord): void {
  exactKeys(outputResources, ['active_files', 'write_bytes', 'buffered_bytes'], [],
    'performance output resources')
  for (const resourceName of ['active_files', 'write_bytes', 'buffered_bytes'] as const) {
    const resource = recordValue(outputResources[resourceName], `performance ${resourceName}`)
    exactKeys(resource, ['wait_ms', 'peak'], [], `performance ${resourceName}`)
    validatePerformanceHistogram(
      recordValue(resource.wait_ms, `performance ${resourceName} wait`),
      `performance ${resourceName} wait`,
    )
    if (resourceName === 'active_files') uint32(resource.peak, 'performance peak active files')
    else decimalUint64(resource.peak, `performance peak ${resourceName}`)
  }
}

function validatePerformanceCorrelation(correlation: UnknownRecord): void {
  exactKeys(correlation, [
    'receive_operation_id',
    'transfer_job_id',
    'output_session_id',
  ], ['protocol_session_id', 'protocol_generation'], 'performance correlation')
  canonicalIdentity(correlation.receive_operation_id, 'performance receive operation ID')
  canonicalIdentity(correlation.transfer_job_id, 'performance transfer job ID')
  canonicalIdentity(correlation.output_session_id, 'performance output session ID')
  const hasProtocolSession = correlation.protocol_session_id !== undefined
  const hasProtocolGeneration = correlation.protocol_generation !== undefined
  if (hasProtocolSession !== hasProtocolGeneration) {
    throw new TypeError('performance protocol correlation must be complete')
  }
  if (hasProtocolSession) {
    canonicalIdentity(correlation.protocol_session_id, 'performance protocol session ID')
    if (uint32(correlation.protocol_generation, 'performance protocol generation') === 0) {
      throw new RangeError('performance protocol generation must be positive')
    }
  }
}

function validatePerformanceHistogram(histogram: UnknownRecord, field: string): void {
  exactKeys(histogram, [
    'upper_bounds_ms',
    'bucket_counts',
    'sample_count',
    'total_ms',
    'maximum_ms',
  ], [], `${field} histogram`)
  if (
    !Array.isArray(histogram.upper_bounds_ms) ||
    histogram.upper_bounds_ms.length !== PERFORMANCE_HISTOGRAM_UPPER_BOUNDS_MS.length ||
    !histogram.upper_bounds_ms.every(
      (bound, index) => bound === PERFORMANCE_HISTOGRAM_UPPER_BOUNDS_MS[index],
    )
  ) {
    throw new TypeError(`${field} histogram bounds contradict the frozen schema`)
  }
  if (
    !Array.isArray(histogram.bucket_counts) ||
    histogram.bucket_counts.length !== PERFORMANCE_HISTOGRAM_UPPER_BOUNDS_MS.length + 1
  ) {
    throw new RangeError(`${field} histogram bucket count contradicts its bounds`)
  }
  let countedSamples = 0n
  for (const [index, count] of histogram.bucket_counts.entries()) {
    countedSamples += BigInt(decimalUint64(count, `${field} histogram bucket ${index}`))
  }
  const sampleCount = BigInt(decimalUint64(
    histogram.sample_count,
    `${field} histogram sample count`,
  ))
  const total = BigInt(decimalUint64(histogram.total_ms, `${field} histogram total`))
  const maximum = BigInt(decimalUint64(histogram.maximum_ms, `${field} histogram maximum`))
  if (countedSamples !== sampleCount) {
    throw new TypeError(`${field} histogram buckets contradict the sample count`)
  }
  if (
    (sampleCount === 0n && (total !== 0n || maximum !== 0n)) ||
    (sampleCount > 0n && total < maximum)
  ) {
    throw new TypeError(`${field} histogram totals contradict its samples`)
  }
}

function validatePerformanceMilestones(milestones: UnknownRecord): void {
  exactKeys(milestones, PERFORMANCE_MILESTONES_V1, [], 'performance milestones')
  let previous: bigint | undefined
  for (const milestone of PERFORMANCE_MILESTONES_V1) {
    const value = milestones[milestone]
    if (value === null) continue
    const elapsed = BigInt(decimalUint64(value, `performance milestone ${milestone}`))
    if (previous !== undefined && elapsed < previous) {
      throw new TypeError('performance milestones must be monotonic')
    }
    previous = elapsed
  }
}

function equalDecimal(left: unknown, right: unknown, field: string): void {
  if (left !== right) throw new TypeError(`${field} contradicts its histogram`)
}

function histogramDecimal(
  summary: UnknownRecord,
  histogramKey: string,
  field: 'sample_count' | 'total_ms',
): bigint {
  const histogram = recordValue(summary[histogramKey], `performance ${histogramKey}`)
  return BigInt(histogram[field] as string)
}

function equalBigint(left: bigint, right: bigint, field: string): void {
  if (left !== right) throw new TypeError(`${field} is contradictory`)
}

function canonicalIdentity(value: unknown, field: string): void {
  if (typeof value !== 'string' || !CANONICAL_IDENTITY.test(value) || value === ZERO_IDENTITY) {
    throw new TypeError(`${field} must be a canonical non-zero 16-byte base64url identity`)
  }
}

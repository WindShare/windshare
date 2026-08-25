import {
  PERFORMANCE_CLAIM_INSPECTOR_REASONS_V1,
  PERFORMANCE_CLAIM_PHASES_V1,
  PERFORMANCE_FILE_PIPELINE_STAGES_V1,
  PERFORMANCE_HISTOGRAM_UPPER_BOUNDS_MS,
  PERFORMANCE_MILESTONES_V1,
  PERFORMANCE_NAMESPACE_KINDS_V1,
  type PerformanceCorrelationPayloadV1,
  type PerformanceCorrelationProjectionInput,
  type PerformanceHistogramPayloadV1,
  type PerformanceHistogramSnapshot,
  type PerformanceMilestonePayloadV1,
  type PerformanceSummaryPayloadV1,
  type PerformanceSummaryProjectionInput,
} from '../trace/transfer-payload'
import { decimalUint64, deepFreezeJson, uint32 } from './json'

export function projectPerformanceCorrelationV1(
  input: PerformanceCorrelationProjectionInput,
): PerformanceCorrelationPayloadV1 {
  const protocolSessionId = input.protocolSessionId === undefined
    ? undefined
    : performanceIdentity(input.protocolSessionId, 'protocol session')
  if ((protocolSessionId === undefined) !== (input.protocolGeneration === undefined)) {
    throw new TypeError('performance protocol session and generation must be captured together')
  }
  const protocolGeneration = input.protocolGeneration === undefined
    ? undefined
    : uint32(input.protocolGeneration, 'performance protocol generation')
  if (protocolGeneration === 0) {
    throw new RangeError('performance protocol generation must be positive')
  }
  const correlation = {
    receive_operation_id: performanceIdentity(
      input.receiveOperationId,
      'receive operation',
    ),
    transfer_job_id: performanceIdentity(input.transferJobId, 'transfer job'),
    output_session_id: performanceIdentity(input.outputSessionId, 'output session'),
  }
  if (protocolSessionId === undefined) return Object.freeze(correlation)
  if (protocolGeneration === undefined) {
    throw new TypeError('performance protocol correlation lost its generation')
  }
  return Object.freeze({
    ...correlation,
    protocol_session_id: protocolSessionId,
    protocol_generation: protocolGeneration,
  })
}

export function projectPerformanceHistogramV1(
  input: PerformanceHistogramSnapshot,
  field: string,
): PerformanceHistogramPayloadV1 {
  if (
    input.upperBoundsMilliseconds.length !== PERFORMANCE_HISTOGRAM_UPPER_BOUNDS_MS.length ||
    !input.upperBoundsMilliseconds.every(
      (bound, index) => bound === PERFORMANCE_HISTOGRAM_UPPER_BOUNDS_MS[index],
    )
  ) {
    throw new TypeError(`${field} histogram bounds contradict the frozen schema`)
  }
  if (input.bucketCounts.length !== PERFORMANCE_HISTOGRAM_UPPER_BOUNDS_MS.length + 1) {
    throw new RangeError(`${field} histogram bucket count contradicts its bounds`)
  }
  const projectedCounts = input.bucketCounts.map((count, index) =>
    decimalUint64(count, `${field} histogram bucket ${index}`))
  const countedSamples = input.bucketCounts.reduce((sum, count) => sum + count, 0n)
  if (countedSamples !== input.sampleCount) {
    throw new TypeError(`${field} histogram buckets contradict the sample count`)
  }
  if (
    (input.sampleCount === 0n &&
      (input.totalMilliseconds !== 0n || input.maximumMilliseconds !== 0n)) ||
    (input.sampleCount > 0n && input.totalMilliseconds < input.maximumMilliseconds)
  ) {
    throw new TypeError(`${field} histogram totals contradict its samples`)
  }
  return Object.freeze({
    upper_bounds_ms: PERFORMANCE_HISTOGRAM_UPPER_BOUNDS_MS,
    bucket_counts: Object.freeze(projectedCounts),
    sample_count: decimalUint64(input.sampleCount, `${field} histogram samples`),
    total_ms: decimalUint64(input.totalMilliseconds, `${field} histogram total`),
    maximum_ms: decimalUint64(input.maximumMilliseconds, `${field} histogram maximum`),
  })
}

export function projectPerformanceMilestonesV1(
  milestones: PerformanceSummaryProjectionInput['milestones'],
): PerformanceMilestonePayloadV1 {
  let previous: bigint | undefined
  const projected = {} as Record<(typeof PERFORMANCE_MILESTONES_V1)[number], string | null>
  for (const milestone of PERFORMANCE_MILESTONES_V1) {
    const value = milestones[milestone]
    if (value !== undefined && previous !== undefined && value < previous) {
      throw new TypeError('performance milestones must be monotonic')
    }
    projected[milestone] = value === undefined
      ? null
      : decimalUint64(value, `performance milestone ${milestone}`)
    if (value !== undefined) previous = value
  }
  return Object.freeze(projected)
}

export function projectPerformanceSummaryPayloadV1(
  input: PerformanceSummaryProjectionInput,
): PerformanceSummaryPayloadV1 {
  if (typeof input.counterOverflowed !== 'boolean') {
    throw new TypeError('performance counter overflow marker must be boolean')
  }
  return deepFreezeJson({
    correlation: projectPerformanceCorrelationV1(input.correlation),
    queue: projectPerformanceQueues(input),
    namespace_by_kind: projectPerformanceNamespaceKinds(input),
    file_pipeline: projectPerformanceFilePipeline(input),
    peaks: {
      active_writers: uint32(input.peaks.activeWriters, 'peak active writers'),
      queued_writers: uint32(input.peaks.queuedWriters, 'peak queued writers'),
      active_namespace: uint32(input.peaks.activeNamespace, 'peak active namespace work'),
      queued_namespace: uint32(input.peaks.queuedNamespace, 'peak queued namespace work'),
    },
    authority_cache: projectPerformanceAuthorityCache(input),
    bytes: {
      pending: decimalUint64(input.bytes.pending, 'pending bytes'),
      durable: decimalUint64(input.bytes.durable, 'durable bytes'),
      final: decimalUint64(input.bytes.final, 'final bytes'),
    },
    checkpoints: projectPerformanceCheckpoints(input),
    final_transactions: {
      count: decimalUint64(input.finalTransactions.count, 'final transactions'),
      elapsed_ms: projectPerformanceHistogramV1(
        input.finalTransactions.elapsed,
        'final transaction elapsed',
      ),
    },
    ledger: {
      entries: decimalUint64(input.ledger.entries, 'ledger entries'),
      pages: decimalUint64(input.ledger.pages, 'ledger pages'),
      seals: decimalUint64(input.ledger.seals, 'ledger seals'),
      recovery_scan_fallbacks: decimalUint64(
        input.ledger.recoveryScanFallbacks,
        'ledger recovery scan fallbacks',
      ),
      seal_elapsed_ms: projectPerformanceHistogramV1(
        input.ledger.sealElapsed,
        'ledger seal elapsed',
      ),
    },
    revision_opens: projectPerformanceRevisionOpens(input),
    claim_batches: projectPerformanceClaimBatches(input),
    output_resources: projectPerformanceOutputResources(input),
    milestones: projectPerformanceMilestonesV1(input.milestones),
    counter_overflowed: input.counterOverflowed,
  })
}

function projectPerformanceQueues(
  input: PerformanceSummaryProjectionInput,
): PerformanceSummaryPayloadV1['queue'] {
  return {
    writer: {
      wait_ms: projectPerformanceHistogramV1(input.queue.writer.wait, 'writer queue wait'),
      run_ms: projectPerformanceHistogramV1(input.queue.writer.run, 'writer queue run'),
    },
    namespace: {
      wait_ms: projectPerformanceHistogramV1(input.queue.namespace.wait, 'namespace queue wait'),
      run_ms: projectPerformanceHistogramV1(input.queue.namespace.run, 'namespace queue run'),
    },
  }
}

function projectPerformanceNamespaceKinds(
  input: PerformanceSummaryProjectionInput,
): PerformanceSummaryPayloadV1['namespace_by_kind'] {
  return Object.fromEntries(PERFORMANCE_NAMESPACE_KINDS_V1.map(kind => [
    kind,
    {
      wait_ms: projectPerformanceHistogramV1(
        input.namespaceByKind[kind].wait,
        `namespace ${kind} wait`,
      ),
      run_ms: projectPerformanceHistogramV1(
        input.namespaceByKind[kind].run,
        `namespace ${kind} run`,
      ),
    },
  ])) as PerformanceSummaryPayloadV1['namespace_by_kind']
}

function projectPerformanceFilePipeline(
  input: PerformanceSummaryProjectionInput,
): PerformanceSummaryPayloadV1['file_pipeline'] {
  const stages = Object.fromEntries(PERFORMANCE_FILE_PIPELINE_STAGES_V1.map(stage => [
    stage,
    {
      entries: decimalUint64(
        input.filePipeline.stages[stage].entries,
        `file-pipeline ${stage} entries`,
      ),
      occupancy_ms: decimalUint64(
        input.filePipeline.stages[stage].occupancyMilliseconds,
        `file-pipeline ${stage} occupancy milliseconds`,
      ),
      peak_active: uint32(
        input.filePipeline.stages[stage].peakActive,
        `file-pipeline ${stage} peak active`,
      ),
      active_at_completion: uint32(
        input.filePipeline.stages[stage].activeAtCompletion,
        `file-pipeline ${stage} active at completion`,
      ),
    },
  ])) as PerformanceSummaryPayloadV1['file_pipeline']['stages']
  return {
    worker_starts: decimalUint64(input.filePipeline.workerStarts, 'file-pipeline worker starts'),
    worker_stops: decimalUint64(input.filePipeline.workerStops, 'file-pipeline worker stops'),
    worker_ms: decimalUint64(
      input.filePipeline.workerMilliseconds,
      'file-pipeline worker milliseconds',
    ),
    stages,
  }
}

function projectPerformanceAuthorityCache(
  input: PerformanceSummaryProjectionInput,
): PerformanceSummaryPayloadV1['authority_cache'] {
  return {
    directory_hits: decimalUint64(input.authorityCache.directoryHits, 'directory cache hits'),
    directory_misses: decimalUint64(input.authorityCache.directoryMisses, 'directory cache misses'),
    file_hits: decimalUint64(input.authorityCache.fileHits, 'file cache hits'),
    file_misses: decimalUint64(input.authorityCache.fileMisses, 'file cache misses'),
    invalidations: decimalUint64(input.authorityCache.invalidations, 'authority cache invalidations'),
    evictions: decimalUint64(input.authorityCache.evictions, 'authority cache evictions'),
    walked_segments: decimalUint64(
      input.authorityCache.walkedSegments,
      'authority cache walked segments',
    ),
    maximum_walk_depth: uint32(
      input.authorityCache.maximumWalkDepth,
      'authority cache maximum walk depth',
    ),
  }
}

function projectPerformanceCheckpoints(
  input: PerformanceSummaryProjectionInput,
): PerformanceSummaryPayloadV1['checkpoints'] {
  return {
    automatic_triggers: decimalUint64(
      input.checkpoints.automaticTriggers,
      'automatic checkpoint triggers',
    ),
    forced_pause_triggers: decimalUint64(
      input.checkpoints.forcedPauseTriggers,
      'forced-pause checkpoint triggers',
    ),
    advanced: decimalUint64(input.checkpoints.advanced, 'advanced checkpoints'),
    declined: decimalUint64(input.checkpoints.declined, 'declined checkpoints'),
    constant_cost: decimalUint64(input.checkpoints.constantCost, 'constant-cost checkpoints'),
    prefix_copy_cost: decimalUint64(input.checkpoints.prefixCopyCost, 'prefix-copy checkpoints'),
    space_preflight_cost: decimalUint64(
      input.checkpoints.spacePreflightCost,
      'space-preflight checkpoints',
    ),
    estimated_copy_bytes: decimalUint64(
      input.checkpoints.estimatedCopyBytes,
      'checkpoint estimated copy bytes',
    ),
    elapsed_ms: projectPerformanceHistogramV1(input.checkpoints.elapsed, 'checkpoint elapsed'),
  }
}

function projectPerformanceRevisionOpens(
  input: PerformanceSummaryProjectionInput,
): PerformanceSummaryPayloadV1['revision_opens'] {
  return {
    attempts: decimalUint64(input.revisionOpens.attempts, 'revision-open attempts'),
    count: decimalUint64(input.revisionOpens.count, 'revision opens'),
    wait_ms: projectPerformanceHistogramV1(input.revisionOpens.wait, 'revision-open wait'),
    run_ms: projectPerformanceHistogramV1(input.revisionOpens.run, 'revision-open run'),
    overlap_ms: decimalUint64(
      input.revisionOpens.overlapMilliseconds,
      'revision-open overlap milliseconds',
    ),
    maximum_active: uint32(input.revisionOpens.maximumActive, 'maximum active revision opens'),
    active_at_completion: uint32(
      input.revisionOpens.activeAtCompletion,
      'active revision opens at completion',
    ),
  }
}

function projectPerformanceClaimBatches(
  input: PerformanceSummaryProjectionInput,
): PerformanceSummaryPayloadV1['claim_batches'] {
  return {
    count: decimalUint64(input.claimBatches.count, 'claim batches'),
    members: decimalUint64(input.claimBatches.members, 'claim batch members'),
    maximum_size: uint32(input.claimBatches.maximumSize, 'maximum claim batch size'),
    oldest_wait_ms: projectPerformanceHistogramV1(
      input.claimBatches.oldestWait,
      'claim batch oldest-member wait',
    ),
    newest_wait_ms: projectPerformanceHistogramV1(
      input.claimBatches.newestWait,
      'claim batch newest-member wait',
    ),
    run_ms: projectPerformanceHistogramV1(input.claimBatches.run, 'claim batch run'),
    phases: Object.fromEntries(PERFORMANCE_CLAIM_PHASES_V1.map(phase => [phase, {
      batch_count: decimalUint64(
        input.claimBatches.phases[phase].batchCount,
        `claim ${phase} batch count`,
      ),
      member_count: decimalUint64(
        input.claimBatches.phases[phase].memberCount,
        `claim ${phase} member count`,
      ),
      queue_ms: projectPerformanceHistogramV1(
        input.claimBatches.phases[phase].queue,
        `claim ${phase} queue`,
      ),
      run_ms: projectPerformanceHistogramV1(
        input.claimBatches.phases[phase].run,
        `claim ${phase} run`,
      ),
      active_ms: decimalUint64(
        input.claimBatches.phases[phase].activeMilliseconds,
        `claim ${phase} active milliseconds`,
      ),
      overlap_ms: decimalUint64(
        input.claimBatches.phases[phase].overlapMilliseconds,
        `claim ${phase} overlap milliseconds`,
      ),
      maximum_active: uint32(
        input.claimBatches.phases[phase].maximumActive,
        `claim ${phase} maximum active`,
      ),
      active_at_completion: uint32(
        input.claimBatches.phases[phase].activeAtCompletion,
        `claim ${phase} active at completion`,
      ),
    }])) as PerformanceSummaryPayloadV1['claim_batches']['phases'],
    inspector: projectPerformanceClaimInspector(input),
  }
}

function projectPerformanceClaimInspector(
  input: PerformanceSummaryProjectionInput,
): PerformanceSummaryPayloadV1['claim_batches']['inspector'] {
  const inspector = input.claimBatches.inspector
  return {
      drains: decimalUint64(inspector.drains, 'claim inspector drains'),
      wall_ms: decimalUint64(inspector.wallMilliseconds, 'claim inspector wall'),
      maximum_width: uint32(
        inspector.maximumWidth,
        'claim inspector maximum width',
      ),
      at_capacity_ms: decimalUint64(
        inspector.atCapacityMilliseconds,
        'claim inspector at-capacity milliseconds',
      ),
      active_ms: decimalUint64(
        inspector.activeMilliseconds,
        'claim inspector active milliseconds',
      ),
      queued_member_ms: decimalUint64(
        inspector.queuedMemberMilliseconds,
        'claim inspector queued-member milliseconds',
      ),
      pending_member_ms: decimalUint64(
        inspector.pendingMemberMilliseconds,
        'claim inspector pending-member milliseconds',
      ),
      context_ms: {
        resident: decimalUint64(
          inspector.contextMilliseconds.resident,
          'claim inspector resident-context milliseconds',
        ),
        unfinished_inspection: decimalUint64(
          inspector.contextMilliseconds.unfinishedInspection,
          'claim inspector unfinished-inspection context milliseconds',
        ),
        ordered_settlement: decimalUint64(
          inspector.contextMilliseconds.orderedSettlement,
          'claim inspector ordered-settlement context milliseconds',
        ),
      },
      maximum: {
        active: uint32(inspector.maximum.active, 'claim inspector peak active'),
        queued_members: uint32(
          inspector.maximum.queuedMembers,
          'claim inspector peak queued members',
        ),
        pending_members: uint32(
          inspector.maximum.pendingMembers,
          'claim inspector peak pending members',
        ),
        resident_contexts: uint32(
          inspector.maximum.residentContexts,
          'claim inspector peak resident contexts',
        ),
        unfinished_inspection_contexts: uint32(
          inspector.maximum.unfinishedInspectionContexts,
          'claim inspector peak unfinished-inspection contexts',
        ),
        ordered_settlement_contexts: uint32(
          inspector.maximum.orderedSettlementContexts,
          'claim inspector peak ordered-settlement contexts',
        ),
      },
      at_completion: {
        active: uint32(
          inspector.atCompletion.active,
          'claim inspector active at completion',
        ),
        queued_members: uint32(
          inspector.atCompletion.queuedMembers,
          'claim inspector queued members at completion',
        ),
        pending_members: uint32(
          inspector.atCompletion.pendingMembers,
          'claim inspector pending members at completion',
        ),
        resident_contexts: uint32(
          inspector.atCompletion.residentContexts,
          'claim inspector resident contexts at completion',
        ),
        unfinished_inspection_contexts: uint32(
          inspector.atCompletion.unfinishedInspectionContexts,
          'claim inspector unfinished-inspection contexts at completion',
        ),
        ordered_settlement_contexts: uint32(
          inspector.atCompletion.orderedSettlementContexts,
          'claim inspector ordered-settlement contexts at completion',
        ),
      },
      under_capacity: Object.fromEntries(PERFORMANCE_CLAIM_INSPECTOR_REASONS_V1.map(reason => [
        reason,
        {
          wall_ms: decimalUint64(
            inspector.underCapacity[reason].wallMilliseconds,
            `claim inspector ${reason} wall milliseconds`,
          ),
          idle_slot_ms: decimalUint64(
            inspector.underCapacity[reason].idleSlotMilliseconds,
            `claim inspector ${reason} idle-slot milliseconds`,
          ),
        },
      ])) as PerformanceSummaryPayloadV1['claim_batches']['inspector']['under_capacity'],
    }
}

function projectPerformanceOutputResources(
  input: PerformanceSummaryProjectionInput,
): PerformanceSummaryPayloadV1['output_resources'] {
  return {
    active_files: {
      wait_ms: projectPerformanceHistogramV1(
        input.outputResources.activeFiles.wait,
        'output active-file wait',
      ),
      peak: uint32(input.outputResources.activeFiles.peak, 'peak active output files'),
    },
    write_bytes: {
      wait_ms: projectPerformanceHistogramV1(
        input.outputResources.writeBytes.wait,
        'output write-byte wait',
      ),
      peak: decimalUint64(input.outputResources.writeBytes.peak, 'peak output write bytes'),
    },
    buffered_bytes: {
      wait_ms: projectPerformanceHistogramV1(
        input.outputResources.bufferedBytes.wait,
        'output buffered-byte wait',
      ),
      peak: decimalUint64(input.outputResources.bufferedBytes.peak, 'peak buffered bytes'),
    },
  }
}

function performanceIdentity(value: string, field: string): string {
  if (
    typeof value !== 'string' ||
    !/^[A-Za-z0-9_-]{21}[AQgw]$/.test(value) ||
    value === 'AAAAAAAAAAAAAAAAAAAAAA'
  ) {
    throw new TypeError(`performance ${field} ID must be a canonical non-zero identity`)
  }
  return value
}

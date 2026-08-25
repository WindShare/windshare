export type CapacityWaitTransitionPayloadV1 = Readonly<{
  transition:
    | 'capacity_retry_scheduled'
    | 'capacity_retry_succeeded'
    | 'capacity_wait_budget_paused'
    | 'capacity_wait_cancelled'
    | 'capacity_generation_replaced'
  capacity_wait_id: string
  capacity_surface: 'revision_open' | 'block_range'
  receive_operation_id: string
  transfer_job_id: string
  protocol_session_id: string
  protocol_operation_id: string
  attempt: string
  sender_hint_ms: number
  jitter_ms: number
  delay_ms: number
  accumulated_wait_ms: number
  active_waiters: number
}>

export type TransferProgressPayloadV1 = Readonly<{
  discovered_files: string
  discovered_bytes: string
  written_bytes: string
  completed_files: string
  completed_bytes: string
  file_errors: string
  selection_errors: string
  failed_directories: string
  content_lanes: number
  capacity_waiting_files: string
  capacity_accumulated_wait_ms: string
  capacity_wait_attempts: string
  capacity_wait_visible: boolean
  discovery: 'open' | 'complete' | 'failed'
  partial: boolean
}>

export const PERFORMANCE_HISTOGRAM_UPPER_BOUNDS_MS = Object.freeze([
  1,
  4,
  16,
  64,
  256,
  1_024,
  4_096,
] as const)

export const PERFORMANCE_MILESTONES_V1 = Object.freeze([
  'authority_acquired',
  'first_content_request',
  'first_write',
  'last_byte',
  'last_final',
  'settlement_started',
  'published',
] as const)

export const PERFORMANCE_NAMESPACE_KINDS_V1 = Object.freeze([
  'reserve_name',
  'create_directory',
  'create_file',
  'settle_operation',
  'remove_entry',
  'rename_entry',
  'repair_compatible_name',
] as const)

export const PERFORMANCE_FILE_PIPELINE_STAGES_V1 = Object.freeze([
  'revision_open',
  'initial_lineage',
  'namespace_inspection',
  'namespace_creation',
  'block_read_authentication',
  'writer_lifecycle',
  'final_transaction',
  'idle_no_ready_file',
] as const)

export const PERFORMANCE_CLAIM_PHASES_V1 = Object.freeze([
  'classification',
  'inspection_union',
  'reclassification',
  'installation',
] as const)

export const PERFORMANCE_CLAIM_INSPECTOR_REASONS_V1 = Object.freeze([
  'no_pending_arrival',
  'batch_serialization',
  'ordered_settlement',
] as const)

export type PerformanceMilestoneV1 = (typeof PERFORMANCE_MILESTONES_V1)[number]
export type PerformanceNamespaceKindV1 = (typeof PERFORMANCE_NAMESPACE_KINDS_V1)[number]
export type PerformanceFilePipelineStageV1 =
  (typeof PERFORMANCE_FILE_PIPELINE_STAGES_V1)[number]
export type PerformanceClaimPhaseV1 = (typeof PERFORMANCE_CLAIM_PHASES_V1)[number]
export type PerformanceClaimInspectorReasonV1 =
  (typeof PERFORMANCE_CLAIM_INSPECTOR_REASONS_V1)[number]

export type PerformanceCorrelationProjectionInput = Readonly<{
  receiveOperationId: string
  transferJobId: string
  outputSessionId: string
  protocolSessionId?: string
  protocolGeneration?: number
}>

export type PerformanceCorrelationPayloadV1 = Readonly<{
  receive_operation_id: string
  transfer_job_id: string
  output_session_id: string
  protocol_session_id?: string
  protocol_generation?: number
}>

export type PerformanceHistogramSnapshot = Readonly<{
  upperBoundsMilliseconds: typeof PERFORMANCE_HISTOGRAM_UPPER_BOUNDS_MS
  bucketCounts: readonly bigint[]
  sampleCount: bigint
  totalMilliseconds: bigint
  maximumMilliseconds: bigint
}>

export type PerformanceHistogramPayloadV1 = Readonly<{
  upper_bounds_ms: readonly number[]
  bucket_counts: readonly string[]
  sample_count: string
  total_ms: string
  maximum_ms: string
}>

export type PerformanceMilestoneSnapshot = Readonly<
  Partial<Record<PerformanceMilestoneV1, bigint>>
>

export type PerformanceMilestonePayloadV1 = Readonly<
  Record<PerformanceMilestoneV1, string | null>
>

export type PerformancePhaseProjectionInput = Readonly<{
  correlation: PerformanceCorrelationProjectionInput
  milestone: PerformanceMilestoneV1
  observerElapsedMilliseconds: bigint
}>

export type PerformancePhasePayloadV1 = Readonly<{
  correlation: PerformanceCorrelationPayloadV1
  milestone: PerformanceMilestoneV1
  observer_elapsed_ms: string
}>

export type PerformanceSummaryProjectionInput = Readonly<{
  correlation: PerformanceCorrelationProjectionInput
  queue: Readonly<{
    writer: Readonly<{
      wait: PerformanceHistogramSnapshot
      run: PerformanceHistogramSnapshot
    }>
    namespace: Readonly<{
      wait: PerformanceHistogramSnapshot
      run: PerformanceHistogramSnapshot
    }>
  }>
  namespaceByKind: Readonly<Record<PerformanceNamespaceKindV1, Readonly<{
    wait: PerformanceHistogramSnapshot
    run: PerformanceHistogramSnapshot
  }>>>
  filePipeline: Readonly<{
    workerStarts: bigint
    workerStops: bigint
    workerMilliseconds: bigint
    stages: Readonly<Record<PerformanceFilePipelineStageV1, Readonly<{
      entries: bigint
      occupancyMilliseconds: bigint
      peakActive: number
      activeAtCompletion: number
    }>>>
  }>
  peaks: Readonly<{
    activeWriters: number
    queuedWriters: number
    activeNamespace: number
    queuedNamespace: number
  }>
  authorityCache: Readonly<{
    directoryHits: bigint
    directoryMisses: bigint
    fileHits: bigint
    fileMisses: bigint
    invalidations: bigint
    evictions: bigint
    walkedSegments: bigint
    maximumWalkDepth: number
  }>
  bytes: Readonly<{
    pending: bigint
    durable: bigint
    final: bigint
  }>
  checkpoints: Readonly<{
    automaticTriggers: bigint
    forcedPauseTriggers: bigint
    advanced: bigint
    declined: bigint
    constantCost: bigint
    prefixCopyCost: bigint
    spacePreflightCost: bigint
    estimatedCopyBytes: bigint
    elapsed: PerformanceHistogramSnapshot
  }>
  finalTransactions: Readonly<{
    count: bigint
    elapsed: PerformanceHistogramSnapshot
  }>
  ledger: Readonly<{
    entries: bigint
    pages: bigint
    seals: bigint
    recoveryScanFallbacks: bigint
    sealElapsed: PerformanceHistogramSnapshot
  }>
  revisionOpens: Readonly<{
    attempts: bigint
    count: bigint
    wait: PerformanceHistogramSnapshot
    run: PerformanceHistogramSnapshot
    overlapMilliseconds: bigint
    maximumActive: number
    activeAtCompletion: number
  }>
  claimBatches: Readonly<{
    count: bigint
    members: bigint
    maximumSize: number
    oldestWait: PerformanceHistogramSnapshot
    newestWait: PerformanceHistogramSnapshot
    run: PerformanceHistogramSnapshot
    phases: Readonly<Record<PerformanceClaimPhaseV1, Readonly<{
      batchCount: bigint
      memberCount: bigint
      queue: PerformanceHistogramSnapshot
      run: PerformanceHistogramSnapshot
      activeMilliseconds: bigint
      overlapMilliseconds: bigint
      maximumActive: number
      activeAtCompletion: number
    }>>>
    inspector: Readonly<{
      drains: bigint
      wallMilliseconds: bigint
      maximumWidth: number
      atCapacityMilliseconds: bigint
      activeMilliseconds: bigint
      queuedMemberMilliseconds: bigint
      pendingMemberMilliseconds: bigint
      contextMilliseconds: Readonly<{
        resident: bigint
        unfinishedInspection: bigint
        orderedSettlement: bigint
      }>
      maximum: Readonly<{
        active: number
        queuedMembers: number
        pendingMembers: number
        residentContexts: number
        unfinishedInspectionContexts: number
        orderedSettlementContexts: number
      }>
      atCompletion: Readonly<{
        active: number
        queuedMembers: number
        pendingMembers: number
        residentContexts: number
        unfinishedInspectionContexts: number
        orderedSettlementContexts: number
      }>
      underCapacity: Readonly<Record<PerformanceClaimInspectorReasonV1, Readonly<{
        wallMilliseconds: bigint
        idleSlotMilliseconds: bigint
      }>>>
    }>
  }>
  outputResources: Readonly<{
    activeFiles: Readonly<{
      wait: PerformanceHistogramSnapshot
      peak: number
    }>
    writeBytes: Readonly<{
      wait: PerformanceHistogramSnapshot
      peak: bigint
    }>
    bufferedBytes: Readonly<{
      wait: PerformanceHistogramSnapshot
      peak: bigint
    }>
  }>
  milestones: PerformanceMilestoneSnapshot
  counterOverflowed: boolean
}>

export type PerformanceSummaryPayloadV1 = Readonly<{
  correlation: PerformanceCorrelationPayloadV1
  queue: Readonly<{
    writer: Readonly<{
      wait_ms: PerformanceHistogramPayloadV1
      run_ms: PerformanceHistogramPayloadV1
    }>
    namespace: Readonly<{
      wait_ms: PerformanceHistogramPayloadV1
      run_ms: PerformanceHistogramPayloadV1
    }>
  }>
  namespace_by_kind: Readonly<Record<PerformanceNamespaceKindV1, Readonly<{
    wait_ms: PerformanceHistogramPayloadV1
    run_ms: PerformanceHistogramPayloadV1
  }>>>
  file_pipeline: Readonly<{
    worker_starts: string
    worker_stops: string
    worker_ms: string
    stages: Readonly<Record<PerformanceFilePipelineStageV1, Readonly<{
      entries: string
      occupancy_ms: string
      peak_active: number
      active_at_completion: number
    }>>>
  }>
  peaks: Readonly<{
    active_writers: number
    queued_writers: number
    active_namespace: number
    queued_namespace: number
  }>
  authority_cache: Readonly<{
    directory_hits: string
    directory_misses: string
    file_hits: string
    file_misses: string
    invalidations: string
    evictions: string
    walked_segments: string
    maximum_walk_depth: number
  }>
  bytes: Readonly<{
    pending: string
    durable: string
    final: string
  }>
  checkpoints: Readonly<{
    automatic_triggers: string
    forced_pause_triggers: string
    advanced: string
    declined: string
    constant_cost: string
    prefix_copy_cost: string
    space_preflight_cost: string
    estimated_copy_bytes: string
    elapsed_ms: PerformanceHistogramPayloadV1
  }>
  final_transactions: Readonly<{
    count: string
    elapsed_ms: PerformanceHistogramPayloadV1
  }>
  ledger: Readonly<{
    entries: string
    pages: string
    seals: string
    recovery_scan_fallbacks: string
    seal_elapsed_ms: PerformanceHistogramPayloadV1
  }>
  revision_opens: Readonly<{
    attempts: string
    count: string
    wait_ms: PerformanceHistogramPayloadV1
    run_ms: PerformanceHistogramPayloadV1
    overlap_ms: string
    maximum_active: number
    active_at_completion: number
  }>
  claim_batches: Readonly<{
    count: string
    members: string
    maximum_size: number
    oldest_wait_ms: PerformanceHistogramPayloadV1
    newest_wait_ms: PerformanceHistogramPayloadV1
    run_ms: PerformanceHistogramPayloadV1
    phases: Readonly<Record<PerformanceClaimPhaseV1, Readonly<{
      batch_count: string
      member_count: string
      queue_ms: PerformanceHistogramPayloadV1
      run_ms: PerformanceHistogramPayloadV1
      active_ms: string
      overlap_ms: string
      maximum_active: number
      active_at_completion: number
    }>>>
    inspector: Readonly<{
      drains: string
      wall_ms: string
      maximum_width: number
      at_capacity_ms: string
      active_ms: string
      queued_member_ms: string
      pending_member_ms: string
      context_ms: Readonly<{
        resident: string
        unfinished_inspection: string
        ordered_settlement: string
      }>
      maximum: Readonly<{
        active: number
        queued_members: number
        pending_members: number
        resident_contexts: number
        unfinished_inspection_contexts: number
        ordered_settlement_contexts: number
      }>
      at_completion: Readonly<{
        active: number
        queued_members: number
        pending_members: number
        resident_contexts: number
        unfinished_inspection_contexts: number
        ordered_settlement_contexts: number
      }>
      under_capacity: Readonly<Record<PerformanceClaimInspectorReasonV1, Readonly<{
        wall_ms: string
        idle_slot_ms: string
      }>>>
    }>
  }>
  output_resources: Readonly<{
    active_files: Readonly<{
      wait_ms: PerformanceHistogramPayloadV1
      peak: number
    }>
    write_bytes: Readonly<{
      wait_ms: PerformanceHistogramPayloadV1
      peak: string
    }>
    buffered_bytes: Readonly<{
      wait_ms: PerformanceHistogramPayloadV1
      peak: string
    }>
  }>
  milestones: PerformanceMilestonePayloadV1
  counter_overflowed: boolean
}>

import {
  checkpointMatchesNamespace,
  validateFileCheckpointPage,
  type CheckpointNamespaceBinding,
  type CheckpointLineageDecision,
  type CheckpointLineageLookupRequest,
  type InitialCheckpointCASResult,
  type FileCheckpointPage,
  type FileCheckpointScan,
} from '../persistence/journal'
import { FILE_CHECKPOINT_BATCH_REQUEST_LIMIT } from '../persistence/journal'
import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_COMMIT_QUARANTINED,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  deriveCheckpointLineageID,
  newFileCheckpointV2,
  validateFileCheckpoint,
  type FileCheckpointV2,
} from '../persistence/checkpoint'
import type {
  AutomaticCheckpointResult,
  OutputCheckpointCost,
  OutputCheckpointCostBudget,
} from '../../transfer/output-session'
import type {
  OpenedFileRevision,
  PersistentOutputTree,
  PersistentWriterPreflight,
  SemanticPersistentOutputJournal,
} from './contracts'
import {
  type MaterializationRootRelativePath,
} from '../../transfer/job/coordinate/direct-tree'
import type { PersistentOutputStageScope } from './stage-diagnostics'
import { runPersistentOutputStage } from './stage-diagnostics'
import { DestinationCollisionError } from './errors'
import {
  observePerformance,
  performanceElapsedMilliseconds,
  performanceNowMilliseconds,
  type PerformanceSummaryObservations,
} from '../diagnostics/performance-summary'
import {
  createPerformanceClaimBatchTimeline,
  type PerformanceClaimBatchTimeline,
  type PerformanceClaimBatchTimelineResult,
} from '../diagnostics/claim-batch-performance'
import {
  createPerformanceClaimInspectorObservation,
  type PerformanceClaimInspectorContextObservation,
  type PerformanceClaimInspectorObservation,
} from '../diagnostics/claim-inspector-performance'
import type { PerformanceFilePipelineObservation } from '../diagnostics/performance-runtime-observations'
import {
  inspectInitialClaimGroup,
} from './initial-claim-inspection-group'

export type PersistentCheckpointDeclineReason = Extract<
  AutomaticCheckpointResult,
  { readonly kind: 'declined' }
>['reason']

export class PersistentRecoveryPreflightError extends Error {
  readonly reason: PersistentCheckpointDeclineReason | 'space-confirmation-required'
  readonly preflight: PersistentWriterPreflight
  readonly budget: OutputCheckpointCostBudget | undefined

  constructor(input: Readonly<{
    reason: PersistentRecoveryPreflightError['reason']
    preflight: PersistentWriterPreflight
    budget?: OutputCheckpointCostBudget
  }>) {
    super('Persistent output recovery requires an explicit prefix-copy and temporary-space decision')
    this.name = 'PersistentRecoveryPreflightError'
    this.reason = input.reason
    this.preflight = snapshotPreflight(input.preflight)
    this.budget = input.budget === undefined ? undefined : Object.freeze({ ...input.budget })
  }
}

export function persistentCheckpointDeclineReason(
  cost: OutputCheckpointCost,
  budget: OutputCheckpointCostBudget,
): PersistentCheckpointDeclineReason | undefined {
  if (cost.prefixCopyBytes > budget.maximumPrefixCopyBytes) return 'prefix-copy-budget'
  if (cost.cumulativeWriteAmplificationBytes >
      budget.maximumCumulativeWriteAmplificationBytes) {
    return 'cumulative-write-amplification-budget'
  }
  if (cost.peakTemporaryBytes > budget.maximumPeakTemporaryBytes) {
    return 'peak-temporary-space-budget'
  }
  return undefined
}

interface PendingInitialClaim {
  readonly revision: OpenedFileRevision
  readonly path: MaterializationRootRelativePath
  readonly stageScope?: PersistentOutputStageScope
  readonly queuedAtMilliseconds?: number
  readonly performancePipeline?: PerformanceFilePipelineObservation
  readonly resolve: (decision: InitialCheckpointCASResult | CheckpointLineageDecision) => void
  readonly reject: (error: unknown) => void
}

interface GroupedInitialClaim {
  readonly pending: PendingInitialClaim
  readonly lookup: CheckpointLineageLookupRequest
}

interface AbsentInitialClaimInspection {
  readonly groupedIndex: number
  readonly item: GroupedInitialClaim
}

interface InspectedAbsentInitialClaim extends AbsentInitialClaimInspection {
  readonly ownedObjectId: string
  readonly proposedScope?: PersistentOutputStageScope
  readonly destination: Awaited<ReturnType<PersistentOutputTree['inspectFileDestination']>>
}

export const DEFAULT_MAXIMUM_CONCURRENT_INITIAL_CLAIM_INSPECTIONS = 1

/** Keeps claim batching at the authority boundary, so file writers never wait on discovery. */
export class PersistentInitialClaimCoordinator {
  readonly #tree: PersistentOutputTree
  readonly #journal: SemanticPersistentOutputJournal
  readonly #maximumConcurrentInspections: number
  readonly #performance: PerformanceSummaryObservations | undefined
  readonly #pending: PendingInitialClaim[] = []
  #claimInspector: PerformanceClaimInspectorObservation | undefined
  #drain: Promise<void> | undefined

  constructor(
    tree: PersistentOutputTree,
    journal: SemanticPersistentOutputJournal,
    maximumConcurrentInspections: number,
    performance?: PerformanceSummaryObservations,
  ) {
    if (!Number.isSafeInteger(maximumConcurrentInspections) ||
        maximumConcurrentInspections < DEFAULT_MAXIMUM_CONCURRENT_INITIAL_CLAIM_INSPECTIONS ||
        maximumConcurrentInspections > FILE_CHECKPOINT_BATCH_REQUEST_LIMIT) {
      throw new TypeError('maximum concurrent initial claim inspections is invalid')
    }
    this.#tree = tree
    this.#journal = journal
    this.#maximumConcurrentInspections = maximumConcurrentInspections
    this.#performance = performance
  }

  select(
    revision: OpenedFileRevision,
    path: MaterializationRootRelativePath,
    stageScope?: PersistentOutputStageScope,
    performancePipeline?: PerformanceFilePipelineObservation,
  ): Promise<InitialCheckpointCASResult | CheckpointLineageDecision> {
    const queuedAtMilliseconds = performanceNowMilliseconds(this.#performance)
    return new Promise((resolve, reject) => {
      this.#pending.push({
        revision,
        path,
        ...(queuedAtMilliseconds === undefined ? {} : { queuedAtMilliseconds }),
        ...(stageScope === undefined ? {} : { stageScope }),
        ...(performancePipeline === undefined ? {} : { performancePipeline }),
        resolve,
        reject,
      })
      this.#claimInspector?.setPendingMembers(this.#pending.length)
      this.#startDrain()
    })
  }

  #startDrain(): void {
    if (this.#drain !== undefined) return
    this.#claimInspector = createPerformanceClaimInspectorObservation(
      this.#performance,
      this.#maximumConcurrentInspections,
      performanceNowMilliseconds(this.#performance),
    )
    this.#claimInspector?.setPendingMembers(this.#pending.length)
    this.#drain = this.#drainClaims().finally(() => {
      const observation = this.#claimInspector?.complete()
      if (observation !== undefined) {
        observePerformance(this.#performance, summary => summary.observeClaimInspector(observation))
      }
      this.#claimInspector = undefined
      this.#drain = undefined
      if (this.#pending.length !== 0) this.#startDrain()
    })
  }

  async #drainClaims(): Promise<void> {
    while (this.#pending.length !== 0) {
      const batch = this.#pending.splice(0, FILE_CHECKPOINT_BATCH_REQUEST_LIMIT)
      this.#claimInspector?.setPendingMembers(this.#pending.length)
      const inspectorContext = this.#claimInspector?.beginContext()
      try {
        await this.#processBatch(batch, inspectorContext)
      } finally {
        inspectorContext?.finish()
      }
    }
  }

  async #processBatch(
    batch: readonly PendingInitialClaim[],
    inspectorContext: PerformanceClaimInspectorContextObservation | undefined,
  ): Promise<void> {
    const startedAtMilliseconds = performanceNowMilliseconds(this.#performance)
    const timeline = createPerformanceClaimBatchTimeline(this.#performance, startedAtMilliseconds)
    const unique = new Map<string, GroupedInitialClaim>()
    for (const pending of batch) {
      const lookup = persistentCheckpointLookup(this.#journal.binding, pending.revision, pending.path)
      if (!unique.has(lookup.lineageId)) unique.set(lookup.lineageId, { pending, lookup })
    }
    const grouped = [...unique.values()]
    try {
      const classified = await this.#classifyBatch(grouped, timeline)
      const resolved = new Map<string, InitialCheckpointCASResult | CheckpointLineageDecision>()
      const absent: AbsentInitialClaimInspection[] = []
      grouped.forEach((item, index) => {
        const decision = classified[index]!
        if (decision.lineageId !== item.lookup.lineageId) {
          throw new TypeError('checkpoint batch classification order changed')
        }
        if (decision.kind === 'absent') absent.push({ groupedIndex: index, item })
        else resolved.set(item.lookup.lineageId, decision)
      })
      const inspectionPhase = timeline?.beginPhase('inspection_union', absent.length)
      inspectorContext?.inspectionStarted()
      let inspected: readonly InspectedAbsentInitialClaim[]
      try {
        inspected = await inspectInitialClaimGroup({
          work: absent,
          maximumConcurrent: this.#maximumConcurrentInspections,
          inspect: claim => this.#inspectAbsentClaim(claim),
          ...(inspectionPhase === undefined ? {} : { performance: inspectionPhase }),
          observeState: state => this.#claimInspector?.observePoolState(state),
        })
      } finally {
        inspectionPhase?.finish()
        inspectorContext?.inspectionFinished()
      }
      inspectorContext?.settlementStarted()
      await this.#settleInspectedBatch(grouped, resolved, inspected, timeline)
      const decisions = batch.map((pending) => {
        const lineageId = persistentCheckpointLookup(
          this.#journal.binding,
          pending.revision,
          pending.path,
        ).lineageId
        const decision = resolved.get(lineageId)
        if (decision === undefined) throw new TypeError('checkpoint claim batch omitted a lineage')
        return decision
      })
      this.#observeClaimBatch(batch, startedAtMilliseconds, timeline?.complete())
      batch.forEach((pending, index) => pending.resolve(decisions[index]!))
    } catch (error) {
      batch.forEach(pending => pending.reject(error))
    }
  }

  async #classifyBatch(
    grouped: readonly GroupedInitialClaim[],
    timeline?: PerformanceClaimBatchTimeline,
  ): Promise<readonly CheckpointLineageDecision[]> {
    const classification = timeline?.beginPhase('classification', grouped.length)
    const classificationActive = classification?.beginActive()
    let classified: readonly CheckpointLineageDecision[]
    try {
      classified = await runPersistentOutputStage(
        grouped[0]?.pending.stageScope,
        'indexeddb.checkpoint.lineage-read',
        () => this.#journal.classifyLineages(grouped.map(item => item.lookup)),
      )
    } finally {
      classificationActive?.finish()
      classification?.finish()
    }
    if (classified.length !== grouped.length) {
      throw new TypeError('checkpoint batch classification cardinality changed')
    }
    return classified
  }

  async #settleInspectedBatch(
    grouped: readonly GroupedInitialClaim[],
    resolved: Map<string, InitialCheckpointCASResult | CheckpointLineageDecision>,
    inspected: readonly InspectedAbsentInitialClaim[],
    timeline?: PerformanceClaimBatchTimeline,
  ): Promise<void> {
    const occupied = inspected.filter(item => item.destination === 'occupied')
    const reclassificationPhase = timeline?.beginPhase('reclassification', occupied.length)
    const reclassificationActive = occupied.length === 0
      ? undefined
      : reclassificationPhase?.beginActive()
    let reclassified: readonly CheckpointLineageDecision[]
    try {
      reclassified = await this.#reclassifyOccupiedClaims(occupied)
    } finally {
      reclassificationActive?.finish()
      reclassificationPhase?.finish()
    }
    const candidates: FileCheckpointV2[] = []
    const candidateLineages: string[] = []
    let occupiedIndex = 0
    for (const inspection of inspected) {
      if (inspection.destination === 'occupied') {
        const decision = reclassified[occupiedIndex++]!
        resolved.set(inspection.item.lookup.lineageId, decision)
        continue
      }
      candidates.push(persistentInitialCheckpoint(
        this.#journal.binding,
        inspection.item.pending.revision,
        inspection.item.pending.path,
        inspection.ownedObjectId,
      ))
      candidateLineages.push(inspection.item.lookup.lineageId)
    }
    const installationPhase = timeline?.beginPhase('installation', candidates.length)
    const installationActive = candidates.length === 0 ? undefined : installationPhase?.beginActive()
    try {
      await this.#installProposedClaims(grouped, candidates, candidateLineages, resolved)
    } finally {
      installationActive?.finish()
    }
  }

  async #inspectAbsentClaim(
    claim: AbsentInitialClaimInspection,
  ): Promise<InspectedAbsentInitialClaim> {
    const ownedObjectId = await this.#tree.proposeFileOwnedObjectId(
      claim.item.pending.path,
      claim.item.pending.revision,
    )
    const proposedScope = claim.item.pending.stageScope?.withCorrelation({ ownedObjectId })
    claim.item.pending.performancePipeline?.transition('namespace_inspection')
    try {
      const destination = await this.#tree.inspectFileDestination(
        claim.item.pending.path,
        ownedObjectId,
        proposedScope,
      )
      return Object.freeze({
        ...claim,
        ownedObjectId,
        ...(proposedScope === undefined ? {} : { proposedScope }),
        destination,
      })
    } finally {
      claim.item.pending.performancePipeline?.transition('initial_lineage')
    }
  }

  async #reclassifyOccupiedClaims(
    occupied: readonly InspectedAbsentInitialClaim[],
  ): Promise<readonly CheckpointLineageDecision[]> {
    if (occupied.length === 0) return []
    const reclassified = await runPersistentOutputStage(
      occupied[0]?.proposedScope,
      'indexeddb.checkpoint.lineage-read',
      () => this.#journal.classifyLineages(occupied.map(item => item.item.lookup)),
    )
    if (reclassified.length !== occupied.length) {
      throw new TypeError('occupied checkpoint batch classification cardinality changed')
    }
    reclassified.forEach((decision, index) => {
      if (decision.lineageId !== occupied[index]?.item.lookup.lineageId) {
        throw new TypeError('occupied checkpoint batch classification order changed')
      }
      if (decision.kind === 'absent') throw new DestinationCollisionError()
    })
    return reclassified
  }

  async #installProposedClaims(
    grouped: readonly GroupedInitialClaim[],
    candidates: readonly FileCheckpointV2[],
    candidateLineages: readonly string[],
    resolved: Map<string, InitialCheckpointCASResult | CheckpointLineageDecision>,
  ): Promise<void> {
    if (candidates.length === 0) return
    const installed = await runPersistentOutputStage(
      grouped[0]?.pending.stageScope,
      'indexeddb.checkpoint.candidate-install',
      () => this.#journal.installInitialClaims(candidates),
    )
    if (installed.length !== candidates.length) {
      throw new TypeError('checkpoint claim batch cardinality changed')
    }
    installed.forEach((decision, index) => {
      if (decision.lineageId !== candidateLineages[index]) {
        throw new TypeError('checkpoint claim batch order changed')
      }
      resolved.set(decision.lineageId, decision)
    })
  }

  #observeClaimBatch(
    batch: readonly PendingInitialClaim[],
    startedAtMilliseconds: number | undefined,
    timeline: PerformanceClaimBatchTimelineResult | undefined,
  ): void {
    const queued = batch.flatMap(pending => pending.queuedAtMilliseconds === undefined
      ? []
      : [pending.queuedAtMilliseconds])
    if (queued.length !== batch.length || startedAtMilliseconds === undefined ||
        timeline === undefined) return
    const completedAtMilliseconds = timeline.completedAtMilliseconds
    const oldestWaitMilliseconds = performanceElapsedMilliseconds(
      Math.min(...queued),
      startedAtMilliseconds,
    )
    const newestWaitMilliseconds = performanceElapsedMilliseconds(
      Math.max(...queued),
      startedAtMilliseconds,
    )
    const runMilliseconds = performanceElapsedMilliseconds(
      startedAtMilliseconds,
      completedAtMilliseconds,
    )
    if (oldestWaitMilliseconds === undefined || newestWaitMilliseconds === undefined ||
        runMilliseconds === undefined) return
    observePerformance(this.#performance, summary => summary.observeClaimBatch({
      size: batch.length,
      oldestWaitMilliseconds,
      newestWaitMilliseconds,
      runMilliseconds,
      phases: timeline.phases,
    }))
  }
}

export function persistentCheckpointLookup(
  binding: CheckpointNamespaceBinding,
  revision: OpenedFileRevision,
  path: MaterializationRootRelativePath,
): CheckpointLineageLookupRequest {
  return Object.freeze({
    lineageId: deriveCheckpointLineageID({ ...binding, fileId: revision.fileId, canonicalPath: path }),
    fileId: revision.fileId,
    canonicalPath: path,
    fileRevision: revision.fileRevision,
    exactSize: revision.exactSize,
  })
}

export function persistentInitialCheckpoint(
  binding: CheckpointNamespaceBinding,
  revision: OpenedFileRevision,
  path: MaterializationRootRelativePath,
  ownedObjectId: string,
): FileCheckpointV2 {
  return newFileCheckpointV2({
    operationId: binding.operationId,
    receiveIntentDigest: binding.receiveIntentDigest,
    materializationBindingDigest: binding.materializationBindingDigest,
    fileId: revision.fileId,
    fileRevision: revision.fileRevision,
    canonicalPath: path,
    exactSize: revision.exactSize,
    materializerKind: binding.materializerKind,
    authorityRef: binding.authorityRef,
    ownedObjectId,
    stateGeneration: 1n,
    checkpointGeneration: 0n,
    verifiedRanges: [],
    phase: FILE_CHECKPOINT_PHASE_ACTIVE,
    commitState: FILE_CHECKPOINT_COMMIT_CANDIDATE,
  })
}

function snapshotPreflight(input: PersistentWriterPreflight): PersistentWriterPreflight {
  return Object.freeze({
    cost: Object.freeze({ ...input.cost }),
    space: input.space,
  })
}

export type FileCheckpointCandidateObservation =
  | Readonly<{ kind: 'verified'; committed: FileCheckpointV2 }>
  | Readonly<{ kind: 'quarantined'; checkpoint: FileCheckpointV2 }>
  | Readonly<{ kind: 'ownership-unknown' }>

export interface FileCheckpointRecoveryRepository {
  readonly binding: CheckpointNamespaceBinding
  scanCandidates(scan: FileCheckpointScan): Promise<FileCheckpointPage>
  readCommitted(recordId: string): Promise<FileCheckpointV2 | undefined>
  resolveCandidate(
    candidate: FileCheckpointV2,
    observation: Exclude<FileCheckpointCandidateObservation, { kind: 'ownership-unknown' }>,
  ): Promise<void>
}

export interface FileCheckpointCandidateProbe {
  observe(
    candidate: FileCheckpointV2,
    committed: FileCheckpointV2 | undefined,
  ): Promise<FileCheckpointCandidateObservation>
}

export interface FileCheckpointRecoveryReport {
  readonly resolved: number
  readonly unknownRecordIds: readonly string[]
}

/**
 * Candidate resolution is idempotent: the repository commits or quarantines a
 * candidate atomically. A crash can replay the probe, but cannot invent range truth.
 */
export async function recoverFileCheckpointCandidates(
  repository: FileCheckpointRecoveryRepository,
  probe: FileCheckpointCandidateProbe,
): Promise<FileCheckpointRecoveryReport> {
  let cursor: string | undefined
  let resolved = 0
  const unknownRecordIds: string[] = []

  do {
    const scan: FileCheckpointScan = {
      direction: 'ascending',
      ...(cursor === undefined ? {} : { cursor }),
    }
    const page = validateFileCheckpointPage(
      await repository.scanCandidates(scan),
      scan,
      repository.binding,
    )
    for (const candidate of page.records) {
      const candidateResolved = await recoverCandidate(repository, probe, candidate)
      if (candidateResolved) resolved += 1
      else unknownRecordIds.push(candidate.recordId)
    }
    cursor = page.nextCursor
  } while (cursor !== undefined)

  return Object.freeze({
    resolved,
    unknownRecordIds: Object.freeze(unknownRecordIds),
  })
}

async function recoverCandidate(
  repository: FileCheckpointRecoveryRepository,
  probe: FileCheckpointCandidateProbe,
  candidate: FileCheckpointV2,
): Promise<boolean> {
  if (!checkpointMatchesNamespace(candidate, repository.binding)) {
    throw new TypeError('candidate checkpoint escaped its recovery namespace')
  }
  const committed = await repository.readCommitted(candidate.recordId)
  if (committed !== undefined &&
      !checkpointMatchesNamespace(committed, repository.binding)) {
    throw new TypeError('committed checkpoint escaped its recovery namespace')
  }
  const observation = await probe.observe(candidate, committed)
  if (observation.kind === 'ownership-unknown') {
    // The aggregate owns the frozen receive.operation.recovery trace because only it
    // has enough operation context to report a contract-complete decision.
    return false
  }
  assertResolvedCandidateIdentity(candidate, observation, repository.binding)
  await repository.resolveCandidate(candidate, observation)
  return true
}

function assertResolvedCandidateIdentity(
  candidate: FileCheckpointV2,
  observation: Exclude<FileCheckpointCandidateObservation, { kind: 'ownership-unknown' }>,
  binding: CheckpointNamespaceBinding,
): void {
  const resolved = observation.kind === 'verified'
    ? observation.committed
    : observation.checkpoint
  validateFileCheckpoint(resolved)
  const expectedCommitState = observation.kind === 'verified'
    ? FILE_CHECKPOINT_COMMIT_VERIFIED
    : FILE_CHECKPOINT_COMMIT_QUARANTINED
  if (resolved.recordId !== candidate.recordId ||
      resolved.commitState !== expectedCommitState ||
      !checkpointMatchesNamespace(resolved, binding)) {
    throw new TypeError('checkpoint probe returned a foreign resolved record')
  }
}

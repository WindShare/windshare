import {
  checkpointMatchesNamespace,
  validateFileCheckpointPage,
  type CheckpointNamespaceBinding,
  type CheckpointLineageDecision,
  type CheckpointLineageLookupRequest,
  type InitialCheckpointCASResult,
  type FileCheckpointPage,
  type FileCheckpointScan,
  type FileCheckpointJournal,
  type SemanticFileCheckpointJournal,
} from '../persistence/journal'
import { FILE_CHECKPOINT_BATCH_REQUEST_LIMIT } from '../persistence/journal'
import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_COMMIT_QUARANTINED,
  FILE_CHECKPOINT_COMMIT_VERIFIED,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  FILE_CHECKPOINT_PHASE_PAUSED,
  fileCheckpointIsComplete,
  deriveCheckpointLineageID,
  newFileCheckpointV2,
  validateFileCheckpoint,
  type FileCheckpointV2,
} from '../persistence/checkpoint'
import type {
  OpenedFileRevision,
  PersistentFileRequest,
  PersistentTreeFile,
  PersistentOutputTree,
  PreservingWriterCapacityPurpose,
  PreservingWriterCost,
  SemanticPersistentOutputJournal,
} from './contracts'
import {
  snapshotMaterializationRootRelativePath,
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
  createPerformanceLineageClaimTimeline,
  type PerformanceLineageClaimTimeline,
} from '../diagnostics/lineage-claim-performance'
import {
  createPerformanceClaimInspectorObservation,
  type PerformanceClaimInspectorContextObservation,
  type PerformanceClaimInspectorObservation,
} from '../diagnostics/claim-inspector-performance'
import type { PerformanceFilePipelineObservation } from '../diagnostics/performance-runtime-observations'
import { InitialClaimPipeline } from './initial-claim-pipeline'

/** Restores receiving authority before a recovered file accepts any new writes. */
export async function preparePersistentFileRecovery(input: Readonly<{
  request: Pick<PersistentFileRequest, 'recovery'>
  handle: PersistentTreeFile
  selected: FileCheckpointV2
  checkpoints: FileCheckpointJournal
  semantic: SemanticFileCheckpointJournal | undefined
  stageScope: PersistentOutputStageScope | undefined
}>): Promise<FileCheckpointV2> {
  const { request, handle, selected, checkpoints, semantic, stageScope } = input
  const recovery = request.recovery ?? Object.freeze({ pausedFile: 'preserve' as const })
  let checkpoint = selected
  if (recovery.pausedFile === 'restart-owned-file' &&
      checkpoint.verifiedRanges.length > 0 && !fileCheckpointIsComplete(checkpoint)) {
    if (semantic === undefined || handle.persistedHandle === undefined) {
      throw new DOMException(
        'Explicit restart requires an exact durable handle authority',
        'InvalidStateError',
      )
    }
    if (checkpoint.phase === FILE_CHECKPOINT_PHASE_ACTIVE) {
      const paused = newFileCheckpointV2({
        ...checkpoint,
        stateGeneration: checkpoint.stateGeneration + 1n,
        phase: FILE_CHECKPOINT_PHASE_PAUSED,
        commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
      })
      await runPersistentOutputStage(
        stageScope?.withCorrelation({
          checkpointRecordId: paused.recordId,
          checkpointGeneration: paused.checkpointGeneration,
        }),
        'indexeddb.checkpoint.pause-commit',
        () => semantic.commitDurableCut(checkpoint, paused),
      )
      checkpoint = paused
    }
    const reset = newFileCheckpointV2({
      ...checkpoint,
      stateGeneration: checkpoint.stateGeneration + 1n,
      checkpointGeneration: checkpoint.checkpointGeneration + 1n,
      verifiedRanges: [],
      phase: FILE_CHECKPOINT_PHASE_PAUSED,
      commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
    })
    await runPersistentOutputStage(
      stageScope?.withCorrelation({
        checkpointRecordId: reset.recordId,
        checkpointGeneration: reset.checkpointGeneration,
      }),
      'indexeddb.checkpoint.restart-commit',
      () => semantic.restartOwnedFile({
        previous: checkpoint,
        reset,
        expectedHandle: handle.persistedHandle!,
      }),
    )
    checkpoint = reset
  }
  if (recovery.pausedFile === 'restart-owned-file' && !fileCheckpointIsComplete(checkpoint) &&
      handle.durability !== 'native-in-place' && await handle.size() > 0n) {
    // A crash can leave a closed full target with no final proof. Publish an empty
    // owned target before copying again so its old content cannot coexist with a
    // new full replacement and the complete OPFS source.
    if (handle.openWriter === undefined) throw new TypeError('Owned restart requires a truncating writer')
    await handle.openWriter('truncate')
    try { await handle.flush() } finally { await handle.close() }
    await handle.verify('checkpoint')
  }
  if (checkpoint.phase !== FILE_CHECKPOINT_PHASE_PAUSED) return checkpoint
  const active = newFileCheckpointV2({
    ...checkpoint,
    stateGeneration: checkpoint.stateGeneration + 1n,
    checkpointGeneration: checkpoint.checkpointGeneration +
      (semantic === undefined ? 1n : 0n),
    phase: FILE_CHECKPOINT_PHASE_ACTIVE,
    commitState: FILE_CHECKPOINT_COMMIT_VERIFIED,
  })
  if (semantic !== undefined) {
    await runPersistentOutputStage(
      stageScope?.withCorrelation({
        checkpointRecordId: active.recordId,
        checkpointGeneration: active.checkpointGeneration,
      }),
      'indexeddb.checkpoint.resume-commit',
      () => semantic.resumePausedCheckpoint(checkpoint, active),
    )
    return active
  }
  const candidate = newFileCheckpointV2({
    ...active,
    commitState: FILE_CHECKPOINT_COMMIT_CANDIDATE,
  })
  await checkpoints.stageCheckpointUpdate(checkpoint, candidate)
  await checkpoints.commitCheckpointCandidate(candidate, active)
  return active
}

/** A preserving open failed after its durable prefix was already committed. */
export class PersistentPreservingWriterOpenError extends Error {
  readonly materializationRelativePath: MaterializationRootRelativePath
  readonly cost: PreservingWriterCost
  readonly purpose: PreservingWriterCapacityPurpose

  constructor(input: Readonly<{
    materializationRelativePath: MaterializationRootRelativePath
    cost: PreservingWriterCost
    purpose: PreservingWriterCapacityPurpose
    cause: unknown
  }>) {
    super('Persistent output could not reopen its preserving writer', { cause: input.cause })
    this.name = 'PersistentPreservingWriterOpenError'
    this.materializationRelativePath = snapshotMaterializationRootRelativePath(
      input.materializationRelativePath,
    )
    this.cost = Object.freeze({ ...input.cost })
    this.purpose = input.purpose
  }
}

interface InitialClaim {
  readonly key: string
  readonly lineageId: string
  readonly revision: OpenedFileRevision
  readonly path: MaterializationRootRelativePath
  readonly lookup: CheckpointLineageLookupRequest
  readonly stageScope?: PersistentOutputStageScope
  readonly queuedAtMilliseconds?: number
  readonly performancePipeline?: PerformanceFilePipelineObservation
  admittedAtMilliseconds?: number
  timeline?: PerformanceLineageClaimTimeline
  inspectorContext?: PerformanceClaimInspectorContextObservation
  inspected?: boolean
}

interface InspectedInitialClaim {
  readonly ownedObjectId: string
  readonly proposedScope?: PersistentOutputStageScope
  readonly destination: Awaited<ReturnType<PersistentOutputTree['inspectFileDestination']>>
}

type InitialClaimDecision = InitialCheckpointCASResult | CheckpointLineageDecision
type ReadyInitialClaim = Readonly<{ input: InitialClaim; inspection: InspectedInitialClaim }>

export const DEFAULT_MAXIMUM_CONCURRENT_INITIAL_CLAIM_INSPECTIONS = 1

/** Independent lineage authority advances through bounded inspection and journal lanes. */
export class PersistentInitialClaimCoordinator {
  readonly #tree: PersistentOutputTree
  readonly #journal: SemanticPersistentOutputJournal
  readonly #maximumConcurrentInspections: number
  readonly #performance: PerformanceSummaryObservations | undefined
  readonly #pipeline: InitialClaimPipeline<InitialClaim, InspectedInitialClaim, InitialClaimDecision>
  #claimInspector: PerformanceClaimInspectorObservation | undefined

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
    this.#pipeline = new InitialClaimPipeline({
      maximumResident: FILE_CHECKPOINT_BATCH_REQUEST_LIMIT,
      maximumInspecting: maximumConcurrentInspections,
      classify: claims => this.#classify(claims),
      inspect: claim => this.#inspect(claim),
      settle: ready => this.#settle(ready),
      admitted: claim => this.#admit(claim),
      completed: (claim, succeeded) => this.#complete(claim, succeeded),
      observe: state => {
        this.#claimInspector?.setPendingMembers(state.pendingMembers)
        this.#claimInspector?.observePoolState(state)
      },
      drained: () => this.#drained(),
    })
  }

  select(
    revision: OpenedFileRevision,
    path: MaterializationRootRelativePath,
    stageScope?: PersistentOutputStageScope,
    performancePipeline?: PerformanceFilePipelineObservation,
  ): Promise<InitialClaimDecision> {
    const lookup = persistentCheckpointLookup(this.#journal.binding, revision, path)
    const queuedAtMilliseconds = performanceNowMilliseconds(this.#performance)
    return this.#pipeline.select({
      // The lineage excludes revision and size. Different revision requests must
      // reclassify after the preceding claim has established durable authority.
      key: JSON.stringify([lookup.lineageId, revision.fileRevision, revision.exactSize.toString()]),
      lineageId: lookup.lineageId,
      revision,
      path,
      lookup,
      ...(queuedAtMilliseconds === undefined ? {} : { queuedAtMilliseconds }),
      ...(stageScope === undefined ? {} : { stageScope }),
      ...(performancePipeline === undefined ? {} : { performancePipeline }),
    })
  }

  #admit(claim: InitialClaim): void {
    this.#claimInspector ??= createPerformanceClaimInspectorObservation(
      this.#performance,
      this.#maximumConcurrentInspections,
      performanceNowMilliseconds(this.#performance),
    )
    const admittedAtMilliseconds = performanceNowMilliseconds(this.#performance)
    if (admittedAtMilliseconds !== undefined) claim.admittedAtMilliseconds = admittedAtMilliseconds
    const timeline = createPerformanceLineageClaimTimeline(this.#performance, admittedAtMilliseconds)
    if (timeline !== undefined) claim.timeline = timeline
    const inspectorContext = this.#claimInspector?.beginContext()
    if (inspectorContext !== undefined) claim.inspectorContext = inspectorContext
  }

  async #classify(claims: readonly InitialClaim[]): Promise<readonly (InitialClaimDecision | undefined)[]> {
    const phases = claims.map(claim => claim.timeline?.beginPhase('classification', 1))
    const active = phases.map(phase => phase?.beginActive())
    let classified: readonly CheckpointLineageDecision[]
    try {
      classified = await runPersistentOutputStage(
        claims[0]?.stageScope,
        'indexeddb.checkpoint.lineage-read',
        () => this.#journal.classifyLineages(claims.map(claim => claim.lookup)),
      )
    } finally {
      active.forEach(sample => sample?.finish())
      phases.forEach(phase => phase?.finish())
    }
    if (classified.length !== claims.length) {
      throw new TypeError('checkpoint batch classification cardinality changed')
    }
    return classified.map((decision, index) => {
      if (decision.lineageId !== claims[index]?.lineageId) {
        throw new TypeError('checkpoint batch classification order changed')
      }
      return decision.kind === 'absent' ? undefined : decision
    })
  }

  async #inspect(claim: InitialClaim): Promise<InspectedInitialClaim> {
    claim.inspected = true
    claim.inspectorContext?.inspectionStarted()
    const phase = claim.timeline?.beginPhase('inspection_union', 1)
    const active = phase?.beginActive()
    try {
      const ownedObjectId = await this.#tree.proposeFileOwnedObjectId(claim.path, claim.revision)
      const proposedScope = claim.stageScope?.withCorrelation({ ownedObjectId })
      claim.performancePipeline?.transition('namespace_inspection')
      const destination = await this.#tree.inspectFileDestination(claim.path, ownedObjectId, proposedScope)
      return { ownedObjectId, ...(proposedScope === undefined ? {} : { proposedScope }), destination }
    } finally {
      claim.performancePipeline?.transition('initial_lineage')
      active?.finish()
      phase?.finish()
      claim.inspectorContext?.inspectionFinished()
    }
  }

  async #settle(ready: readonly ReadyInitialClaim[]): Promise<readonly InitialClaimDecision[]> {
    ready.forEach(({ input }) => input.inspectorContext?.settlementStarted())
    const occupied = ready.filter(item => item.inspection.destination === 'occupied')
    const resolved = new Map<string, InitialClaimDecision>()
    const reclassification = ready.map(item => item.input.timeline?.beginPhase(
      'reclassification', item.inspection.destination === 'occupied' ? 1 : 0,
    ))
    const reclassifying = ready.map((item, index) => item.inspection.destination === 'occupied'
      ? reclassification[index]?.beginActive() : undefined)
    try {
      const decisions = await this.#reclassifyOccupied(occupied)
      occupied.forEach((item, index) => resolved.set(item.input.lineageId, decisions[index]!))
    } finally {
      reclassifying.forEach(active => active?.finish())
      reclassification.forEach(phase => phase?.finish())
    }
    const absent = ready.filter(item => item.inspection.destination === 'absent')
    const installation = ready.map(item => item.input.timeline?.beginPhase(
      'installation', item.inspection.destination === 'absent' ? 1 : 0,
    ))
    const installing = ready.map((item, index) => item.inspection.destination === 'absent'
      ? installation[index]?.beginActive() : undefined)
    try {
      await this.#install(absent, resolved)
    } finally {
      installing.forEach(active => active?.finish())
      installation.forEach(phase => phase?.finish())
    }
    return ready.map(({ input }) => {
      const decision = resolved.get(input.lineageId)
      if (decision === undefined) throw new TypeError('checkpoint claim settlement omitted a lineage')
      return decision
    })
  }

  async #reclassifyOccupied(occupied: readonly ReadyInitialClaim[]): Promise<readonly CheckpointLineageDecision[]> {
    if (occupied.length === 0) return []
    const reclassified = await runPersistentOutputStage(
      occupied[0]?.inspection.proposedScope,
      'indexeddb.checkpoint.lineage-read',
      () => this.#journal.classifyLineages(occupied.map(item => item.input.lookup)),
    )
    if (reclassified.length !== occupied.length) {
      throw new TypeError('occupied checkpoint batch classification cardinality changed')
    }
    reclassified.forEach((decision, index) => {
      if (decision.lineageId !== occupied[index]?.input.lineageId) {
        throw new TypeError('occupied checkpoint batch classification order changed')
      }
      if (decision.kind === 'absent') throw new DestinationCollisionError()
    })
    return reclassified
  }

  async #install(
    absent: readonly ReadyInitialClaim[],
    resolved: Map<string, InitialClaimDecision>,
  ): Promise<void> {
    if (absent.length === 0) return
    const candidates = absent.map(({ input, inspection }) => persistentInitialCheckpoint(
      this.#journal.binding, input.revision, input.path, inspection.ownedObjectId,
    ))
    const installed = await runPersistentOutputStage(
      absent[0]?.input.stageScope,
      'indexeddb.checkpoint.candidate-install',
      () => this.#journal.installInitialClaims(candidates),
    )
    if (installed.length !== candidates.length) throw new TypeError('checkpoint claim batch cardinality changed')
    installed.forEach((decision, index) => {
      if (decision.lineageId !== absent[index]?.input.lineageId) {
        throw new TypeError('checkpoint claim batch order changed')
      }
      resolved.set(decision.lineageId, decision)
    })
  }

  #complete(claim: InitialClaim, succeeded: boolean): void {
    claim.inspectorContext?.finish()
    if (!succeeded) return
    if (!claim.inspected) {
      for (const phase of ['inspection_union', 'reclassification', 'installation'] as const) {
        claim.timeline?.beginPhase(phase, 0)?.finish()
      }
    }
    const timeline = claim.timeline?.complete()
    const waitMilliseconds = performanceElapsedMilliseconds(
      claim.queuedAtMilliseconds, claim.admittedAtMilliseconds,
    )
    const runMilliseconds = performanceElapsedMilliseconds(
      claim.admittedAtMilliseconds, timeline?.completedAtMilliseconds,
    )
    if (timeline === undefined || waitMilliseconds === undefined || runMilliseconds === undefined) return
    // Timelines follow independently settled lineage claims. Shared journal calls
    // appear in each affected claim's latency; the inspector measures global wall time.
    observePerformance(this.#performance, summary => summary.observeLineageClaim({
      waitMilliseconds,
      runMilliseconds,
      phases: timeline.phases,
    }))
  }

  #drained(): void {
    const observation = this.#claimInspector?.complete()
    if (observation !== undefined) {
      observePerformance(this.#performance, summary => summary.observeClaimInspector(observation))
    }
    this.#claimInspector = undefined
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

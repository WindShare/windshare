import type { V2CommittedDirectory } from '../catalog/v2-page-store'
import type { ReceiveLifecycleState } from '../output/workspace/state'
import {
  createDirectoryAdmissionScope,
  type DirectoryAdmissionScope,
} from './directory-admission'
import type {
  AuthenticatedDirectory,
  DirectoryCursor,
  DirectoryWork,
  PendingFile,
  TransferJobResult,
} from './job/contract'
import { ExactPreparationCollector } from './job/preparation'
import type { AsyncBoundedQueue } from './job/scheduler'
import type { SelectionMeasure } from './measure'
import type {
  DirectAtomicPlan,
  DirectTreePlan,
  OriginalFileArtifact,
  PortableHandoffPlan,
  ReceiveIntent,
  WorkspaceThenPublishPlan,
  ZipArchiveArtifact,
} from './intent'
import {
  validatePlanExecutionBinding,
  type ExactPreparationEvidence,
  type ExactSingleFileEvidence,
  type IncrementalDirectoryOutput,
  type PlanExecution,
  type V2PlanExecutionAuthority,
} from './output-session'

type DirectTreeIntent = ReceiveIntent & Readonly<{ plan: DirectTreePlan }>
type DirectAtomicIntent = ReceiveIntent & Readonly<{ plan: DirectAtomicPlan }>
type WorkspaceIntent = ReceiveIntent & Readonly<{ plan: WorkspaceThenPublishPlan }>
type WorkspaceOriginalIntent = WorkspaceIntent & Readonly<{ artifact: OriginalFileArtifact }>
type WorkspaceZipIntent = WorkspaceIntent & Readonly<{ artifact: ZipArchiveArtifact }>
type PortableIntent = ReceiveIntent & Readonly<{ plan: PortableHandoffPlan }>

type MaterializationPlanAdmission = Pick<
  V2PlanExecutionAuthority,
  | 'openDirectTree'
  | 'openDirectAtomic'
  | 'openWorkspaceOriginal'
  | 'prepareWorkspaceZip'
  | 'preparePortable'
>

interface MaterializationRootPort {
  load(): Promise<V2CommittedDirectory>
  cursor(): DirectoryCursor
  authenticated(committed: V2CommittedDirectory): DirectoryWork
  direct(): Promise<DirectoryWork>
  singleFileEvidence(
    intent: WorkspaceOriginalIntent,
    committed: V2CommittedDirectory,
  ): Promise<ExactSingleFileEvidence>
}

interface MaterializationDiscoveryPort {
  createDirectFileQueue(): AsyncBoundedQueue<PendingFile>
  run(
    root: DirectoryWork,
    directFiles?: AsyncBoundedQueue<PendingFile>,
    collector?: ExactPreparationCollector,
  ): Promise<void>
  finish(): SelectionMeasure
  hasFailures(): boolean
  prepareDirectory(
    collector: ExactPreparationCollector,
    cursor: DirectoryCursor,
    committed: V2CommittedDirectory,
    role: 'selected' | 'ancestor',
  ): AuthenticatedDirectory
}

interface MaterializationExecutionPort {
  bind(execution: PlanExecution): void
  bindDirectoryOutput(output: IncrementalDirectoryOutput | undefined): void
  bindDirectoryScope(scope: DirectoryAdmissionScope): void
  recordPreparation(evidence: ExactPreparationEvidence): void
  requireBound(): PlanExecution
  materializationStarted(): void
  finalizeDirectories(): Promise<void>
  transferPreparedFiles(files: readonly PendingFile[]): Promise<void>
  completeWorkers(measure: SelectionMeasure): Promise<TransferJobResult>
  settleIncompletePreparation(measure: SelectionMeasure): Promise<TransferJobResult>
  preparationRejected(state: ReceiveLifecycleState): TransferJobResult
}

export interface TransferJobMaterializationContext {
  readonly signal: AbortSignal
  readonly admission: MaterializationPlanAdmission
  readonly root: MaterializationRootPort
  readonly discovery: MaterializationDiscoveryPort
  readonly execution: MaterializationExecutionPort
}

/**
 * Owns plan-specific preparation and run selection without acquiring transfer
 * settlement, worker, or lifecycle authority from the enclosing job.
 */
export class TransferJobMaterialization {
  readonly #context: TransferJobMaterializationContext

  constructor(context: TransferJobMaterializationContext) {
    this.#context = context
  }

  async run(intent: ReceiveIntent): Promise<TransferJobResult> {
    switch (intent.plan.kind) {
      case 'direct-tree': return this.#runDirectTree(intent as DirectTreeIntent)
      case 'direct-atomic': return this.#runDirectAtomic(intent as DirectAtomicIntent)
      case 'workspace-then-publish': return this.#runWorkspace(intent as WorkspaceIntent)
      case 'portable-handoff': return this.#runPreparedPortable(intent as PortableIntent)
    }
    throw new TypeError('receive intent has an unknown materialization plan')
  }

  async #runDirectTree(intent: DirectTreeIntent): Promise<TransferJobResult> {
    const execution = validatePlanExecutionBinding(
      intent,
      await this.#context.admission.openDirectTree(intent, this.#context.signal),
    )
    this.#context.execution.bind(execution)
    this.#context.execution.bindDirectoryOutput(execution.directories)
    this.#context.execution.bindDirectoryScope(await createDirectoryAdmissionScope(intent))
    this.#context.execution.materializationStarted()
    const root = await this.#context.root.direct()
    await this.#context.discovery.run(root, this.#context.discovery.createDirectFileQueue())
    const measure = this.#context.discovery.finish()
    await this.#context.execution.finalizeDirectories()
    return this.#context.execution.completeWorkers(measure)
  }

  async #runDirectAtomic(intent: DirectAtomicIntent): Promise<TransferJobResult> {
    const execution = validatePlanExecutionBinding(
      intent,
      await this.#context.admission.openDirectAtomic(intent, this.#context.signal),
    )
    this.#context.execution.bind(execution)
    this.#context.execution.bindDirectoryOutput(execution.directories)
    if (execution.directories !== undefined) {
      this.#context.execution.bindDirectoryScope(await createDirectoryAdmissionScope(intent))
    }
    this.#context.execution.materializationStarted()
    const root = execution.directories === undefined
      ? this.#context.root.authenticated(await this.#context.root.load())
      : await this.#context.root.direct()
    await this.#context.discovery.run(root, this.#context.discovery.createDirectFileQueue())
    const measure = this.#context.discovery.finish()
    if (execution.directories !== undefined) {
      await this.#context.execution.finalizeDirectories()
    }
    return this.#context.execution.completeWorkers(measure)
  }

  #runWorkspace(intent: WorkspaceIntent): Promise<TransferJobResult> {
    switch (intent.artifact.kind) {
      case 'original-file': return this.#runWorkspaceOriginal(intent as WorkspaceOriginalIntent)
      case 'zip-archive': return this.#runPreparedWorkspaceZip(intent as WorkspaceZipIntent)
      case 'directory-tree':
        throw new TypeError('WorkspaceThenPublish does not support DirectoryTree artifacts')
    }
  }

  async #runWorkspaceOriginal(intent: WorkspaceOriginalIntent): Promise<TransferJobResult> {
    const committed = await this.#context.root.load()
    const evidence = await this.#context.root.singleFileEvidence(intent, committed)
    const admitted = await this.#context.admission.openWorkspaceOriginal(
      intent,
      evidence,
      this.#context.signal,
    )
    if (admitted.kind === 'rejected') {
      return this.#context.execution.preparationRejected(admitted.state)
    }
    this.#context.execution.bind(validatePlanExecutionBinding(intent, admitted.execution))
    this.#context.execution.materializationStarted()
    const root = this.#context.root.authenticated(committed)
    await this.#context.discovery.run(root, this.#context.discovery.createDirectFileQueue())
    const measure = this.#context.discovery.finish()
    return this.#context.execution.completeWorkers(measure)
  }

  async #runPreparedWorkspaceZip(intent: WorkspaceZipIntent): Promise<TransferJobResult> {
    const { collector, measure } = await this.#collectExactPreparation(intent)
    if (this.#context.discovery.hasFailures()) {
      return this.#context.execution.settleIncompletePreparation(measure)
    }
    const evidence = collector.evidence()
    this.#context.execution.recordPreparation(evidence)
    const prepared = await this.#context.admission.prepareWorkspaceZip(
      intent,
      evidence,
      this.#context.signal,
    )
    if (prepared.kind === 'rejected') {
      return this.#context.execution.preparationRejected(prepared.state)
    }
    this.#context.execution.bind(validatePlanExecutionBinding(intent, prepared.execution))
    return this.#runPreparedContent(collector, measure)
  }

  async #runPreparedPortable(intent: PortableIntent): Promise<TransferJobResult> {
    const { collector, measure } = await this.#collectExactPreparation(intent)
    if (this.#context.discovery.hasFailures()) {
      return this.#context.execution.settleIncompletePreparation(measure)
    }
    const evidence = collector.evidence()
    this.#context.execution.recordPreparation(evidence)
    const prepared = await this.#context.admission.preparePortable(
      intent,
      evidence,
      this.#context.signal,
    )
    if (prepared.kind === 'rejected') {
      return this.#context.execution.preparationRejected(prepared.state)
    }
    this.#context.execution.bind(validatePlanExecutionBinding(intent, prepared.execution))
    return this.#runPreparedContent(collector, measure)
  }

  async #collectExactPreparation(intent: ReceiveIntent): Promise<Readonly<{
    collector: ExactPreparationCollector
    measure: SelectionMeasure
  }>> {
    const collector = new ExactPreparationCollector(intent)
    const committed = await this.#context.root.load()
    const cursor = this.#context.root.cursor()
    const root: DirectoryWork = {
      cursor,
      materializeParent: async (role = 'ancestor') => this.#context.discovery.prepareDirectory(
        collector,
        cursor,
        committed,
        role,
      ),
    }
    await this.#context.discovery.run(root, undefined, collector)
    return Object.freeze({
      collector,
      measure: this.#context.discovery.finish(),
    })
  }

  async #runPreparedContent(
    collector: ExactPreparationCollector,
    measure: SelectionMeasure,
  ): Promise<TransferJobResult> {
    this.#context.execution.requireBound()
    this.#context.execution.materializationStarted()
    await this.#context.execution.transferPreparedFiles(collector.pendingFiles())
    return this.#context.execution.completeWorkers(measure)
  }
}

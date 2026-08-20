import type { ReceiveIntent } from '../../transfer/intent'
import type { OutputDiagnosticsPorts } from '../diagnostics'
import {
  createPersistedReceiveRecord,
  createReceiveOperationV1,
  createWorkspaceActivationCandidate,
  receiveOperationHandleRecord,
  RECEIVE_RECORD_WORKSPACE_BINDING,
  storedWorkspaceActivationCandidate,
  storedReceiveOperationRecord,
  type WorkspaceActivationCandidateV1,
} from './records'
import type { ReceiveOperationRepository } from './repository'
import { initialReceiveLifecycleState } from './state'
import { WorkspaceAdmissionStages } from './stages/admission'
import { WorkspaceArtifactStages } from './stages/artifact'
import { WorkspaceCleanupStages } from './stages/cleanup'
import { WorkspaceContinuationStages } from './stages/continuation'
import {
  WORKSPACE_HANDLE_ROOT,
  requireWorkspaceIntent,
  type WorkspaceContentRequestCounter,
  type WorkspaceStageTraceListener,
} from './stages/contracts'
import { WorkspacePublicationStages } from './stages/publication'
import { WorkspaceStageRuntime } from './stages/runtime'

export * from './stages/contracts'

export async function journalWorkspaceActivation(input: {
  readonly repository: ReceiveOperationRepository
  readonly receiveIntent: ReceiveIntent
  readonly entryIdentity: string
  readonly workspaceRootHandleId: string
  readonly workspaceOwnedObjectId: string
}): Promise<WorkspaceActivationCandidateV1> {
  const intent = await requireWorkspaceIntent(input.receiveIntent)
  const operation = await createReceiveOperationV1({ receiveIntent: intent })
  const workspaceRecord = await createPersistedReceiveRecord({
    operationId: intent.operationId,
    kind: RECEIVE_RECORD_WORKSPACE_BINDING,
    canonicalBytes: intent.plan.workspace.canonicalBytes,
  })
  const candidate = await createWorkspaceActivationCandidate({
    operationId: intent.operationId,
    entryIdentity: input.entryIdentity,
    rootHandleId: input.workspaceRootHandleId,
    rootOwnedObjectId: input.workspaceOwnedObjectId,
    repositoryAuthority: intent.plan.workspace.repositoryRef,
  })
  const lifecycle = initialReceiveLifecycleState({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
  })
  await input.repository.commitTransition({
    operationId: intent.operationId,
    records: [
      storedReceiveOperationRecord(operation),
      workspaceRecord,
      await storedWorkspaceActivationCandidate(candidate),
    ],
    lifecycle,
  })
  return candidate
}

export async function promoteWorkspaceActivation(input: {
  readonly repository: ReceiveOperationRepository
  readonly candidate: WorkspaceActivationCandidateV1
  readonly workspaceRootHandle: FileSystemDirectoryHandle
  readonly expectedLifecycleGeneration?: bigint
}): Promise<void> {
  const candidateRecord = await storedWorkspaceActivationCandidate(input.candidate)
  const root = receiveOperationHandleRecord({
    id: input.candidate.rootHandleId,
    operationId: input.candidate.operationId,
    kind: WORKSPACE_HANDLE_ROOT,
    authorityRef: input.candidate.repositoryAuthority,
    ownedObjectId: input.candidate.rootOwnedObjectId,
    handle: input.workspaceRootHandle,
  })
  await input.repository.commitTransition({
    operationId: input.candidate.operationId,
    expectedLifecycleGeneration: input.expectedLifecycleGeneration ?? 1n,
    handles: [root],
    deleteRecordIds: [candidateRecord.id],
  })
}

/**
 * Stable facade over workflow-specific deep modules. Every workflow shares one
 * runtime so lifecycle generation, lease fencing, trace, and clock authority
 * cannot drift as the implementation evolves.
 */
export class WorkspaceOperationStages {
  readonly #admission: WorkspaceAdmissionStages
  readonly #artifact: WorkspaceArtifactStages
  readonly #cleanup: WorkspaceCleanupStages
  readonly #continuation: WorkspaceContinuationStages
  readonly #publication: WorkspacePublicationStages

  private constructor(runtime: WorkspaceStageRuntime) {
    this.#cleanup = new WorkspaceCleanupStages(runtime)
    this.#admission = new WorkspaceAdmissionStages(runtime, this.#cleanup)
    this.#artifact = new WorkspaceArtifactStages(runtime)
    this.#continuation = new WorkspaceContinuationStages(runtime)
    this.#publication = new WorkspacePublicationStages(runtime)
  }

  static async open(input: {
    readonly repository: ReceiveOperationRepository
    readonly receiveIntent: ReceiveIntent
    readonly leaseId: string
    readonly clock: () => number
    readonly contentRequests: WorkspaceContentRequestCounter
    readonly onTrace?: WorkspaceStageTraceListener
    readonly diagnostics?: OutputDiagnosticsPorts
  }): Promise<WorkspaceOperationStages> {
    return new WorkspaceOperationStages(new WorkspaceStageRuntime({
      repository: input.repository,
      intent: await requireWorkspaceIntent(input.receiveIntent),
      leaseId: input.leaseId,
      clock: input.clock,
      contentRequests: input.contentRequests,
      ...(input.onTrace === undefined ? {} : { trace: input.onTrace }),
      ...(input.diagnostics === undefined ? {} : { diagnostics: input.diagnostics }),
    }))
  }

  beginReceive(
    ...args: Parameters<WorkspaceAdmissionStages['beginReceive']>
  ): ReturnType<WorkspaceAdmissionStages['beginReceive']> {
    return this.#admission.beginReceive(...args)
  }

  admitPreparedZip(
    ...args: Parameters<WorkspaceAdmissionStages['admitPreparedZip']>
  ): ReturnType<WorkspaceAdmissionStages['admitPreparedZip']> {
    return this.#admission.admitPreparedZip(...args)
  }

  admitSingleFile(
    ...args: Parameters<WorkspaceAdmissionStages['admitSingleFile']>
  ): ReturnType<WorkspaceAdmissionStages['admitSingleFile']> {
    return this.#admission.admitSingleFile(...args)
  }

  reopenAdmittedContent(
    ...args: Parameters<WorkspaceAdmissionStages['reopenAdmittedContent']>
  ): ReturnType<WorkspaceAdmissionStages['reopenAdmittedContent']> {
    return this.#admission.reopenAdmittedContent(...args)
  }

  reopenAdmittedPackage(
    ...args: Parameters<WorkspaceAdmissionStages['reopenAdmittedPackage']>
  ): ReturnType<WorkspaceAdmissionStages['reopenAdmittedPackage']> {
    return this.#admission.reopenAdmittedPackage(...args)
  }

  readRetainedPackage(
    ...args: Parameters<WorkspaceArtifactStages['readRetainedPackage']>
  ): ReturnType<WorkspaceArtifactStages['readRetainedPackage']> {
    return this.#artifact.readRetainedPackage(...args)
  }

  sealMaterialization(
    ...args: Parameters<WorkspaceArtifactStages['sealMaterialization']>
  ): ReturnType<WorkspaceArtifactStages['sealMaterialization']> {
    return this.#artifact.sealMaterialization(...args)
  }

  startPackage(
    ...args: Parameters<WorkspaceArtifactStages['startPackage']>
  ): ReturnType<WorkspaceArtifactStages['startPackage']> {
    return this.#artifact.startPackage(...args)
  }

  recordRetryablePackageFailure(
    ...args: Parameters<WorkspaceArtifactStages['recordRetryablePackageFailure']>
  ): ReturnType<WorkspaceArtifactStages['recordRetryablePackageFailure']> {
    return this.#artifact.recordRetryablePackageFailure(...args)
  }

  sealPackage(
    ...args: Parameters<WorkspaceArtifactStages['sealPackage']>
  ): ReturnType<WorkspaceArtifactStages['sealPackage']> {
    return this.#artifact.sealPackage(...args)
  }

  startManagedPublication(
    ...args: Parameters<WorkspacePublicationStages['startManagedPublication']>
  ): ReturnType<WorkspacePublicationStages['startManagedPublication']> {
    return this.#publication.startManagedPublication(...args)
  }

  recordManagedPublicationCommitted(
    ...args: Parameters<WorkspacePublicationStages['recordManagedPublicationCommitted']>
  ): ReturnType<WorkspacePublicationStages['recordManagedPublicationCommitted']> {
    return this.#publication.recordManagedPublicationCommitted(...args)
  }

  recordManagedPublicationNotCommitted(
    ...args: Parameters<WorkspacePublicationStages['recordManagedPublicationNotCommitted']>
  ): ReturnType<WorkspacePublicationStages['recordManagedPublicationNotCommitted']> {
    return this.#publication.recordManagedPublicationNotCommitted(...args)
  }

  recordManagedPublicationUnknown(
    ...args: Parameters<WorkspacePublicationStages['recordManagedPublicationUnknown']>
  ): ReturnType<WorkspacePublicationStages['recordManagedPublicationUnknown']> {
    return this.#publication.recordManagedPublicationUnknown(...args)
  }

  recordTargetOwnershipUnknown(
    ...args: Parameters<WorkspacePublicationStages['recordTargetOwnershipUnknown']>
  ): ReturnType<WorkspacePublicationStages['recordTargetOwnershipUnknown']> {
    return this.#publication.recordTargetOwnershipUnknown(...args)
  }

  startHandoff(
    ...args: Parameters<WorkspacePublicationStages['startHandoff']>
  ): ReturnType<WorkspacePublicationStages['startHandoff']> {
    return this.#publication.startHandoff(...args)
  }

  recordHandoffStarted(
    ...args: Parameters<WorkspacePublicationStages['recordHandoffStarted']>
  ): ReturnType<WorkspacePublicationStages['recordHandoffStarted']> {
    return this.#publication.recordHandoffStarted(...args)
  }

  recordHandoffNotStarted(
    ...args: Parameters<WorkspacePublicationStages['recordHandoffNotStarted']>
  ): ReturnType<WorkspacePublicationStages['recordHandoffNotStarted']> {
    return this.#publication.recordHandoffNotStarted(...args)
  }

  recordHandoffUnknown(
    ...args: Parameters<WorkspacePublicationStages['recordHandoffUnknown']>
  ): ReturnType<WorkspacePublicationStages['recordHandoffUnknown']> {
    return this.#publication.recordHandoffUnknown(...args)
  }

  pauseReceive(
    ...args: Parameters<WorkspaceContinuationStages['pauseReceive']>
  ): ReturnType<WorkspaceContinuationStages['pauseReceive']> {
    return this.#continuation.pauseReceive(...args)
  }

  pausePackage(
    ...args: Parameters<WorkspaceContinuationStages['pausePackage']>
  ): ReturnType<WorkspaceContinuationStages['pausePackage']> {
    return this.#continuation.pausePackage(...args)
  }

  resumeReceive(
    ...args: Parameters<WorkspaceContinuationStages['resumeReceive']>
  ): ReturnType<WorkspaceContinuationStages['resumeReceive']> {
    return this.#continuation.resumeReceive(...args)
  }

  restoreReceiveContinuation(
    ...args: Parameters<WorkspaceContinuationStages['restoreReceiveContinuation']>
  ): ReturnType<WorkspaceContinuationStages['restoreReceiveContinuation']> {
    return this.#continuation.restoreReceiveContinuation(...args)
  }

  resumePackage(
    ...args: Parameters<WorkspaceContinuationStages['resumePackage']>
  ): ReturnType<WorkspaceContinuationStages['resumePackage']> {
    return this.#continuation.resumePackage(...args)
  }

  discard(
    ...args: Parameters<WorkspaceCleanupStages['discard']>
  ): ReturnType<WorkspaceCleanupStages['discard']> {
    return this.#cleanup.discard(...args)
  }

  expireIfDue(
    ...args: Parameters<WorkspaceCleanupStages['expireIfDue']>
  ): ReturnType<WorkspaceCleanupStages['expireIfDue']> {
    return this.#cleanup.expireIfDue(...args)
  }

  retryTerminalCleanup(
    ...args: Parameters<WorkspaceCleanupStages['retryTerminalCleanup']>
  ): ReturnType<WorkspaceCleanupStages['retryTerminalCleanup']> {
    return this.#cleanup.retryTerminalCleanup(...args)
  }
}

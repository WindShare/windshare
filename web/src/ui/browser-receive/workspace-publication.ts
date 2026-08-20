import type { OutputDiagnosticsPorts } from '../../output/diagnostics'
import { removeOriginPrivateWorkspaceNamespace, type OriginPrivateWorkspaceNamespace } from '../../output/origin-private/namespace'
import type { OriginPrivateRetainedArtifactBackend } from '../../output/origin-private/session'
import { BrowserHandoffNotStartedError, createWindowBrowserHandoffPublisher } from '../../output/portable/browser-download'
import { createPackagedArtifactHandoffPublisher } from '../../output/portable/packaged-handoff'
import { artifactRequestedName } from '../../output/planning'
import type { ReopenedWorkspaceOperation } from '../../output/resume/reopen-authority'
import type {
  WorkspaceCheckpointCleanupObservation,
  WorkspaceOwnedCleanupPort,
  WorkspaceOwnedObjectCleanupObservation,
} from '../../output/workspace/cleanup'
import type { ReceiveOperationRepository } from '../../output/workspace/repository'
import type { ReceiveLifecycleState } from '../../output/workspace/state'
import {
  createV2PlanExecutionAuthority,
  type V2PlanExecutionRouteRegistry,
} from '../../transfer/settlement/v2-plan-authority'
import { createOperationID, type ReceiveIntent } from '../../transfer/intent'
import type { V2PlanExecutionAuthority } from '../../transfer/output-session'
import type { BrowserReceiveWindow } from './contracts'
import { unavailableRoute } from './shared'
import type { WorkspaceReceiveOperation } from './workspace-operation'

type RetainedWorkspaceHandoffOperation =
  Pick<ReopenedWorkspaceOperation, 'intent' | 'lifecycle' | 'stages'>

export async function handoffRetainedWorkspacePackage(
  windowPort: BrowserReceiveWindow,
  operation: RetainedWorkspaceHandoffOperation,
  backend: OriginPrivateRetainedArtifactBackend,
  diagnostics?: OutputDiagnosticsPorts,
): Promise<ReceiveLifecycleState> {
  const { lifecycle } = operation
  if (lifecycle.kind !== 'waiting-to-save' &&
      !(lifecycle.kind === 'download-started' && lifecycle.attemptKind === 'workspace')) {
    throw unavailableRoute()
  }
  const artifact = await operation.stages.readRetainedPackage()
  const attempt = await operation.stages.startHandoff({
    package: artifact,
    publicationAttemptId: createOperationID(),
    suggestedName: artifactRequestedName(operation.intent.artifact),
    packagedFileSupported: true,
  })
  const retryableUntil = lifecycle.kind === 'waiting-to-save'
    ? lifecycle.expiresAt
    : lifecycle.retryableUntil
  try {
    const publisher = createPackagedArtifactHandoffPublisher({
      packages: backend.packagedArtifacts,
      browser: createWindowBrowserHandoffPublisher(
        windowPort,
        undefined,
        diagnostics,
      ),
      File: windowPort.File,
    })
    const started = await publisher.handoff({ artifact, attempt, retryableUntil })
    return (await operation.stages.recordHandoffStarted({
      package: artifact,
      attempt,
      urlLeaseStartedAt: started.urlLeaseStartedAt,
      urlLeaseEndsAt: started.urlLeaseEndsAt,
    })).state
  } catch (error) {
    return error instanceof BrowserHandoffNotStartedError
      ? await operation.stages.recordHandoffNotStarted({
          package: artifact,
          attempt,
          reason: error.externalAttemptReason,
        })
      : await operation.stages.recordHandoffUnknown({
          package: artifact,
          attempt,
          lastVerifiedRecordDigest: artifact.digest,
        })
  }
}

export async function workspacePlanAuthority(
  intent: ReceiveIntent,
  owner: WorkspaceReceiveOperation,
): Promise<V2PlanExecutionAuthority> {
  const routes: V2PlanExecutionRouteRegistry = {
    ...(intent.artifact.kind === 'original-file'
      ? {
          workspaceOriginal: {
            admit: (boundIntent, evidence, signal) => owner.admitOriginal(boundIntent, evidence, signal),
          },
        }
      : {}),
    ...(intent.artifact.kind === 'zip-archive'
      ? {
          workspaceZip: {
            prepare: (boundIntent, evidence, signal) => owner.prepareZip(boundIntent, evidence, signal),
          },
        }
      : {}),
    lifecycle: owner,
  }
  return createV2PlanExecutionAuthority({ intent, routes })
}

export class NamespaceOnlyCleanupPort implements WorkspaceOwnedCleanupPort {
  readonly #namespace: OriginPrivateWorkspaceNamespace
  readonly #repository: ReceiveOperationRepository
  readonly #intent: ReceiveIntent

  constructor(
    namespace: OriginPrivateWorkspaceNamespace,
    repository: ReceiveOperationRepository,
    intent: ReceiveIntent,
  ) {
    this.#namespace = namespace
    this.#repository = repository
    this.#intent = intent
  }

  removeOwnedObject(): Promise<WorkspaceOwnedObjectCleanupObservation> {
    return Promise.resolve(Object.freeze({ kind: 'ownership-unknown' }))
  }

  async removeFileCheckpoints(input: {
    readonly operationId: string
    readonly receiveIntentDigest: string
  }): Promise<WorkspaceCheckpointCleanupObservation> {
    if (input.operationId !== this.#intent.operationId ||
        input.receiveIntentDigest !== this.#intent.digest) {
      return Object.freeze({ kind: 'ownership-unknown' })
    }
    try {
      await removeOriginPrivateWorkspaceNamespace(this.#namespace, this.#repository)
      return Object.freeze({ kind: 'clean', removedRecordDigests: Object.freeze([]) })
    } catch {
      return Object.freeze({ kind: 'ownership-unknown' })
    }
  }
}

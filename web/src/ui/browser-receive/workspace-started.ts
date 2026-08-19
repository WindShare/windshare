import {
  acquireBrowserReceiveOperationLease,
  type BrowserReceiveOperationLease,
} from '../../output/browser/session-lease'
import { IndexedDbReceiveOperationRepository } from '../../output/browser/indexeddb-repository'
import {
  emitOutputTrace,
  outputTraceEvent,
  recordOutputException,
  type OutputDiagnosticsPorts,
} from '../../output/diagnostics'
import {
  openOriginPrivateWorkspaceNamespace,
  removeOriginPrivateWorkspaceNamespace,
  type OriginPrivateWorkspaceNamespace,
} from '../../output/origin-private/namespace'
import type { AcquiredMaterializationAuthority, ArtifactAction } from '../../output/planning'
import {
  WorkspaceOperationStages,
  type WorkspaceContentRequestCounter,
  type WorkspaceStageTraceListener,
} from '../../output/workspace/stages'
import {
  createOperationID,
  createWorkspaceBinding,
  createWorkspaceID,
  type ReceiveIntent,
} from '../../transfer/intent'
import type { V2BoundReceiveOperation, V2StartedArtifactAuthority } from '../v2-receive-runtime'
import type { BrowserReceiveWindow } from './contracts'
import { randomAuthorityReference, requireWorkspaceAction } from './shared'
import { WorkspaceReceiveOperation } from './workspace-operation'

const ZERO_CONTENT_REQUESTS: WorkspaceContentRequestCounter = Object.freeze({ count: () => 0n })

export class StartedWorkspaceReceive implements V2StartedArtifactAuthority {
  readonly #window: BrowserReceiveWindow
  readonly #action: ArtifactAction
  readonly #trace: WorkspaceStageTraceListener | undefined
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  #released = false
  #claimed = false

  constructor(
    windowPort: BrowserReceiveWindow,
    action: ArtifactAction,
    trace: WorkspaceStageTraceListener | undefined,
    diagnostics?: OutputDiagnosticsPorts,
  ) {
    this.#window = windowPort
    this.#action = action
    this.#trace = trace
    this.#diagnostics = diagnostics
  }

  async finalize(
    freezeIntent: (acquired: AcquiredMaterializationAuthority) => Promise<ReceiveIntent>,
    signal: AbortSignal,
  ): Promise<V2BoundReceiveOperation> {
    this.#claim()
    signal.throwIfAborted()
    const action = requireWorkspaceAction(this.#action)
    const operationId = createOperationID()
    const workspace = await createWorkspaceBinding({
      operationId,
      workspaceId: createWorkspaceID(),
      artifact: action.artifact,
      repositoryRef: randomAuthorityReference(),
    })
    const intent = await freezeIntent(Object.freeze({
      kind: 'workspace-binding',
      workspaceOfferId: action.plan.workspace.id,
      workspace,
    }))
    signal.throwIfAborted()
    this.#requireLive()
    const repository = await IndexedDbReceiveOperationRepository.open().catch((error: unknown) => {
      observeWorkspaceReservationFailure(this.#diagnostics, error)
      throw error
    })
    let lease: BrowserReceiveOperationLease | undefined
    let namespace: OriginPrivateWorkspaceNamespace | undefined
    try {
      namespace = await openOriginPrivateWorkspaceNamespace({
        receiveIntent: intent,
        repository,
        storage: this.#window.navigator.storage,
      })
      lease = await acquireBrowserReceiveOperationLease(repository, intent.operationId)
      const stages = await WorkspaceOperationStages.open({
        repository,
        receiveIntent: intent,
        leaseId: lease.leaseId,
        clock: Date.now,
        contentRequests: ZERO_CONTENT_REQUESTS,
        ...(this.#trace === undefined ? {} : { onTrace: this.#trace }),
        ...(this.#diagnostics === undefined ? {} : { diagnostics: this.#diagnostics }),
      })
      const runtime = await WorkspaceReceiveOperation.create({
        windowPort: this.#window,
        intent,
        repository,
        namespace,
        lease,
        stages,
        ...(this.#trace === undefined ? {} : { trace: this.#trace }),
        ...(this.#diagnostics === undefined ? {} : { diagnostics: this.#diagnostics }),
      })
      signal.throwIfAborted()
      emitOutputTrace(this.#diagnostics?.trace, () =>
        outputTraceEvent('output_reservation', {
          backend: 'origin_private',
          transition: 'acquired',
        }))
      return runtime
    } catch (error) {
      return releaseFailedWorkspaceActivation(
        namespace,
        repository,
        lease,
        this.#diagnostics,
        error,
      )
    }
  }

  release(): void {
    this.#released = true
  }

  #claim(): void {
    if (this.#claimed) {
      throw new DOMException('Workspace artifact authority was already finalized', 'InvalidStateError')
    }
    this.#claimed = true
    this.#requireLive()
  }

  #requireLive(): void {
    if (this.#released) throw new DOMException('Artifact authority was released', 'AbortError')
  }
}

async function releaseFailedWorkspaceActivation(
  namespace: OriginPrivateWorkspaceNamespace | undefined,
  repository: IndexedDbReceiveOperationRepository,
  lease: BrowserReceiveOperationLease | undefined,
  diagnostics: OutputDiagnosticsPorts | undefined,
  error: unknown,
): Promise<never> {
  observeWorkspaceReservationFailure(diagnostics, error)
  const cleanupFailures: unknown[] = []
  if (namespace !== undefined) {
    try {
      await removeOriginPrivateWorkspaceNamespace(namespace, repository)
    } catch (cleanupFailure) {
      cleanupFailures.push(cleanupFailure)
      recordOutputException(
        diagnostics?.failures?.cleanup,
        cleanupFailure,
        { recoveryDisposition: 'needs_attention' },
      )
    }
  }
  try {
    await lease?.release()
  } catch (cleanupFailure) {
    cleanupFailures.push(cleanupFailure)
    recordOutputException(diagnostics?.failures?.cleanup, cleanupFailure)
  }
  try {
    repository.close()
  } catch (cleanupFailure) {
    cleanupFailures.push(cleanupFailure)
    recordOutputException(diagnostics?.failures?.cleanup, cleanupFailure)
  }
  if (cleanupFailures.length !== 0) {
    emitOutputTrace(diagnostics?.trace, () =>
      outputTraceEvent('cleanup', {
        backend: 'origin_private',
        transition: 'failed',
      }))
    throw new AggregateError(
      [error, ...cleanupFailures],
      'Workspace output reservation failed and could not release all authorities',
      { cause: error },
    )
  }
  throw error
}

function observeWorkspaceReservationFailure(
  diagnostics: OutputDiagnosticsPorts | undefined,
  error: unknown,
): void {
  recordOutputException(diagnostics?.failures?.outputReservation, error)
  emitOutputTrace(diagnostics?.trace, () =>
    outputTraceEvent('output_reservation', {
      backend: 'origin_private',
      transition: 'failed',
    }))
}

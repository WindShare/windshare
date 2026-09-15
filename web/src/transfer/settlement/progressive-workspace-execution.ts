import { createProgressiveZipOutput } from '../../output/progressive-zip/output'
import type { ProgressiveZipArchive } from '../../output/progressive-zip/archive'
import type { TaskCheckpoint } from '../../output/origin-private/task-checkpoint/model'
import type { ReceiveLifecycleState } from '../../output/workspace/state'
import type { ReceiveContentWarning } from '../../output/workspace/lifecycle/content-warning'
import type { ReceiveIntent } from '../intent'
import type {
  OutputSessionIdentity, PlanPauseRequest, PlanSettlementRequest, WorkspaceExecution,
} from '../output-session'
import type { SuccessfulTransferWorkerSettlement } from '../outcome'
import { WorkspaceSettlementOwner } from './workspace-settlement-owner'

export type ProgressivePauseEvidence =
  | Readonly<{ kind: 'checkpoint-committed'; checkpoint: TaskCheckpoint }>
  | Readonly<{ kind: 'last-committed-checkpoint'; checkpoint: TaskCheckpoint; failure: unknown }>

export interface ProgressiveWorkspaceSettlement {
  pause(request: PlanPauseRequest, evidence: ProgressivePauseEvidence, signal: AbortSignal): Promise<ReceiveLifecycleState>
  settle(request: PlanSettlementRequest<SuccessfulTransferWorkerSettlement>, checkpoint: TaskCheckpoint, signal: AbortSignal): Promise<ReceiveLifecycleState>
}

export async function createProgressiveWorkspaceExecution(input: {
  readonly intent: ReceiveIntent
  readonly archive: ProgressiveZipArchive
  readonly outputIdentity: OutputSessionIdentity
  readonly settlement: ProgressiveWorkspaceSettlement
}): Promise<WorkspaceExecution> {
  const ports = await createProgressiveZipOutput({
    intent: input.intent, archive: input.archive, identity: input.outputIdentity,
  })
  const owner = new WorkspaceSettlementOwner()
  let discoveryComplete = false
  return Object.freeze({
    planKind: 'workspace-then-publish' as const,
    ...ports,
    discoveryGeneration: async (directoryId: string, generation: string, sourcePath: readonly string[], signal: AbortSignal) => {
      signal.throwIfAborted()
      await input.archive.pinDirectory(directoryId, generation, sourcePath)
    },
    discoveryComplete: async (signal: AbortSignal) => {
      signal.throwIfAborted()
      // Commit closure with the worker outcome, so recovery cannot outrun a missing-file warning.
      discoveryComplete = true
    },
    pause: (request: PlanPauseRequest, signal: AbortSignal) => owner.pause(async () => {
      let evidence: ProgressivePauseEvidence
      try {
        if (discoveryComplete) {
          const completedFileCount = request.materialization.fileCount
          const selectedFileCount = request.selectionFacts.discoveredFileCount
          const contentWarning: ReceiveContentWarning | undefined = completedFileCount < selectedFileCount
            ? { kind: 'partial-zip', completedFileCount, selectedFileCount,
              missingFiles: ports.missingFiles().slice(0, Number(selectedFileCount - completedFileCount)) }
            : undefined
          await input.archive.markDiscoveryComplete(contentWarning)
        }
        await input.archive.checkpoint('task-pause')
        evidence = { kind: 'checkpoint-committed', checkpoint: input.archive.state }
      } catch (failure) {
        // File-level pause still fails its exact accepted-cut contract. Task recovery
        // independently retains only the previously committed, store-verified generation.
        evidence = { kind: 'last-committed-checkpoint', checkpoint: input.archive.state, failure }
      }
      await input.archive.close()
      return input.settlement.pause(request, Object.freeze(evidence), signal)
    }),
    settle: (request: PlanSettlementRequest<SuccessfulTransferWorkerSettlement>, signal: AbortSignal) => owner.settle(async () => {
      signal.throwIfAborted()
      if (discoveryComplete) await input.archive.markDiscoveryComplete()
      const checkpoint = await input.archive.finalize(signal)
      return input.settlement.settle(request, checkpoint, signal)
    }),
  })
}

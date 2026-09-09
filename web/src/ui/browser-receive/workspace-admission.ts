import { TargetOwnershipUnknownError } from '../../output/persistent-tree/errors'
import type { ReceiveLifecycleState } from '../../output/workspace/state'
import type { WorkspaceUsage } from '../v2-lifecycle-presentation'
import type { V2LifecycleMutation } from '../v2-receive-runtime'
import { isWorkspaceTerminal } from './shared'

export type WorkspaceReceiveAdmission =
  | Readonly<{ kind: 'fresh' }>
  | Readonly<{
      kind: 'continuation'
      restore: () => Promise<ReceiveLifecycleState>
    }>

type ExecutionAdmission =
  | Readonly<{ kind: 'pending'; origin: WorkspaceReceiveAdmission }>
  | Readonly<{ kind: 'admitted' }>

interface WorkspaceExecutionAdmissionSettlementPort {
  readonly operationId: string
  readonly currentLifecycle: () => Promise<ReceiveLifecycleState>
  readonly discard: () => Promise<V2LifecycleMutation>
  readonly recordUnknown: () => Promise<Extract<ReceiveLifecycleState, { kind: 'needs-attention' }>>
  readonly workspaceUsage: (state: ReceiveLifecycleState) => WorkspaceUsage | null
}

export class WorkspaceExecutionAdmissionSettlement {
  readonly #port: WorkspaceExecutionAdmissionSettlementPort
  #admission: ExecutionAdmission
  #settlement: Promise<V2LifecycleMutation> | undefined

  constructor(port: WorkspaceExecutionAdmissionSettlementPort, origin: WorkspaceReceiveAdmission) {
    this.#port = port
    this.#admission = { kind: 'pending', origin }
  }

  beginContinuation(restore: () => Promise<ReceiveLifecycleState>): void {
    this.#admission = { kind: 'pending', origin: { kind: 'continuation', restore } }
    this.#settlement = undefined
  }

  markExecutionAdmitted(): void {
    this.#admission = { kind: 'admitted' }
  }

  settle(reason?: unknown): Promise<V2LifecycleMutation> {
    this.#settlement ??= this.#settle(reason).catch(error => {
      this.#settlement = undefined
      throw error
    })
    return this.#settlement
  }

  async #settle(reason?: unknown): Promise<V2LifecycleMutation> {
    if (reason instanceof TargetOwnershipUnknownError) {
      return this.#settleOwnershipUnknown(reason)
    }
    const current = await this.#port.currentLifecycle()
    if (isWorkspaceTerminal(current) || isStable(current)) return this.#mutation(current)
    const admission = this.#admission
    if (admission.kind === 'pending') {
      // Retained bytes belong to the operation, even before this attempt opens execution.
      if (admission.origin.kind === 'continuation') {
        const lifecycle = await admission.origin.restore()
        if (lifecycle.operationId !== this.#port.operationId || !isStable(lifecycle)) {
          throw new TypeError('Workspace continuation did not restore its owned stable state')
        }
        return this.#mutation(lifecycle)
      }
      if (current.kind === 'intent-frozen' || current.kind === 'preparing' ||
          current.kind === 'receiving') return this.#port.discard()
    }
    return this.#mutation(await this.#port.recordUnknown())
  }

  #mutation(lifecycle: ReceiveLifecycleState): V2LifecycleMutation {
    return Object.freeze({ lifecycle, workspaceUsage: this.#port.workspaceUsage(lifecycle) })
  }

  async #settleOwnershipUnknown(reason: TargetOwnershipUnknownError): Promise<V2LifecycleMutation> {
    if (reason.operationId !== null && reason.operationId !== this.#port.operationId) {
      throw new TypeError('Workspace admission ownership evidence belongs to another operation', {
        cause: reason,
      })
    }
    return this.#mutation(await this.#port.recordUnknown())
  }
}

function isStable(state: ReceiveLifecycleState): boolean {
  return state.kind === 'resumable-receive' || state.kind === 'materialization-sealed' ||
    state.kind === 'resumable-package' || state.kind === 'artifact-sealed' ||
    state.kind === 'waiting-to-save' ||
    (state.kind === 'download-started' && state.attemptKind === 'workspace')
}

import { TargetOwnershipUnknownError } from '../../output/persistent-tree/errors'
import type { ReceiveLifecycleState } from '../../output/workspace/state'
import type { WorkspaceUsage } from '../v2-lifecycle-presentation'
import type { V2LifecycleMutation } from '../v2-receive-runtime'
import { isWorkspaceTerminal } from './shared'

export type WorkspaceReceiveAdmissionFallback = Extract<
  ReceiveLifecycleState,
  { kind: 'resumable-receive' }
>

interface WorkspaceExecutionAdmissionSettlementPort {
  readonly operationId: string
  readonly currentLifecycle: () => Promise<ReceiveLifecycleState>
  readonly restoreContinuation: (
    fallback: WorkspaceReceiveAdmissionFallback,
  ) => Promise<WorkspaceReceiveAdmissionFallback>
  readonly discard: () => Promise<V2LifecycleMutation>
  readonly recordUnknown: () => Promise<Extract<ReceiveLifecycleState, { kind: 'needs-attention' }>>
  readonly workspaceUsage: (state: ReceiveLifecycleState) => WorkspaceUsage | null
}

export class WorkspaceExecutionAdmissionSettlement {
  readonly #port: WorkspaceExecutionAdmissionSettlementPort
  #fallback: WorkspaceReceiveAdmissionFallback | undefined
  #executionAdmitted = false

  constructor(
    port: WorkspaceExecutionAdmissionSettlementPort,
    fallback?: WorkspaceReceiveAdmissionFallback,
  ) {
    this.#port = port
    this.#fallback = fallback
  }

  beginContinuation(fallback: WorkspaceReceiveAdmissionFallback): void {
    this.#fallback = fallback
    this.#executionAdmitted = false
  }

  markExecutionAdmitted(): void {
    this.#executionAdmitted = true
  }

  async settle(reason?: unknown): Promise<V2LifecycleMutation> {
    if (reason instanceof TargetOwnershipUnknownError) {
      return this.#settleOwnershipUnknown(reason)
    }
    const current = await this.#port.currentLifecycle()
    if (isWorkspaceTerminal(current) || isStable(current)) {
      return Object.freeze({
        lifecycle: current,
        workspaceUsage: this.#port.workspaceUsage(current),
      })
    }
    if (current.kind === 'receiving' && this.#fallback !== undefined &&
        !this.#executionAdmitted) {
      const lifecycle = await this.#port.restoreContinuation(this.#fallback)
      this.#fallback = undefined
      return Object.freeze({
        lifecycle,
        workspaceUsage: this.#port.workspaceUsage(lifecycle),
      })
    }
    const safelyUnopened = !this.#executionAdmitted &&
      (current.kind === 'intent-frozen' || current.kind === 'preparing' ||
       (current.kind === 'receiving' && this.#fallback === undefined))
    if (safelyUnopened) return this.#port.discard()
    const lifecycle = await this.#port.recordUnknown()
    return Object.freeze({
      lifecycle,
      workspaceUsage: this.#port.workspaceUsage(lifecycle),
    })
  }

  async #settleOwnershipUnknown(
    reason: TargetOwnershipUnknownError,
  ): Promise<V2LifecycleMutation> {
    if (reason.operationId !== null && reason.operationId !== this.#port.operationId) {
      throw new TypeError('Workspace admission ownership evidence belongs to another operation', {
        cause: reason,
      })
    }
    const lifecycle = await this.#port.recordUnknown()
    return Object.freeze({
      lifecycle,
      workspaceUsage: this.#port.workspaceUsage(lifecycle),
    })
  }
}

function isStable(state: ReceiveLifecycleState): boolean {
  return state.kind === 'resumable-receive' || state.kind === 'materialization-sealed' ||
    state.kind === 'resumable-package' || state.kind === 'artifact-sealed' ||
    state.kind === 'waiting-to-save' ||
    (state.kind === 'download-started' && state.attemptKind === 'workspace')
}

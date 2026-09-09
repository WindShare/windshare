import type { ReceiveLifecycleState } from '../../output/workspace/state'

type WorkspaceSettlement = () => Promise<ReceiveLifecycleState>

/** Validation precedes ownership; admitted terminal work keeps it until the promise drains. */
export class WorkspaceSettlementOwner {
  #completion: Promise<ReceiveLifecycleState> | undefined
  #pause: Promise<ReceiveLifecycleState> | undefined

  settle(run: WorkspaceSettlement): Promise<ReceiveLifecycleState> {
    if (this.#pause !== undefined) return this.#pause
    this.#completion ??= Promise.resolve().then(run)
    return this.#completion
  }

  pause(run: WorkspaceSettlement): Promise<ReceiveLifecycleState> {
    this.#pause ??= this.#pauseAfterCompletion(run)
    return this.#pause
  }

  async #pauseAfterCompletion(run: WorkspaceSettlement): Promise<ReceiveLifecycleState> {
    if (this.#completion !== undefined) {
      try {
        // A deadline cannot overwrite an artifact or handoff that actually completed.
        return await this.#completion
      } catch {
        // Only the drained failed attempt may yield authority to checkpoint recovery.
      }
    }
    return run()
  }
}

import type { V2OutputSettlementDeadline } from '../../../transfer/settlement/v2-output'
import type { V2ActiveReceiveControl } from '../actions'
import type { ReceiveRuntimeState } from '../state'

export interface ReceiveSettlementDeadlineOptions {
  readonly state: ReceiveRuntimeState
  readonly operationIsCurrent: () => boolean
}

/**
 * Keeps an execution deadline on the presentation side of the durability boundary.
 * Expiry changes only what the user waits for; the transfer still owns the original
 * terminal promise and therefore cannot publish a fabricated paused/stopped state.
 */
export class ReceiveSettlementDeadline implements V2OutputSettlementDeadline {
  readonly #state: ReceiveRuntimeState
  readonly #operationIsCurrent: () => boolean
  #control: V2ActiveReceiveControl | undefined

  constructor(options: ReceiveSettlementDeadlineOptions) {
    this.#state = options.state
    this.#operationIsCurrent = options.operationIsCurrent
  }

  begin(control: V2ActiveReceiveControl): void {
    this.#control = control
    if (!this.#operationIsCurrent()) return
    this.#state.updateReceiveInterruption(Object.freeze({ operation: control, phase: 'waiting' }))
  }

  cancel(control: V2ActiveReceiveControl): void {
    if (this.#control !== control) return
    this.#control = undefined
    if (this.#operationIsCurrent()) this.#state.updateReceiveInterruption(null)
  }

  schedule(delayMilliseconds: number, expire: () => void): Readonly<{ cancel(): void }> {
    const timer = setTimeout(() => {
      if (this.#operationIsCurrent()) {
        this.#state.updateReceiveInterruption(Object.freeze({
          operation: this.#control ?? 'finish', phase: 'background',
        }))
      }
      expire()
    }, delayMilliseconds)
    return Object.freeze({ cancel: () => clearTimeout(timer) })
  }
}

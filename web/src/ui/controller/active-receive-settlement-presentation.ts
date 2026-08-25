import type { V2OutputSettlementDeadline } from '../../transfer/settlement/v2-output'
import type { V2ActiveReceiveControl } from '../v2-lifecycle-presentation'
import type { V2OutputPresentationController } from '../v2-output'

export interface ActiveReceiveSettlementPresentationOptions {
  readonly outputs: V2OutputPresentationController
  readonly operationIsCurrent: () => boolean
}

/**
 * Keeps an execution deadline on the presentation side of the durability boundary.
 * Expiry changes only what the user waits for; the transfer still owns the original
 * terminal promise and therefore cannot publish a fabricated paused/stopped state.
 */
export class ActiveReceiveSettlementPresentation implements V2OutputSettlementDeadline {
  readonly #outputs: V2OutputPresentationController
  readonly #operationIsCurrent: () => boolean
  #control: V2ActiveReceiveControl | undefined

  constructor(options: ActiveReceiveSettlementPresentationOptions) {
    this.#outputs = options.outputs
    this.#operationIsCurrent = options.operationIsCurrent
  }

  begin(control: V2ActiveReceiveControl): void {
    this.#control = control
    if (!this.#operationIsCurrent()) return
    this.#outputs.updateReceiveInterruption(Object.freeze({ control, phase: 'waiting' }))
  }

  cancel(control: V2ActiveReceiveControl): void {
    if (this.#control !== control) return
    this.#control = undefined
    if (this.#operationIsCurrent()) this.#outputs.updateReceiveInterruption(null)
  }

  schedule(delayMilliseconds: number, expire: () => void): Readonly<{ cancel(): void }> {
    const timer = setTimeout(() => {
      const control = this.#control
      if (control !== undefined && this.#operationIsCurrent()) {
        this.#outputs.updateReceiveInterruption(Object.freeze({ control, phase: 'background' }))
      }
      expire()
    }, delayMilliseconds)
    return Object.freeze({ cancel: () => clearTimeout(timer) })
  }
}

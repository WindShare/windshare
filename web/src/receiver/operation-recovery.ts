const MAXIMUM_UNCHANGED_AVAILABILITY_RETRIES = 2
const INITIAL_OPERATION_RETRY_DELAY_MILLISECONDS = 100

export interface OperationAvailability {
  readonly generationId: number
  readonly revision: number
}

export type OperationRecoveryDecision =
  | Readonly<{ transition: 'retry_available_lanes' | 'wait_for_generation' | 'exhausted' }>
  | Readonly<{ transition: 'wait_for_availability'; delayMilliseconds: number }>

/** Retry pressure belongs to one observed connection set, not the lifetime of a session. */
export class OperationRecovery {
  #availability: OperationAvailability | undefined
  #unchangedAvailabilityRetries = 0

  get unchangedAvailabilityRetries(): number { return this.#unchangedAvailabilityRetries }

  beginAttempt(availability: OperationAvailability): void {
    if (!this.#sameAvailability(availability)) this.#unchangedAvailabilityRetries = 0
    this.#availability = availability
  }

  decide(current: OperationAvailability | undefined): OperationRecoveryDecision {
    if (current === undefined || current.generationId !== this.#availability?.generationId) {
      return { transition: 'wait_for_generation' }
    }
    if (!this.#sameAvailability(current)) return { transition: 'retry_available_lanes' }
    if (this.#unchangedAvailabilityRetries >= MAXIMUM_UNCHANGED_AVAILABILITY_RETRIES) {
      return { transition: 'exhausted' }
    }
    // Congestion can clear without a lane event. Yield real time, but do not let
    // repeated failures on an unchanged connection set park the caller forever.
    const delayMilliseconds = INITIAL_OPERATION_RETRY_DELAY_MILLISECONDS *
      2 ** this.#unchangedAvailabilityRetries++
    return { transition: 'wait_for_availability', delayMilliseconds }
  }

  #sameAvailability(availability: OperationAvailability): boolean {
    return this.#availability?.generationId === availability.generationId &&
      this.#availability.revision === availability.revision
  }
}

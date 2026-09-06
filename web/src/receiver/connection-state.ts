import { V2RelayReceiverError } from '../transport/relay/v2-receiver'
import { V2_RELAY_ERROR } from '../transport/relay/v2-protocol'
import { RelayEndpointFailure } from './relay-race'
import { GenerationRecoveryExhaustedError } from './generation-recovery'
import { V2StaleShareInstanceError } from './v2-session-factory'

export type ReceiverConnectionSnapshot =
  | Readonly<{ kind: 'connected' }>
  | Readonly<{ kind: 'reconnecting' }>
  | Readonly<{ kind: 'ended'; reason: 'share-replaced' | 'share-stopped' }>
  | Readonly<{ kind: 'unavailable'; reason: 'recovery-exhausted' | 'protocol-failed' }>

export class ReceiverConnectionState {
  readonly #listeners = new Set<(snapshot: ReceiverConnectionSnapshot) => void>()
  #snapshot: ReceiverConnectionSnapshot = Object.freeze({ kind: 'connected' })

  subscribe(listener: (snapshot: ReceiverConnectionSnapshot) => void): () => void {
    this.#listeners.add(listener)
    try { listener(this.#snapshot) } catch { /* Connection observation is passive. */ }
    return () => this.#listeners.delete(listener)
  }

  connected(): void { this.#publish(Object.freeze({ kind: 'connected' })) }
  reconnecting(): void { this.#publish(Object.freeze({ kind: 'reconnecting' })) }

  failed(error: unknown): void {
    const reason = confirmedShareEnd(error)
    if (reason !== null) this.#publish(Object.freeze({ kind: 'ended', reason }))
    else this.#publish(Object.freeze({ kind: 'unavailable',
      reason: error instanceof GenerationRecoveryExhaustedError ? 'recovery-exhausted' : 'protocol-failed' }))
  }

  close(): void { this.#listeners.clear() }

  #publish(snapshot: ReceiverConnectionSnapshot): void {
    if (JSON.stringify(snapshot) === JSON.stringify(this.#snapshot)) return
    this.#snapshot = snapshot
    for (const listener of this.#listeners) {
      try { listener(snapshot) } catch { /* Observers cannot revoke session authority. */ }
    }
  }
}

function confirmedShareEnd(error: unknown): 'share-replaced' | 'share-stopped' | null {
  if (error instanceof RelayEndpointFailure) return confirmedShareEnd(error.cause)
  if (error instanceof V2StaleShareInstanceError) return 'share-replaced'
  if (error instanceof V2RelayReceiverError && error.relayError?.code === V2_RELAY_ERROR.stopped) return 'share-stopped'
  if (error instanceof AggregateError) {
    const reasons = error.errors.map(confirmedShareEnd)
    if (reasons.includes('share-replaced')) return 'share-replaced'
    if (reasons.length > 0 && reasons.every(reason => reason === 'share-stopped')) return 'share-stopped'
  }
  return null
}

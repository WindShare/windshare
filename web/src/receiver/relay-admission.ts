import type { RelayReceiverConnection } from '../transport/relay/receiver'

export const RELAY_POLICY_RETRY_MS = 60_000
export class RelayPolicyDeniedError extends Error {
  constructor(message = 'Official relaying is disabled for this share', options?: ErrorOptions) {
    super(message, options)
    this.name = 'RelayPolicyDeniedError'
  }
}
export interface RelayAdmission {
  prepare(base: string): Promise<void>
  refusal(base: string): Error | undefined
  observe(listener: () => void): () => void
}

function prepareAdmission(pending: Promise<void>, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const abort = () => reject(signal.reason)
    signal.addEventListener('abort', abort, { once: true })
    pending.then(resolve, reject).finally(() => signal.removeEventListener('abort', abort))
    if (signal.aborted) abort()
  })
}

/** The policy owns only its relay connection, never an independently usable P2P lane. */
export async function admittedRelay(
  admission: RelayAdmission | undefined, base: string, parent: AbortSignal,
  dial: (signal: AbortSignal) => Promise<RelayReceiverConnection>,
): Promise<RelayReceiverConnection> {
  parent.throwIfAborted()
  if (!admission) return dial(parent)
  await prepareAdmission(admission.prepare(base), parent)
  parent.throwIfAborted()
  const initial = admission.refusal(base)
  if (initial) throw initial
  const lifetime = new AbortController()
  let relay: RelayReceiverConnection | undefined
  const abort = () => lifetime.abort(parent.reason)
  const stop = admission.observe(() => {
    const refusal = admission.refusal(base)
    if (!refusal) return
    lifetime.abort(refusal)
    void relay?.close().catch(() => undefined)
  })
  parent.addEventListener('abort', abort, { once: true })
  if (parent.aborted) abort()
  try {
    lifetime.signal.throwIfAborted()
    relay = await dial(lifetime.signal)
    if (lifetime.signal.aborted) {
      await relay.close().catch(() => undefined)
      lifetime.signal.throwIfAborted()
    }
    relay.channel.unavailable.addEventListener('abort', stop, { once: true })
    if (relay.channel.unavailable.aborted) stop()
    return relay
  } catch (error) {
    stop()
    throw error
  } finally { parent.removeEventListener('abort', abort) }
}

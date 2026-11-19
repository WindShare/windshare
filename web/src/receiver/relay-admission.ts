import type { RelayReceiverConnection } from '../transport/relay/receiver'

export const RELAY_POLICY_RETRY_MS = 60_000
export class RelayPolicyDeniedError extends Error {
  constructor(message = 'Official relaying is disabled for this share', options?: ErrorOptions) {
    super(message, options)
    this.name = 'RelayPolicyDeniedError'
  }
}
export interface RelayAdmission {
  watch(base: string, deny: (reason: Error) => void): RelayAdmissionWatch
}

interface RelayAdmissionWatch {
  readonly ready: Promise<void>
  close(): void
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
  const lifetime = new AbortController()
  let relay: RelayReceiverConnection | undefined
  const watch = admission.watch(base, refusal => {
    lifetime.abort(refusal)
    void relay?.close().catch(() => undefined)
  })
  const stop = () => {
    lifetime.signal.removeEventListener('abort', stop)
    watch.close()
  }
  lifetime.signal.addEventListener('abort', stop, { once: true })
  const abort = () => lifetime.abort(parent.reason)
  parent.addEventListener('abort', abort, { once: true })
  if (parent.aborted) abort()
  try {
    await prepareAdmission(watch.ready, lifetime.signal)
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

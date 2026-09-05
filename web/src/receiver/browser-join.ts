import { openV2ShareDescriptor, type V2ShareDescriptor } from '../catalog/v2-records'
import type { Suite02CapabilityKey } from '../crypto/suite02-link'
import type { V2ProtocolTraceSource } from '../session/v2-diagnostics'
import { V2ReceiverSessionRuntime } from '../session/v2-runtime'
import { dialV2RelayReceiver } from '../transport/relay/v2-receiver'
import type { V2ProtocolGenerationCore } from './v2-session-factory'
import { firstUsableRelay } from './relay-race'

export interface BrowserRelayJoin extends V2ProtocolGenerationCore {
  readonly descriptor: V2ShareDescriptor
}

export function joinBrowserRelays(
  relayBases: readonly string[],
  capability: Suite02CapabilityKey,
  signal: AbortSignal,
  protocolTrace?: V2ProtocolTraceSource,
): Promise<BrowserRelayJoin> {
  return firstUsableRelay(relayBases, signal, async (relayBase, attemptSignal) => {
    // Losing joins may settle after publication; they never borrow the winner's erased secret.
    const ownedCapability = { ...capability, readSecret: capability.readSecret.slice() }
    try {
      const relay = await dialV2RelayReceiver(relayBase, ownedCapability, { signal: attemptSignal })
      try {
        const descriptor = await openV2ShareDescriptor(relay.descriptorObject, ownedCapability)
        attemptSignal.throwIfAborted()
        const session = await V2ReceiverSessionRuntime.connect({
          descriptor,
          readSecret: ownedCapability.readSecret,
          initialChannel: relay.channel,
          signal: attemptSignal,
          ...(protocolTrace === undefined ? {} : { protocolTrace }),
        })
        return Object.freeze({ relayBase, relay, descriptor, session, relayLaneId: session.initialLaneId })
      } catch (error) {
        await relay.close().catch(() => undefined)
        throw error
      }
    } finally {
      ownedCapability.readSecret.fill(0)
    }
  }, async (core) => { await Promise.allSettled([core.session.close(), core.relay.close()]) })
}

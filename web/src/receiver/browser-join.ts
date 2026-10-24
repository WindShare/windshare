import { openShareDescriptor, type ShareDescriptor } from '../catalog/records'
import type { Suite02CapabilityKey } from '../crypto/suite02-link'
import type { ProtocolTraceSource } from '../session/diagnostics'
import { ReceiverSessionRuntime } from '../session/runtime'
import { dialRelayReceiver } from '../transport/relay/receiver'
import type { ProtocolGenerationCore } from './session-factory'
import { firstUsableRelay, RelayEndpointFailure } from './relay-race'
import { runInitialJoin, type InitialJoinOptions } from './initial-join'
import { isShareRecoveryFailure, isTerminalRecoveryFailure } from './recovery-failure'
import { observeRecovery } from './recovery-observation'
import { emitRelayHeartbeat, emitRelayDelivery } from '../diagnostics/trace/connection-payload'

export interface BrowserRelayJoin extends ProtocolGenerationCore {
  readonly descriptor: ShareDescriptor
}

export function joinBrowserRelays(
  relayBases: readonly string[],
  capability: Suite02CapabilityKey,
  signal: AbortSignal,
  protocolTrace?: ProtocolTraceSource,
  options: InitialJoinOptions = {},
): Promise<BrowserRelayJoin> {
  const disabled = new Map<string, RelayEndpointFailure>()
  return runInitialJoin({ ...options, signal,
    observe: observation => {
      observeRecovery(protocolTrace, { correlation: {}, generationId: 0, shareId: capability.shareId }, observation)
      options.observe?.(observation)
    }, connect: attemptSignal => {
    const eligible = relayBases.filter(base => !disabled.has(base))
    if (eligible.length === 0) throw new AggregateError([...disabled.values()], 'All relay endpoints rejected this share')
    return firstUsableRelay(eligible, attemptSignal, (relayBase, relaySignal) =>
      connect(relayBase, relaySignal).catch((cause: unknown) => {
        if (isTerminalRecoveryFailure(cause)) disabled.set(relayBase, new RelayEndpointFailure(relayBase, cause))
        throw cause
      }), close, isShareRecoveryFailure)
  }, close })

  async function connect(relayBase: string, attemptSignal: AbortSignal): Promise<BrowserRelayJoin> {
    // Losing joins may settle after publication; they never borrow the winner's erased secret.
    const ownedCapability = { ...capability, readSecret: capability.readSecret.slice() }
    try {
      let session: ReceiverSessionRuntime | undefined
      const relay = await dialRelayReceiver(relayBase, ownedCapability, { signal: attemptSignal,
        receiveTrace: event => emitRelayDelivery(protocolTrace, event, relayBase, () => ({
          correlation: session === undefined ? {} : { protocolSessionId: session.protocolSessionIdentity },
          shareId: capability.shareId,
        })),
        heartbeatTrace: event => emitRelayHeartbeat(protocolTrace, event, relayBase, () => ({
          correlation: session === undefined ? {} : { protocolSessionId: session.protocolSessionIdentity },
          shareId: capability.shareId,
        })),
      })
      try {
        const descriptor = await openShareDescriptor(relay.descriptorObject, ownedCapability, {
          observe: event => protocolTrace?.current?.({
            ...event, eventName: 'sender_verification', shareId: capability.shareId,
            correlation: session === undefined ? {} : { protocolSessionId: session.protocolSessionIdentity },
          }),
        })
        attemptSignal.throwIfAborted()
        session = await ReceiverSessionRuntime.connect({
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
  }
}

async function close(core: BrowserRelayJoin): Promise<void> {
  await Promise.allSettled([core.session.close(), core.relay.close()])
}

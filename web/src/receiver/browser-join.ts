import { openV2ShareDescriptor, type V2ShareDescriptor } from '../catalog/v2-records'
import type { Suite02CapabilityKey } from '../crypto/suite02-link'
import type { V2ProtocolTraceSource } from '../session/v2-diagnostics'
import { V2ReceiverSessionRuntime } from '../session/v2-runtime'
import { dialV2RelayReceiver } from '../transport/relay/v2-receiver'
import type { V2ProtocolGenerationCore } from './v2-session-factory'
import { firstUsableRelay, RelayEndpointFailure } from './relay-race'
import { runInitialJoin, type InitialJoinOptions } from './initial-join'
import { isShareRecoveryFailure, isTerminalRecoveryFailure } from './recovery-failure'
import { observeRecovery } from './recovery-observation'
import { emitRelayHeartbeat } from '../diagnostics/trace/connection-payload'

export interface BrowserRelayJoin extends V2ProtocolGenerationCore {
  readonly descriptor: V2ShareDescriptor
}

export function joinBrowserRelays(
  relayBases: readonly string[],
  capability: Suite02CapabilityKey,
  signal: AbortSignal,
  protocolTrace?: V2ProtocolTraceSource,
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
      let session: V2ReceiverSessionRuntime | undefined
      const relay = await dialV2RelayReceiver(relayBase, ownedCapability, { signal: attemptSignal,
        heartbeatTrace: event => emitRelayHeartbeat(protocolTrace, event, relayBase, () => ({
          correlation: session === undefined ? {} : { protocolSessionId: session.protocolSessionIdentity },
          shareId: capability.shareId,
        })),
      })
      try {
        const descriptor = await openV2ShareDescriptor(relay.descriptorObject, ownedCapability, {
          observe: event => protocolTrace?.current?.({
            ...event, eventName: 'sender_verification', shareId: capability.shareId,
            correlation: session === undefined ? {} : { protocolSessionId: session.protocolSessionIdentity },
          }),
        })
        attemptSignal.throwIfAborted()
        session = await V2ReceiverSessionRuntime.connect({
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

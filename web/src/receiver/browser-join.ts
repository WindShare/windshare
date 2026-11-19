import { openShareDescriptor, type ShareDescriptor } from '../catalog/records'
import type { Suite02CapabilityKey } from '../crypto/suite02-link'
import type { ProtocolTraceSource } from '../session/diagnostics'
import { ReceiverSessionRuntime } from '../session/runtime'
import { dialRelayReceiver } from '../transport/relay/receiver'
import type { ProtocolGenerationCore } from './session-factory'
import { RelayRecovery } from './relay-selection/recovery'
import { admittedRelay } from './relay-admission'
import type { RelayCandidateSource } from './relay-selection/candidates'
import { runInitialJoin, type InitialJoinOptions } from './initial-join'
import type { InitialJoinAttempt } from './connection-attempt/initial'
import { observeRecovery } from './recovery-observation'
import { emitRelayHeartbeat, emitRelayDelivery } from '../diagnostics/trace/connection-payload'

export interface BrowserRelayJoin extends ProtocolGenerationCore {
  readonly descriptor: ShareDescriptor
  readonly relayRecovery: RelayRecovery
}

export function joinBrowserRelays(
  relayBases: RelayCandidateSource,
  capability: Suite02CapabilityKey,
  signal: AbortSignal,
  protocolTrace?: ProtocolTraceSource,
  options: InitialJoinOptions = {},
): Promise<BrowserRelayJoin> {
  const relayRecovery = new RelayRecovery(relayBases, options.clock)
  return runInitialJoin({
    ...options, signal, close, relayRecovery,
    observe: observation => {
      observeRecovery(protocolTrace, { correlation: {}, generationId: 0, shareId: capability.shareId }, observation)
      options.observe?.(observation)
    },
    connect: attempt => relayRecovery.race(attempt.signal,
      (relayBase, relaySignal) => connect(relayBase, relaySignal, attempt), close),
  })

  async function connect(relayBase: string, attemptSignal: AbortSignal, attempt: InitialJoinAttempt): Promise<BrowserRelayJoin> {
    // Losing joins may settle after publication; they never borrow the winner's erased secret.
    const ownedCapability = { ...capability, readSecret: capability.readSecret.slice() }
    try {
      let session: ReceiverSessionRuntime | undefined
      const traceContext = () => ({
        correlation: session === undefined ? {} : { protocolSessionId: session.protocolSessionIdentity },
        shareId: capability.shareId,
      })
      const relay = await admittedRelay(relayRecovery.candidates.admission, relayBase, attemptSignal, signal => dialRelayReceiver(relayBase, ownedCapability, { signal,
        receiveTrace: event => emitRelayDelivery(protocolTrace, event, relayBase, traceContext),
        heartbeatTrace: event => emitRelayHeartbeat(protocolTrace, event, relayBase, traceContext),
      }))
      try {
        const descriptor = await openShareDescriptor(relay.descriptorObject, ownedCapability, {
          observe: event => protocolTrace?.current?.({
            ...event, eventName: 'sender_verification', shareId: capability.shareId,
            correlation: session === undefined ? {} : { protocolSessionId: session.protocolSessionIdentity },
          }),
        })
        attemptSignal.throwIfAborted()
        const untrack = attempt.trackHandshake(relayBase, relay.channel, attemptSignal)
        try {
          session = await ReceiverSessionRuntime.connect({
            descriptor,
            readSecret: ownedCapability.readSecret,
            initialChannel: relay.channel,
            signal: attemptSignal,
            ...(protocolTrace === undefined ? {} : { protocolTrace }),
          })
          return Object.freeze({ relayBase, relay, descriptor, session, relayLaneId: session.initialLaneId, relayRecovery })
        } finally { untrack() }
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

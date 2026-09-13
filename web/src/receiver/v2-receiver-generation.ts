import type { V2ShareDescriptor } from '../catalog/v2-records'
import {
  V2ReceiverConnectivity,
  type V2ConnectivityPolicy,
  type V2ContentLaneAdmissionObservation,
  type V2ContentLaneDetachmentObservation,
} from '../connectivity/v2-receiver-policy'
import type { OfferChannelFactory } from '../connectivity/peer-offer'
import type { V2ConnectivityTraceSource } from '../connectivity/diagnostics'
import type { V2PeerRecoveryDependencies } from '../connectivity/peer-set/path'
import {
  V2BlockBroker,
  V2BlockDispatchSequenceAuthority,
  V2LaneSet,
  type V2BlockDispatchObservation,
  type V2BlockRouteObservation,
} from '../content/v2-broker'
import { V2RevisionService, V2SessionBlockLane } from '../content/v2-session-services'
import { traceContentScheduling } from '../diagnostics/trace/content-scheduling'
import type { V2ProtocolTraceSource } from '../session/v2-diagnostics'
import type { V2ReceiverSessionRuntime } from '../session/v2-runtime'
import type { V2LaneChange } from '../session/v2-runtime-types'
import type { OperationAvailability } from './operation-recovery'
import type { ReceiverPathActivity } from './path-activity'
import { reconnectPhase, requireBackoff, waitingReconnectBackoff, type V2ReconnectClock } from './recovery-clock'
import { recoveryRetryAfter } from './recovery-failure'
import { observeRecovery } from './recovery-observation'
import type { RecoveryWake } from './recovery-wake'
import { ReceiverRelaySet } from './relay-set'
import type { V2ProtocolGenerationCore, V2ReceiverSessionFactory } from './v2-session-factory'
import type { V2ContentGeneration } from './v2-supervised-content'

export interface V2ReceiverGeneration extends V2ContentGeneration {
  readonly broker: V2BlockBroker
  readonly relays: ReceiverRelaySet
  readonly session: V2ReceiverSessionRuntime
  readonly connectivity: V2ReceiverConnectivity
  availability: OperationAvailability
  retired: boolean
  close(): Promise<void>
}

interface V2ReceiverGenerationDependencies {
  readonly descriptor: V2ShareDescriptor
  readonly factory: V2ReceiverSessionFactory
  readonly policy: V2ConnectivityPolicy
  readonly clock: V2ReconnectClock
  readonly recoveryWake: RecoveryWake
  readonly backoffMilliseconds: (attempt: number) => number
  readonly pathActivity: ReceiverPathActivity
  readonly offersFactory: (() => OfferChannelFactory) | undefined
  readonly randomBytes: ((length: number) => Uint8Array) | undefined
  readonly nativePeerUsable: (() => boolean) | undefined
  readonly protocolTrace: V2ProtocolTraceSource | undefined
  readonly connectivityTrace: V2ConnectivityTraceSource | undefined
  readonly peerRecovery: V2PeerRecoveryDependencies
  readonly onBlockDispatched: ((observation: V2BlockDispatchObservation) => void) | undefined
  readonly onBlockFetched: ((observation: V2BlockRouteObservation) => void) | undefined
  readonly onContentLaneAdmitted: ((observation: V2ContentLaneAdmissionObservation) => void) | undefined
  readonly onContentLaneDetached: ((observation: V2ContentLaneDetachmentObservation) => void) | undefined
  readonly onLaneChanged: (generation: V2ReceiverGeneration, change: V2LaneChange) => void
  readonly onRelayFailure: (error: unknown) => 'retry' | 'stop'
}

/**
 * Assembles one session's content and transport resources with matching teardown.
 * Publication and failure authority stay with the supervisor that owns the share.
 */
export class V2ReceiverGenerationFactory {
  readonly #options: V2ReceiverGenerationDependencies
  // Replacement sessions continue the joined share's single route-evidence stream.
  readonly #dispatchSequence = new V2BlockDispatchSequenceAuthority()

  constructor(options: V2ReceiverGenerationDependencies) {
    this.#options = options
  }

  create(generationId: number, core: V2ProtocolGenerationCore): V2ReceiverGeneration {
    const options = this.#options
    const lanes = new V2LaneSet(
      {
        dispatchSequence: this.#dispatchSequence,
        ...(options.onBlockDispatched === undefined
          ? {}
          : { onBlockDispatched: options.onBlockDispatched }),
        onRequestScheduled: fact => options.protocolTrace?.current?.({
          ...fact, eventName: 'request_scheduling',
          correlation: { protocolSessionId: core.session.protocolSessionIdentity,
            lane: { id: fact.laneId, epoch: fact.laneEpoch } },
        }),
        onBlockScheduled: (fact) =>
          traceContentScheduling(fact, core.session.protocolSessionIdentity, options.protocolTrace),
        onBlockFetched: (fact) => {
          options.pathActivity.fetched(generationId, fact)
          options.onBlockFetched?.(fact)
        },
      },
    )
    lanes.requests.add({ id: core.session.initialLaneId, epoch: core.session.keys.initialLaneEpoch, route: 'application-relay' })
    const brokerOwner: { current?: V2BlockBroker } = {}
    const readSecret = options.factory.copyReadSecret()
    let revisions: V2RevisionService | undefined
    let broker: V2BlockBroker | undefined
    let connectivity: V2ReceiverConnectivity | undefined
    let laneSecret: Uint8Array<ArrayBuffer> | undefined
    let unsubscribe: (() => void) | undefined
    let closeTask: Promise<void> | undefined
    try {
      revisions = new V2RevisionService(
        core.session,
        options.descriptor,
        readSecret,
        lanes,
        {
          onLeaseRetirement: fact => options.protocolTrace?.current?.({
            ...fact, eventName: 'lease_retirement',
            correlation: { protocolSessionId: core.session.protocolSessionIdentity },
          }),
          beforeLeaseRelease: (leaseId) => brokerOwner.current?.waitForLeaseIdle(leaseId) ??
            Promise.reject(new Error('Generation block broker is unavailable during lease release')),
        },
      )
      const revisionService = revisions
      broker = new V2BlockBroker(lanes, {
        validateDemand: (demand) => revisionService.leaseError(demand.leaseId),
      })
      brokerOwner.current = broker
      const contentSecret = readSecret.slice()
      laneSecret = contentSecret
      connectivity = new V2ReceiverConnectivity({
        policy: options.policy,
        session: core.session,
        lanes,
        relayLaneId: core.relayLaneId,
        createBlockLane: (laneId) => new V2SessionBlockLane(
          laneId,
          core.session,
          options.descriptor,
          contentSecret,
          revisionService,
        ),
        ...(options.offersFactory === undefined ? {} : { offers: options.offersFactory() }),
        ...(options.randomBytes === undefined ? {} : { randomBytes: options.randomBytes }),
        ...(options.nativePeerUsable === undefined
          ? {}
          : { nativePeerUsable: options.nativePeerUsable }),
        ...(options.connectivityTrace === undefined
          ? {}
          : { connectivityTrace: options.connectivityTrace }),
        peerRecovery: options.peerRecovery,
        onContentLaneAdmitted: (lane) => {
          options.pathActivity.admitted(generationId, lane)
          options.onContentLaneAdmitted?.(lane)
        },
        onContentLaneDetached: (lane) => {
          options.pathActivity.detached(generationId, lane)
          options.onContentLaneDetached?.(lane)
        },
      })
      const generation: V2ReceiverGeneration = {
        id: generationId,
        relays: new ReceiverRelaySet({
          initial: core,
          factory: options.factory,
          admit: (laneId) => connectivity!.addRelayLane(laneId),
          sleep: (attempt, signal, elapsed, error, relayBase) => {
            const phase = reconnectPhase(attempt, elapsed)
            const delay = Math.max(recoveryRetryAfter(error), phase === 'waiting'
              ? waitingReconnectBackoff() : requireBackoff(options.backoffMilliseconds(attempt)))
            observeRecovery(options.protocolTrace, { generationId, relayBase, shareInstanceId: options.descriptor.shareInstanceId,
              correlation: { protocolSessionId: core.session.protocolSessionIdentity } },
            { attempt, phase, transition: 'waiting', delayMilliseconds: delay, failure: error })
            return options.recoveryWake.sleep(options.clock, delay, signal)
          },
          now: () => options.clock.now(),
          observe: (relayBase, observation) => observeRecovery(options.protocolTrace,
            { generationId, relayBase, shareInstanceId: options.descriptor.shareInstanceId,
              correlation: { protocolSessionId: core.session.protocolSessionIdentity } }, observation),
          failure: options.onRelayFailure,
        }),
        session: core.session,
        lanes,
        revisions,
        broker,
        connectivity,
        availability: Object.freeze({ generationId, revision: 0 }),
        retired: false,
        close: () => {
          closeTask ??= closeGeneration(generation, contentSecret, unsubscribe)
          return closeTask
        },
      }
      unsubscribe = core.session.subscribeLaneChanges((change) =>
        options.onLaneChanged(generation, change))
      connectivity.beginBrowse()
      return generation
    } catch (error) {
      connectivity?.close().catch(() => undefined)
      broker?.close()
      revisions?.close()
      lanes.close()
      laneSecret?.fill(0)
      throw error
    } finally {
      readSecret.fill(0)
    }
  }
}

async function closeGeneration(
  generation: V2ReceiverGeneration,
  laneSecret: Uint8Array<ArrayBuffer>,
  unsubscribe: (() => void) | undefined,
): Promise<void> {
  generation.retired = true
  unsubscribe?.()
  laneSecret.fill(0)
  generation.broker.close()
  generation.revisions.close()
  generation.lanes.close()
  await Promise.allSettled([
    generation.connectivity.close(),
    generation.session.close(),
    generation.relays.close(),
  ])
}

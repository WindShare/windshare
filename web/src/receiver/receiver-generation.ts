import type { ShareDescriptor } from '../catalog/records'
import {
  ReceiverConnectivity,
  type ConnectivityPolicy,
  type ContentLaneAdmissionObservation,
  type ContentLaneDetachmentObservation,
} from '../connectivity/receiver-policy'
import type { OfferChannelFactory } from '../connectivity/peer-offer'
import type { ConnectivityTraceSource } from '../connectivity/diagnostics'
import type { PeerRecoveryDependencies } from '../connectivity/peer-set/path'
import {
  BlockBroker,
  BlockDispatchSequenceAuthority,
  LaneSet,
  type BlockDispatchObservation,
  type BlockRouteObservation,
} from '../content/broker'
import { RevisionService, SessionBlockLane } from '../content/session-services'
import { traceContentRetry, traceContentScheduling } from '../diagnostics/trace/content-scheduling'
import type { ProtocolTraceSource } from '../session/diagnostics'
import type { ReceiverSessionRuntime } from '../session/runtime'
import type { LaneChange } from '../session/runtime-types'
import type { OperationAvailability } from './operation-recovery'
import type { ReceiverPathActivity } from './path-activity'
import { reconnectPhase, requireBackoff, waitingReconnectBackoff, type ReconnectClock } from './recovery-clock'
import { recoveryRetryAfter } from './recovery-failure'
import { observeRecovery } from './recovery-observation'
import type { RecoveryWake } from './recovery-wake'
import { ReceiverRelaySet } from './relay-set'
import type { ProtocolGenerationCore, ReceiverSessionFactory } from './session-factory'
import type { ContentGeneration } from './supervised-content'

export interface ReceiverGeneration extends ContentGeneration {
  readonly broker: BlockBroker
  readonly relays: ReceiverRelaySet
  readonly session: ReceiverSessionRuntime
  readonly connectivity: ReceiverConnectivity
  availability: OperationAvailability
  retired: boolean
  close(): Promise<void>
}

interface ReceiverGenerationDependencies {
  readonly descriptor: ShareDescriptor
  readonly factory: ReceiverSessionFactory
  readonly policy: ConnectivityPolicy
  readonly clock: ReconnectClock
  readonly recoveryWake: RecoveryWake
  readonly backoffMilliseconds: (attempt: number) => number
  readonly pathActivity: ReceiverPathActivity
  readonly offersFactory: (() => OfferChannelFactory) | undefined
  readonly randomBytes: ((length: number) => Uint8Array) | undefined
  readonly nativePeerUsable: (() => boolean) | undefined
  readonly protocolTrace: ProtocolTraceSource | undefined
  readonly connectivityTrace: ConnectivityTraceSource | undefined
  readonly peerRecovery: PeerRecoveryDependencies
  readonly onBlockDispatched: ((observation: BlockDispatchObservation) => void) | undefined
  readonly onBlockFetched: ((observation: BlockRouteObservation) => void) | undefined
  readonly onContentLaneAdmitted: ((observation: ContentLaneAdmissionObservation) => void) | undefined
  readonly onContentLaneDetached: ((observation: ContentLaneDetachmentObservation) => void) | undefined
  readonly onLaneChanged: (generation: ReceiverGeneration, change: LaneChange) => void
  readonly onRelayFailure: (error: unknown) => 'retry' | 'stop'
}

/**
 * Assembles one session's content and transport resources with matching teardown.
 * Publication and failure authority stay with the supervisor that owns the share.
 */
export class ReceiverGenerationFactory {
  readonly #options: ReceiverGenerationDependencies
  // Replacement sessions continue the joined share's single route-evidence stream.
  readonly #dispatchSequence = new BlockDispatchSequenceAuthority()

  constructor(options: ReceiverGenerationDependencies) {
    this.#options = options
  }

  create(generationId: number, core: ProtocolGenerationCore): ReceiverGeneration {
    const options = this.#options
    const lanes = new LaneSet(
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
        onBlockRetry: fact => traceContentRetry(fact, core.session.protocolSessionIdentity, options.protocolTrace),
        onBlockFetched: (fact) => {
          options.pathActivity.fetched(generationId, fact)
          options.onBlockFetched?.(fact)
        },
      },
    )
    lanes.requests.add({ id: core.session.initialLaneId, epoch: core.session.keys.initialLaneEpoch, route: 'application-relay' })
    const brokerOwner: { current?: BlockBroker } = {}
    const readSecret = options.factory.copyReadSecret()
    let revisions: RevisionService | undefined
    let broker: BlockBroker | undefined
    let connectivity: ReceiverConnectivity | undefined
    let laneSecret: Uint8Array<ArrayBuffer> | undefined
    let unsubscribe: (() => void) | undefined
    let closeTask: Promise<void> | undefined
    try {
      revisions = new RevisionService(
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
      broker = new BlockBroker(lanes, {
        validateDemand: (demand) => revisionService.leaseError(demand.leaseId),
      })
      brokerOwner.current = broker
      const contentSecret = readSecret.slice()
      laneSecret = contentSecret
      connectivity = new ReceiverConnectivity({
        policy: options.policy,
        session: core.session,
        lanes,
        relayLaneId: core.relayLaneId,
        createBlockLane: (laneId) => new SessionBlockLane(
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
        onTurnObservation: (event) => options.pathActivity.turn(generationId, event),
        onContentLaneAdmitted: (lane) => {
          options.pathActivity.admitted(generationId, lane)
          options.onContentLaneAdmitted?.(lane)
        },
        onContentLaneDetached: (lane) => {
          options.pathActivity.detached(generationId, lane)
          options.onContentLaneDetached?.(lane)
        },
      })
      const generation: ReceiverGeneration = {
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
  generation: ReceiverGeneration,
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

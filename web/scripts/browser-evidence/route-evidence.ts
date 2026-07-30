import { type LaneIdentity } from './attempt-evidence.ts'
import {
  contractError,
  freezeRecord,
  requireArray,
  requireBoolean,
  requireCanonicalIdentity,
  requireEnum,
  requireExactKeys,
  requireRecord,
  requireSafeInteger,
} from './contract/json.ts'

export const MAIN_ROUTE_MODES = Object.freeze(['relay-only', 'hot-switch'] as const)
export const DISPATCH_ROUTES = Object.freeze(['relay', 'peer'] as const)

export interface RouteDispatchObservation {
  readonly observationSequence: number
  readonly kind: 'dispatch'
  readonly dispatchSequence: number
  readonly route: (typeof DISPATCH_ROUTES)[number]
  readonly lane: LaneIdentity
}

export interface PeerAdmissionObservation {
  readonly observationSequence: number
  readonly kind: 'peer-admitted'
  readonly sessionId: string
  readonly peerPathId: string
  readonly attemptId: string
  readonly lane: LaneIdentity
}

export interface RelayCutFenceObservation {
  readonly observationSequence: number
  readonly kind: 'relay-cut-fence'
  readonly dispatchSequenceBoundary: number
  readonly proxyAccepting: false
  readonly receiverRelayEligible: false
}

export type RouteObservation =
  | RouteDispatchObservation
  | PeerAdmissionObservation
  | RelayCutFenceObservation

export interface MainRouteEvidence {
  readonly mode: (typeof MAIN_ROUTE_MODES)[number]
  readonly observations: readonly RouteObservation[]
}

export function parseMainRouteEvidence(value: unknown): MainRouteEvidence | null {
  if (value === null) return null
  const evidence = requireRecord(value, 'main route evidence')
  requireExactKeys(evidence, ['mode', 'observations'], [], 'main route evidence')
  const mode = requireEnum(evidence.mode, MAIN_ROUTE_MODES, 'main route mode')
  const observations = Object.freeze(requireArray(
    evidence.observations,
    'main route observations',
  ).map((item, index) => parseRouteObservation(item, index)))
  validateObservationSequences(observations)
  if (mode === 'relay-only') validateRelayOnly(observations)
  else validateHotSwitch(observations)
  return freezeRecord({ mode, observations })
}

function parseRouteObservation(value: unknown, index: number): RouteObservation {
  const observation = requireRecord(value, `route observation ${index}`)
  const kind = requireEnum(
    observation.kind,
    ['dispatch', 'peer-admitted', 'relay-cut-fence'] as const,
    `route observation ${index} kind`,
  )
  const observationSequence = requireSafeInteger(
    observation.observationSequence,
    1,
    Number.MAX_SAFE_INTEGER,
    `route observation ${index} sequence`,
  )
  if (kind === 'dispatch') {
    requireExactKeys(
      observation,
      ['observationSequence', 'kind', 'dispatchSequence', 'route', 'lane'],
      [],
      `route dispatch observation ${index}`,
    )
    const route = requireEnum(observation.route, DISPATCH_ROUTES, `route dispatch ${index} route`)
    return freezeRecord({
      observationSequence,
      kind,
      dispatchSequence: requireSafeInteger(
        observation.dispatchSequence,
        1,
        Number.MAX_SAFE_INTEGER,
        `route dispatch ${index} sequence`,
      ),
      route,
      lane: parseLane(
        observation.lane,
        `route dispatch ${index} lane`,
        // Relay epoch zero is the protocol's real pre-admission identity. Peer
        // work is authenticated and therefore must carry an admitted epoch.
        route === 'relay' ? 'relay' : 'peer',
      ),
    })
  }
  if (kind === 'peer-admitted') {
    requireExactKeys(
      observation,
      ['observationSequence', 'kind', 'sessionId', 'peerPathId', 'attemptId', 'lane'],
      [],
      `peer admission observation ${index}`,
    )
    return freezeRecord({
      observationSequence,
      kind,
      sessionId: requireCanonicalIdentity(observation.sessionId, 'route admission session ID'),
      peerPathId: requireCanonicalIdentity(observation.peerPathId, 'route admission peer path ID'),
      attemptId: requireCanonicalIdentity(observation.attemptId, 'route admission attempt ID'),
      lane: parseLane(observation.lane, 'route admission lane', 'peer'),
    })
  }
  requireExactKeys(
    observation,
    [
      'observationSequence',
      'kind',
      'dispatchSequenceBoundary',
      'proxyAccepting',
      'receiverRelayEligible',
    ],
    [],
    `relay cut fence observation ${index}`,
  )
  if (
    requireBoolean(observation.proxyAccepting, 'relay cut proxy accepting') ||
    requireBoolean(observation.receiverRelayEligible, 'receiver relay eligibility')
  ) {
    contractError('completed relay cut fence must stop proxy admission and receiver relay eligibility')
  }
  return freezeRecord({
    observationSequence,
    kind,
    dispatchSequenceBoundary: requireSafeInteger(
      observation.dispatchSequenceBoundary,
      1,
      Number.MAX_SAFE_INTEGER,
      'relay cut dispatch sequence boundary',
    ),
    proxyAccepting: false as const,
    receiverRelayEligible: false as const,
  })
}

function validateObservationSequences(observations: readonly RouteObservation[]): void {
  let expectedDispatchSequence = 1
  for (let index = 0; index < observations.length; index += 1) {
    const observation = observations[index]
    if (observation === undefined || observation.observationSequence !== index + 1) {
      contractError('route observation sequence must be contiguous from one')
    }
    if (observation.kind === 'dispatch') {
      if (observation.dispatchSequence !== expectedDispatchSequence) {
        contractError('route dispatch sequence must be contiguous from one')
      }
      expectedDispatchSequence += 1
    }
  }
}

function validateRelayOnly(observations: readonly RouteObservation[]): void {
  if (
    observations.length === 0 ||
    observations.some((observation) => observation.kind !== 'dispatch' || observation.route !== 'relay')
  ) {
    contractError('relay-only evidence must contain only relay dispatch observations')
  }
}

function validateHotSwitch(observations: readonly RouteObservation[]): void {
  const admissions = observations.filter(
    (observation): observation is PeerAdmissionObservation => observation.kind === 'peer-admitted',
  )
  const fences = observations.filter(
    (observation): observation is RelayCutFenceObservation => observation.kind === 'relay-cut-fence',
  )
  if (admissions.length !== 1 || fences.length !== 1) {
    contractError('hot-switch evidence requires exactly one peer admission and relay cut fence')
  }
  const admission = admissions[0]
  const fence = fences[0]
  if (admission === undefined || fence === undefined || admission.observationSequence >= fence.observationSequence) {
    contractError('hot-switch peer admission must precede the relay cut fence')
  }
  const dispatches = observations.filter(
    (observation): observation is RouteDispatchObservation => observation.kind === 'dispatch',
  )
  const relayBeforeAdmission = dispatches.some(
    (dispatch) => dispatch.route === 'relay' &&
      dispatch.observationSequence < admission.observationSequence,
  )
  const peerBeforeAdmission = dispatches.some(
    (dispatch) => dispatch.route === 'peer' &&
      dispatch.observationSequence < admission.observationSequence,
  )
  const peerOnUnadmittedLane = dispatches.some(
    (dispatch) => dispatch.route === 'peer' && !sameLane(dispatch.lane, admission.lane),
  )
  const dispatchesBeforeFence = dispatches.filter(
    (dispatch) => dispatch.observationSequence < fence.observationSequence,
  )
  const lastBeforeFence = dispatchesBeforeFence.at(-1)
  const peerAfterFence = dispatches.some(
    (dispatch) => dispatch.route === 'peer' &&
      dispatch.observationSequence > fence.observationSequence &&
      dispatch.dispatchSequence > fence.dispatchSequenceBoundary &&
      sameLane(dispatch.lane, admission.lane),
  )
  const relayAfterFence = dispatches.some(
    (dispatch) => dispatch.route === 'relay' && dispatch.observationSequence > fence.observationSequence,
  )
  if (
    !relayBeforeAdmission || peerBeforeAdmission || peerOnUnadmittedLane || lastBeforeFence === undefined ||
    lastBeforeFence.dispatchSequence !== fence.dispatchSequenceBoundary ||
    !peerAfterFence || relayAfterFence
  ) {
    contractError('hot-switch evidence does not prove relay, admission, cut fence, and post-fence peer dispatch')
  }
}

function parseLane(value: unknown, label: string, authority: 'relay' | 'peer'): LaneIdentity {
  const lane = requireRecord(value, label)
  requireExactKeys(lane, ['laneId', 'laneEpoch'], [], label)
  return freezeRecord({
    laneId: requireSafeInteger(lane.laneId, 1, 0xffff_ffff, `${label} ID`),
    laneEpoch: requireSafeInteger(
      lane.laneEpoch,
      authority === 'relay' ? 0 : 1,
      authority === 'relay' ? 0 : 0xffff_ffff,
      `${label} epoch`,
    ),
  })
}

function sameLane(left: LaneIdentity, right: LaneIdentity): boolean {
  return left.laneId === right.laneId && left.laneEpoch === right.laneEpoch
}

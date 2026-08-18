import type { V2BrowserConnectivityAttemptDiagnostic } from '../../src/connectivity/diagnostics'
import type {
  HotSwitchLaneObservation,
  HotSwitchPageEvent,
  HotSwitchRecoveryControl,
  ObservedTransferFailure,
} from './hot-switch-contract'
import type { V2FrozenSelectionPolicy } from '../../src/catalog/v2-selection'
import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  OneShotRelease,
  OutputFence,
  PagePeerRecoveryHarness,
} from './hot-switch-page-transfer'
import type {
  ReceiveLifecycleState,
  ReceiveLifecycleStatePayload,
} from '../../src/output/workspace/state'
import type { ReceiveIntent } from '../../src/transfer/intent'
import type {
  BeginOutputFileResult,
  DirectAtomicExecution,
  OutputFileRequest,
  OutputFileTransaction,
  OutputSession,
  V2PlanExecutionAuthority,
} from '../../src/transfer/output-session'

const GATEWAY_MODULE_PATH = '/src/ui/v2-gateway.ts'
const INTENT_MODULE_PATH = '/src/transfer/intent.ts'
const OFFER_MODULE_PATH = '/src/connectivity/peer-offer.ts'
const PLAN_AUTHORITY_MODULE_PATH = '/src/transfer/settlement/v2-plan-authority.ts'
const PLANNING_MODULE_PATH = '/src/output/planning/index.ts'
const PROJECTION_MODULE_PATH = '/src/transfer/projection/index.ts'
const STREAM_MODULE_PATH = '/src/output/streams/single-file.ts'
const WORKSPACE_STATE_MODULE_PATH = '/src/output/workspace/state.ts'
const AUTHORITY_REFERENCE_BYTES = 32
const HOT_SWITCH_RECEIPT_DOMAIN = 'windshare/e2e/hot-switch-receipt/v1'
const STREAM_TARGET_OFFER_ID = 'hot-switch-managed-atomic-stream'
const TEXT_ENCODER = new TextEncoder()

type GatewayModule = typeof import('../../src/ui/v2-gateway')
type IntentModule = typeof import('../../src/transfer/intent')
type OfferModule = typeof import('../../src/connectivity/peer-offer')
type PlanAuthorityModule = typeof import('../../src/transfer/settlement/v2-plan-authority')
type PlanningModule = typeof import('../../src/output/planning')
type ProjectionModule = typeof import('../../src/transfer/projection')
type StreamModule = typeof import('../../src/output/streams/single-file')
type WorkspaceStateModule = typeof import('../../src/output/workspace/state')
type JoinedReceiver = Awaited<ReturnType<
  InstanceType<GatewayModule['V2BrowserReceiverGateway']>['join']
>>
type DownloadActivation = ReturnType<JoinedReceiver['beginDownloadConnectivity']>

export interface HotSwitchPageTransferRuntimeInput {
  readonly expectedHash: string
  readonly failureDiagnosticMaximumDepth: number
  readonly key: string
  readonly nativePeerUsable: boolean
  readonly peerRecovery?: HotSwitchRecoveryControl
  readonly rtcConfiguration: RTCConfiguration
  readonly runtimePath: string
  readonly transferBytes: number
}

interface HotSwitchWindow extends Window {
  __windshareAdvanceHotSwitchOutput?: () => void
  __windshareDetachHotSwitchPeer?: () => Promise<void>
  __windshareHotSwitchEvent?: (event: HotSwitchPageEvent) => Promise<void>
  __windshareReleaseHotSwitchAdmission?: () => void
  __windshareReleaseHotSwitchOutput?: () => void
  __windshareSealHotSwitchRelayCut?: () => Promise<void>
}

interface ProductModules {
  readonly gateway: GatewayModule
  readonly intent: IntentModule
  readonly offer: OfferModule
  readonly planAuthority: PlanAuthorityModule
  readonly planning: PlanningModule
  readonly projection: ProjectionModule
  readonly stream: StreamModule
  readonly workspaceState: WorkspaceStateModule
}

interface BoundDirectAtomicOperation {
  readonly intent: ReceiveIntent
  readonly plans: V2PlanExecutionAuthority
  readonly selection: V2FrozenSelectionPolicy
}

class EvidenceBridge {
  readonly #bridge: (event: HotSwitchPageEvent) => Promise<void>
  readonly #maximumFailureDepth: number
  #failure: string | undefined
  #queue = Promise.resolve()

  constructor(
    bridge: (event: HotSwitchPageEvent) => Promise<void>,
    maximumFailureDepth: number,
  ) {
    this.#bridge = bridge
    this.#maximumFailureDepth = maximumFailureDepth
  }

  publish(event: HotSwitchPageEvent): Promise<void> {
    this.#queue = this.#queue
      .then(() => this.#bridge(event))
      .catch((error: unknown) => {
        this.#failure ??= describeFailure(error, this.#maximumFailureDepth)
      })
    return this.#queue
  }

  async terminalFailure(): Promise<string | undefined> {
    // Observer callbacks deliberately do not block product control flow. The
    // terminal drains their serialized bridge so a rejected event cannot vanish.
    await this.#queue
    return this.#failure
  }

  describe(reason: unknown): string {
    return describeFailure(reason, this.#maximumFailureDepth)
  }
}

class RelayCutEvidence {
  readonly #bridge: EvidenceBridge
  readonly #activeRelayLanes = new Set<string>()
  #ineligibilityPublished = false
  #sealed = false

  constructor(bridge: EvidenceBridge) {
    this.#bridge = bridge
  }

  admit(observation: HotSwitchLaneObservation): void {
    if (observation.route === 'relay') this.#activeRelayLanes.add(laneKey(observation))
    this.#bridge.publish({ kind: 'lane-admitted', observation }).catch(() => undefined)
  }

  detach(observation: HotSwitchLaneObservation): void {
    if (observation.route === 'relay') this.#activeRelayLanes.delete(laneKey(observation))
    this.#bridge.publish({ kind: 'lane-detached', observation })
      .then(() => this.#publishIneligibility())
      .catch(() => undefined)
  }

  async seal(): Promise<void> {
    this.#sealed = true
    await this.#publishIneligibility()
  }

  async #publishIneligibility(): Promise<void> {
    if (
      !this.#sealed || this.#ineligibilityPublished || this.#activeRelayLanes.size !== 0
    ) return
    this.#ineligibilityPublished = true
    await this.#bridge.publish({ kind: 'relay-ineligible' })
  }
}

class DeliveryBuffer {
  readonly #chunks: Uint8Array[] = []

  outputSession(
    stream: StreamModule,
    outputSessionId: string,
    outputFence: OutputFence,
  ): DirectAtomicStreamOutput {
    const session = new stream.SingleFileStreamOutputSession(
      outputSessionId,
      new WritableStream<Uint8Array>({
        write: async (chunk) => {
          await outputFence.waitForWrite()
          this.#chunks.push(chunk.slice())
        },
      }),
    )
    return new DirectAtomicStreamOutput(session)
  }

  async snapshot(): Promise<{ readonly bytes: number; readonly sha256: string }> {
    const length = this.#chunks.reduce((total, chunk) => total + chunk.byteLength, 0)
    const bytes = new Uint8Array(length)
    let offset = 0
    for (const chunk of this.#chunks) {
      bytes.set(chunk, offset)
      offset += chunk.byteLength
    }
    const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', bytes))
    return Object.freeze({
      bytes: length,
      sha256: Array.from(digest, (byte) => byte.toString(16).padStart(2, '0')).join(''),
    })
  }
}

class DirectAtomicStreamOutput implements OutputSession {
  readonly identity
  readonly capabilities
  readonly #session: OutputSession
  #transaction: OutputFileTransaction | undefined

  constructor(session: OutputSession) {
    this.#session = session
    this.identity = session.identity
    this.capabilities = session.capabilities
  }

  async beginFile(
    request: OutputFileRequest,
    signal: AbortSignal,
  ): Promise<BeginOutputFileResult> {
    const opened = await this.#session.beginFile(request, signal)
    this.#transaction = opened.transaction
    return opened
  }

  async pauseActiveTransaction(reason: unknown): Promise<void> {
    // OutputSession intentionally owns no job-wide pause. The plan route retains
    // the one transaction it opened so rollback remains at the authority boundary.
    await this.#transaction?.pause(reason)
  }
}

export function startHotSwitchPageTransfer(input: HotSwitchPageTransferRuntimeInput): void {
  const hotSwitchWindow = window as HotSwitchWindow
  const exposedBridge = hotSwitchWindow.__windshareHotSwitchEvent
  if (exposedBridge === undefined) throw new Error('Hot-switch evidence bridge is unavailable')

  const bridge = new EvidenceBridge(exposedBridge, input.failureDiagnosticMaximumDepth)
  const relayCut = new RelayCutEvidence(bridge)
  const peerRelease = new OneShotRelease()
  const outputFence = new OutputFence(input.peerRecovery !== undefined)
  const peerHarness = new PagePeerRecoveryHarness(bridge, input.peerRecovery !== undefined)
  hotSwitchWindow.__windshareAdvanceHotSwitchOutput = () => outputFence.advance()
  hotSwitchWindow.__windshareDetachHotSwitchPeer = () => peerHarness.detachCurrentPeer()
  hotSwitchWindow.__windshareReleaseHotSwitchAdmission = () => peerHarness.releaseAdmission()
  hotSwitchWindow.__windshareReleaseHotSwitchOutput = () => outputFence.release()
  hotSwitchWindow.__windshareSealHotSwitchRelayCut = () => relayCut.seal()

  let runtimeTerminalPublished = false
  const transferTask = runTransfer(
    input,
    bridge,
    relayCut,
    peerRelease,
    outputFence,
    peerHarness,
    () => {
      runtimeTerminalPublished = true
    },
  )
  transferTask.catch(async (error: unknown) => {
    if (runtimeTerminalPublished) return
    runtimeTerminalPublished = true
    await bridge.publish({ kind: 'runtime-settled', error: bridge.describe(error) })
  }).catch(() => undefined)
}

async function runTransfer(
  input: HotSwitchPageTransferRuntimeInput,
  bridge: EvidenceBridge,
  relayCut: RelayCutEvidence,
  peerRelease: OneShotRelease,
  outputFence: OutputFence,
  peerHarness: PagePeerRecoveryHarness,
  markRuntimeTerminalPublished: () => void,
): Promise<void> {
  const modules = await loadProductModules()
  let joined: JoinedReceiver | undefined
  let activation: DownloadActivation | undefined
  const delivery = new DeliveryBuffer()
  let deliveryStarted = false
  let runtimeError: string | undefined

  try {
    const gateway = createGateway(input, modules, bridge, relayCut, peerRelease, peerHarness)
    joined = await gateway.join(input.key, window.location.href)
    const operation = await bindDirectAtomicOperation(
      input,
      joined,
      modules,
      delivery,
      outputFence,
    )
    activation = joined.beginDownloadConnectivity()
    deliveryStarted = true
    const result = await joined.transferJob(
      operation.plans,
      operation.intent,
      activation,
      { selection: operation.selection },
    ).run()
    const received = await delivery.snapshot()
    const jobOutcome = {
      status: result.worker.status,
      failures: result.worker.failures.map((failure): ObservedTransferFailure => (
        failure.kind === 'file'
          ? {
              kind: 'file',
              id: failure.fileId,
              reason: bridge.describe(failure.reason),
            }
          : {
              kind: 'directory',
              id: failure.directoryId,
              reason: bridge.describe(failure.reason),
            }
      )),
      failureCount: result.worker.failureCount,
      omittedFailureCount: result.worker.omittedFailureCount,
    } as const
    const succeeded = jobOutcome.status === 'Succeeded' &&
      result.lifecycle.kind === 'published' &&
      received.bytes === input.transferBytes && received.sha256 === input.expectedHash
    await bridge.publish({
      kind: 'delivery',
      outcome: succeeded ? 'succeeded' : 'failed',
      evidence: deliveryEvidence(input, received, succeeded ? 'succeeded' : 'failed'),
      jobOutcome,
    })
  } catch (error) {
    runtimeError = bridge.describe(error)
    if (deliveryStarted) {
      const received = await delivery.snapshot()
      await bridge.publish({
        kind: 'delivery',
        outcome: 'failed',
        evidence: deliveryEvidence(input, received, 'failed'),
        failureMessage: runtimeError,
      })
    }
  } finally {
    runtimeError = closeActivation(activation, bridge, runtimeError)
    runtimeError = await closeReceiver(joined, bridge, runtimeError)
    runtimeError ??= await bridge.terminalFailure()
    markRuntimeTerminalPublished()
    await bridge.publish({
      kind: 'runtime-settled',
      ...(runtimeError === undefined ? {} : { error: runtimeError }),
    })
  }
}

async function bindDirectAtomicOperation(
  input: HotSwitchPageTransferRuntimeInput,
  joined: JoinedReceiver,
  modules: ProductModules,
  delivery: DeliveryBuffer,
  outputFence: OutputFence,
): Promise<BoundDirectAtomicOperation> {
  const selection = joined.selection.snapshot()
  const selectionSpec = await modules.intent.createSelectionSpec({
    shareInstance: joined.descriptor.shareInstanceId,
    syntheticRoot: joined.descriptor.syntheticRootId,
    rules: modules.intent.selectionRulesSpecFromPolicy(selection),
  })
  const projection = new modules.projection.SelectionProjectionController()
  let current = projection.beginSelection(selectionSpec, joined.protocolSessionId)
  for await (const state of modules.projection.discoverAuthenticatedSelection(
    projection,
    joined.projectionSource(selection),
    new AbortController().signal,
  )) {
    // Iterator snapshots come from the reducer, so retaining the latest state
    // observes progress without constructing a parallel fixture projection.
    current = state
  }
  if (current.discovery.kind !== 'complete' || current.projection.proof.kind !== 'single-file') {
    throw new Error('Hot-switch fixture requires a complete single-file projection')
  }

  const environment = modules.planning.createEnvironmentOffers({
    targets: [{
      id: STREAM_TARGET_OFFER_ID,
      kind: 'managed-atomic-file-target',
      guarantees: modules.intent.managedAtomicGuarantees('application-chosen'),
      persistence: 'operation-scoped',
      hardMaximumOutputBytes: BigInt(input.transferBytes),
    }],
  })
  const offers = await modules.planning.offerArtifacts(
    current.projection,
    current.discovery,
    environment,
  )
  if (offers.kind !== 'artifact-actions') {
    throw new Error(`Hot-switch stream offer is unavailable: ${offers.kind}`)
  }
  const action = offers.primary
  if (action.operation !== 'download-original' || action.plan.kind !== 'direct-atomic' ||
      action.artifact?.kind !== 'original-file' || action.suggestedName === null) {
    throw new Error('Hot-switch fixture received an incompatible artifact action')
  }

  const reservation = await modules.intent.createManagedAtomicReservation({
    operationId: modules.intent.createOperationID(),
    reservationId: modules.intent.createDestinationReservationID(),
    artifact: action.artifact,
    authorityRef: randomAuthorityReference(),
    nameAuthority: 'application-chosen',
    requestedName: action.suggestedName,
    reservedName: action.suggestedName,
    collisionIndex: 0,
  })
  const bound = await modules.planning.bindMaterialization({
    selection: selectionSpec,
    chosenAction: action,
    currentProjection: current.projection,
    currentDiscovery: current.discovery,
    currentEnvironment: environment,
    acquired: {
      kind: 'destination-reservation',
      environmentTargetOfferId: STREAM_TARGET_OFFER_ID,
      reservation,
    },
  })
  if (bound.kind !== 'bound' || bound.intent.plan.kind !== 'direct-atomic') {
    throw new Error(`Hot-switch intent binding did not remain direct-atomic: ${bound.kind}`)
  }
  const intent = bound.intent
  const plans = await modules.planAuthority.createV2PlanExecutionAuthority({
    intent,
    routes: {
      directAtomic: {
        open: async (routeIntent, signal) => {
          signal.throwIfAborted()
          const output = delivery.outputSession(
            modules.stream,
            modules.intent.createOutputSessionID(),
            outputFence,
          )
          return Object.freeze({
            planKind: 'direct-atomic' as const,
            output,
            settle: async (
              _request: Parameters<DirectAtomicExecution['settle']>[0],
              settlementSignal: AbortSignal,
            ) => {
              settlementSignal.throwIfAborted()
              return lifecycleState(modules, routeIntent, {
                kind: 'published',
                receiptDigest: await operationReceipt(routeIntent, 'published'),
                cleanupState: 'clean',
              })
            },
            pause: async (
              request: Parameters<DirectAtomicExecution['pause']>[0],
              settlementSignal: AbortSignal,
            ) => {
              settlementSignal.throwIfAborted()
              await output.pauseActiveTransaction(request.reason)
              settlementSignal.throwIfAborted()
              return lifecycleState(modules, routeIntent, {
                kind: 'restart-required',
                reason: 'direct-atomic-rolled-back',
                receiptDigest: await operationReceipt(routeIntent, 'rolled-back'),
              })
            },
          })
        },
      },
      lifecycle: {
        abortUnopened: async (routeIntent, _reason, signal) => {
          signal.throwIfAborted()
          return lifecycleState(modules, routeIntent, {
            kind: 'discarded',
            cleanupReceiptDigest: await operationReceipt(routeIntent, 'discarded'),
          })
        },
        recordSettlementUnknown: async (routeIntent, signal) => {
          signal.throwIfAborted()
          const state = lifecycleState(modules, routeIntent, {
            kind: 'needs-attention',
            reason: 'target-ownership-unknown',
            lastVerifiedRecordDigest: await operationReceipt(routeIntent, 'settlement-unknown'),
          })
          if (state.kind !== 'needs-attention') {
            throw new TypeError('Unknown stream settlement must require attention')
          }
          return state
        },
      },
    },
  })
  return Object.freeze({ intent, plans, selection })
}

function lifecycleState(
  modules: ProductModules,
  intent: ReceiveIntent,
  payload: ReceiveLifecycleStatePayload,
): ReceiveLifecycleState {
  return modules.workspaceState.nextReceiveLifecycleState(
    modules.workspaceState.initialReceiveLifecycleState({
      operationId: intent.operationId,
      receiveIntentDigest: intent.digest,
    }),
    payload,
  )
}

function randomAuthorityReference(): string {
  const bytes = new Uint8Array(AUTHORITY_REFERENCE_BYTES)
  crypto.getRandomValues(bytes)
  return encodeBase64Url(bytes)
}

async function operationReceipt(intent: ReceiveIntent, purpose: string): Promise<string> {
  const digest = await crypto.subtle.digest(
    'SHA-256',
    TEXT_ENCODER.encode(`${HOT_SWITCH_RECEIPT_DOMAIN}\n${intent.digest}\n${purpose}`),
  )
  return encodeBase64Url(new Uint8Array(digest))
}

function createGateway(
  input: HotSwitchPageTransferRuntimeInput,
  modules: ProductModules,
  bridge: EvidenceBridge,
  relayCut: RelayCutEvidence,
  peerRelease: OneShotRelease,
  peerHarness: PagePeerRecoveryHarness,
): InstanceType<GatewayModule['V2BrowserReceiverGateway']> {
  const realOffers = new modules.offer.BrowserOfferChannelFactory({
    configuration: input.rtcConfiguration,
  })
  const gatedOffers = {
    offer: async (
      route: Parameters<typeof realOffers.offer>[0],
      signal: AbortSignal,
      observer?: Parameters<typeof realOffers.offer>[2],
    ) => {
      const peer = input.peerRecovery === undefined
        ? (await Promise.all([
            realOffers.offer(route, signal, observer),
            peerRelease.waitUntilReleased(signal),
          ]))[0]
        : await realOffers.offer(route, signal, observer)
      return peerHarness.wrap(peer)
    },
  }
  return new modules.gateway.V2BrowserReceiverGateway({
    offersFactory: () => gatedOffers,
    nativePeerUsable: () => input.nativePeerUsable,
    connectivityObserver: (diagnostic: V2BrowserConnectivityAttemptDiagnostic) => {
      bridge.publish({ kind: 'attempt', evidence: diagnostic }).catch(() => undefined)
    },
    ...(input.peerRecovery === undefined
      ? {}
      : {
          peerRecovery: {
            policy: input.peerRecovery.policy,
            random: () => 0,
            observer: (diagnostic) => {
              bridge.publish({ kind: 'recovery', evidence: diagnostic }).catch(() => undefined)
            },
          },
        }),
    onBlockDispatched: (observation) => {
      bridge.publish({
        kind: 'dispatch',
        observation: {
          dispatchSequence: observation.dispatchSequence,
          laneId: observation.laneId,
          laneEpoch: observation.laneEpoch,
          route: observation.route,
        },
      }).catch(() => undefined)
      if (observation.route === 'relay') peerRelease.release()
    },
    onContentLaneAdmitted: (observation) => relayCut.admit(observation),
    onContentLaneDetached: (observation) => {
      relayCut.detach(observation)
      peerHarness.observeDetachment(observation)
    },
  })
}

async function loadProductModules(): Promise<ProductModules> {
  const [gateway, intent, offer, planAuthority, planning, projection, stream, workspaceState] =
    await Promise.all([
    import(GATEWAY_MODULE_PATH) as Promise<GatewayModule>,
    import(INTENT_MODULE_PATH) as Promise<IntentModule>,
    import(OFFER_MODULE_PATH) as Promise<OfferModule>,
    import(PLAN_AUTHORITY_MODULE_PATH) as Promise<PlanAuthorityModule>,
    import(PLANNING_MODULE_PATH) as Promise<PlanningModule>,
    import(PROJECTION_MODULE_PATH) as Promise<ProjectionModule>,
    import(STREAM_MODULE_PATH) as Promise<StreamModule>,
    import(WORKSPACE_STATE_MODULE_PATH) as Promise<WorkspaceStateModule>,
  ])
  return Object.freeze({
    gateway,
    intent,
    offer,
    planAuthority,
    planning,
    projection,
    stream,
    workspaceState,
  })
}

function deliveryEvidence(
  input: HotSwitchPageTransferRuntimeInput,
  received: { readonly bytes: number; readonly sha256: string },
  terminal: 'succeeded' | 'failed',
) {
  return Object.freeze({
    expectedBytes: input.transferBytes,
    receivedBytes: received.bytes,
    expectedSha256: input.expectedHash,
    receivedSha256: received.sha256,
    terminal,
  })
}

function closeActivation(
  activation: DownloadActivation | undefined,
  bridge: EvidenceBridge,
  runtimeError: string | undefined,
): string | undefined {
  try {
    activation?.close()
  } catch (error) {
    return runtimeError ?? bridge.describe(error)
  }
  return runtimeError
}

async function closeReceiver(
  joined: JoinedReceiver | undefined,
  bridge: EvidenceBridge,
  runtimeError: string | undefined,
): Promise<string | undefined> {
  try {
    await joined?.close()
  } catch (error) {
    return runtimeError ?? bridge.describe(error)
  }
  return runtimeError
}

function describeFailure(reason: unknown, maximumDepth: number, depth = 0): string {
  if (depth >= maximumDepth) return '[nested failure truncated]'
  if (reason instanceof AggregateError) {
    const nested = reason.errors.map((error) => describeFailure(error, maximumDepth, depth + 1))
    const summary = `${reason.name}: ${reason.message}`
    const failures = nested.length === 0 ? summary : `${summary}; errors=[${nested.join(' | ')}]`
    return reason.cause === undefined
      ? failures
      : `${failures}; cause=${describeFailure(reason.cause, maximumDepth, depth + 1)}`
  }
  if (reason instanceof Error) {
    const summary = `${reason.name}: ${reason.message}`
    return reason.cause === undefined
      ? summary
      : `${summary}; cause=${describeFailure(reason.cause, maximumDepth, depth + 1)}`
  }
  try {
    return String(reason)
  } catch {
    return '[unprintable non-Error failure]'
  }
}

function laneKey(lane: { readonly laneId: number; readonly laneEpoch: number }): string {
  return `${lane.laneId}/${lane.laneEpoch}`
}

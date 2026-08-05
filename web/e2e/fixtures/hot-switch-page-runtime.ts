import type { V2BrowserConnectivityAttemptDiagnostic } from '../../src/connectivity/diagnostics'
import type {
  HotSwitchLaneObservation,
  HotSwitchPageEvent,
  ObservedTransferFailure,
} from './hot-switch-contract'

const GATEWAY_MODULE_PATH = '/src/ui/v2-gateway.ts'
const OFFER_MODULE_PATH = '/src/connectivity/peer-offer.ts'
const STREAM_MODULE_PATH = '/src/output/streams/single-file.ts'

type GatewayModule = typeof import('../../src/ui/v2-gateway')
type OfferModule = typeof import('../../src/connectivity/peer-offer')
type StreamModule = typeof import('../../src/output/streams/single-file')
type JoinedReceiver = Awaited<ReturnType<
  InstanceType<GatewayModule['V2BrowserReceiverGateway']>['join']
>>
type DownloadActivation = ReturnType<JoinedReceiver['beginDownloadConnectivity']>

export interface HotSwitchPageTransferRuntimeInput {
  readonly expectedHash: string
  readonly failureDiagnosticMaximumDepth: number
  readonly key: string
  readonly nativePeerUsable: boolean
  readonly rtcConfiguration: RTCConfiguration
  readonly runtimePath: string
  readonly transferBytes: number
}

interface HotSwitchWindow extends Window {
  __windshareHotSwitchEvent?: (event: HotSwitchPageEvent) => Promise<void>
  __windshareReleaseHotSwitchOutput?: () => void
  __windshareSealHotSwitchRelayCut?: () => Promise<void>
}

interface ProductModules {
  readonly gateway: GatewayModule
  readonly offer: OfferModule
  readonly stream: StreamModule
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

class OneShotRelease {
  readonly #released: Promise<void>
  #release!: () => void
  #didRelease = false

  constructor() {
    this.#released = new Promise<void>((resolveRelease) => {
      this.#release = resolveRelease
    })
  }

  release(): void {
    if (this.#didRelease) return
    this.#didRelease = true
    this.#release()
  }

  wait(): Promise<void> {
    return this.#released
  }

  async waitUntilReleased(signal: AbortSignal): Promise<void> {
    signal.throwIfAborted()
    await new Promise<void>((resolveRelease, rejectRelease) => {
      const aborted = () => {
        cleanup()
        rejectRelease(signal.reason ?? new DOMException('Peer release aborted', 'AbortError'))
      }
      const released = () => {
        cleanup()
        resolveRelease()
      }
      const cleanup = () => signal.removeEventListener('abort', aborted)
      signal.addEventListener('abort', aborted, { once: true })
      if (signal.aborted) {
        aborted()
        return
      }
      this.#released.then(released, rejectRelease)
    })
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

  outputSession(stream: StreamModule, outputRelease: OneShotRelease) {
    let outputFenceUsed = false
    return new stream.SingleFileStreamOutputSession(
      `browser-${crypto.randomUUID()}`,
      new WritableStream<Uint8Array>({
        write: async (chunk) => {
          if (!outputFenceUsed) {
            outputFenceUsed = true
            await outputRelease.wait()
          }
          this.#chunks.push(chunk.slice())
        },
      }),
    )
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

export function startHotSwitchPageTransfer(input: HotSwitchPageTransferRuntimeInput): void {
  const hotSwitchWindow = window as HotSwitchWindow
  const exposedBridge = hotSwitchWindow.__windshareHotSwitchEvent
  if (exposedBridge === undefined) throw new Error('Hot-switch evidence bridge is unavailable')

  const bridge = new EvidenceBridge(exposedBridge, input.failureDiagnosticMaximumDepth)
  const relayCut = new RelayCutEvidence(bridge)
  const peerRelease = new OneShotRelease()
  const outputRelease = new OneShotRelease()
  hotSwitchWindow.__windshareReleaseHotSwitchOutput = () => outputRelease.release()
  hotSwitchWindow.__windshareSealHotSwitchRelayCut = () => relayCut.seal()

  let runtimeTerminalPublished = false
  const transferTask = runTransfer(input, bridge, relayCut, peerRelease, outputRelease, () => {
    runtimeTerminalPublished = true
  })
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
  outputRelease: OneShotRelease,
  markRuntimeTerminalPublished: () => void,
): Promise<void> {
  const modules = await loadProductModules()
  let joined: JoinedReceiver | undefined
  let activation: DownloadActivation | undefined
  const delivery = new DeliveryBuffer()
  let deliveryStarted = false
  let runtimeError: string | undefined

  try {
    const gateway = createGateway(input, modules, bridge, relayCut, peerRelease)
    joined = await gateway.join(input.key, window.location.href)
    activation = joined.beginDownloadConnectivity('large')
    const output = delivery.outputSession(modules.stream, outputRelease)
    const outputAuthority = {
      openSelection: async () => output,
      abort: (reason: unknown) => output.abortJob(reason),
    }
    deliveryStarted = true
    const result = await joined.transferJob(outputAuthority, activation).run()
    const received = await delivery.snapshot()
    const jobOutcome = {
      status: result.outcome.status,
      failures: result.outcome.failures.map((failure): ObservedTransferFailure => (
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
      failureCount: result.outcome.failureCount,
      omittedFailureCount: result.outcome.omittedFailureCount,
    } as const
    const succeeded = jobOutcome.status === 'Succeeded' &&
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

function createGateway(
  input: HotSwitchPageTransferRuntimeInput,
  modules: ProductModules,
  bridge: EvidenceBridge,
  relayCut: RelayCutEvidence,
  peerRelease: OneShotRelease,
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
      const [peer] = await Promise.all([
        realOffers.offer(route, signal, observer),
        peerRelease.waitUntilReleased(signal),
      ])
      return peer
    },
  }
  return new modules.gateway.V2BrowserReceiverGateway({
    offersFactory: () => gatedOffers,
    nativePeerUsable: () => input.nativePeerUsable,
    connectivityObserver: (diagnostic: V2BrowserConnectivityAttemptDiagnostic) => {
      bridge.publish({ kind: 'attempt', evidence: diagnostic }).catch(() => undefined)
    },
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
    onContentLaneDetached: (observation) => relayCut.detach(observation),
  })
}

async function loadProductModules(): Promise<ProductModules> {
  const [gateway, offer, stream] = await Promise.all([
    import(GATEWAY_MODULE_PATH) as Promise<GatewayModule>,
    import(OFFER_MODULE_PATH) as Promise<OfferModule>,
    import(STREAM_MODULE_PATH) as Promise<StreamModule>,
  ])
  return Object.freeze({ gateway, offer, stream })
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

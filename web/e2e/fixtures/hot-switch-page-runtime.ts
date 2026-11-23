import { BrowserReceiverGateway } from '../../src/application/browser-share/gateway'
import { BrowserOfferChannelFactory } from '../../src/connectivity/peer-offer'
import { SessionSignalingRoute } from '../../src/connectivity/session-signaling'
import { createTaskId } from '../../src/transfer/selection'

import type {
  HotSwitchPageEvent,
  HotSwitchRecoveryControl,
  ObservedTransferFailure,
} from './hot-switch-contract'
import {
  DeliveryBuffer,
  EvidenceBridge,
  projectAttemptEvidence,
  projectRecoveryEvidence,
  RelayCutEvidence,
} from './hot-switch-page-evidence'
import {
  OneShotRelease,
  OutputFence,
  PagePeerRecoveryHarness,
} from './hot-switch-page-transfer'

type JoinedReceiver = Awaited<ReturnType<BrowserReceiverGateway['join']>>
type DownloadActivation = ReturnType<JoinedReceiver['beginDownloadConnectivity']>

export interface HotSwitchPageTransferRuntimeInput {
  readonly expectedHash: string
  readonly failureDiagnosticMaximumDepth: number
  readonly key: string
  readonly nativePeerUsable: boolean
  readonly peerRecovery?: HotSwitchRecoveryControl
  readonly rtcConfiguration: RTCConfiguration
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

export function startHotSwitchPageTransfer(input: HotSwitchPageTransferRuntimeInput): void {
  const hotSwitchWindow = window as HotSwitchWindow
  const exposedBridge = hotSwitchWindow.__windshareHotSwitchEvent
  if (exposedBridge === undefined) throw new Error('Hot-switch evidence bridge is unavailable')

  const bridge = new EvidenceBridge(exposedBridge, input.failureDiagnosticMaximumDepth)
  const relayCut = new RelayCutEvidence(bridge)
  const peerRelease = new OneShotRelease()
  const outputFence = new OutputFence(input.peerRecovery !== undefined)
  const peerHarness = input.peerRecovery === undefined ? undefined : new PagePeerRecoveryHarness(bridge)
  hotSwitchWindow.__windshareAdvanceHotSwitchOutput = () => outputFence.advance()
  if (peerHarness !== undefined) {
    hotSwitchWindow.__windshareDetachHotSwitchPeer = () => peerHarness.detachCurrentPeer()
    hotSwitchWindow.__windshareReleaseHotSwitchAdmission = () => peerHarness.releaseAdmission()
  }
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
    () => { runtimeTerminalPublished = true },
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
  peerHarness: PagePeerRecoveryHarness | undefined,
  markRuntimeTerminalPublished: () => void,
): Promise<void> {
  let joined: JoinedReceiver | undefined
  let activation: DownloadActivation | undefined
  const delivery = new DeliveryBuffer()
  let deliveryStarted = false
  let runtimeError: string | undefined

  try {
    const gateway = createGateway(input, bridge, relayCut, peerRelease, peerHarness)
    joined = await gateway.join(input.key, window.location.href)
    const selection = joined.selection.snapshot()
    const shape = await joined.inspectSelection(selection, new AbortController().signal)
    if (shape.kind !== 'single-file') {
      throw new Error('Hot-switch fixture requires a single-file share')
    }
    const output = delivery.outputSession(createTaskId(), outputFence)
    activation = joined.beginDownloadConnectivity()
    deliveryStarted = true
    const result = await joined.createTransferJob(output, activation, {
      selection,
      rootPath: shape.sourcePath.slice(0, -1),
    }).run()
    const received = await delivery.snapshot()
    const failures: ObservedTransferFailure[] = (result.sourceRevisionFailures?.files ?? []).map(failure => ({
      kind: 'file',
      id: failure.entryId,
      reason: failure.reason,
    }))
    const jobOutcome = {
      status: result.status === 'completed' ? 'Succeeded' : 'CompletedWithErrors',
      failures,
      failureCount: Number(result.failedFiles + result.failedDirectories),
      omittedFailureCount: 0,
    } as const
    const succeeded = result.status === 'completed' &&
      received.bytes === input.transferBytes &&
      received.sha256 === input.expectedHash
    await bridge.publish({
      kind: 'delivery',
      outcome: succeeded ? 'succeeded' : 'failed',
      evidence: deliveryEvidence(input, received, succeeded ? 'succeeded' : 'failed'),
      jobOutcome,
      ...(result.error === undefined ? {} : { failureMessage: bridge.describe(result.error) }),
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
  bridge: EvidenceBridge,
  relayCut: RelayCutEvidence,
  peerRelease: OneShotRelease,
  peerHarness: PagePeerRecoveryHarness | undefined,
): BrowserReceiverGateway {
  const realOffers = new BrowserOfferChannelFactory({
    configuration: input.rtcConfiguration,
  })
  const gatedOffers = {
    offer: async (
      route: Parameters<typeof realOffers.offer>[0],
      signal: AbortSignal,
      observer?: Parameters<typeof realOffers.offer>[2],
      profile?: Parameters<typeof realOffers.offer>[3],
      turnServer?: Parameters<typeof realOffers.offer>[4],
    ) => {
      if (peerHarness === undefined) {
        const [peer] = await Promise.all([
          realOffers.offer(route, signal, observer, profile, turnServer),
          peerRelease.waitUntilReleased(signal),
        ])
        return peer
      }
      if (!(route instanceof SessionSignalingRoute)) {
        throw new TypeError('Recovery fixture requires authenticated session signaling')
      }
      const peer = await realOffers.offer(route, signal, observer, profile, turnServer)
      return peerHarness.wrap(peer, [...route.binding.attemptId])
    },
  }
  return new BrowserReceiverGateway({
    offersFactory: () => gatedOffers,
    nativePeerUsable: () => input.nativePeerUsable,
    connectivityTrace: {
      current: event => {
        const observed = event.eventName === 'peer_attempt'
          ? { kind: 'attempt' as const, evidence: projectAttemptEvidence(event) }
          : { kind: 'recovery' as const, evidence: projectRecoveryEvidence(event) }
        bridge.publish(observed).catch(() => undefined)
      },
    },
    ...(input.peerRecovery === undefined
      ? {}
      : {
          peerRecovery: {
            policy: input.peerRecovery.policy,
            random: () => 0,
          },
        }),
    onBlockDispatched: observation => {
      relayCut.dispatch({
        dispatchSequence: observation.dispatchSequence,
        laneId: observation.laneId,
        laneEpoch: observation.laneEpoch,
        route: observation.route,
      })
      if (observation.route === 'application-relay') peerRelease.release()
    },
    onContentLaneAdmitted: observation => relayCut.admit(observation),
    onContentLaneDetached: observation => {
      relayCut.detach(observation)
      peerHarness?.observeDetachment(observation)
    },
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

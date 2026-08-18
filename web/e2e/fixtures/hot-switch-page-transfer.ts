import type { Page } from '@playwright/test'

import type { PeerChannel } from '../../src/connectivity/peer-channel'
import type {
  HotSwitchLaneObservation,
  HotSwitchPageEvent,
  HotSwitchRecoveryControl,
} from './hot-switch-contract'

const FAILURE_DIAGNOSTIC_MAXIMUM_DEPTH = 4
const PAGE_TRANSFER_RUNTIME_PATH = '/e2e/fixtures/hot-switch-page-runtime.ts'

interface PageRecoveryEventBridge {
  publish(event: HotSwitchPageEvent): Promise<void>
}

export class OneShotRelease {
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

export class OutputFence {
  readonly #stepEveryWrite: boolean
  readonly #waiters: Array<() => void> = []
  #availablePermits = 0
  #released = false
  #writeOrdinal = 0

  constructor(stepEveryWrite: boolean) {
    this.#stepEveryWrite = stepEveryWrite
  }

  async waitForWrite(): Promise<void> {
    const ordinal = this.#writeOrdinal++
    if (this.#released || (!this.#stepEveryWrite && ordinal > 0)) return
    if (this.#availablePermits > 0) {
      this.#availablePermits -= 1
      return
    }
    await new Promise<void>((resolve) => this.#waiters.push(resolve))
  }

  advance(): void {
    if (this.#released) return
    const waiter = this.#waiters.shift()
    if (waiter === undefined) {
      this.#availablePermits += 1
      return
    }
    waiter()
  }

  release(): void {
    if (this.#released) return
    this.#released = true
    for (const waiter of this.#waiters.splice(0)) waiter()
  }
}

class AdmissionGatedPeerChannel implements PeerChannel {
  readonly frames: ReadableStream<Uint8Array>
  readonly opened: Promise<void>
  readonly done: Promise<void>
  readonly #channel: PeerChannel
  readonly #gateAbort = new AbortController()
  #gatePassed = false

  constructor(
    channel: PeerChannel,
    release: OneShotRelease,
    onGated: () => Promise<void>,
  ) {
    this.#channel = channel
    this.opened = channel.opened
    this.done = channel.done
    let firstInbound = true
    this.frames = channel.frames.pipeThrough(new TransformStream<Uint8Array, Uint8Array>({
      transform: async (frame, controller) => {
        if (firstInbound) {
          firstInbound = false
          await onGated()
          await release.waitUntilReleased(this.#gateAbort.signal)
          this.#gatePassed = true
        }
        controller.enqueue(frame)
      },
    }), { signal: this.#gateAbort.signal })
  }

  get state() {
    return this.#channel.state
  }

  get reason(): unknown {
    return this.#channel.reason
  }

  send(frame: Uint8Array, signal?: AbortSignal): Promise<void> {
    return this.#channel.send(frame, signal)
  }

  sendTerminal(frame: Uint8Array, signal?: AbortSignal): Promise<void> {
    return this.#channel.sendTerminal(frame, signal)
  }

  close(): Promise<void> {
    if (!this.#gatePassed && !this.#gateAbort.signal.aborted) {
      this.#gateAbort.abort(new DOMException('Controlled admission gate closed', 'AbortError'))
    }
    return this.#channel.close()
  }
}

export class PagePeerRecoveryHarness {
  readonly #bridge: PageRecoveryEventBridge
  readonly #enabled: boolean
  readonly #detachmentWaiters: Array<() => void> = []
  #currentPeer: PeerChannel | undefined
  #manualAdmissionRelease: OneShotRelease | undefined
  #offerOrdinal = 0

  constructor(bridge: PageRecoveryEventBridge, enabled: boolean) {
    this.#bridge = bridge
    this.#enabled = enabled
  }

  wrap(peer: PeerChannel): PeerChannel {
    this.#offerOrdinal += 1
    const offerOrdinal = this.#offerOrdinal
    const gated = this.#enabled && (offerOrdinal === 1 || offerOrdinal === 3)
    if (!gated) {
      this.#currentPeer = peer
      return peer
    }
    const release = new OneShotRelease()
    const releaseAuthority = offerOrdinal === 1 ? 'attempt-timeout' : 'page-controlled'
    if (releaseAuthority === 'page-controlled') this.#manualAdmissionRelease = release
    const wrapped = new AdmissionGatedPeerChannel(
      peer,
      release,
      () => this.#bridge.publish({
        kind: 'admission-response-gated',
        observation: { offerOrdinal, release: releaseAuthority },
      }),
    )
    this.#currentPeer = wrapped
    return wrapped
  }

  observeDetachment(observation: HotSwitchLaneObservation): void {
    if (observation.route !== 'peer') return
    for (const resolve of this.#detachmentWaiters.splice(0)) resolve()
  }

  async detachCurrentPeer(): Promise<void> {
    if (!this.#enabled || this.#currentPeer === undefined) {
      throw new Error('No controlled peer is available for detachment')
    }
    const detached = new Promise<void>((resolve) => this.#detachmentWaiters.push(resolve))
    await this.#currentPeer.close()
    await detached
  }

  releaseAdmission(): void {
    if (this.#manualAdmissionRelease === undefined) {
      throw new Error('The page-controlled admission response is not gated')
    }
    this.#manualAdmissionRelease.release()
    this.#manualAdmissionRelease = undefined
  }
}

export async function startPageTransfer(
  page: Page,
  input: {
    readonly expectedHash: string
    readonly key: string
    readonly nativePeerUsable: boolean
    readonly peerRecovery?: HotSwitchRecoveryControl
    readonly rtcConfiguration: RTCConfiguration
    readonly transferBytes: number
  },
): Promise<void> {
  await page.evaluate(async (runtimeInput) => {
    const runtime = await import(runtimeInput.runtimePath) as typeof import(
      './hot-switch-page-runtime'
    )
    runtime.startHotSwitchPageTransfer(runtimeInput)
  }, {
    ...input,
    failureDiagnosticMaximumDepth: FAILURE_DIAGNOSTIC_MAXIMUM_DEPTH,
    runtimePath: PAGE_TRANSFER_RUNTIME_PATH,
    transferBytes: input.transferBytes,
  })
}

export async function sealPageRelayCut(page: Page): Promise<void> {
  await page.evaluate(async () => {
    const seal = (
      window as Window & { __windshareSealHotSwitchRelayCut?: () => Promise<void> }
    ).__windshareSealHotSwitchRelayCut
    if (seal === undefined) throw new Error('Hot-switch relay-cut seal is unavailable')
    await seal()
  })
}

export async function advancePageOutput(page: Page): Promise<void> {
  await page.evaluate(() => {
    const advance = (
      window as Window & { __windshareAdvanceHotSwitchOutput?: () => void }
    ).__windshareAdvanceHotSwitchOutput
    if (advance === undefined) throw new Error('Hot-switch output checkpoint is unavailable')
    advance()
  })
}

export async function detachPagePeer(page: Page): Promise<void> {
  await page.evaluate(async () => {
    const detach = (
      window as Window & { __windshareDetachHotSwitchPeer?: () => Promise<void> }
    ).__windshareDetachHotSwitchPeer
    if (detach === undefined) throw new Error('Hot-switch peer detachment control is unavailable')
    await detach()
  })
}

export async function releasePageAdmissionResponse(page: Page): Promise<void> {
  await page.evaluate(() => {
    const release = (
      window as Window & { __windshareReleaseHotSwitchAdmission?: () => void }
    ).__windshareReleaseHotSwitchAdmission
    if (release === undefined) throw new Error('Hot-switch admission gate is unavailable')
    release()
  })
}

export async function releasePageOutput(page: Page): Promise<void> {
  if (page.isClosed()) return
  await page.evaluate(() => {
    const release = (
      window as Window & { __windshareReleaseHotSwitchOutput?: () => void }
    ).__windshareReleaseHotSwitchOutput
    release?.()
  })
}

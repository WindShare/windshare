import { openShareDescriptor, type ShareDescriptor } from '../catalog/records'
import { equalBytes } from '../crypto/bytes'
import type { Suite02CapabilityKey } from '../crypto/suite02-link'
import type { ProtocolTraceSource } from '../session/diagnostics'
import { ReceiverSessionRuntime, type ReceiverSessionOptions } from '../session/runtime'
import {
  dialRelayReceiver,
  type RelayReceiverConnection,
} from '../transport/relay/receiver'

import { firstUsableRelay, receiverRelayBases, RelayEndpointFailure } from './relay-race'
import { isShareRecoveryFailure, isTerminalRecoveryFailure } from './recovery-failure'
import { emitRelayHeartbeat, emitRelayDelivery } from '../diagnostics/trace/connection-payload'

export const RELAY_LANE_ADMISSION_TIMEOUT_MILLISECONDS = 30_000

export interface ProtocolGenerationCore {
  readonly relayBase: string
  readonly relay: RelayReceiverConnection
  readonly session: ReceiverSessionRuntime
  readonly relayLaneId: number
}

export interface AttachedRelay {
  readonly relay: RelayReceiverConnection
  readonly laneId: number
}

export interface ReceiverSessionFactory {
  readonly relayBases: readonly string[]
  connectFresh(signal: AbortSignal): Promise<ProtocolGenerationCore>
  attachRelay(
    session: ReceiverSessionRuntime,
    signal: AbortSignal,
    relayBase: string,
  ): Promise<AttachedRelay>
  copyReadSecret(): Uint8Array<ArrayBuffer>
  close(): void
}

export interface BrowserSessionFactoryOptions {
  readonly relayBases: readonly string[]
  readonly capability: Suite02CapabilityKey
  readonly descriptor: ShareDescriptor
  readonly descriptorObject: Uint8Array
  readonly dialRelay?: typeof dialRelayReceiver
  readonly openDescriptor?: typeof openShareDescriptor
  readonly protocolTrace?: ProtocolTraceSource
  readonly connectSession?: (
    options: ReceiverSessionOptions,
  ) => Promise<ReceiverSessionRuntime>
}

export class StaleShareInstanceError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'StaleShareInstanceError'
  }
}

/**
 * Owns the capability copy needed to authenticate every relay redial. Relay IDs
 * remain delivery-only; only an authenticated descriptor can enter a generation.
 */
export class BrowserSessionFactory implements ReceiverSessionFactory {
  readonly relayBases: readonly string[]
  readonly #capability: Suite02CapabilityKey
  readonly #descriptor: ShareDescriptor
  readonly #descriptorObject: Uint8Array<ArrayBuffer>
  readonly #dialRelay: typeof dialRelayReceiver
  readonly #openDescriptor: typeof openShareDescriptor
  readonly #protocolTrace: ProtocolTraceSource | undefined
  readonly #connectSession: (
    options: ReceiverSessionOptions,
  ) => Promise<ReceiverSessionRuntime>
  readonly #terminalRelays = new Map<string, RelayEndpointFailure>()
  #closed = false

  constructor(options: BrowserSessionFactoryOptions) {
    this.relayBases = receiverRelayBases(options.relayBases)
    this.#capability = Object.freeze({
      suite: options.capability.suite,
      readSecret: options.capability.readSecret.slice(),
      pkHash: options.capability.pkHash.slice(),
      shareIdRaw: options.capability.shareIdRaw.slice(),
      shareId: options.capability.shareId,
    })
    this.#descriptor = options.descriptor
    this.#descriptorObject = options.descriptorObject.slice()
    this.#dialRelay = options.dialRelay ?? dialRelayReceiver
    this.#openDescriptor = options.openDescriptor ?? openShareDescriptor
    this.#protocolTrace = options.protocolTrace
    this.#connectSession = options.connectSession ?? ((sessionOptions) =>
      ReceiverSessionRuntime.connect(sessionOptions))
  }

  async connectFresh(signal: AbortSignal): Promise<ProtocolGenerationCore> {
    this.#requireOpen()
    const eligible = this.relayBases.filter((relayBase) => !this.#terminalRelays.has(relayBase))
    if (eligible.length === 0) throw new AggregateError([...this.#terminalRelays.values()], 'All relay endpoints rejected this share')
    return firstUsableRelay(eligible, signal,
      (relayBase, attemptSignal) => this.#attemptRelay(relayBase, attemptSignal,
        () => this.#connectFreshRelay(relayBase, attemptSignal)),
      async (core) => { await Promise.allSettled([core.session.close(), core.relay.close()]) }, isShareRecoveryFailure)
  }

  async #connectFreshRelay(relayBase: string, signal: AbortSignal): Promise<ProtocolGenerationCore> {
    let session: ReceiverSessionRuntime | undefined
    const relay = await this.#dialValidatedRelay(relayBase, signal, () => session)
    try {
      session = await this.#connectSession({
        descriptor: this.#descriptor,
        readSecret: this.#capability.readSecret,
        initialChannel: relay.channel,
        signal,
        ...(this.#protocolTrace === undefined ? {} : { protocolTrace: this.#protocolTrace }),
      })
      return Object.freeze({
        relayBase,
        relay,
        session,
        relayLaneId: session.initialLaneId,
      })
    } catch (error) {
      await relay.close().catch(() => undefined)
      throw error
    }
  }

  async attachRelay(
    session: ReceiverSessionRuntime,
    signal: AbortSignal,
    relayBase: string,
  ): Promise<AttachedRelay> {
    this.#requireOpen()
    if (!this.relayBases.includes(relayBase)) throw new TypeError('Relay endpoint is not configured')
    return this.#attemptRelay(relayBase, signal, () => this.#attachRelay(session, signal, relayBase))
  }

  async #attachRelay(
    session: ReceiverSessionRuntime,
    signal: AbortSignal,
    relayBase: string,
  ): Promise<AttachedRelay> {
    const relay = await this.#dialValidatedRelay(relayBase, signal, () => session)
    const admission = relayAdmissionDeadline(signal)
    try {
      // A redial gets a new delivery route and a new logical lane grant. It
      // never treats RelaySessionID as ProtocolSession resumption authority.
      const grant = await session.requestLaneGrant(0, { signal: admission.signal })
      const result = await session.adoptGrantedLane(
        relay.channel,
        grant,
        { signal: admission.signal },
      )
      if (result.installation !== 'installed') throw result.error
      return Object.freeze({ relay, laneId: grant.laneId })
    } catch (error) {
      await relay.close().catch(() => undefined)
      throw error
    } finally {
      admission.close()
    }
  }

  copyReadSecret(): Uint8Array<ArrayBuffer> {
    this.#requireOpen()
    return this.#capability.readSecret.slice()
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.#capability.readSecret.fill(0)
    this.#capability.pkHash.fill(0)
    this.#capability.shareIdRaw.fill(0)
  }

  async #attemptRelay<T>(relayBase: string, signal: AbortSignal, connect: () => Promise<T>): Promise<T> {
    signal.throwIfAborted()
    const terminal = this.#terminalRelays.get(relayBase)
    if (terminal !== undefined) throw terminal.cause
    try {
      return await connect()
    } catch (cause) {
      // Dialing, descriptor validation and admission belong to the same endpoint.
      // Remember permanent rejections across generations, but never remember a lost race as one.
      if (!signal.aborted && isTerminalRecoveryFailure(cause) && !isShareRecoveryFailure(cause)) {
        this.#terminalRelays.set(relayBase, new RelayEndpointFailure(relayBase, cause))
      }
      throw cause
    }
  }

  async #dialValidatedRelay(relayBase: string, signal: AbortSignal,
    currentSession: () => ReceiverSessionRuntime | undefined,
  ): Promise<RelayReceiverConnection> {
    const relay = await this.#dialRelay(relayBase, this.#capability, { signal,
      receiveTrace: event => emitRelayDelivery(this.#protocolTrace, event, relayBase, () => {
        const session = currentSession()
        return { correlation: session === undefined ? {} : { protocolSessionId: session.protocolSessionIdentity },
          shareId: this.#capability.shareId, shareInstanceId: this.#descriptor.shareInstanceId }
      }),
      heartbeatTrace: event => emitRelayHeartbeat(this.#protocolTrace, event, relayBase, () => {
        const session = currentSession()
        return { correlation: session === undefined ? {} : { protocolSessionId: session.protocolSessionIdentity },
          shareId: this.#capability.shareId, shareInstanceId: this.#descriptor.shareInstanceId }
      }),
    })
    try {
      const candidate = await this.#openDescriptor(relay.descriptorObject, this.#capability)
      requireSameDescriptor(this.#descriptor, candidate)
      if (!equalBytes(this.#descriptorObject, relay.descriptorObject)) {
        throw new StaleShareInstanceError(
          'Relay redial equivocated on the authenticated descriptor object',
        )
      }
      return relay
    } catch (error) {
      await relay.close().catch(() => undefined)
      throw error
    }
  }

  #requireOpen(): void {
    if (this.#closed) throw new Error('Receiver session factory is closed')
  }
}

function relayAdmissionDeadline(parent: AbortSignal): {
  readonly signal: AbortSignal
  readonly close: () => void
} {
  const controller = new AbortController()
  const abort = () => controller.abort(
    parent.reason ?? new DOMException('Relay lane admission aborted', 'AbortError'),
  )
  parent.addEventListener('abort', abort, { once: true })
  if (parent.aborted) abort()
  const timer = globalThis.setTimeout(() => {
    controller.abort(new DOMException('Relay lane admission timed out', 'TimeoutError'))
  }, RELAY_LANE_ADMISSION_TIMEOUT_MILLISECONDS)
  return Object.freeze({
    signal: controller.signal,
    close: () => {
      globalThis.clearTimeout(timer)
      parent.removeEventListener('abort', abort)
    },
  })
}

function requireSameDescriptor(expected: ShareDescriptor, candidate: ShareDescriptor): void {
  if (!equalBytes(expected.shareInstance, candidate.shareInstance)) {
    throw new StaleShareInstanceError('Relay redial returned another ShareInstance')
  }
  if (
    !equalBytes(expected.sender.publicKey, candidate.sender.publicKey) ||
    !equalBytes(expected.syntheticRoot, candidate.syntheticRoot) ||
    expected.chunkSize !== candidate.chunkSize ||
    expected.capabilities !== candidate.capabilities ||
    expected.createdAtSeconds !== candidate.createdAtSeconds ||
    expected.pathPolicy !== candidate.pathPolicy
  ) {
    throw new StaleShareInstanceError(
      'Relay redial equivocated on an authenticated descriptor identity',
    )
  }
}

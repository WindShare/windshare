import { openV2ShareDescriptor, type V2ShareDescriptor } from '../catalog/v2-records'
import { equalBytes } from '../crypto/bytes'
import type { Suite02CapabilityKey } from '../crypto/suite02-link'
import { V2ReceiverSessionRuntime, type V2ReceiverSessionOptions } from '../session/v2-runtime'
import {
  dialV2RelayReceiver,
  type V2RelayReceiverConnection,
} from '../transport/relay/v2-receiver'

export interface V2ProtocolGenerationCore {
  readonly relay: V2RelayReceiverConnection
  readonly session: V2ReceiverSessionRuntime
  readonly relayLaneId: number
}

export interface V2AttachedRelay {
  readonly relay: V2RelayReceiverConnection
  readonly laneId: number
}

export interface V2ReceiverSessionFactory {
  connectFresh(signal: AbortSignal): Promise<V2ProtocolGenerationCore>
  attachRelay(
    session: V2ReceiverSessionRuntime,
    signal: AbortSignal,
  ): Promise<V2AttachedRelay>
  copyReadSecret(): Uint8Array<ArrayBuffer>
  close(): void
}

export interface V2BrowserSessionFactoryOptions {
  readonly relayBase: string
  readonly capability: Suite02CapabilityKey
  readonly descriptor: V2ShareDescriptor
  readonly descriptorObject: Uint8Array
  readonly dialRelay?: typeof dialV2RelayReceiver
  readonly openDescriptor?: typeof openV2ShareDescriptor
  readonly connectSession?: (
    options: V2ReceiverSessionOptions,
  ) => Promise<V2ReceiverSessionRuntime>
}

export class V2StaleShareInstanceError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'V2StaleShareInstanceError'
  }
}

/**
 * Owns the capability copy needed to authenticate every relay redial. Relay IDs
 * remain delivery-only; only an authenticated descriptor can enter a generation.
 */
export class V2BrowserSessionFactory implements V2ReceiverSessionFactory {
  readonly #relayBase: string
  readonly #capability: Suite02CapabilityKey
  readonly #descriptor: V2ShareDescriptor
  readonly #descriptorObject: Uint8Array<ArrayBuffer>
  readonly #dialRelay: typeof dialV2RelayReceiver
  readonly #openDescriptor: typeof openV2ShareDescriptor
  readonly #connectSession: (
    options: V2ReceiverSessionOptions,
  ) => Promise<V2ReceiverSessionRuntime>
  #closed = false

  constructor(options: V2BrowserSessionFactoryOptions) {
    this.#relayBase = options.relayBase
    this.#capability = Object.freeze({
      suite: options.capability.suite,
      readSecret: options.capability.readSecret.slice(),
      pkHash: options.capability.pkHash.slice(),
      shareIdRaw: options.capability.shareIdRaw.slice(),
      shareId: options.capability.shareId,
    })
    this.#descriptor = options.descriptor
    this.#descriptorObject = options.descriptorObject.slice()
    this.#dialRelay = options.dialRelay ?? dialV2RelayReceiver
    this.#openDescriptor = options.openDescriptor ?? openV2ShareDescriptor
    this.#connectSession = options.connectSession ?? ((sessionOptions) =>
      V2ReceiverSessionRuntime.connect(sessionOptions))
  }

  async connectFresh(signal: AbortSignal): Promise<V2ProtocolGenerationCore> {
    this.#requireOpen()
    const relay = await this.#dialValidatedRelay(signal)
    try {
      const session = await this.#connectSession({
        descriptor: this.#descriptor,
        readSecret: this.#capability.readSecret,
        initialChannel: relay.channel,
        signal,
      })
      return Object.freeze({
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
    session: V2ReceiverSessionRuntime,
    signal: AbortSignal,
  ): Promise<V2AttachedRelay> {
    this.#requireOpen()
    const relay = await this.#dialValidatedRelay(signal)
    try {
      // A redial gets a new delivery route and a new logical lane grant. It
      // never treats RelaySessionID as ProtocolSession resumption authority.
      const grant = await session.requestLaneGrant(0, { signal })
      await session.attachGrantedLane(relay.channel, grant, signal)
      return Object.freeze({ relay, laneId: grant.laneId })
    } catch (error) {
      await relay.close().catch(() => undefined)
      throw error
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

  async #dialValidatedRelay(signal: AbortSignal): Promise<V2RelayReceiverConnection> {
    signal.throwIfAborted()
    const relay = await this.#dialRelay(this.#relayBase, this.#capability, { signal })
    try {
      const candidate = await this.#openDescriptor(relay.descriptorObject, this.#capability)
      requireSameDescriptor(this.#descriptor, candidate)
      if (!equalBytes(this.#descriptorObject, relay.descriptorObject)) {
        throw new V2StaleShareInstanceError(
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

function requireSameDescriptor(expected: V2ShareDescriptor, candidate: V2ShareDescriptor): void {
  if (!equalBytes(expected.shareInstance, candidate.shareInstance)) {
    throw new V2StaleShareInstanceError('Relay redial returned another ShareInstance')
  }
  if (
    !equalBytes(expected.senderPublicKey, candidate.senderPublicKey) ||
    !equalBytes(expected.syntheticRoot, candidate.syntheticRoot) ||
    expected.chunkSize !== candidate.chunkSize ||
    expected.capabilities !== candidate.capabilities ||
    expected.createdAtSeconds !== candidate.createdAtSeconds ||
    expected.pathPolicy !== candidate.pathPolicy
  ) {
    throw new V2StaleShareInstanceError(
      'Relay redial equivocated on an authenticated descriptor identity',
    )
  }
}

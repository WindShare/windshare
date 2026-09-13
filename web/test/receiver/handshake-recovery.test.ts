import { createHash, createPrivateKey, sign } from 'node:crypto'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { openV2ShareDescriptor } from '../../src/catalog/v2-records'
import type { FrameChannel } from '../../src/contracts/channel'
import { V2BrowserSessionFactory } from '../../src/receiver/v2-session-factory'
import { V2ReceiverReconnectSupervisor } from '../../src/receiver/v2-supervisor'
import { systemReconnectClock } from '../../src/receiver/recovery-clock'
import type { ReceiverConnectionSnapshot } from '../../src/receiver/connection-state'
import { isTerminalRecoveryFailure } from '../../src/receiver/recovery-failure'
import { V2SessionHandshakeTimeoutError } from '../../src/session/v2-runtime-types'
import { V2_SESSION_HANDSHAKE_TIMEOUT_MILLISECONDS } from '../../src/session/v2-runtime'
import * as relayReceiver from '../../src/transport/relay/v2-receiver'
import type { V2RelayReceiverConnection } from '../../src/transport/relay/v2-receiver'
import { decodeV2DescriptorDelivery } from '../../src/transport/relay/v2-protocol'
import { joinBrowserRelays } from '../../src/receiver/browser-join'
import { b64ToBytes, loadVectorFile, type VectorCase } from '../vectors'
import { identity, namedCase, senderObjects } from '../protocol/r0-contract-support'
import { deferred } from './v2-supervisor-fixture'

type Response = 'silent' | 'valid' | 'signature' | 'version' | 'key-agreement'
interface TranscriptVector extends VectorCase { readonly serverBodyB64: string }
const transcript = namedCase<TranscriptVector>(loadVectorFile(
  new URL('../../../core/testvectors/v2-session.json', import.meta.url),
).cases, 'sender-authenticated-x25519-transcript')
const signingSeed = (identity as typeof identity & { readonly senderSeedB64: string }).senderSeedB64
const signingKey = createPrivateKey({
  key: Buffer.concat([Buffer.from('302e020100300506032b657004220420', 'hex'), Buffer.from(signingSeed, 'base64')]),
  format: 'der', type: 'pkcs8',
})

class HandshakeChannel implements FrameChannel {
  readonly frames: ReadableStream<Uint8Array>
  readonly helloSent = deferred<void>()
  state: FrameChannel['state'] = 'open'
  #controller!: ReadableStreamDefaultController<Uint8Array>
  readonly #response: Response

  constructor(response: Response) {
    this.#response = response
    this.frames = new ReadableStream({ start: controller => { this.#controller = controller } })
  }

  async send(clientHello: Uint8Array, signal?: AbortSignal): Promise<void> {
    signal?.throwIfAborted()
    if (this.#response !== 'silent') {
      const body = b64ToBytes(transcript.serverBodyB64)
      body.set(createHash('sha256').update(clientHello).digest(), 5)
      if (this.#response === 'version') body[4] = 3
      if (this.#response === 'key-agreement') body.fill(0, 69, 101)
      const preimage = Buffer.concat([Buffer.from('windshare/v2 server-hello\0'), createHash('sha256').update(body).digest()])
      const signature = sign(null, preimage, signingKey)
      if (this.#response === 'signature') signature[0] = signature[0]! ^ 1
      this.#controller.enqueue(Uint8Array.from(Buffer.concat([body, signature])))
    }
    this.helloSent.resolve()
  }

  async sendTerminal(frame: Uint8Array, signal?: AbortSignal): Promise<void> { await this.send(frame, signal) }

  disconnect(): void { this.#controller.error(new Error('Transport went away')) }

  async close(): Promise<void> {
    if (this.state === 'closed') return
    this.state = 'closed'
    try { this.#controller.close() } catch { /* Admission or lane teardown already cancelled its reader. */ }
  }
}

async function shareFixture() {
  const capability = { suite: 2 as const, readSecret: b64ToBytes(identity.readSecretB64).slice(),
    pkHash: b64ToBytes(identity.pkHashB64).slice(), shareIdRaw: b64ToBytes(identity.shareIdRawB64).slice(), shareId: identity.shareId }
  const descriptorObject = b64ToBytes(senderObjects.find(value => value.domain === 'windshare/v2 object/descriptor')!.objectB64).slice()
  const descriptor = await openV2ShareDescriptor(descriptorObject, capability)
  return { capability, descriptorObject, descriptor }
}

async function factoryFixture() {
  const { capability, descriptorObject, descriptor } = await shareFixture()
  const channels: HandshakeChannel[] = []
  const control = { response: 'valid' as Response, dialed: deferred<HandshakeChannel>() }
  const factory = new V2BrowserSessionFactory({ relayBases: ['https://relay.example'], capability, descriptor, descriptorObject,
    dialRelay: async () => {
      const channel = new HandshakeChannel(control.response)
      channels.push(channel)
      control.dialed.resolve(channel)
      control.dialed = deferred<HandshakeChannel>()
      return { endpoint: {} as V2RelayReceiverConnection['endpoint'], relaySessionId: new Uint8Array(8),
        descriptorObject, channel, close: () => channel.close() }
    },
  })
  return { factory, descriptor, channels, control }
}

afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks() })

describe('production browser handshake recovery', () => {
  it.each(['malformed', 'signature'] as const)('joins through a healthy relay after another supplies %s data', async failure => {
    vi.useFakeTimers()
    const { capability, descriptorObject } = await shareFixture()
    const healthy = new HandshakeChannel('valid')
    const ready = deferred<void>()
    const rejected = deferred<void>()
    const invalidObject = descriptorObject.slice()
    invalidObject[invalidObject.length - 1] = invalidObject.at(-1)! ^ 1
    let healthySignal: AbortSignal | undefined
    const dial = vi.spyOn(relayReceiver, 'dialV2RelayReceiver').mockImplementation(async (base, _key, options) => {
      if (base === 'invalid') {
        if (failure === 'malformed') {
          rejected.resolve()
          decodeV2DescriptorDelivery(Uint8Array.of(0))
        }
        return { endpoint: {} as V2RelayReceiverConnection['endpoint'], relaySessionId: new Uint8Array(8),
          descriptorObject: invalidObject, channel: new HandshakeChannel('silent'),
          close: async () => { rejected.resolve() } }
      }
      healthySignal = options?.signal
      await ready.promise
      return { endpoint: {} as V2RelayReceiverConnection['endpoint'], relaySessionId: new Uint8Array(8),
        descriptorObject, channel: healthy, close: () => healthy.close() }
    })
    const parent = new AbortController()
    const outcome = joinBrowserRelays(['invalid', 'healthy'], capability, parent.signal).then(
      value => ({ value }), error => ({ error }),
    )
    try {
      await rejected.promise
      await vi.advanceTimersByTimeAsync(0)
      expect(healthySignal?.aborted).toBe(false)
      ready.resolve()
      const result = await outcome
      expect(result).toHaveProperty('value')
      if (!('value' in result)) throw result.error
      expect(result.value.relayBase).toBe('healthy')
      expect(result.value.session.isClosed).toBe(false)
      expect(dial).toHaveBeenCalledTimes(2)
    } finally {
      parent.abort()
      ready.resolve()
      const result = await outcome
      if ('value' in result) await result.value.session.close()
      await healthy.close()
    }
    expect(vi.getTimerCount()).toBe(0)
  })

  it('keeps the existing operation and content authority after a missing ServerHello deadline, then recovers', async () => {
    vi.useFakeTimers()
    const { factory, descriptor, channels, control } = await factoryFixture()
    const initial = await factory.connectFresh(new AbortController().signal)
    const errors: unknown[] = []
    const supervisor = new V2ReceiverReconnectSupervisor({ descriptor, initial, sessionFactory: factory,
      clock: systemReconnectClock, nativePeerUsable: () => false, onRecoveryError: error => errors.push(error) })
    const content = supervisor.content
    const states: ReceiverConnectionSnapshot[] = []
    supervisor.connection.subscribe(state => states.push(state))
    const activation = supervisor.beginConnectivity('download')
    try {
      control.response = 'silent'
      const stalledDial = control.dialed.promise
      channels[0]!.disconnect()
      const stalled = await stalledDial
      await stalled.helloSent.promise
      let resumed = false
      const operation = supervisor.execute(undefined, async () => { resumed = true; return 'same-operation' })
      operation.catch(() => undefined)
      await vi.advanceTimersByTimeAsync(V2_SESSION_HANDSHAKE_TIMEOUT_MILLISECONDS)
      expect(errors).toHaveLength(1)
      expect(errors[0]).toMatchObject({ cause: expect.any(V2SessionHandshakeTimeoutError) })
      expect(isTerminalRecoveryFailure(errors[0])).toBe(false)
      expect(states.at(-1)?.kind).toBe('reconnecting')
      expect(supervisor.generationId).toBe(1)
      expect(resumed).toBe(false)
      expect(stalled.state).toBe('closed')
      expect(supervisor.content).toBe(content)
      expect(activation.routes.active).toBe(true)
      control.response = 'valid'
      supervisor.requestReconnect()
      await expect(operation).resolves.toMatchObject({ value: 'same-operation' })
      expect(supervisor.generationId).toBe(2)
      expect(states.at(-1)?.kind).toBe('connected')
      expect(supervisor.content).toBe(content)
      expect(channels).toHaveLength(3)
    } finally {
      activation.close()
      await supervisor.close()
    }
    expect(vi.getTimerCount()).toBe(0)
  })

  it.each(['signature', 'version', 'key-agreement'] as const)('keeps received %s faults terminal', async response => {
    const { factory, control, channels } = await factoryFixture()
    control.response = response
    try {
      const error: unknown = await factory.connectFresh(new AbortController().signal).catch((cause: unknown) => cause)
      expect(error).toMatchObject({ cause: { name: 'V2TranscriptError' } })
      expect(isTerminalRecoveryFailure(error)).toBe(true)
      expect(channels[0]!.state).toBe('closed')
    } finally { factory.close() }
  })
})

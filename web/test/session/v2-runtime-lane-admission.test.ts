import { createEd25519Verifier } from '../../src/crypto/ed25519'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ShareDescriptor } from '../../src/catalog/records'
import type { FrameChannel } from '../../src/contracts/channel'
import { LaneSet } from '../../src/content/lane-set'
import { ReceiverConnectivity } from '../../src/connectivity/receiver-policy'
import type { ProtocolTraceEvent } from '../../src/session/diagnostics'
import {
  LaneInstallationError,
  ReceiverSessionRuntime,
} from '../../src/session/runtime'
import type { LaneGrant } from '../../src/session/lane-codec'
import { encodeCanonicalCbor } from '../../src/protocol/cbor'
import { encodeTURNDemand } from '../../src/protocol/turn-control'
import { EnvelopeOpener } from '../../src/session/envelope'
import { encodeMessage, MESSAGE_KIND, OPERATION_RETRY_REASON } from '../../src/session/message'
import type { ReceiverSessionOptions } from '../../src/session/runtime-types'
import type { SessionKeys } from '../../src/session/transcript'
import { b64ToBytes, loadVectorFile, type VectorCase } from '../vectors'
import { identity as identityVector } from '../protocol/r0-contract-support'

interface LaneVector extends VectorCase {
  readonly attachNonceB64: string
  readonly attachedLaneEpoch: number
  readonly attachedLaneId: number
  readonly grantOperationId: string
  readonly laneAckB64: string
  readonly laneHelloB64: string
  readonly laneRejectB64: string
  readonly laneRejectCode: number
  readonly laneRejectRetryAfterMilliseconds: number
  readonly protocolSessionIdB64: string
  readonly receiverToSenderKeyB64: string
  readonly senderToReceiverKeyB64: string
  readonly shareInstanceB64: string
}

class ScriptedChannel implements FrameChannel {
  readonly availability = new AbortController()
  readonly unavailable = this.availability.signal
  readonly frames: ReadableStream<Uint8Array>
  state = 'open' as const
  readonly sent: Uint8Array[] = []
  closes = 0
  readonly #response: Uint8Array | undefined
  #controller!: ReadableStreamDefaultController<Uint8Array>
  #closed = false

  constructor(response?: Uint8Array) {
    this.#response = response?.slice()
    this.frames = new ReadableStream({
      start: (controller) => {
        this.#controller = controller
      },
    })
  }

  receive(frame: Uint8Array): void {
    this.#controller.enqueue(frame)
  }

  async send(frame: Uint8Array, signal?: AbortSignal): Promise<void> {
    signal?.throwIfAborted()
    this.sent.push(frame.slice())
    if (this.#response !== undefined) this.#controller.enqueue(this.#response.slice())
  }

  async sendTerminal(frame: Uint8Array, signal?: AbortSignal): Promise<void> {
    await this.send(frame, signal)
  }

  async close(): Promise<void> {
    if (this.#closed) return
    this.#closed = true
    this.availability.abort(new Error('Channel closed'))
    this.closes += 1
    try {
      this.#controller.close()
    } catch {
      // Runtime lane teardown may already have cancelled the receive stream.
    }
  }
}

const vectors = loadVectorFile(
  new URL('../../../core/testvectors/v2-session.json', import.meta.url),
)
const lane = named<LaneVector>('sender-granted-lane-attach')

afterEach(() => vi.useRealTimers())

describe('receiver runtime caller-owned lane admission', () => {
  it('records lane replacement and ignores delayed retirement of its old epoch', async () => {
    const events: ProtocolTraceEvent[] = []
    const { runtime, initialChannel } = runtimeFixture({ current: event => events.push(event) }, lane.attachedLaneId)
    const replacement = new ScriptedChannel(b64ToBytes(lane.laneAckB64))
    try {
      await expect(runtime.adoptGrantedLane(replacement, grant())).resolves.toMatchObject({ installation: 'installed' })
      await runtime.retireLaneIfCovered({ laneId: lane.attachedLaneId, laneEpoch: 0 })
      await runtime.retireLane(lane.attachedLaneId, 0)
      expect(initialChannel.closes).toBe(1)
      expect(replacement.closes).toBe(0)
      expect(runtime.availableLaneIds()).toEqual([lane.attachedLaneId])
      expect(events.filter(event => event.eventName === 'lane_transition' && event.transition === 'detached')).toMatchObject([{
        termination: { kind: 'closed', reason: 'replaced' },
        correlation: { lane: { id: lane.attachedLaneId, epoch: 0 } },
      }])
      await runtime.retireLane(lane.attachedLaneId, lane.attachedLaneEpoch)
      expect(replacement.closes).toBe(1)
      expect(runtime.isClosed).toBe(false)
    } finally {
      await runtime.close()
    }
  })

  it.each(['ordinary', 'spare'] as const)('guards %s retirement through synchronous trace and content callbacks', async (renewal) => {
    const next = grant()
    const observations: Array<{
      source: string; usable: readonly number[]; reciprocal: Promise<void> | undefined; joined: Promise<void> | undefined
    }> = []
    let observe: (source: string) => void = () => {}
    const { runtime, initialChannel } = runtimeFixture({ current: event => {
      if (event.eventName === 'lane_transition' && event.transition === 'unavailable' &&
        event.correlation.lane?.id === runtime.initialLaneId) observe('trace')
    } })
    const old = { laneId: runtime.initialLaneId, laneEpoch: 0 }
    const successor = new ScriptedChannel(b64ToBytes(lane.laneAckB64))
    const connectivity = new ReceiverConnectivity({
      session: runtime, lanes: new LaneSet(), nativePeerUsable: () => false,
      createBlockLane: ({ laneId }) => ({ id: laneId, fetchBlock: async () => { throw new Error('unexpected block request') } }),
      onContentLaneDetached: removed => { if (removed.laneId === old.laneId) observe('content') },
    })
    try {
      connectivity.begin('download')
      expect(runtime.retireLaneIfCovered(old)).toBeUndefined()
      await runtime.adoptGrantedLane(successor, next)
      connectivity.addRelayLane(next.laneId)
      expect(runtime.retireLaneIfCovered(old, { ...next, laneEpoch: next.laneEpoch + 1 })).toBeUndefined()
      expect(runtime.retireLaneIfCovered(old, old)).toBeUndefined()
      observe = source => {
        const reciprocal = runtime.retireLaneIfCovered(next)
        const joined = runtime.retireLaneIfCovered(old, next)
        observations.push({ source, usable: runtime.availableLaneIds(), reciprocal, joined })
      }
      const retirement = runtime.retireLaneIfCovered(old, renewal === 'spare' ? next : undefined)
      expect(retirement).toBeInstanceOf(Promise)
      expect(observations.map(observation => observation.source)).toEqual(['trace', 'content'])
      for (const observation of observations) {
        expect(observation.usable).toEqual([next.laneId])
        expect(observation.reciprocal).toBeUndefined()
        expect(observation.joined).toBe(retirement)
      }
      await retirement
      expect(initialChannel.closes).toBe(1)
      expect(successor.closes).toBe(0)
    } finally {
      await connectivity.close()
      await runtime.close()
    }
  })

  it('keeps the last usable lane while an authenticated alternative is draining', async () => {
    const { runtime } = runtimeFixture()
    const old = { laneId: runtime.initialLaneId, laneEpoch: 0 }
    const next = grant()
    const successor = new ScriptedChannel(b64ToBytes(lane.laneAckB64))
    try {
      await runtime.adoptGrantedLane(successor, next)
      successor.availability.abort()
      expect(runtime.laneEpoch(next.laneId)).toBe(next.laneEpoch)
      expect(runtime.retireLaneIfCovered(old)).toBeUndefined()
      expect(runtime.retireLaneIfCovered(old, next)).toBeUndefined()
      await runtime.retireLaneIfCovered(next)
      expect(runtime.availableLaneIds()).toEqual([old.laneId])
    } finally {
      await runtime.close()
    }
  })

  it.each([false, true])('accepts a recycled automatic slot only with an advancing epoch (stale=%s)', async (stale) => {
    const { runtime } = runtimeFixture()
    const current = grant()
    await runtime.adoptGrantedLane(new ScriptedChannel(b64ToBytes(lane.laneAckB64)), current)
    const epoch = current.laneEpoch + (stale ? 0 : 1)
    vi.spyOn(runtime, 'beginOperation').mockResolvedValue({
      id: current.grantOperationId,
      requestKind: MESSAGE_KIND.laneAttach,
      next: async () => encodeMessage(
        MESSAGE_KIND.laneAttach,
        current.grantOperationId,
        encodeCanonicalCbor(new Map<number, unknown>([
          [0, 1], [1, 1], [2, current.laneId], [3, epoch], [4, current.attachNonce],
        ])),
      ),
      cancel: () => undefined,
    })
    if (stale) {
      await expect(runtime.requestLaneGrant()).rejects.toMatchObject({ scope: 'session' })
      expect(runtime.isClosed).toBe(true)
    } else {
      await expect(runtime.requestLaneGrant()).resolves.toMatchObject({ laneId: current.laneId, laneEpoch: epoch })
      expect(runtime.isClosed).toBe(false)
    }
    await runtime.close()
  })

  it('retains the underlying authenticated lane failure before retiring the session', async () => {
    const events: ProtocolTraceEvent[] = []
    const { runtime, initialChannel } = runtimeFixture({ current: event => events.push(event) })
    initialChannel.receive(new Uint8Array([0]))

    await vi.waitFor(() => expect(runtime.isClosed).toBe(true))
    expect(events.find(event => event.eventName === 'lane_transition' && event.transition === 'detached')).toMatchObject({
      eventName: 'lane_transition',
      transition: 'detached',
      correlation: { lane: { id: 0x0102_0304, epoch: 0 } },
      termination: {
        kind: 'authenticated_failure',
        failure: {
          name: 'SessionRuntimeError',
          cause: {
            name: 'EnvelopeError',
            message: 'Envelope header is malformed or has the wrong direction',
          },
        },
      },
    })
    await runtime.close()
  })

  it('ends the whole session on a direction-invalid TURN control even with a healthy second lane', async () => {
    const events: ProtocolTraceEvent[] = []
    const { runtime, initialChannel } = runtimeFixture({ current: event => events.push(event) })
    const second = new ScriptedChannel(b64ToBytes(lane.laneAckB64))
    try {
      await runtime.adoptGrantedLane(second, grant())
      expect(runtime.availableLaneIds()).toHaveLength(2)
      const demand = encodeTURNDemand({ requestId: 1n, mode: 'parallel', serviceIds: [] })
      const { plaintext } = encodeMessage(MESSAGE_KIND.turnControl, undefined, demand)
      vi.spyOn(EnvelopeOpener.prototype, 'open').mockResolvedValueOnce({ sequence: 0n, plaintext })
      initialChannel.receive(new Uint8Array())
      await vi.waitFor(() => expect(runtime.isClosed).toBe(true))
      expect(events).toContainEqual(expect.objectContaining({
        transition: 'detached', termination: expect.objectContaining({ kind: 'authenticated_failure' }),
      }))
      expect(second.closes).toBe(1)
    } finally {
      vi.restoreAllMocks()
      await runtime.close()
    }
  })

  it('publishes authenticated milestones and transfers an accepted channel to the runtime', async () => {
    const events: ProtocolTraceEvent[] = []
    const { runtime } = runtimeFixture({
      current: (event) => {
        events.push(event)
        throw new Error('observer failure is not admission authority')
      },
    })
    const channel = new ScriptedChannel(b64ToBytes(lane.laneAckB64))

    const result = await runtime.adoptGrantedLane(channel, grant())

    expect(result).toMatchObject({ disposition: 'accepted', installation: 'installed' })
    expect(channel.sent).toEqual([b64ToBytes(lane.laneHelloB64)])
    expect(runtime.availableLaneIds()).toEqual([0x0102_0304, lane.attachedLaneId])
    expect(channel.closes).toBe(0)
    expect(events.map((event) => event.eventName === 'lane_transition' && event.transition)).toEqual([
      'attached',
      'hello_sent',
      'admission_response_received',
      'admission_accepted',
      'attached',
      'installed',
    ])
    await runtime.close()
    expect(channel.closes).toBe(1)
  })

  it('preserves the full authenticated rejection and closes the consumed candidate once', async () => {
    const events: ProtocolTraceEvent[] = []
    const { runtime } = runtimeFixture({ current: (event) => events.push(event) })
    const channel = new ScriptedChannel(b64ToBytes(lane.laneRejectB64))

    const result = await runtime.adoptGrantedLane(channel, grant())
    expect(result).toMatchObject({
      disposition: 'rejected',
      installation: 'not-attempted',
      rejection: {
        code: lane.laneRejectCode,
        retryAfterMilliseconds: lane.laneRejectRetryAfterMilliseconds,
      },
      error: {
        rejection: {
          code: lane.laneRejectCode,
          retryAfterMilliseconds: lane.laneRejectRetryAfterMilliseconds,
        },
      },
    })
    expect(events.at(-1)).toMatchObject({
      eventName: 'lane_transition',
      transition: 'admission_rejected',
      rejectionCode: lane.laneRejectCode,
      retryAfterMilliseconds: lane.laneRejectRetryAfterMilliseconds,
    })
    expect(channel.closes).toBe(1)
    expect(runtime.availableLaneIds()).toEqual([0x0102_0304])
    await runtime.close()
  })

  it('returns authenticated acceptance with its typed installation failure', async () => {
    const { runtime } = runtimeFixture()
    const installed = new ScriptedChannel(b64ToBytes(lane.laneAckB64))
    await expect(runtime.adoptGrantedLane(installed, grant())).resolves.toMatchObject({
      disposition: 'accepted',
      installation: 'installed',
    })
    const candidate = new ScriptedChannel(b64ToBytes(lane.laneAckB64))

    const result = await runtime.adoptGrantedLane(candidate, grant())

    expect(result).toMatchObject({
      disposition: 'accepted',
      installation: 'failed',
      grantOperationId: { kind: 'protocol_operation', value: BigInt(lane.grantOperationId) },
      laneId: lane.attachedLaneId,
      laneEpoch: lane.attachedLaneEpoch,
    })
    expect(result.installation === 'failed' && result.error).toBeInstanceOf(LaneInstallationError)
    expect(candidate.closes).toBe(1)
    expect(runtime.availableLaneIds()).toEqual([0x0102_0304, lane.attachedLaneId])
    await runtime.close()
    expect(installed.closes).toBe(1)
    expect(candidate.closes).toBe(1)
  })

  it('has no hidden admission timer and settles only from its caller signal', async () => {
    vi.useFakeTimers()
    const { runtime } = runtimeFixture()
    const channel = new ScriptedChannel()
    const controller = new AbortController()
    let settled = false
    const adoption = runtime.adoptGrantedLane(channel, grant(), { signal: controller.signal })
      .finally(() => { settled = true })

    await vi.advanceTimersByTimeAsync(60_000)
    expect(settled).toBe(false)
    expect(vi.getTimerCount()).toBe(0)

    controller.abort(new DOMException('caller admission budget expired', 'TimeoutError'))
    await expect(adoption).resolves.toMatchObject({
      disposition: 'unverified',
      installation: 'not-attempted',
      error: { name: 'TimeoutError' },
    })
    expect(channel.closes).toBe(1)
    await runtime.close()
  })

  it('retries requestLaneGrant with a fresh operation ID on operationRetry without closing the session', async () => {
    const { runtime, initialChannel } = runtimeFixture()
    try {
      const retryBody = encodeCanonicalCbor(new Map<number, unknown>([[0, 1], [1, OPERATION_RETRY_REASON.replayWindow]]))
      const grantBody = encodeCanonicalCbor(new Map<number, unknown>([
        [0, 1],
        [1, 1],
        [2, lane.attachedLaneId],
        [3, lane.attachedLaneEpoch],
        [4, b64ToBytes(lane.attachNonceB64)],
      ]))
      const openSpy = vi.spyOn(EnvelopeOpener.prototype, 'open')
      openSpy
        .mockResolvedValueOnce({ sequence: 0n, plaintext: encodeMessage(MESSAGE_KIND.operationRetry, 1n, retryBody).plaintext })
        .mockResolvedValueOnce({ sequence: 1n, plaintext: encodeMessage(MESSAGE_KIND.laneAttach, 2n, grantBody).plaintext })

      const pendingGrant = runtime.requestLaneGrant(0)
      await vi.waitFor(() => expect(initialChannel.sent).toHaveLength(1))
      initialChannel.receive(new Uint8Array())
      await vi.waitFor(() => expect(initialChannel.sent).toHaveLength(2))
      initialChannel.receive(new Uint8Array())

      const granted = await pendingGrant
      expect(granted.laneId).toBe(lane.attachedLaneId)
      expect(granted.laneEpoch).toBe(lane.attachedLaneEpoch)
      expect(granted.grantOperationId).toBe(2n)
      expect(runtime.isClosed).toBe(false)
    } finally {
      vi.restoreAllMocks()
      await runtime.close()
    }
  })
})

function runtimeFixture(
  protocolTrace?: ReceiverSessionOptions['protocolTrace'],
  initialLaneId = 0x0102_0304,
): { readonly runtime: ReceiverSessionRuntime; readonly initialChannel: ScriptedChannel } {
  const initialChannel = new ScriptedChannel()
  const initialReader = initialChannel.frames.getReader()
  const descriptor = {
    shareInstance: b64ToBytes(lane.shareInstanceB64),
    sender: createEd25519Verifier(b64ToBytes(identityVector.senderPublicKeyB64)),
  } as ShareDescriptor
  const keys: SessionKeys = Object.freeze({
    protocolSessionId: vectorBytes(lane.protocolSessionIdB64),
    transcriptHash: new Uint8Array(32),
    receiverToSenderKey: vectorBytes(lane.receiverToSenderKeyB64),
    senderToReceiverKey: vectorBytes(lane.senderToReceiverKeyB64),
    initialLaneId,
    initialLaneEpoch: 0,
  })
  const options = {
    descriptor,
    readSecret: new Uint8Array(32),
    initialChannel,
    randomBytes: (length: number) => new Uint8Array(length).fill(1),
    ...(protocolTrace === undefined ? {} : { protocolTrace }),
  } as ReceiverSessionOptions
  const Runtime = ReceiverSessionRuntime as unknown as new (
    options: ReceiverSessionOptions,
    keys: SessionKeys,
    receiverInstanceId: Uint8Array,
    reader: ReadableStreamDefaultReader<Uint8Array>,
  ) => ReceiverSessionRuntime
  return {
    runtime: new Runtime(options, keys, new Uint8Array(16).fill(2), initialReader),
    initialChannel,
  }
}

function grant(): LaneGrant {
  return Object.freeze({
    laneId: lane.attachedLaneId,
    laneEpoch: lane.attachedLaneEpoch,
    grantOperationId: BigInt(lane.grantOperationId),
    attachNonce: vectorBytes(lane.attachNonceB64),
  })
}

function vectorBytes(value: string): Uint8Array<ArrayBuffer> {
  return new Uint8Array(b64ToBytes(value))
}

function named<T extends VectorCase>(name: string): T {
  const candidate = vectors.cases.find((item) => item.name === name)
  if (candidate === undefined) throw new Error(`missing vector ${name}`)
  return candidate as T
}

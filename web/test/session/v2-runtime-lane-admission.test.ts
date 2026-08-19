import { afterEach, describe, expect, it, vi } from 'vitest'
import type { V2ShareDescriptor } from '../../src/catalog/v2-records'
import type { FrameChannel } from '../../src/contracts/channel'
import type { V2ProtocolTraceEvent } from '../../src/session/v2-diagnostics'
import {
  V2LaneInstallationError,
  V2ReceiverSessionRuntime,
} from '../../src/session/v2-runtime'
import type { V2LaneGrant } from '../../src/session/v2-lane-codec'
import type { V2ReceiverSessionOptions } from '../../src/session/v2-runtime-types'
import type { V2SessionKeys } from '../../src/session/v2-transcript'
import { b64ToBytes, loadVectorFile, type VectorCase } from '../vectors'
import { identity as identityVector } from '../protocol/r0-contract-support'

interface LaneVector extends VectorCase {
  readonly attachNonceB64: string
  readonly attachedLaneEpoch: number
  readonly attachedLaneId: number
  readonly grantOperationIdB64: string
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
  it('publishes authenticated milestones and transfers an accepted channel to the runtime', async () => {
    const events: V2ProtocolTraceEvent[] = []
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
    expect(runtime.laneIds()).toEqual([0x0102_0304, lane.attachedLaneId])
    expect(channel.closes).toBe(0)
    expect(events.map((event) => event.eventName === 'lane_transition' && event.transition)).toEqual([
      'attached',
      'hello_sent',
      'admission_accepted',
      'attached',
      'installed',
    ])
    await runtime.close()
    expect(channel.closes).toBe(1)
  })

  it('preserves the full authenticated rejection and closes the consumed candidate once', async () => {
    const events: V2ProtocolTraceEvent[] = []
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
    expect(runtime.laneIds()).toEqual([0x0102_0304])
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
      grantOperationId: { kind: 'protocol_operation', byteLength: 16 },
      laneId: lane.attachedLaneId,
      laneEpoch: lane.attachedLaneEpoch,
    })
    expect(result.installation === 'failed' && result.error).toBeInstanceOf(V2LaneInstallationError)
    expect(candidate.closes).toBe(1)
    expect(runtime.laneIds()).toEqual([0x0102_0304, lane.attachedLaneId])
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
})

function runtimeFixture(
  protocolTrace?: V2ReceiverSessionOptions['protocolTrace'],
): { readonly runtime: V2ReceiverSessionRuntime } {
  const initialChannel = new ScriptedChannel()
  const initialReader = initialChannel.frames.getReader()
  const descriptor = {
    shareInstance: b64ToBytes(lane.shareInstanceB64),
    senderPublicKey: b64ToBytes(identityVector.senderPublicKeyB64),
  } as V2ShareDescriptor
  const keys: V2SessionKeys = Object.freeze({
    protocolSessionId: vectorBytes(lane.protocolSessionIdB64),
    transcriptHash: new Uint8Array(32),
    receiverToSenderKey: vectorBytes(lane.receiverToSenderKeyB64),
    senderToReceiverKey: vectorBytes(lane.senderToReceiverKeyB64),
    initialLaneId: 0x0102_0304,
    initialLaneEpoch: 0,
  })
  const options = {
    descriptor,
    readSecret: new Uint8Array(32),
    initialChannel,
    randomBytes: (length: number) => new Uint8Array(length).fill(1),
    ...(protocolTrace === undefined ? {} : { protocolTrace }),
  } as V2ReceiverSessionOptions
  const Runtime = V2ReceiverSessionRuntime as unknown as new (
    options: V2ReceiverSessionOptions,
    keys: V2SessionKeys,
    receiverInstanceId: Uint8Array,
    reader: ReadableStreamDefaultReader<Uint8Array>,
  ) => V2ReceiverSessionRuntime
  return {
    runtime: new Runtime(options, keys, new Uint8Array(16).fill(2), initialReader),
  }
}

function grant(): V2LaneGrant {
  return Object.freeze({
    laneId: lane.attachedLaneId,
    laneEpoch: lane.attachedLaneEpoch,
    grantOperationId: vectorBytes(lane.grantOperationIdB64),
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

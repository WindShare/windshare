import { describe, expect, it } from 'vitest'

import { V2_PATH_POLICY, type V2ShareDescriptor } from '../../src/catalog/v2-records'
import type { ChannelState, FrameChannel } from '../../src/contracts/channel'
import {
  decodeV2PeerCandidate,
  decodeV2PeerOffer,
  type V2PeerBinding,
} from '../../src/connectivity/v2-signaling-codec'
import {
  type V2SessionSignalingTrace,
  V2SessionSignalingRoute,
} from '../../src/connectivity/v2-session-signaling'
import {
  SIGNAL_KIND_ANSWER,
  SIGNAL_KIND_CANDIDATE,
  SIGNAL_KIND_OFFER,
  type ConnectivitySignal,
} from '../../src/connectivity/signaling'
import { encodeCanonicalCbor } from '../../src/protocol/cbor'
import { V2EnvelopeSealer } from '../../src/session/v2-envelope'
import { V2SessionLane } from '../../src/session/v2-lane-runtime'
import {
  encodeV2Body,
  encodeV2Message,
  type V2MessageKind,
  V2_MESSAGE_KIND,
} from '../../src/session/v2-message'
import { V2OperationRouter } from '../../src/session/v2-operation-router'
import type { V2ReceiverSessionRuntime } from '../../src/session/v2-runtime'
import type { V2SessionOperation } from '../../src/session/v2-runtime-types'
import type { V2SessionKeys } from '../../src/session/v2-transcript'
import { senderControlKeyPair, signSenderOperationControl } from '../session/signed-control-fixture'

const EXACT_CANDIDATE_REPLAY_STRESS_COUNT = 300

class InMemoryFrameChannel implements FrameChannel {
  readonly frames: ReadableStream<Uint8Array<ArrayBuffer>>
  readonly sent: Uint8Array<ArrayBuffer>[] = []
  #controller!: ReadableStreamDefaultController<Uint8Array<ArrayBuffer>>
  state: ChannelState = 'open'

  constructor() {
    this.frames = new ReadableStream({
      start: (controller) => { this.#controller = controller },
    })
  }

  receive(frame: Uint8Array): void {
    this.#controller.enqueue(frame.slice())
  }

  async send(frame: Uint8Array): Promise<void> {
    this.sent.push(frame.slice())
  }

  async sendTerminal(frame: Uint8Array): Promise<void> {
    this.sent.push(frame.slice())
  }

  async close(): Promise<void> {
    if (this.state === 'closed') return
    this.state = 'closed'
    try {
      this.#controller.close()
    } catch {
      // The lane reader may already have cancelled the stream.
    }
  }
}

class SignalingSessionFacade {
  readonly #router: V2OperationRouter
  readonly #operationId: Uint8Array<ArrayBuffer>
  operation: V2SessionOperation | undefined
  offerBody: Uint8Array<ArrayBuffer> | undefined
  followups: { readonly kind: V2MessageKind; readonly body: Uint8Array<ArrayBuffer> }[] = []
  closeCalls = 0

  constructor(router: V2OperationRouter, operationId: Uint8Array<ArrayBuffer>) {
    this.#router = router
    this.#operationId = operationId
  }

  async beginOperation(kind: V2MessageKind, body: Uint8Array): Promise<V2SessionOperation> {
    this.offerBody = body.slice()
    this.operation = this.#router.create(this.#operationId, kind, body)
    return this.operation
  }

  async sendOperationMessage(
    operation: V2SessionOperation,
    kind: V2MessageKind,
    body: Uint8Array,
  ): Promise<void> {
    if (operation !== this.operation) throw new Error('unexpected signaling operation')
    this.followups.push(Object.freeze({ kind, body: body.slice() }))
  }

  async cancelOperation(operation: V2SessionOperation): Promise<void> {
    operation.close()
  }

  async close(): Promise<void> {
    this.closeCalls += 1
    this.operation?.close()
  }
}

describe('v2 authenticated session signaling', () => {
  it('carries an offer through signed answer and candidate delivery on the production path', async () => {
    const senderKeys = await senderControlKeyPair()
    const descriptor = shareDescriptor(senderKeys.publicKey)
    const sessionKeys = protocolKeys()
    const operationId = identity(40)
    const binding: V2PeerBinding = Object.freeze({
      peerPathId: identity(41),
      attemptId: identity(42),
    })
    const router = new V2OperationRouter(() => undefined)
    const session = new SignalingSessionFacade(router, operationId)
    const route = new V2SessionSignalingRoute(
      session as unknown as V2ReceiverSessionRuntime,
      binding,
    )
    const incoming = route.messages.getReader()
    const channel = new InMemoryFrameChannel()
    const lane = new V2SessionLane({
      channel,
      reader: channel.frames.getReader(),
      keys: sessionKeys,
      descriptor,
      laneId: sessionKeys.initialLaneId,
      laneEpoch: sessionKeys.initialLaneEpoch,
      router,
      onClosed: () => undefined,
    })
    let nonce = 0
    const senderSealer = new V2EnvelopeSealer(sessionKeys.senderToReceiverKey, {
      shareInstance: descriptor.shareInstance,
      protocolSessionId: sessionKeys.protocolSessionId,
      laneId: sessionKeys.initialLaneId,
      laneEpoch: sessionKeys.initialLaneEpoch,
      direction: 1,
    }, { randomBytes: (length) => new Uint8Array(length).fill(++nonce) })

    await route.send({
      kind: SIGNAL_KIND_OFFER,
      payload: { type: SIGNAL_KIND_OFFER, sdp: 'v=0\r\na=setup:actpass\r\n' },
    })
    expect(decodeV2PeerOffer(requireValue(session.offerBody))).toEqual({
      ...binding,
      sdp: 'v=0\r\na=setup:actpass\r\n',
    })

    const answerBody = [1, binding.peerPathId, binding.attemptId, 'v=0\r\na=setup:active\r\n']
    const answer = await signSenderOperationControl({
      kind: V2_MESSAGE_KIND.peerAnswer,
      operationId,
      semanticBody: encodeCanonicalCbor(answerBody),
      binding: controlBinding(descriptor, sessionKeys, 0n),
      privateKey: senderKeys.privateKey,
    })
    channel.receive(await senderSealer.seal(answer.message.plaintext))
    await expect(incoming.read()).resolves.toEqual({
      done: false,
      value: {
        kind: SIGNAL_KIND_ANSWER,
        payload: { type: SIGNAL_KIND_ANSWER, sdp: 'v=0\r\na=setup:active\r\n' },
      },
    })

    const replayedAnswer = await signSenderOperationControl({
      kind: V2_MESSAGE_KIND.peerAnswer,
      operationId,
      semanticBody: encodeCanonicalCbor(answerBody),
      binding: controlBinding(descriptor, sessionKeys, 1n),
      privateKey: senderKeys.privateKey,
    })
    channel.receive(await senderSealer.seal(replayedAnswer.message.plaintext))

    const candidateBody = [
      1,
      binding.peerPathId,
      binding.attemptId,
      'candidate:1 1 udp 1 192.0.2.1 5000 typ host',
      'data',
      0,
      'windshare',
    ]
    const candidate = await signSenderOperationControl({
      kind: V2_MESSAGE_KIND.peerCandidate,
      operationId,
      semanticBody: encodeCanonicalCbor(candidateBody),
      binding: controlBinding(descriptor, sessionKeys, 2n),
      privateKey: senderKeys.privateKey,
    })
    channel.receive(await senderSealer.seal(candidate.message.plaintext))
    await expect(incoming.read()).resolves.toEqual({
      done: false,
      value: {
        kind: SIGNAL_KIND_CANDIDATE,
        payload: {
          candidate: 'candidate:1 1 udp 1 192.0.2.1 5000 typ host',
          sdpMid: 'data',
          sdpMLineIndex: 0,
          usernameFragment: 'windshare',
        },
      },
    })

    const replayedCandidate = await signSenderOperationControl({
      kind: V2_MESSAGE_KIND.peerCandidate,
      operationId,
      semanticBody: encodeCanonicalCbor(candidateBody),
      binding: controlBinding(descriptor, sessionKeys, 3n),
      privateKey: senderKeys.privateKey,
    })
    const distinctCandidateBody = [
      1,
      binding.peerPathId,
      binding.attemptId,
      'candidate:3 1 udp 1 192.0.2.3 5002 typ host',
      'data',
      0,
      'windshare',
    ]
    const distinctCandidate = await signSenderOperationControl({
      kind: V2_MESSAGE_KIND.peerCandidate,
      operationId,
      semanticBody: encodeCanonicalCbor(distinctCandidateBody),
      binding: controlBinding(descriptor, sessionKeys, 4n),
      privateKey: senderKeys.privateKey,
    })
    channel.receive(await senderSealer.seal(replayedCandidate.message.plaintext))
    channel.receive(await senderSealer.seal(distinctCandidate.message.plaintext))
    await expect(incoming.read()).resolves.toEqual({
      done: false,
      value: {
        kind: SIGNAL_KIND_CANDIDATE,
        payload: {
          candidate: 'candidate:3 1 udp 1 192.0.2.3 5002 typ host',
          sdpMid: 'data',
          sdpMLineIndex: 0,
          usernameFragment: 'windshare',
        },
      },
    })

    const receiverCandidate: ConnectivitySignal = {
      kind: SIGNAL_KIND_CANDIDATE,
      payload: {
        candidate: 'candidate:2 1 udp 1 192.0.2.2 5001 typ host',
        sdpMid: null,
        sdpMLineIndex: null,
        usernameFragment: null,
      },
    }
    await route.send(receiverCandidate)
    expect(session.followups).toHaveLength(1)
    expect(session.followups[0]?.kind).toBe(V2_MESSAGE_KIND.peerCandidate)
    expect(decodeV2PeerCandidate(requireValue(session.followups[0]?.body))).toEqual({
      ...binding,
      candidate: 'candidate:2 1 udp 1 192.0.2.2 5001 typ host',
      sdpMid: null,
      sdpMLineIndex: null,
      usernameFragment: null,
    })

    await route.send({
      kind: SIGNAL_KIND_CANDIDATE,
      payload: {
        candidate: 'candidate:4 1 udp 1 192.0.2.4 5003 typ host',
        sdpMid: null,
        sdpMLineIndex: null,
        usernameFragment: null,
      },
    })
    expect(session.followups).toHaveLength(2)

    await route.close()
    incoming.releaseLock()
    await lane.close()
    expect(session.closeCalls).toBe(0)
  })

  it('dedupes concurrent candidate replays that arrive before the answer', async () => {
    const router = new V2OperationRouter(() => undefined)
    const operationId = identity(45)
    const binding: V2PeerBinding = Object.freeze({
      peerPathId: identity(46),
      attemptId: identity(47),
    })
    const session = new SignalingSessionFacade(router, operationId)
    const route = new V2SessionSignalingRoute(
      session as unknown as V2ReceiverSessionRuntime,
      binding,
    )
    const incoming = route.messages.getReader()
    await route.send({
      kind: SIGNAL_KIND_OFFER,
      payload: { type: SIGNAL_KIND_OFFER, sdp: 'v=0\r\ns=pre-answer-replay\r\n' },
    })

    const firstCandidateBody = encodedCandidateBody(binding, 45)
    const replayRoutes = await Promise.all(Array.from(
      { length: EXACT_CANDIDATE_REPLAY_STRESS_COUNT },
      () => router.route(encodeV2Message(
        V2_MESSAGE_KIND.peerCandidate,
        operationId,
        firstCandidateBody,
      )),
    ))
    expect(replayRoutes).toHaveLength(EXACT_CANDIDATE_REPLAY_STRESS_COUNT)
    await router.route(encodeV2Message(
      V2_MESSAGE_KIND.peerAnswer,
      operationId,
      encodeV2Body([1, binding.peerPathId, binding.attemptId, 'v=0\r\ns=answer\r\n']),
    ))
    await router.route(encodeV2Message(
      V2_MESSAGE_KIND.peerCandidate,
      operationId,
      encodedCandidateBody(binding, 46),
    ))

    await expect(incoming.read()).resolves.toMatchObject({
      done: false,
      value: {
        kind: SIGNAL_KIND_CANDIDATE,
        payload: { candidate: 'candidate:45 1 udp 1 192.0.2.45 5045 typ host' },
      },
    })
    await expect(incoming.read()).resolves.toMatchObject({
      done: false,
      value: { kind: SIGNAL_KIND_ANSWER },
    })
    await expect(incoming.read()).resolves.toMatchObject({
      done: false,
      value: {
        kind: SIGNAL_KIND_CANDIDATE,
        payload: { candidate: 'candidate:46 1 udp 1 192.0.2.46 5046 typ host' },
      },
    })
    await route.close()
    incoming.releaseLock()
  })

  it('contains a peer operation failure to its attempt while other session work stays healthy', async () => {
    const router = new V2OperationRouter(() => undefined)
    const operationId = identity(50)
    const binding: V2PeerBinding = Object.freeze({
      peerPathId: identity(51),
      attemptId: identity(52),
    })
    const session = new SignalingSessionFacade(router, operationId)
    const traces: V2SessionSignalingTrace[] = []
    const route = new V2SessionSignalingRoute(
      session as unknown as V2ReceiverSessionRuntime,
      binding,
      (event) => traces.push(event),
    )
    const incoming = route.messages.getReader()
    await route.send({
      kind: SIGNAL_KIND_OFFER,
      payload: { type: SIGNAL_KIND_OFFER, sdp: 'v=0\r\ns=attempt-local\r\n' },
    })

    const failedRead = incoming.read()
    await router.route(encodeV2Message(
      V2_MESSAGE_KIND.operationError,
      operationId,
      encodeV2Body(new Map<number, unknown>([
        [0, 1], [1, 5], [2, 0x5001], [3, false], [4, null], [5, 'peer rejected'],
      ])),
    ))
    await expect(failedRead).rejects.toThrow(/Sender rejected peer negotiation/)
    expect(session.closeCalls).toBe(0)
    expect(traces).toContainEqual(expect.objectContaining({
      type: 'route-failed',
      failureScope: 'attempt',
    }))

    const healthyId = identity(53)
    const healthy = router.create(
      healthyId,
      V2_MESSAGE_KIND.listChildren,
      encodeV2Body([]),
    )
    const result = encodeV2Message(
      V2_MESSAGE_KIND.catalogResult,
      healthyId,
      encodeV2Body(new Map<number, unknown>([[0, 1], [1, identity(54)]])),
    )
    await router.route(result)
    await expect(healthy.next()).resolves.toEqual(result)
    incoming.releaseLock()
  })

  it('keeps authenticated binding conflicts session-fatal', async () => {
    const router = new V2OperationRouter(() => undefined)
    const operationId = identity(60)
    const binding: V2PeerBinding = Object.freeze({
      peerPathId: identity(61),
      attemptId: identity(62),
    })
    const session = new SignalingSessionFacade(router, operationId)
    const traces: V2SessionSignalingTrace[] = []
    const route = new V2SessionSignalingRoute(
      session as unknown as V2ReceiverSessionRuntime,
      binding,
      (event) => traces.push(event),
    )
    const incoming = route.messages.getReader()
    await route.send({
      kind: SIGNAL_KIND_OFFER,
      payload: { type: SIGNAL_KIND_OFFER, sdp: 'v=0\r\ns=binding-conflict\r\n' },
    })

    const failedRead = incoming.read()
    const routeFailure = await router.route(encodeV2Message(
      V2_MESSAGE_KIND.peerCandidate,
      operationId,
      encodeV2Body([
        1,
        binding.peerPathId,
        identity(63),
        'candidate:63 1 udp 1 192.0.2.63 5063 typ host',
        null,
        null,
        null,
      ]),
    )).then(
      () => new Error('binding conflict unexpectedly entered the operation queue'),
      (reason: unknown) => reason,
    )
    expect(routeFailure).toMatchObject({ scope: 'session' })
    router.terminate(routeFailure)
    await expect(failedRead).rejects.toMatchObject({ scope: 'session' })
    expect(session.closeCalls).toBe(1)
    expect(traces).toContainEqual(expect.objectContaining({
      type: 'route-failed',
      failureScope: 'session',
    }))
    incoming.releaseLock()
  })
})

function controlBinding(
  descriptor: V2ShareDescriptor,
  keys: V2SessionKeys,
  sequence: bigint,
) {
  return Object.freeze({
    shareInstance: descriptor.shareInstance,
    protocolSessionId: keys.protocolSessionId,
    laneId: keys.initialLaneId,
    laneEpoch: keys.initialLaneEpoch,
    direction: 1 as const,
    sequence,
  })
}

function protocolKeys(): V2SessionKeys {
  return Object.freeze({
    protocolSessionId: identity(30),
    transcriptHash: new Uint8Array(32).fill(31),
    receiverToSenderKey: new Uint8Array(32).fill(32),
    senderToReceiverKey: new Uint8Array(32).fill(33),
    initialLaneId: 34,
    initialLaneEpoch: 0,
  })
}

function shareDescriptor(senderPublicKey: Uint8Array<ArrayBuffer>): V2ShareDescriptor {
  return Object.freeze({
    wireVersion: 2,
    suite: 2,
    shareInstance: identity(20),
    shareInstanceId: 'share-20',
    syntheticRoot: identity(21),
    syntheticRootId: 'root-21',
    chunkSize: 65_536,
    capabilities: 0n,
    senderPublicKey: senderPublicKey.slice(),
    createdAtSeconds: 1n,
    pathPolicy: V2_PATH_POLICY,
  })
}

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

function requireValue<T>(value: T | undefined): T {
  if (value === undefined) throw new Error('expected test value')
  return value
}

function encodedCandidateBody(binding: V2PeerBinding, seed: number): Uint8Array<ArrayBuffer> {
  return encodeV2Body([
    1,
    binding.peerPathId,
    binding.attemptId,
    `candidate:${seed} 1 udp 1 192.0.2.${seed} ${5_000 + seed} typ host`,
    'data',
    0,
    'windshare',
  ])
}

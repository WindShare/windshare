import { describe, expect, it } from 'vitest'

import {
  BrowserOfferChannelFactory,
  browserPeerConnectionAvailable,
  CandidateLimitExceededError,
  MAX_ICE_CANDIDATES_PER_PEER,
  PeerNegotiationError,
  SIGNAL_KIND_ANSWER,
  SIGNAL_KIND_CANDIDATE,
  SIGNAL_KIND_OFFER,
  UnexpectedDataChannelError,
  type ConnectivitySignal,
  type SignalingRoute,
} from '../../src/connectivity'
import { TERMINAL_INTENT_CONTROL } from '../../src/transport/webrtc'
import { FakeRTCDataChannel } from '../transport/webrtc/fakes'

type FatalWaitKind = 'SDP' | 'ICE' | 'signaling'
type NegotiationFixture = ReturnType<typeof negotiationFixture>
type OpenedPeerChannel = Awaited<ReturnType<BrowserOfferChannelFactory['offer']>>
type ErrorConstructor = new (...args: never[]) => Error

interface FatalCallbackCase {
  readonly fatalName: string
  readonly maximumCandidates: number
  readonly errorType: ErrorConstructor
  readonly expectedMessage: RegExp
  readonly trigger: (fixture: NegotiationFixture) => FakeRTCDataChannel | undefined
}

type InFlightFatalWait =
  | {
    readonly phase: 'opening'
    readonly fixture: NegotiationFixture
    readonly opening: Promise<OpenedPeerChannel>
  }
  | {
    readonly phase: 'established'
    readonly fixture: NegotiationFixture
    readonly channel: OpenedPeerChannel
    readonly closing: Promise<void>
  }

const FATAL_WAIT_KINDS = ['SDP', 'ICE', 'signaling'] as const satisfies readonly FatalWaitKind[]
const LOCAL_CANDIDATE_REPLAY_STRESS_COUNT = 300

const FATAL_CALLBACK_CASES = [
  {
    fatalName: 'an unexpected remote DataChannel',
    maximumCandidates: MAX_ICE_CANDIDATES_PER_PEER,
    errorType: UnexpectedDataChannelError,
    expectedMessage: /unexpected DataChannel/u,
    trigger: (fixture) => fixture.peer.emitUnexpectedDataChannel(),
  },
  {
    fatalName: 'PeerConnection failure',
    maximumCandidates: MAX_ICE_CANDIDATES_PER_PEER,
    errorType: PeerNegotiationError,
    expectedMessage: /entered failed state/u,
    trigger: (fixture) => {
      fixture.peer.fail()
      return undefined
    },
  },
  {
    fatalName: 'local candidate overflow',
    maximumCandidates: 1,
    errorType: CandidateLimitExceededError,
    expectedMessage: /candidate limit 1 exceeded/iu,
    trigger: (fixture) => {
      fixture.peer.emitCandidate('fatal-limit-one')
      fixture.peer.emitCandidate('fatal-limit-two')
      return undefined
    },
  },
  {
    fatalName: 'local candidate serialization failure',
    maximumCandidates: MAX_ICE_CANDIDATES_PER_PEER,
    errorType: PeerNegotiationError,
    expectedMessage: /encode a local ICE candidate/u,
    trigger: (fixture) => {
      fixture.peer.emitBrokenCandidate()
      return undefined
    },
  },
] satisfies readonly FatalCallbackCase[]

const FATAL_WAIT_MATRIX = FATAL_WAIT_KINDS.flatMap((waitKind) =>
  FATAL_CALLBACK_CASES.map((fatal) => ({ waitKind, fatal })),
)

describe('browser offer negotiation', () => {
  it('probes the injected runtime instead of inferring peer support from browser identity', () => {
    expect(browserPeerConnectionAvailable({})).toBe(false)
    expect(browserPeerConnectionAvailable({ RTCPeerConnection: undefined })).toBe(false)
    expect(browserPeerConnectionAvailable({ RTCPeerConnection: class {} })).toBe(true)
  })

  it('has no PeerConnection, offer, or ICE side effect before explicit offer', () => {
    let creations = 0
    const factory = new BrowserOfferChannelFactory({
      createPeerConnection: () => {
        creations += 1
        return new FakeNegotiationPeer().asPeer()
      },
    })

    expect(factory).toBeDefined()
    expect(creations).toBe(0)
  })

  it('sends offer then candidates and buffers remote candidates until the answer', async () => {
    const fixture = negotiationFixture()
    const opening = fixture.factory.offer(fixture.route, fixture.signal)
    await settle()
    expect(fixture.route.sent[0]).toEqual({
      kind: SIGNAL_KIND_OFFER,
      payload: { type: 'offer', sdp: 'local-offer' },
    })

    fixture.peer.emitCandidate('local-one')
    fixture.route.push(candidateSignal('remote-before-answer'))
    await settle()
    expect(fixture.peer.addedCandidates).toHaveLength(0)
    expect(fixture.route.sent[1]).toMatchObject({
      kind: SIGNAL_KIND_CANDIDATE,
      payload: { candidate: 'local-one' },
    })

    fixture.route.push(answerSignal())
    await settle()
    expect(fixture.peer.remoteDescriptions).toEqual([
      { type: 'answer', sdp: 'remote-answer' },
    ])
    expect(fixture.peer.addedCandidates).toEqual([
      { candidate: 'remote-before-answer' },
    ])

    fixture.peer.raw.open()
    const channel = await opening
    expect(channel.state).toBe('open')
    fixture.route.push(candidateSignal('remote-after-answer'))
    await settle()
    expect(fixture.peer.addedCandidates.at(-1)).toEqual({
      candidate: 'remote-after-answer',
    })
    await channel.close()
  })

  it('bounds remote candidates across the pre-description lifetime', async () => {
    const fixture = negotiationFixture(2)
    const opening = fixture.factory.offer(fixture.route, fixture.signal)
    await settle()
    fixture.route.push(candidateSignal('one'))
    fixture.route.push(candidateSignal('two'))
    fixture.route.push(candidateSignal('three'))

    await expect(opening).rejects.toBeInstanceOf(CandidateLimitExceededError)
    expect(fixture.peer.closeCalls).toBe(1)
    expect(fixture.peer.raw.readyState).toBe('closed')
  })

  it('dedupes browser candidate callback replays before quota and event admission', async () => {
    const fixture = negotiationFixture(1)
    const channel = await openChannel(fixture)

    for (let index = 0; index < LOCAL_CANDIDATE_REPLAY_STRESS_COUNT; index += 1) {
      if (index % 2 === 0) fixture.peer.emitCandidate('same-local-candidate')
      else fixture.peer.emitCandidateWithNullOptionals('same-local-candidate')
    }
    await settle()
    expect(fixture.route.sent.filter((message) => (
      message.kind === SIGNAL_KIND_CANDIDATE
    ))).toEqual([
      {
        kind: SIGNAL_KIND_CANDIDATE,
        payload: {
          candidate: 'same-local-candidate',
          sdpMid: null,
          sdpMLineIndex: null,
          usernameFragment: null,
        },
      },
    ])
    expect(channel.state).toBe('open')
    expect(fixture.peer.closeCalls).toBe(0)

    fixture.peer.emitCandidate('distinct-local-candidate')
    await channel.done
    expect(reasonContains(channel.reason, CandidateLimitExceededError)).toBe(true)
    expect(fixture.peer.closeCalls).toBe(1)
  })

  it('cancels a hung browser SDP operation and closes both ownership layers', async () => {
    const fixture = negotiationFixture()
    fixture.peer.offerOperation = new Promise(() => undefined)
    const opening = fixture.factory.offer(fixture.route, fixture.signal)
    fixture.controller.abort(new DOMException('cancel offer', 'AbortError'))

    await expect(opening).rejects.toMatchObject({ name: 'AbortError' })
    expect(fixture.peer.closeCalls).toBe(1)
    expect(fixture.peer.raw.readyState).toBe('closed')
  })

  it('uses parent-first cancellation to release a terminal-pending channel owner', async () => {
    const fixture = negotiationFixture()
    const channel = await openChannel(fixture)
    fixture.peer.raw.receiveText(TERMINAL_INTENT_CONTROL)
    await settle()

    let closeSettled = false
    const closing = channel.close().then(() => {
      closeSettled = true
    })
    await settle()
    expect(closeSettled).toBe(false)

    fixture.controller.abort(new DOMException('cancel terminal-pending peer', 'AbortError'))
    await Promise.all([closing, channel.done])
    expect(closeSettled).toBe(true)
    expect(fixture.peer.closeCalls).toBe(1)
  })

  it.each(FATAL_WAIT_MATRIX)(
    'interrupts a pending $waitKind wait when callback reports $fatal.fatalName',
    async ({ waitKind, fatal }) => {
      const state = await beginFatalWait(waitKind, fatal.maximumCandidates)
      const openingFailure = state.phase === 'opening'
        ? state.opening.then(
          () => new Error('fatal negotiation unexpectedly opened a channel'),
          (reason: unknown) => reason,
        )
        : undefined

      const extra = fatal.trigger(state.fixture)
      // Direct browser callbacks own the parent before promise continuations run.
      expect(state.fixture.peer.closeCalls).toBe(1)
      expect(state.fixture.peer.raw.readyState).toBe('closed')

      const laterAbort = new DOMException('later caller cancellation', 'AbortError')
      state.fixture.controller.abort(laterAbort)
      const reason = state.phase === 'opening'
        ? await openingFailure
        : await settleEstablishedFatalWait(state)

      const owner = peerOwnerCause(reason)
      expect(owner).toBeInstanceOf(fatal.errorType)
      expect(owner).toMatchObject({ message: expect.stringMatching(fatal.expectedMessage) })
      expect(reasonMessageContains(reason, laterAbort.message)).toBe(false)
      expect(extra?.closeCalls).toBe(extra === undefined ? undefined : 1)
      expect(state.fixture.peer.closeCalls).toBe(1)
      expect(state.fixture.peer.lifecycle).toEqual(['peer-close', 'datachannel-close'])
      expect(state.fixture.route.cancelCalls).toBe(1)
      if (waitKind === 'signaling') {
        const operationSignal = state.fixture.route.sendSignals.at(-1)
        expect(operationSignal?.aborted).toBe(true)
        expect(operationSignal?.reason).toBe(owner)
      }
    },
  )

  it('keeps caller cancellation as owner when it wins the callback race', async () => {
    const state = await beginFatalWait('ICE', MAX_ICE_CANDIDATES_PER_PEER)
    if (state.phase !== 'established') {
      throw new Error('ICE wait must have an established channel')
    }
    const cancellation = new DOMException('caller won the race', 'AbortError')
    state.fixture.controller.abort(cancellation)
    const extra = state.fixture.peer.emitUnexpectedDataChannel()
    const reason = await settleEstablishedFatalWait(state)

    expect(peerOwnerCause(reason)).toBe(cancellation)
    expect(reasonContains(reason, UnexpectedDataChannelError)).toBe(false)
    expect(extra.closeCalls).toBe(1)
    expect(state.fixture.peer.closeCalls).toBe(1)
    expect(state.fixture.route.cancelCalls).toBe(1)
  })

  it('interrupts a pending operation when the bounded event queue overflows', async () => {
    const state = await beginFatalWait('ICE', 1)
    if (state.phase !== 'established') {
      throw new Error('ICE wait must have an established channel')
    }
    const overflowInputs = 32
    for (let index = 0; index < overflowInputs; index += 1) {
      state.fixture.route.push(candidateSignal(`queued-${index}`))
    }
    await settle(overflowInputs * 2)
    expect(state.fixture.peer.closeCalls).toBe(1)

    const laterAbort = new DOMException('later overflow cleanup', 'AbortError')
    state.fixture.controller.abort(laterAbort)
    const reason = await settleEstablishedFatalWait(state)
    expect(peerOwnerCause(reason)).toMatchObject({
      name: 'PeerNegotiationError',
      message: expect.stringMatching(/event queue is full/u),
    })
    expect(reasonMessageContains(reason, laterAbort.message)).toBe(false)
    expect(state.fixture.peer.lifecycle).toEqual(['peer-close', 'datachannel-close'])
    expect(state.fixture.route.cancelCalls).toBe(1)
  })

  it('rejects every remote-created DataChannel before and after Open', async () => {
    const early = negotiationFixture()
    const earlyOpening = early.factory.offer(early.route, early.signal)
    await settle()
    const earlyExtra = early.peer.emitUnexpectedDataChannel()
    await expect(earlyOpening).rejects.toBeInstanceOf(UnexpectedDataChannelError)
    expect(earlyExtra.closeCalls).toBe(1)
    expect(early.peer.closeCalls).toBe(1)

    const established = negotiationFixture()
    const channel = await openChannel(established)
    const lateExtra = established.peer.emitUnexpectedDataChannel()
    await channel.done
    expect(lateExtra.closeCalls).toBe(1)
    expect(reasonContains(channel.reason, UnexpectedDataChannelError)).toBe(true)
    expect(established.peer.closeCalls).toBe(1)
  })

  it('rejects signaling loss before Open but preserves an established channel', async () => {
    const early = negotiationFixture()
    const earlyOpening = early.factory.offer(early.route, early.signal)
    await settle()
    early.route.close()
    await expect(earlyOpening).rejects.toThrow(/signaling closed before DataChannel Open/u)

    const established = negotiationFixture()
    const opening = established.factory.offer(established.route, established.signal)
    await establish(established)
    const channel = await opening
    established.route.close()
    await settle()
    expect(channel.state).toBe('open')
    expect(established.peer.closeCalls).toBe(0)
    await channel.close()
    await settle()
    expect(established.peer.closeCalls).toBe(1)
  })

  it('treats post-Open protocol violations and candidate overflow as peer-fatal', async () => {
    const duplicate = negotiationFixture()
    const duplicateChannel = await openChannel(duplicate)
    duplicate.route.push(answerSignal())
    await duplicateChannel.done
    expect(duplicate.peer.closeCalls).toBe(1)

    const overflow = negotiationFixture(1)
    const overflowChannel = await openChannel(overflow)
    overflow.peer.emitCandidate('one')
    overflow.peer.emitCandidate('two')
    await overflowChannel.done
    expect(reasonContains(overflowChannel.reason, CandidateLimitExceededError)).toBe(true)
    expect(overflow.peer.closeCalls).toBe(1)
  })

  it('keeps Open P2P alive when a late candidate cannot use relay signaling', async () => {
    const fixture = negotiationFixture()
    const channel = await openChannel(fixture)
    fixture.route.failSends(new Error('relay signaling lost'))
    fixture.peer.emitCandidate('late-local')
    await settle()

    expect(channel.state).toBe('open')
    expect(fixture.peer.closeCalls).toBe(0)
    await channel.close()
  })

  it('reconciles native Open before a same-turn signaling write failure', async () => {
    const fixture = negotiationFixture()
    const opening = fixture.factory.offer(fixture.route, fixture.signal)
    await settle()
    fixture.route.push(answerSignal())
    await settle()
    fixture.route.failSends(new Error('same-turn relay loss'))

    fixture.peer.raw.open()
    fixture.peer.emitCandidate('same-turn-candidate')
    const channel = await opening
    await settle()
    expect(channel.state).toBe('open')
    expect(fixture.peer.closeCalls).toBe(0)
    await channel.close()
  })

  it('fails typed on invalid browser and signaling boundaries', async () => {
    const constructionFailure = new Error('synthetic PeerConnection construction failure')
    const construction = new BrowserOfferChannelFactory({
      createPeerConnection: () => {
        throw constructionFailure
      },
    })
    await expect(construction.offer(
      new ControlledRoute(),
      new AbortController().signal,
    )).rejects.toMatchObject({
      name: 'PeerNegotiationError',
      cause: constructionFailure,
    })

    const candidate = negotiationFixture()
    const candidateOpening = candidate.factory.offer(candidate.route, candidate.signal)
    await settle()
    candidate.peer.emitBrokenCandidate()
    await expect(candidateOpening).rejects.toMatchObject({ name: 'PeerNegotiationError' })

    const invalid = negotiationFixture()
    const invalidOpening = invalid.factory.offer(invalid.route, invalid.signal)
    await settle()
    invalid.route.push({ kind: SIGNAL_KIND_ANSWER, payload: [] })
    await expect(invalidOpening).rejects.toBeInstanceOf(PeerNegotiationError)

    const failed = negotiationFixture()
    const failedOpening = failed.factory.offer(failed.route, failed.signal)
    await settle()
    failed.peer.fail()
    await expect(failedOpening).rejects.toThrow(/failed state/u)
  })
})

async function beginFatalWait(
  waitKind: FatalWaitKind,
  maximumCandidates: number,
): Promise<InFlightFatalWait> {
  const fixture = negotiationFixture(maximumCandidates)
  if (waitKind === 'SDP') {
    fixture.peer.remoteDescriptionOperation = neverSettles()
    const opening = fixture.factory.offer(fixture.route, fixture.signal)
    await settle()
    fixture.route.push(answerSignal())
    await settle()
    expect(fixture.peer.remoteDescriptions).toHaveLength(1)

    // Native Open may race an SDP continuation; the owner still cannot publish
    // until the serialized answer operation completes.
    fixture.peer.raw.open()
    fixture.peer.raw.receiveText(TERMINAL_INTENT_CONTROL)
    await settle()
    return { phase: 'opening', fixture, opening }
  }

  const channel = await openChannel(fixture)
  if (waitKind === 'ICE') {
    fixture.peer.addCandidateOperation = neverSettles()
    fixture.route.push(candidateSignal('pending-ICE'))
    await settle()
    expect(fixture.peer.addedCandidates.at(-1)).toEqual({ candidate: 'pending-ICE' })
  } else {
    fixture.route.sendOperation = neverSettles()
    fixture.peer.emitCandidate('pending-signaling')
    await settle()
    expect(fixture.route.sent.at(-1)).toMatchObject({
      kind: SIGNAL_KIND_CANDIDATE,
      payload: { candidate: 'pending-signaling' },
    })
  }

  fixture.peer.raw.receiveText(TERMINAL_INTENT_CONTROL)
  let closeSettled = false
  const closing = channel.close().then(() => {
    closeSettled = true
  })
  await settle()
  expect(closeSettled).toBe(false)
  return { phase: 'established', fixture, channel, closing }
}

async function settleEstablishedFatalWait(
  state: Extract<InFlightFatalWait, { readonly phase: 'established' }>,
): Promise<unknown> {
  await Promise.all([state.closing, state.channel.done])
  return state.channel.reason
}

function peerOwnerCause(reason: unknown): unknown {
  return reason instanceof AggregateError ? reason.cause : reason
}

function reasonMessageContains(reason: unknown, message: string): boolean {
  if (reason instanceof AggregateError) {
    return reason.errors.some((nested: unknown) => reasonMessageContains(nested, message)) ||
      reasonMessageContains(reason.cause, message)
  }
  if (!(reason instanceof Error)) {
    return false
  }
  return reason.message.includes(message) || reasonMessageContains(reason.cause, message)
}

function neverSettles<T>(): Promise<T> {
  return new Promise(() => undefined)
}

async function establish(fixture: ReturnType<typeof negotiationFixture>): Promise<void> {
  await settle()
  fixture.route.push(answerSignal())
  await settle()
  fixture.peer.raw.open()
}

async function openChannel(fixture: ReturnType<typeof negotiationFixture>) {
  const opening = fixture.factory.offer(fixture.route, fixture.signal)
  await establish(fixture)
  return opening
}

function negotiationFixture(maxCandidates?: number) {
  const peer = new FakeNegotiationPeer()
  const route = new ControlledRoute()
  const controller = new AbortController()
  const factory = new BrowserOfferChannelFactory({
    configuration: { iceServers: [] },
    ...(maxCandidates === undefined ? {} : { maxCandidates }),
    createPeerConnection: () => peer.asPeer(),
  })
  return { peer, route, controller, signal: controller.signal, factory }
}

function answerSignal(): ConnectivitySignal {
  return {
    kind: SIGNAL_KIND_ANSWER,
    payload: { type: 'answer', sdp: 'remote-answer' },
  }
}

function candidateSignal(candidate: string): ConnectivitySignal {
  return { kind: SIGNAL_KIND_CANDIDATE, payload: { candidate } }
}

class ControlledRoute implements SignalingRoute {
  readonly messages: ReadableStream<ConnectivitySignal>
  readonly sent: ConnectivitySignal[] = []
  readonly sendSignals: AbortSignal[] = []
  sendOperation: Promise<void> | undefined
  cancelCalls = 0
  #controller!: ReadableStreamDefaultController<ConnectivitySignal>
  #sendFailure: unknown

  constructor() {
    this.messages = new ReadableStream({
      start: (controller) => {
        this.#controller = controller
      },
      cancel: () => {
        this.cancelCalls += 1
      },
    })
  }

  send(signal: ConnectivitySignal, abort?: AbortSignal): Promise<void> {
    abort?.throwIfAborted()
    if (this.#sendFailure !== undefined) {
      return Promise.reject(this.#sendFailure)
    }
    this.sent.push(structuredClone(signal))
    if (abort !== undefined) {
      this.sendSignals.push(abort)
    }
    return this.sendOperation ?? Promise.resolve()
  }

  push(signal: ConnectivitySignal): void {
    this.#controller.enqueue(structuredClone(signal))
  }

  close(): void {
    this.#controller.close()
  }

  failSends(error: unknown): void {
    this.#sendFailure = error
  }
}

class FakeNegotiationPeer extends EventTarget {
  readonly raw = new FakeRTCDataChannel({ readyState: 'connecting' })
  readonly sctp = { maxMessageSize: 256 * 1024 }
  readonly remoteDescriptions: RTCSessionDescriptionInit[] = []
  readonly addedCandidates: RTCIceCandidateInit[] = []
  connectionState: RTCPeerConnectionState = 'new'
  localDescription: RTCSessionDescription | null = null
  offerOperation: Promise<RTCSessionDescriptionInit> | undefined
  remoteDescriptionOperation: Promise<void> | undefined
  addCandidateOperation: Promise<void> | undefined
  readonly lifecycle: string[] = []
  closeCalls = 0

  constructor() {
    super()
    this.raw.addEventListener('close', () => this.lifecycle.push('datachannel-close'))
  }

  createDataChannel(): RTCDataChannel {
    return this.raw.asDataChannel()
  }

  createOffer(): Promise<RTCSessionDescriptionInit> {
    return this.offerOperation ?? Promise.resolve({ type: 'offer', sdp: 'local-offer' })
  }

  setLocalDescription(description?: RTCLocalSessionDescriptionInit): Promise<void> {
    const value = description ?? { type: 'offer', sdp: 'local-offer' }
    this.localDescription = descriptionObject(value.type ?? 'offer', value.sdp ?? '')
    return Promise.resolve()
  }

  setRemoteDescription(description: RTCSessionDescriptionInit): Promise<void> {
    this.remoteDescriptions.push(structuredClone(description))
    return this.remoteDescriptionOperation ?? Promise.resolve()
  }

  addIceCandidate(candidate?: RTCIceCandidateInit | null): Promise<void> {
    if (candidate !== undefined && candidate !== null) {
      this.addedCandidates.push(structuredClone(candidate))
    }
    return this.addCandidateOperation ?? Promise.resolve()
  }

  close(): void {
    this.closeCalls += 1
    if (this.connectionState === 'closed') {
      return
    }
    this.lifecycle.push('peer-close')
    this.connectionState = 'closed'
    this.raw.remoteClose()
    this.dispatchEvent(new Event('connectionstatechange'))
  }

  emitUnexpectedDataChannel(): FakeRTCDataChannel {
    const channel = new FakeRTCDataChannel({ readyState: 'connecting' })
    const event = new Event('datachannel')
    Object.defineProperty(event, 'channel', { value: channel.asDataChannel() })
    this.dispatchEvent(event)
    return channel
  }

  emitBrokenCandidate(): void {
    const value = {
      candidate: 'broken',
      toJSON: () => {
        throw new Error('synthetic candidate serialization failure')
      },
    } as unknown as RTCIceCandidate
    const event = new Event('icecandidate')
    Object.defineProperty(event, 'candidate', { value })
    this.dispatchEvent(event)
  }

  emitCandidate(candidate: string): void {
    this.#emitCandidate({ candidate })
  }

  emitCandidateWithNullOptionals(candidate: string): void {
    this.#emitCandidate({
      candidate,
      sdpMid: null,
      sdpMLineIndex: null,
      usernameFragment: null,
    })
  }

  #emitCandidate(serialized: RTCIceCandidateInit): void {
    const value = {
      candidate: serialized.candidate,
      toJSON: () => structuredClone(serialized),
    } as RTCIceCandidate
    const event = new Event('icecandidate')
    Object.defineProperty(event, 'candidate', { value })
    this.dispatchEvent(event)
  }

  fail(): void {
    this.connectionState = 'failed'
    this.dispatchEvent(new Event('connectionstatechange'))
  }

  asPeer(): RTCPeerConnection {
    return this as unknown as RTCPeerConnection
  }
}

function descriptionObject(
  type: RTCSdpType,
  sdp: string,
): RTCSessionDescription {
  return {
    type,
    sdp,
    toJSON: () => ({ type, sdp }),
  }
}

function reasonContains(reason: unknown, errorType: new (...args: never[]) => Error): boolean {
  if (reason instanceof errorType) {
    return true
  }
  return reason instanceof AggregateError &&
    reason.errors.some((nested: unknown) => reasonContains(nested, errorType))
}

async function settle(turns = 12): Promise<void> {
  for (let turn = 0; turn < turns; turn += 1) {
    await Promise.resolve()
  }
}

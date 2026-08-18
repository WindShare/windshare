import { describe, expect, it } from 'vitest'
import type { PeerChannel } from '../../src/connectivity/peer-channel'
import type { OfferChannelFactory } from '../../src/connectivity/peer-offer'
import type { V2BrowserConnectivityAttemptDiagnostic } from '../../src/connectivity/diagnostics'
import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  V2BrowserPeerAttemptExecutor,
  type V2PeerAttemptMilestone,
} from '../../src/connectivity/v2-peer-attempt'
import type {
  V2PeerAttemptContext,
  V2PeerRecoveryClock,
} from '../../src/connectivity/v2-peer-recovery'
import {
  V2LaneAdmissionRejectedError,
  V2LaneAdmissionTransportError,
  V2LaneInstallationError,
  type V2LaneAdmissionObserver,
  type V2ReceiverSessionRuntime,
} from '../../src/session/v2-runtime'
import { type V2LaneRejection, V2_LANE_REJECT } from '../../src/session/v2-lane-codec'

class ManualClock implements V2PeerRecoveryClock {
  readonly sleeps: Array<{
    readonly milliseconds: number
    readonly resolve: () => void
    readonly reject: (reason: unknown) => void
  }> = []
  #now = 0

  now(): number {
    return this.#now
  }

  sleep(milliseconds: number, signal: AbortSignal): Promise<void> {
    return new Promise((resolve, reject) => {
      const abort = () => reject(signal.reason)
      signal.addEventListener('abort', abort, { once: true })
      this.sleeps.push({
        milliseconds,
        resolve: () => {
          this.#now += milliseconds
          signal.removeEventListener('abort', abort)
          resolve()
        },
        reject,
      })
    })
  }

  expire(index: number): void {
    this.sleeps[index]?.resolve()
  }
}

class FakeSession {
  readonly keys = Object.freeze({ protocolSessionId: identity(90) })
  readonly grant = Object.freeze({
    laneId: 2,
    laneEpoch: 1,
    grantOperationId: identity(44),
    attachNonce: identity(45),
  })
  readonly ids = new Set([1])
  isClosed = false
  grantGate: Promise<void> | undefined
  adoptGate: Promise<void> | undefined
  installationGate: Promise<void> | undefined
  adoptFailure: unknown
  authenticatedRejection: V2LaneRejection | undefined
  adoptionClosesPeer = false
  adoptionIgnoresSignal = false

  async beginOperation(kind: number) {
    return {
      id: identity(43),
      requestKind: kind,
      next: () => new Promise<never>(() => undefined),
      cancel: () => undefined,
    }
  }

  async cancelOperation(): Promise<void> {}

  async sendOperationMessage(): Promise<void> {}

  laneIds(): readonly number[] {
    return [...this.ids]
  }

  async requestLaneGrant(
    _requestedLaneId: number,
    options: { readonly signal?: AbortSignal; readonly observeAdmission?: V2LaneAdmissionObserver },
  ) {
    const grantOperationId = encodeBase64Url(this.grant.grantOperationId)
    options.observeAdmission?.({
      type: 'grant-requested',
      grantOperationId,
      laneId: 0,
      laneEpoch: 0,
    })
    await optionalGate(this.grantGate, options.signal)
    options.observeAdmission?.({
      type: 'grant-received',
      grantOperationId,
      laneId: this.grant.laneId,
      laneEpoch: this.grant.laneEpoch,
    })
    return this.grant
  }

  async adoptGrantedLane(
    peer: PeerChannel,
    grant: typeof this.grant,
    options: { readonly signal?: AbortSignal; readonly observeAdmission?: V2LaneAdmissionObserver },
  ) {
    const correlation = {
      grantOperationId: encodeBase64Url(grant.grantOperationId),
      laneId: grant.laneId,
      laneEpoch: grant.laneEpoch,
    }
    options.observeAdmission?.({
      type: 'lane-hello-sent',
      ...correlation,
    })
    try {
      if (this.adoptionIgnoresSignal) {
        await this.adoptGate
      } else {
        await optionalGate(this.adoptGate, options.signal)
      }
    } catch (error) {
      if (this.adoptionClosesPeer) await peer.close()
      throw error
    }
    if (this.authenticatedRejection !== undefined) {
      options.observeAdmission?.({
        type: 'admission-response-rejected',
        ...correlation,
        rejection: this.authenticatedRejection,
      })
      const error = new V2LaneAdmissionRejectedError(this.authenticatedRejection)
      if (this.adoptionClosesPeer) await peer.close()
      return Object.freeze({
        ...correlation,
        disposition: 'rejected' as const,
        installation: 'not-attempted' as const,
        rejection: this.authenticatedRejection,
        error,
      })
    }
    if (this.adoptFailure !== undefined && !(this.adoptFailure instanceof V2LaneInstallationError)) {
      if (this.adoptionClosesPeer) await peer.close()
      throw this.adoptFailure
    }
    options.observeAdmission?.({
      type: 'admission-response-accepted',
      ...correlation,
    })
    try {
      if (this.adoptionIgnoresSignal) {
        await this.installationGate
      } else {
        await optionalGate(this.installationGate, options.signal)
      }
    } catch (cause) {
      const error = new V2LaneInstallationError({ cause })
      if (this.adoptionClosesPeer) await peer.close()
      return Object.freeze({
        ...correlation,
        disposition: 'accepted' as const,
        installation: 'failed' as const,
        error,
      })
    }
    if (this.adoptFailure instanceof V2LaneInstallationError) {
      if (this.adoptionClosesPeer) await peer.close()
      return Object.freeze({
        ...correlation,
        disposition: 'accepted' as const,
        installation: 'failed' as const,
        error: this.adoptFailure,
      })
    }
    this.ids.add(grant.laneId)
    options.observeAdmission?.({
      type: 'lane-adopted',
      ...correlation,
    })
    return Object.freeze({
      ...correlation,
      disposition: 'accepted' as const,
      installation: 'installed' as const,
    })
  }

  async close(): Promise<void> {
    this.isClosed = true
  }
}

class ControlledOffers implements OfferChannelFactory {
  readonly opened = deferred<PeerChannel>()
  signal: AbortSignal | undefined

  async offer(
    route: Parameters<OfferChannelFactory['offer']>[0],
    signal: AbortSignal,
    observer?: Parameters<OfferChannelFactory['offer']>[2],
  ): Promise<PeerChannel> {
    this.signal = signal
    signal.addEventListener('abort', () => this.opened.reject(signal.reason), { once: true })
    const counts = Object.freeze({ localEmitted: 0, remoteAccepted: 0 })
    observer?.offerCreated(counts)
    await route.send({ kind: 'offer', payload: { type: 'offer', sdp: 'v=0\r\n' } }, signal)
    observer?.offerSent(counts)
    const peer = await this.opened.promise
    observer?.answerReceived(counts)
    observer?.dataChannelOpened(counts, async () => null)
    return peer
  }
}

class CountingPeer implements PeerChannel {
  readonly state = 'open'
  readonly frames = new ReadableStream<Uint8Array>()
  readonly opened = Promise.resolve()
  readonly done = Promise.resolve()
  readonly reason = undefined
  closes = 0

  async send(): Promise<void> {}
  async sendTerminal(): Promise<void> {}
  async close(): Promise<void> {
    this.closes += 1
  }
}

function fixture() {
  const session = new FakeSession()
  const offers = new ControlledOffers()
  const clock = new ManualClock()
  const milestones: V2PeerAttemptMilestone[] = []
  const diagnostics: V2BrowserConnectivityAttemptDiagnostic[] = []
  const failures: unknown[] = []
  const publications: unknown[] = []
  let randomSeed = 10
  const executor = new V2BrowserPeerAttemptExecutor({
    session: session as unknown as V2ReceiverSessionRuntime,
    offers,
    peerPathIdentity: identity(7),
    clock,
    randomBytes: (length) => new Uint8Array(length).fill(randomSeed++),
    publish: (candidate) => {
      publications.push(candidate)
      return true
    },
    observeAttempt: (milestone) => milestones.push(milestone),
    connectivityObserver: (event) => diagnostics.push(event),
    onFailure: (error) => failures.push(error),
  })
  return { clock, diagnostics, executor, failures, milestones, offers, publications, session }
}

function attemptContext(phases: string[] = []): V2PeerAttemptContext {
  return Object.freeze({
    protocolSessionId: 'protocol-session',
    peerPathId: 'peer-path',
    waveOrdinal: 1,
    waveAttemptOrdinal: 1,
    sessionAttemptOrdinal: 1,
    requestedLaneId: 0,
    negotiationBudgetMilliseconds: 10,
    admissionBudgetMilliseconds: 20,
    phaseChanged: (phase: string) => phases.push(phase),
  }) as V2PeerAttemptContext
}

describe('browser peer attempt phase executor', () => {
  it('starts a fresh admission deadline only after local DataChannel Open', async () => {
    const phases: string[] = []
    const { clock, diagnostics, executor, milestones, offers, publications } = fixture()
    const peer = new CountingPeer()
    const attempt = executor.createAttempt(attemptContext(phases))

    expect(clock.sleeps.map((sleep) => sleep.milliseconds)).toEqual([10])
    offers.opened.resolve(peer)
    await turns()
    expect(phases).toEqual(['admission'])
    expect(clock.sleeps.map((sleep) => sleep.milliseconds)).toEqual([10, 20])

    await expect(attempt.result).resolves.toEqual({
      type: 'admitted',
      lane: { laneId: 2, laneEpoch: 1 },
    })
    expect(publications).toHaveLength(1)
    expect(peer.closes).toBe(0)
    expect(milestones.map((milestone) => milestone.type)).toEqual([
      'phase-deadline-armed',
      'phase-deadline-armed',
      'grant-requested',
      'grant-received',
      'lane-hello-sent',
      'admission-settlement-begun',
      'admission-response-accepted',
      'lane-adopted',
      'lane-published',
    ])
    expect(diagnostics.map((event) => event.stage)).toEqual([
      'started',
      'negotiation-deadline-armed',
      'offer-created',
      'offer-sent',
      'answer-received',
      'datachannel-open',
      'admission-deadline-armed',
      'grant-requested',
      'grant-received',
      'lane-hello-sent',
      'admission-response-received',
      'admission-response-settled',
      'lane-attached',
      'admitted',
    ])
    expect(diagnostics.every((event, index) =>
      event.sideSequence === index + 1 &&
      event.sessionId !== '' && event.peerPathId !== '' && event.attemptId !== ''
    )).toBe(true)
    const forbiddenAuthorityFields = [
      'sdp',
      'candidate',
      'candidateAddress',
      'candidatePort',
      'selectedPair',
      'attachNonce',
      'proof',
      'capability',
    ]
    expect(diagnostics.some((event) =>
      forbiddenAuthorityFields.some((field) => Object.hasOwn(event, field))
    )).toBe(false)
    expect(JSON.stringify(diagnostics)).not.toContain('v=0')
  })

  it('returns a typed negotiation timeout and never starts admission', async () => {
    const { clock, executor } = fixture()
    const attempt = executor.createAttempt(attemptContext())
    clock.expire(0)

    await expect(attempt.result).resolves.toMatchObject({
      type: 'failed',
      failure: {
        kind: 'local-transient',
        phase: 'negotiation',
        reason: 'negotiation-timeout',
      },
    })
  })

  it('uses the admission budget while a grant is delayed and closes its peer once', async () => {
    const { clock, executor, offers, session } = fixture()
    const peer = new CountingPeer()
    session.grantGate = new Promise(() => undefined)
    const attempt = executor.createAttempt(attemptContext())
    offers.opened.resolve(peer)
    await turns()
    clock.expire(1)

    await expect(attempt.result).resolves.toMatchObject({
      type: 'failed',
      failure: {
        kind: 'local-transient',
        phase: 'admission',
        reason: 'admission-timeout',
      },
    })
    expect(peer.closes).toBe(1)
  })

  it('retains an authenticated rejection when timeout and settlement race', async () => {
    const { clock, executor, offers, session } = fixture()
    const peer = new CountingPeer()
    const rejection = Object.freeze({
      code: V2_LANE_REJECT.admissionLimited,
      retryAfterMilliseconds: 1_000,
    })
    session.authenticatedRejection = rejection
    session.adoptionClosesPeer = true
    const attempt = executor.createAttempt(attemptContext())
    offers.opened.resolve(peer)
    await turns()
    clock.expire(1)

    await expect(attempt.result).resolves.toEqual({
      type: 'failed',
      failure: { kind: 'authenticated-lane-rejection', rejection },
    })
    expect(peer.closes).toBe(1)
  })

  it('retains a late authenticated installation failure after admission timeout', async () => {
    const { clock, executor, failures, milestones, offers, publications, session } = fixture()
    const peer = new CountingPeer()
    const installation = deferred<void>()
    const installationError = new V2LaneInstallationError({ cause: new Error('install failed') })
    session.installationGate = installation.promise
    session.adoptionIgnoresSignal = true
    session.adoptFailure = installationError
    session.adoptionClosesPeer = true
    const attempt = executor.createAttempt(attemptContext())
    offers.opened.resolve(peer)
    await turns()
    expect(milestones.at(-1)?.type).toBe('admission-response-accepted')

    clock.expire(1)
    await turns()
    expect(milestones).toContainEqual(expect.objectContaining({
      type: 'phase-deadline-expired',
      phase: 'admission',
    }))
    installation.resolve()

    await expect(attempt.result).resolves.toEqual({
      type: 'failed',
      failure: {
        kind: 'local-transient',
        phase: 'admission',
        reason: 'lane-installation-failed',
      },
    })
    expect(failures).toEqual([installationError])
    expect(publications).toHaveLength(0)
    expect(peer.closes).toBe(1)
  })

  it('retains a late authenticated installation failure after lifecycle cancellation', async () => {
    const { executor, failures, milestones, offers, publications, session } = fixture()
    const peer = new CountingPeer()
    const installation = deferred<void>()
    const installationError = new V2LaneInstallationError({ cause: new Error('install failed') })
    session.installationGate = installation.promise
    session.adoptionIgnoresSignal = true
    session.adoptFailure = installationError
    session.adoptionClosesPeer = true
    const attempt = executor.createAttempt(attemptContext())
    offers.opened.resolve(peer)
    await turns()
    expect(milestones.at(-1)?.type).toBe('admission-response-accepted')

    attempt.cancel('last-activation')
    await turns()
    installation.resolve()

    await expect(attempt.result).resolves.toEqual({
      type: 'failed',
      failure: {
        kind: 'local-transient',
        phase: 'admission',
        reason: 'lane-installation-failed',
      },
    })
    expect(failures).toEqual([installationError])
    expect(milestones.some((milestone) => milestone.type === 'lane-adopted')).toBe(false)
    expect(publications).toHaveLength(0)
    expect(peer.closes).toBe(1)
  })

  it('does not double-close a candidate consumed by failed runtime adoption', async () => {
    const { executor, offers, session } = fixture()
    const peer = new CountingPeer()
    session.adoptionClosesPeer = true
    session.adoptFailure = new V2LaneAdmissionTransportError('synthetic transport loss')
    const attempt = executor.createAttempt(attemptContext())
    offers.opened.resolve(peer)

    await expect(attempt.result).resolves.toMatchObject({
      type: 'failed',
      failure: { kind: 'local-transient', phase: 'admission', reason: 'transport-loss' },
    })
    expect(peer.closes).toBe(1)
  })

  it('retains a verified late success after its deadline has requested cancellation', async () => {
    const { clock, executor, milestones, offers, publications, session } = fixture()
    const peer = new CountingPeer()
    const settlement = deferred<void>()
    session.adoptGate = settlement.promise
    session.adoptionIgnoresSignal = true
    const attempt = executor.createAttempt(attemptContext())
    offers.opened.resolve(peer)
    await turns()

    clock.expire(1)
    await turns()
    settlement.resolve()

    await expect(attempt.result).resolves.toMatchObject({ type: 'admitted' })
    expect(publications).toHaveLength(1)
    expect(milestones).toContainEqual(expect.objectContaining({
      type: 'phase-deadline-expired',
      phase: 'admission',
    }))
    expect(peer.closes).toBe(0)
  })

  it('allocates a fresh AttemptID for every synchronously returned handle', async () => {
    const { executor } = fixture()
    const first = executor.createAttempt(attemptContext())
    const second = executor.createAttempt(attemptContext())
    expect(first.attemptId).not.toBe(second.attemptId)
    first.cancel('runtime-stop')
    second.cancel('runtime-stop')
    await Promise.all([first.result, second.result])
  })
})

async function optionalGate(gate: Promise<void> | undefined, signal?: AbortSignal): Promise<void> {
  signal?.throwIfAborted()
  if (gate === undefined) return
  await Promise.race([
    gate,
    new Promise<void>((_resolve, reject) => {
      const abort = () => reject(signal?.reason)
      signal?.addEventListener('abort', abort, { once: true })
    }),
  ])
}

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

function deferred<T>(): {
  readonly promise: Promise<T>
  readonly resolve: (value: T) => void
  readonly reject: (reason: unknown) => void
} {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((accept, decline) => {
    resolve = accept
    reject = decline
  })
  return { promise, resolve, reject }
}

async function turns(): Promise<void> {
  for (let index = 0; index < 30; index += 1) await Promise.resolve()
}

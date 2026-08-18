import { describe, expect, it } from 'vitest'

import { V2_LANE_REJECT } from '../../src/session/v2-lane-codec'
import type {
  V2PeerAttemptCancellationOwner,
  V2PeerAttemptResult,
} from '../../src/connectivity/v2-peer-failure'
import {
  V2_DEFAULT_PEER_RECOVERY_POLICY,
  V2_DEFAULT_PEER_ADMISSION_BUDGET_MILLISECONDS,
  V2_DEFAULT_PEER_NEGOTIATION_BUDGET_MILLISECONDS,
  V2_MAXIMUM_PEER_ADMISSION_BUDGET_MILLISECONDS,
  V2_PEER_RECOVERY_SESSION_ACTIVE_ELAPSED_BUDGET_MILLISECONDS,
  V2PeerRecoverySupervisor,
  calculateV2PeerRetryDelay,
  createV2PeerRecoveryPolicy,
  type V2PeerAttemptContext,
  type V2PeerAttemptHandle,
  type V2PeerRecoveryAttemptFactory,
  type V2PeerRecoveryClock,
  type V2PeerRecoveryEvent,
  type V2PeerRecoveryRearmSource,
  type V2PeerRecoverySupervisorOptions,
} from '../../src/connectivity/v2-peer-recovery'

const TRANSIENT: V2PeerAttemptResult = Object.freeze({
  type: 'failed',
  failure: Object.freeze({
    kind: 'local-transient',
    phase: 'negotiation',
    reason: 'transport-loss',
  }),
})

const PATH_POLICY: V2PeerAttemptResult = Object.freeze({
  type: 'failed',
  failure: Object.freeze({
    kind: 'local-policy',
    code: 'candidate-limit',
  }),
})

function admitted(laneId = 7, laneEpoch = 1): V2PeerAttemptResult {
  return Object.freeze({
    type: 'admitted',
    lane: Object.freeze({ laneId, laneEpoch }),
  })
}

function admissionLimited(retryAfterMilliseconds: number): V2PeerAttemptResult {
  return Object.freeze({
    type: 'failed',
    failure: Object.freeze({
      kind: 'authenticated-lane-rejection',
      rejection: Object.freeze({
        code: V2_LANE_REJECT.admissionLimited,
        retryAfterMilliseconds,
      }),
    }),
  })
}

class ManualClock implements V2PeerRecoveryClock {
  #now = 0
  readonly sleeps: number[] = []
  readonly #pending = new Set<{
    readonly deadline: number
    readonly resolve: () => void
    readonly reject: (reason: unknown) => void
    readonly signal: AbortSignal
    readonly aborted: () => void
  }>()

  now(): number {
    return this.#now
  }

  sleep(milliseconds: number, signal: AbortSignal): Promise<void> {
    this.sleeps.push(milliseconds)
    if (signal.aborted) return Promise.reject(signal.reason)
    if (milliseconds === 0) return Promise.resolve()
    return new Promise<void>((resolve, reject) => {
      const pending = {
        deadline: this.#now + milliseconds,
        resolve,
        reject,
        signal,
        aborted: () => {
          this.#pending.delete(pending)
          reject(signal.reason)
        },
      }
      signal.addEventListener('abort', pending.aborted, { once: true })
      this.#pending.add(pending)
    })
  }

  async advance(milliseconds: number): Promise<void> {
    this.#now += milliseconds
    for (const pending of [...this.#pending]) {
      if (pending.deadline > this.#now) continue
      this.#pending.delete(pending)
      pending.signal.removeEventListener('abort', pending.aborted)
      pending.resolve()
    }
    await turn()
  }
}

type AttemptScript =
  | V2PeerAttemptResult
  | 'pending'
  | ((context: V2PeerAttemptContext) => V2PeerAttemptResult)

class ScriptedAttempts implements V2PeerRecoveryAttemptFactory {
  readonly contexts: V2PeerAttemptContext[] = []
  readonly cancellations: V2PeerAttemptCancellationOwner[] = []
  readonly #script: AttemptScript[]
  readonly #pending = new Map<number, (result: V2PeerAttemptResult) => void>()

  constructor(script: AttemptScript[]) {
    this.#script = [...script]
  }

  createAttempt(context: V2PeerAttemptContext): V2PeerAttemptHandle {
    const index = this.contexts.length
    this.contexts.push(context)
    const scripted = this.#script[index] ?? 'pending'
    let result: Promise<V2PeerAttemptResult>
    if (scripted === 'pending') {
      result = new Promise((resolve) => this.#pending.set(index, resolve))
    } else {
      result = Promise.resolve(typeof scripted === 'function' ? scripted(context) : scripted)
    }
    return {
      attemptId: `attempt_${index + 1}`,
      result,
      cancel: (owner) => {
        this.cancellations.push(owner)
        this.#pending.get(index)?.(Object.freeze({
          type: 'lifecycle-cancelled',
          owner,
        }))
        this.#pending.delete(index)
      },
    }
  }

  settle(index: number, result: V2PeerAttemptResult): void {
    const resolve = this.#pending.get(index)
    if (resolve === undefined) throw new Error('attempt is not pending')
    this.#pending.delete(index)
    resolve(result)
  }
}

class RearmSource implements V2PeerRecoveryRearmSource {
  #listener: (() => void) | undefined

  subscribe(listener: () => void): () => void {
    this.#listener = listener
    return () => { this.#listener = undefined }
  }

  emit(): void {
    this.#listener?.()
  }
}

function fixture(
  script: AttemptScript[],
  overrides: Partial<V2PeerRecoverySupervisorOptions> = {},
): {
  readonly supervisor: V2PeerRecoverySupervisor
  readonly attempts: ScriptedAttempts
  readonly clock: ManualClock
  readonly events: V2PeerRecoveryEvent[]
} {
  const attempts = new ScriptedAttempts(script)
  const clock = new ManualClock()
  const events: V2PeerRecoveryEvent[] = []
  return {
    attempts,
    clock,
    events,
    supervisor: new V2PeerRecoverySupervisor({
      protocolSessionId: 'session_1',
      peerPathId: 'path_1',
      attempts,
      clock,
      random: () => 0,
      observer: (event) => events.push(event),
      ...overrides,
    }),
  }
}

async function turn(): Promise<void> {
  for (let index = 0; index < 12; index += 1) await Promise.resolve()
}

describe('v2 peer recovery policy', () => {
  it('freezes the authoritative phase and cumulative recovery defaults', () => {
    expect(V2_DEFAULT_PEER_NEGOTIATION_BUDGET_MILLISECONDS).toBe(15_000)
    expect(V2_DEFAULT_PEER_ADMISSION_BUDGET_MILLISECONDS).toBe(20_000)
    expect(V2_MAXIMUM_PEER_ADMISSION_BUDGET_MILLISECONDS).toBe(25_000)
    expect(V2_DEFAULT_PEER_RECOVERY_POLICY).toMatchObject({
      waveMaxAttempts: 3,
      waveElapsedBudgetMilliseconds: 120_000,
      sessionMaxAttempts: 8,
      sessionActiveElapsedBudgetMilliseconds:
        V2_PEER_RECOVERY_SESSION_ACTIVE_ELAPSED_BUDGET_MILLISECONDS,
      retryInitialBackoffMilliseconds: 1_000,
      retryBackoffMultiplier: 2,
      retryBackoffMaximumMilliseconds: 8_000,
      retryJitterMinimumFactor: 0.5,
      retryJitterMaximumFactor: 1,
    })
    expect(Object.isFrozen(V2_DEFAULT_PEER_RECOVERY_POLICY)).toBe(true)
  })

  it('calculates equal-jitter edges, exponential caps, and RetryAfter as a lower bound', () => {
    const policy = V2_DEFAULT_PEER_RECOVERY_POLICY
    expect(calculateV2PeerRetryDelay(policy, 0, 0)).toBe(500)
    expect(calculateV2PeerRetryDelay(policy, 0, 1)).toBe(1_000)
    expect(calculateV2PeerRetryDelay(policy, 4, 1)).toBe(8_000)
    expect(calculateV2PeerRetryDelay(policy, 0, 0, 12_000)).toBe(12_000)
  })

  it.each([
    { negotiationBudgetMilliseconds: 0 },
    { admissionBudgetMilliseconds: 25_001 },
    { waveMaxAttempts: 9, sessionMaxAttempts: 8 },
    {
      waveElapsedBudgetMilliseconds: 61_000,
      sessionActiveElapsedBudgetMilliseconds: 60_000,
    },
    { retryJitterMinimumFactor: 0 },
    { retryJitterMinimumFactor: 0.8, retryJitterMaximumFactor: 0.7 },
    { retryJitterMaximumFactor: 1.1 },
  ])('rejects an invalid recovery configuration %#', (override) => {
    expect(() => createV2PeerRecoveryPolicy(override)).toThrow(RangeError)
  })
})

describe('v2 peer recovery supervisor', () => {
  it('precharges an attempt and carries phase budgets through its consumer-side port', async () => {
    const { supervisor, attempts } = fixture([admitted()])
    const activation = supervisor.activate()
    await supervisor.join()

    expect(attempts.contexts).toHaveLength(1)
    expect(attempts.contexts[0]).toMatchObject({
      protocolSessionId: 'session_1',
      peerPathId: 'path_1',
      waveOrdinal: 1,
      waveAttemptOrdinal: 1,
      sessionAttemptOrdinal: 1,
      requestedLaneId: 0,
      negotiationBudgetMilliseconds: 15_000,
      admissionBudgetMilliseconds: 20_000,
    })
    expect(supervisor.sessionAttemptCount).toBe(1)
    expect(supervisor.state).toEqual({
      kind: 'admitted',
      lane: { laneId: 7, laneEpoch: 1 },
    })

    activation.close()
    expect(supervisor.state.kind).toBe('admitted')
    await supervisor.close()
  })

  it('keeps exactly one task and attempt slot across duplicate activations', async () => {
    const { supervisor, attempts } = fixture(['pending'])
    const first = supervisor.activate()
    const second = supervisor.activate()
    await turn()

    expect(attempts.contexts).toHaveLength(1)
    expect(supervisor.state.kind).toBe('attempting')

    first.close()
    expect(attempts.cancellations).toEqual([])
    second.close()
    await supervisor.join()
    expect(attempts.cancellations).toContain('last-activation')
    expect(supervisor.state.kind).toBe('idle')
  })

  it('honors authenticated RetryAfter across activation cancellation and reopening', async () => {
    const policy = createV2PeerRecoveryPolicy({
      retryInitialBackoffMilliseconds: 2,
      retryBackoffMaximumMilliseconds: 8,
      waveElapsedBudgetMilliseconds: 100,
      sessionActiveElapsedBudgetMilliseconds: 1_000,
    })
    const { supervisor, attempts, clock, events } = fixture([
      admissionLimited(30),
      admitted(7, 2),
    ], { policy })

    const first = supervisor.activate()
    await turn()
    expect(supervisor.state).toMatchObject({
      kind: 'waiting-retry',
      delayMilliseconds: 30,
    })
    first.close()
    await supervisor.join()
    expect(supervisor.state.kind).toBe('idle')

    const second = supervisor.activate()
    await turn()
    expect(attempts.contexts).toHaveLength(1)
    await clock.advance(29)
    expect(attempts.contexts).toHaveLength(1)
    await clock.advance(1)
    await supervisor.join()

    expect(attempts.contexts).toHaveLength(2)
    expect(supervisor.state.kind).toBe('admitted')
    expect(events.filter((event) => event.stage === 'backoff-scheduled')).toEqual([
      expect.objectContaining({
        localDelayMilliseconds: 1,
        authenticatedRetryAfterMilliseconds: 30,
        effectiveDelayMilliseconds: 30,
      }),
    ])
    second.close()
    await supervisor.close()
  })

  it('retains authenticated not-before authority when the current wave is already exhausted', async () => {
    const policy = createV2PeerRecoveryPolicy({
      waveMaxAttempts: 1,
      sessionMaxAttempts: 2,
      waveElapsedBudgetMilliseconds: 100,
      sessionActiveElapsedBudgetMilliseconds: 1_000,
    })
    const { supervisor, attempts, clock } = fixture([
      admissionLimited(30),
      admitted(7, 2),
    ], { policy })
    const first = supervisor.activate()
    await supervisor.join()
    expect(supervisor.state.kind).toBe('quiescent')

    const rearm = supervisor.activate()
    await turn()
    expect(supervisor.state).toMatchObject({
      kind: 'waiting-retry',
      delayMilliseconds: 30,
    })
    expect(attempts.contexts).toHaveLength(1)

    await clock.advance(29)
    expect(attempts.contexts).toHaveLength(1)
    await clock.advance(1)
    await supervisor.join()
    expect(attempts.contexts).toHaveLength(2)
    expect(supervisor.state.kind).toBe('admitted')

    first.close()
    rearm.close()
    await supervisor.close()
  })

  it('rejects stale phase changes from a replaced attempt', async () => {
    const policy = createV2PeerRecoveryPolicy({
      waveMaxAttempts: 2,
      sessionMaxAttempts: 2,
      retryInitialBackoffMilliseconds: 1,
      retryBackoffMaximumMilliseconds: 1,
      retryJitterMinimumFactor: 1,
      retryJitterMaximumFactor: 1,
      waveElapsedBudgetMilliseconds: 100,
      sessionActiveElapsedBudgetMilliseconds: 1_000,
    })
    const { supervisor, attempts, clock } = fixture([TRANSIENT, 'pending'], { policy })
    const activation = supervisor.activate()
    await turn()
    await clock.advance(1)
    expect(attempts.contexts).toHaveLength(2)
    expect(supervisor.state).toMatchObject({ kind: 'attempting', phase: 'negotiation' })

    attempts.contexts[0]?.phaseChanged('admission')
    expect(supervisor.state).toMatchObject({ kind: 'attempting', phase: 'negotiation' })

    activation.close()
    await supervisor.join()
  })

  it('quiesces at a wave attempt ceiling and rearms only on a later activation edge', async () => {
    const policy = createV2PeerRecoveryPolicy({
      waveMaxAttempts: 2,
      sessionMaxAttempts: 3,
      retryInitialBackoffMilliseconds: 1,
      retryBackoffMaximumMilliseconds: 1,
      retryJitterMinimumFactor: 1,
      retryJitterMaximumFactor: 1,
      waveElapsedBudgetMilliseconds: 100,
      sessionActiveElapsedBudgetMilliseconds: 1_000,
    })
    const { supervisor, attempts, clock } = fixture([
      TRANSIENT,
      TRANSIENT,
      TRANSIENT,
    ], { policy })
    const first = supervisor.activate()
    await turn()
    await clock.advance(1)
    await supervisor.join()

    expect(attempts.contexts).toHaveLength(2)
    expect(supervisor.state).toEqual({
      kind: 'quiescent',
      reason: 'wave-attempt-budget',
    })

    const rearm = supervisor.activate()
    await supervisor.join()
    expect(attempts.contexts).toHaveLength(3)
    expect(supervisor.state).toEqual({
      kind: 'session-exhausted',
      reason: 'session-attempt-budget',
    })

    first.close()
    rearm.close()
  })

  it('counts only active wave time and makes session elapsed exhaustion non-rearmable', async () => {
    const policy = createV2PeerRecoveryPolicy({
      waveElapsedBudgetMilliseconds: 10,
      sessionActiveElapsedBudgetMilliseconds: 15,
    })
    const rearm = new RearmSource()
    const { supervisor, attempts, clock } = fixture(['pending', 'pending'], {
      policy,
      rearmSource: rearm,
    })
    supervisor.activate()
    await turn()
    await clock.advance(10)
    await supervisor.join()
    expect(supervisor.state).toEqual({
      kind: 'quiescent',
      reason: 'wave-elapsed-budget',
    })
    expect(supervisor.sessionActiveElapsedMilliseconds).toBe(10)

    await clock.advance(1_000)
    expect(supervisor.sessionActiveElapsedMilliseconds).toBe(10)

    rearm.emit()
    await turn()
    expect(attempts.contexts).toHaveLength(2)
    await clock.advance(5)
    await supervisor.join()
    expect(supervisor.state).toEqual({
      kind: 'session-exhausted',
      reason: 'session-elapsed-budget',
    })

    rearm.emit()
    supervisor.activate()
    await turn()
    expect(attempts.contexts).toHaveLength(2)
  })

  it('coalesces network rearm events and never rearms a permanent path stop', async () => {
    const policy = createV2PeerRecoveryPolicy({
      waveMaxAttempts: 1,
      sessionMaxAttempts: 3,
    })
    const rearm = new RearmSource()
    const { supervisor, attempts } = fixture([TRANSIENT, PATH_POLICY], {
      policy,
      rearmSource: rearm,
    })
    supervisor.activate()
    await supervisor.join()
    expect(supervisor.state.kind).toBe('quiescent')

    rearm.emit()
    rearm.emit()
    await supervisor.join()
    expect(attempts.contexts).toHaveLength(2)
    expect(supervisor.state).toEqual({
      kind: 'path-stopped',
      reason: 'local-policy',
    })

    rearm.emit()
    supervisor.activate()
    await turn()
    expect(attempts.contexts).toHaveLength(2)
  })

  it('recovers exact detachment through the same wave and reuses the preferred logical lane', async () => {
    const { supervisor, attempts, events } = fixture([
      admitted(7, 1),
      admitted(7, 2),
    ])
    const activation = supervisor.activate()
    await supervisor.join()

    expect(supervisor.peerDetached({
      protocolSessionId: 'session_1',
      peerPathId: 'path_1',
      laneId: 7,
      laneEpoch: 0,
    })).toBe(false)
    expect(supervisor.peerDetached({
      protocolSessionId: 'session_1',
      peerPathId: 'path_1',
      laneId: 7,
      laneEpoch: 1,
    })).toBe(true)
    await supervisor.join()

    expect(attempts.contexts).toHaveLength(2)
    expect(attempts.contexts[1]?.requestedLaneId).toBe(7)
    expect(supervisor.state).toEqual({
      kind: 'admitted',
      lane: { laneId: 7, laneEpoch: 2 },
    })
    expect(events.filter((event) => event.stage === 'peer-detached')).toHaveLength(1)

    activation.close()
    await supervisor.close()
  })

  it('does not recover a warm admitted lane after detachment without demand', async () => {
    const { supervisor, attempts } = fixture([admitted(7, 1)])
    const activation = supervisor.activate()
    await supervisor.join()
    activation.close()

    expect(supervisor.peerDetached({
      protocolSessionId: 'session_1',
      peerPathId: 'path_1',
      laneId: 7,
      laneEpoch: 1,
    })).toBe(true)
    expect(supervisor.state.kind).toBe('idle')
    expect(attempts.contexts).toHaveLength(1)

    const later = supervisor.activate()
    await turn()
    expect(attempts.contexts).toHaveLength(2)
    later.close()
    await supervisor.join()
  })

  it('contains observer exceptions and exposes no relay or session mutation authority', async () => {
    let relayClosures = 0
    let sessionClosures = 0
    const attempts = new ScriptedAttempts([TRANSIENT])
    const clock = new ManualClock()
    const options = {
      protocolSessionId: 'session_1',
      peerPathId: 'path_1',
      attempts,
      clock,
      random: () => 0,
      policy: createV2PeerRecoveryPolicy({ waveMaxAttempts: 1 }),
      observer: () => { throw new Error('synthetic observer failure') },
      closeRelay: () => { relayClosures += 1 },
      closeSession: () => { sessionClosures += 1 },
    } satisfies V2PeerRecoverySupervisorOptions & {
      readonly closeRelay: () => void
      readonly closeSession: () => void
    }
    const supervisor = new V2PeerRecoverySupervisor(options)
    supervisor.activate()
    await supervisor.join()

    expect(supervisor.state.kind).toBe('quiescent')
    expect(relayClosures).toBe(0)
    expect(sessionClosures).toBe(0)
  })

  it('reflects explicit ProtocolSession terminal authority and cancels owned work', async () => {
    const { supervisor, attempts } = fixture(['pending'])
    supervisor.activate()
    await turn()

    await supervisor.sessionTerminated(Object.freeze({
      authority: 'protocol-session-terminal',
      code: 'protocol-failure',
    }))

    expect(attempts.cancellations).toContain('runtime-stop')
    expect(supervisor.state).toEqual({
      kind: 'session-stopped',
      reason: 'protocol-failure',
    })
  })
})

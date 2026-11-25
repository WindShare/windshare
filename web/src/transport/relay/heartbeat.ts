import {
  decodeConnectionProbeAck,
  encodeConnectionProbe,
  CONNECTION_PROBE_ACK_MAGIC,
  RelayProtocolError,
} from './protocol'

// Probes leave on this cadence whether or not earlier ones were acknowledged,
// so an acknowledgement queued behind content never silences this direction.
export const RELAY_HEARTBEAT_INTERVAL_MILLISECONDS = 15_000
// Loss is a full timeout without any accepted inbound frame. Three intervals
// absorb a relay reply delayed behind one bounded write plus network delay.
export const RELAY_HEARTBEAT_TIMEOUT_MILLISECONDS = 45_000
// Acknowledgements can lag several probes on a slow downlink while content
// keeps the connection alive; untracked probes lose only their round-trip trace.
const MAXIMUM_TRACKED_PROBES = 8
// Coalesce bursty wake/reconnect triggers so on-demand probes cannot flood a relay.
const MINIMUM_ON_DEMAND_PROBE_INTERVAL_MILLISECONDS = 1_000

let nextHeartbeatConnectionId = 0n

export interface RelayHeartbeatTrace {
  readonly connectionId: bigint
  readonly round: bigint
  readonly stage: 'probe' | 'acknowledged' | 'failed'
  readonly bufferedBytes: number
  /** Round trip for an acknowledgement; inbound silence for a failure. */
  readonly elapsedMilliseconds: number
  readonly timeoutMilliseconds: number
}

export class RelayHeartbeatError extends Error {
  constructor(message = 'Relay heartbeat timed out') {
    super(message)
    this.name = 'RelayHeartbeatError'
  }
}

interface ProbeSocket {
  readonly bufferedAmount: number
  send(data: Uint8Array<ArrayBuffer>): void
}

interface TrackedProbe {
  readonly round: bigint
  readonly sentAt: number
}

/** Fixed-cadence connection probes; only inbound silence for a full timeout is loss. */
export class RelayHeartbeat {
  readonly #connectionId = ++nextHeartbeatConnectionId
  readonly #socket: ProbeSocket
  readonly #fail: (error: unknown) => void
  readonly #trace: ((event: RelayHeartbeatTrace) => void) | undefined
  #probeTimer: ReturnType<typeof setTimeout> | undefined
  #probeDueAt: number | undefined
  #silenceTimer: ReturnType<typeof setTimeout> | undefined
  #verifyTimer: ReturnType<typeof setTimeout> | undefined
  #verifyDeadlineAt: number | undefined
  #round = 0n
  #acknowledged = 0n
  #inboundSequence = 0n
  #tracked: TrackedProbe[] = []
  #lastProbeSentAt = -Infinity
  // Calendar corrections must not postpone relay loss or revoke a healthy path.
  #lastInboundAt = performance.now()
  #closed = false

  constructor(socket: ProbeSocket, fail: (error: unknown) => void, trace?: (event: RelayHeartbeatTrace) => void) {
    this.#socket = socket
    this.#fail = fail
    this.#trace = trace
    this.#scheduleProbe()
    this.#armSilence(RELAY_HEARTBEAT_TIMEOUT_MILLISECONDS)
  }

  /** Records a frame the connection accepted; the silence timer rereads it lazily. */
  observeInbound(): void {
    this.#inboundSequence += 1n
    this.#lastInboundAt = performance.now()
    this.#clearVerify()
  }

  /**
   * Sends an immediate probe (rate-limited) without violating the inbound-silence
   * liveness invariant. When a verification deadline is supplied (e.g. after OS
   * wake or network recovery), any accepted inbound frame before that deadline
   * clears it; continued silence fails the stale connection promptly.
   */
  probeNow(deadlineMilliseconds?: number): void {
    if (this.#closed) return
    const inboundBeforeProbe = this.#inboundSequence
    const now = performance.now()
    const cooldownUntil = this.#lastProbeSentAt + MINIMUM_ON_DEMAND_PROBE_INTERVAL_MILLISECONDS
    if (now >= cooldownUntil) {
      this.#clearProbe()
      this.#probe()
      if (this.#closed || this.#inboundSequence !== inboundBeforeProbe) return
    }
    if (deadlineMilliseconds === undefined || !Number.isFinite(deadlineMilliseconds) || deadlineMilliseconds <= 0) {
      return
    }
    const verificationStartAt = this.#ensureProbeInFlight(now, cooldownUntil)
    const targetDeadline = verificationStartAt + deadlineMilliseconds
    if (this.#verifyDeadlineAt !== undefined && this.#verifyDeadlineAt <= targetDeadline) return
    this.#clearVerify()
    this.#verifyDeadlineAt = targetDeadline
    const expectedInboundSequence = this.#inboundSequence
    this.#verifyTimer = globalThis.setTimeout(
      () => this.#checkVerify(expectedInboundSequence),
      Math.max(0, Math.ceil(targetDeadline - performance.now())),
    )
  }

  /**
   * Consumes a probe acknowledgement. The relay may coalesce replies to its
   * newest nonce, so one acknowledgement covers every earlier probe; a nonce
   * already covered or never sent is a protocol violation.
   */
  receive(encoded: Uint8Array): boolean {
    if (encoded.byteLength < 4 || String.fromCharCode(...encoded.subarray(0, 4)) !== CONNECTION_PROBE_ACK_MAGIC) {
      return false
    }
    const nonce = decodeConnectionProbeAck(encoded)
    if (this.#closed) return true
    if (nonce <= this.#acknowledged || nonce > this.#round) {
      throw new RelayProtocolError('identity', 'connection probe acknowledgement names no outstanding probe')
    }
    this.#acknowledged = nonce
    const acknowledged = this.#tracked.find(probe => probe.round === nonce)
    this.#tracked = this.#tracked.filter(probe => probe.round > nonce)
    if (acknowledged !== undefined) this.#emit('acknowledged', nonce, performance.now() - acknowledged.sentAt)
    return true
  }

  close(): void {
    this.#closed = true
    this.#tracked = []
    this.#clearProbe()
    globalThis.clearTimeout(this.#silenceTimer)
    this.#silenceTimer = undefined
    this.#clearVerify()
  }

  #clearProbe(): void {
    if (this.#probeTimer !== undefined) {
      globalThis.clearTimeout(this.#probeTimer)
      this.#probeTimer = undefined
    }
    this.#probeDueAt = undefined
  }

  #clearVerify(): void {
    if (this.#verifyTimer !== undefined) {
      globalThis.clearTimeout(this.#verifyTimer)
      this.#verifyTimer = undefined
    }
    this.#verifyDeadlineAt = undefined
  }

  // Never arm a verification deadline with no probe on the wire: when rate
  // limiting skips immediate transmission after all earlier probes were already
  // acknowledged, defer the next probe to the cooldown boundary so healthy idle
  // connections still have an opportunity to reply.
  #ensureProbeInFlight(now: number, cooldownUntil: number): number {
    if (now >= cooldownUntil || this.#acknowledged < this.#round) return now
    if (this.#probeDueAt === undefined || this.#probeDueAt > cooldownUntil) {
      this.#clearProbe()
      this.#scheduleProbe(cooldownUntil - performance.now(), cooldownUntil)
    }
    return cooldownUntil
  }

  #scheduleProbe(delay = RELAY_HEARTBEAT_INTERVAL_MILLISECONDS, dueAt = performance.now() + delay): void {
    this.#probeDueAt = dueAt
    this.#probeTimer = globalThis.setTimeout(() => {
      this.#probeTimer = undefined
      this.#probeDueAt = undefined
      this.#probe()
    }, Math.max(0, Math.ceil(delay)))
  }

  #armSilence(delay: number): void {
    this.#silenceTimer = globalThis.setTimeout(() => this.#checkSilence(), delay)
  }

  #probe(): void {
    if (this.#closed) return
    this.#round += 1n
    const now = performance.now()
    this.#lastProbeSentAt = now
    this.#tracked.push({ round: this.#round, sentAt: now })
    if (this.#tracked.length > MAXIMUM_TRACKED_PROBES) this.#tracked.shift()
    this.#emit('probe', this.#round, 0)
    // Append one tiny probe even during pressure: waiting for bufferedAmount to
    // drain first would leave a silent, permanently blocked socket undetectable.
    try {
      this.#socket.send(encodeConnectionProbe(this.#round))
    } catch (error) {
      this.#failed(error, now - this.#lastInboundAt)
      return
    }
    if (this.#closed) return
    this.#scheduleProbe()
  }

  #checkVerify(expectedInboundSequence: bigint): void {
    this.#verifyTimer = undefined
    this.#verifyDeadlineAt = undefined
    if (this.#closed || this.#inboundSequence !== expectedInboundSequence) return
    this.#failed(new RelayHeartbeatError('Relay liveness probe timed out'), performance.now() - this.#lastInboundAt)
  }

  #checkSilence(): void {
    if (this.#closed) return
    const silence = performance.now() - this.#lastInboundAt
    if (silence >= RELAY_HEARTBEAT_TIMEOUT_MILLISECONDS) {
      this.#failed(new RelayHeartbeatError(), silence)
      return
    }
    // Rearming once per timeout window, not per frame, keeps busy connections cheap.
    this.#armSilence(RELAY_HEARTBEAT_TIMEOUT_MILLISECONDS - silence)
  }

  #failed(error: unknown, silence: number): void {
    if (this.#closed) return
    this.#emit('failed', this.#round, silence)
    this.close()
    this.#fail(error)
  }

  #emit(stage: RelayHeartbeatTrace['stage'], round: bigint, elapsedMilliseconds: number): void {
    try {
      this.#trace?.({
        connectionId: this.#connectionId, round, stage, bufferedBytes: this.#socket.bufferedAmount,
        elapsedMilliseconds, timeoutMilliseconds: RELAY_HEARTBEAT_TIMEOUT_MILLISECONDS,
      })
    } catch {
      // Diagnostic consumers cannot suppress liveness detection.
    }
  }
}

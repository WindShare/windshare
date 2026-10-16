import {
  decodeV2ConnectionProbeAck,
  encodeV2ConnectionProbe,
  V2_CONNECTION_PROBE_ACK_MAGIC,
  V2RelayProtocolError,
} from './v2-protocol'

// Probes leave on this cadence whether or not earlier ones were acknowledged,
// so an acknowledgement queued behind content never silences this direction.
export const V2_RELAY_HEARTBEAT_INTERVAL_MILLISECONDS = 15_000
// Loss is a full timeout without any accepted inbound frame. Three intervals
// absorb a relay reply delayed behind one bounded write plus network delay.
export const V2_RELAY_HEARTBEAT_TIMEOUT_MILLISECONDS = 45_000
// Acknowledgements can lag several probes on a slow downlink while content
// keeps the connection alive; untracked probes lose only their round-trip trace.
const MAXIMUM_TRACKED_PROBES = 8

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
  #silenceTimer: ReturnType<typeof setTimeout> | undefined
  #round = 0n
  #acknowledged = 0n
  #tracked: TrackedProbe[] = []
  #lastInboundAt = Date.now()
  #closed = false

  constructor(socket: ProbeSocket, fail: (error: unknown) => void, trace?: (event: RelayHeartbeatTrace) => void) {
    this.#socket = socket
    this.#fail = fail
    this.#trace = trace
    this.#scheduleProbe()
    this.#armSilence(V2_RELAY_HEARTBEAT_TIMEOUT_MILLISECONDS)
  }

  /** Records a frame the connection accepted; the silence timer rereads it lazily. */
  observeInbound(): void {
    this.#lastInboundAt = Date.now()
  }

  /**
   * Consumes a probe acknowledgement. The relay may coalesce replies to its
   * newest nonce, so one acknowledgement covers every earlier probe; a nonce
   * already covered or never sent is a protocol violation.
   */
  receive(encoded: Uint8Array): boolean {
    if (encoded.byteLength < 4 || String.fromCharCode(...encoded.subarray(0, 4)) !== V2_CONNECTION_PROBE_ACK_MAGIC) {
      return false
    }
    const nonce = decodeV2ConnectionProbeAck(encoded)
    if (this.#closed) return true
    if (nonce <= this.#acknowledged || nonce > this.#round) {
      throw new V2RelayProtocolError('identity', 'connection probe acknowledgement names no outstanding probe')
    }
    this.#acknowledged = nonce
    const acknowledged = this.#tracked.find(probe => probe.round === nonce)
    this.#tracked = this.#tracked.filter(probe => probe.round > nonce)
    if (acknowledged !== undefined) this.#emit('acknowledged', nonce, Date.now() - acknowledged.sentAt)
    return true
  }

  close(): void {
    this.#closed = true
    this.#tracked = []
    globalThis.clearTimeout(this.#probeTimer)
    globalThis.clearTimeout(this.#silenceTimer)
  }

  #scheduleProbe(): void {
    this.#probeTimer = globalThis.setTimeout(() => this.#probe(), V2_RELAY_HEARTBEAT_INTERVAL_MILLISECONDS)
  }

  #armSilence(delay: number): void {
    this.#silenceTimer = globalThis.setTimeout(() => this.#checkSilence(), delay)
  }

  #probe(): void {
    if (this.#closed) return
    this.#round += 1n
    const now = Date.now()
    this.#tracked.push({ round: this.#round, sentAt: now })
    if (this.#tracked.length > MAXIMUM_TRACKED_PROBES) this.#tracked.shift()
    this.#emit('probe', this.#round, 0)
    // Append one tiny probe even during pressure: waiting for bufferedAmount to
    // drain first would leave a silent, permanently blocked socket undetectable.
    try {
      this.#socket.send(encodeV2ConnectionProbe(this.#round))
    } catch (error) {
      this.#failed(error, now - this.#lastInboundAt)
      return
    }
    this.#scheduleProbe()
  }

  #checkSilence(): void {
    if (this.#closed) return
    const silence = Date.now() - this.#lastInboundAt
    if (silence >= V2_RELAY_HEARTBEAT_TIMEOUT_MILLISECONDS) {
      this.#failed(new RelayHeartbeatError(), silence)
      return
    }
    // Rearming once per timeout window, not per frame, keeps busy connections cheap.
    this.#armSilence(V2_RELAY_HEARTBEAT_TIMEOUT_MILLISECONDS - silence)
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
        elapsedMilliseconds, timeoutMilliseconds: V2_RELAY_HEARTBEAT_TIMEOUT_MILLISECONDS,
      })
    } catch {
      // Diagnostic consumers cannot suppress liveness detection.
    }
  }
}

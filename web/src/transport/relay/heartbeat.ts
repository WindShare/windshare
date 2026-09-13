import {
  decodeV2ConnectionProbeAck,
  encodeV2ConnectionProbe,
  V2_CONNECTION_PROBE_ACK_MAGIC,
} from './v2-protocol'

export const V2_RELAY_HEARTBEAT_INTERVAL_MILLISECONDS = 15_000
// A receiver's relay reader can pause for a 15-second forward wait, and the
// acknowledgement can follow one bounded write. Keep a further network margin.
export const V2_RELAY_HEARTBEAT_TIMEOUT_MILLISECONDS = 45_000

let nextHeartbeatConnectionId = 0n

export interface RelayHeartbeatTrace {
  readonly connectionId: bigint
  readonly round: bigint
  readonly stage: 'probe' | 'acknowledged' | 'failed'
  readonly bufferedBytes: number
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

/** One outstanding connection probe; traffic never resets its response deadline. */
export class RelayHeartbeat {
  readonly #connectionId = ++nextHeartbeatConnectionId
  readonly #socket: ProbeSocket
  readonly #fail: (error: unknown) => void
  readonly #trace: ((event: RelayHeartbeatTrace) => void) | undefined
  #timer: ReturnType<typeof setTimeout> | undefined
  #round = 0n
  #pending: bigint | undefined
  #startedAt = 0
  #closed = false

  constructor(socket: ProbeSocket, fail: (error: unknown) => void, trace?: (event: RelayHeartbeatTrace) => void) {
    this.#socket = socket
    this.#fail = fail
    this.#trace = trace
    this.#schedule()
  }

  receive(encoded: Uint8Array): boolean {
    if (encoded.byteLength < 4 || String.fromCharCode(...encoded.subarray(0, 4)) !== V2_CONNECTION_PROBE_ACK_MAGIC) {
      return false
    }
    const nonce = decodeV2ConnectionProbeAck(encoded)
    // A duplicate or stale response cannot extend the next outstanding probe.
    if (!this.#closed && nonce === this.#pending) {
      this.#emit('acknowledged')
      this.#pending = undefined
      globalThis.clearTimeout(this.#timer)
      this.#schedule()
    }
    return true
  }

  close(): void {
    this.#closed = true
    this.#pending = undefined
    globalThis.clearTimeout(this.#timer)
  }

  #schedule(): void {
    this.#timer = globalThis.setTimeout(() => this.#probe(), V2_RELAY_HEARTBEAT_INTERVAL_MILLISECONDS)
  }

  #probe(): void {
    if (this.#closed) return
    this.#round += 1n
    this.#pending = this.#round
    this.#startedAt = Date.now()
    // Append one tiny probe even during pressure: waiting for bufferedAmount to
    // drain first would leave a silent, permanently blocked socket undetectable.
    this.#timer = globalThis.setTimeout(
      () => this.#failed(new RelayHeartbeatError()),
      V2_RELAY_HEARTBEAT_TIMEOUT_MILLISECONDS,
    )
    this.#emit('probe')
    try {
      this.#socket.send(encodeV2ConnectionProbe(this.#round))
    } catch (error) {
      this.#failed(error)
    }
  }

  #failed(error: unknown): void {
    if (this.#closed) return
    this.#emit('failed')
    this.close()
    this.#fail(error)
  }

  #emit(stage: RelayHeartbeatTrace['stage']): void {
    try {
      this.#trace?.({
        connectionId: this.#connectionId, round: this.#round, stage, bufferedBytes: this.#socket.bufferedAmount,
        elapsedMilliseconds: Date.now() - this.#startedAt,
        timeoutMilliseconds: V2_RELAY_HEARTBEAT_TIMEOUT_MILLISECONDS,
      })
    } catch {
      // Diagnostic consumers cannot suppress liveness detection.
    }
  }
}

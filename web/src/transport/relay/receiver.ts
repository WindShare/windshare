import { MAX_SEALED_MANIFEST_BYTES } from '../../contracts/manifest'
import { RelayFrameChannel } from './channel'
import { relayWebSocketUrl } from './endpoint'
import {
  RELAY_ERROR_RATE_LIMITED,
  decodeRelayEnvelope,
  decodeSignaling,
  encodeSignaling,
  formatSessionId,
  parseSessionId,
  sessionIdsEqual,
  validateShareId,
  type RelayErrorMessage,
  type RelaySignalingMessage,
  type SessionId,
} from './protocol'
import {
  RelayJoinWindowError,
  relayDeadlineSignal,
  relayTimerDuration,
  sleepForRelayRetry,
} from './retry-timing'
import {
  BrowserRelaySocketFactory,
  RelaySocketClosedError,
  RelaySocketIngressError,
  type RelaySocket,
  type RelaySocketFactory,
  type RelaySocketMessage,
} from './socket'

export const DEFAULT_JOIN_RETRY_WINDOW_MS = 10_000
export const DEFAULT_RETRY_INITIAL_MS = 200
export const DEFAULT_RETRY_MAX_MS = 5_000
export const DEFAULT_KEEPALIVE_MS = 20_000

export { RelayJoinWindowError } from './retry-timing'

export class RelayShareNotFoundError extends Error {
  constructor() {
    super('relay share was not found')
    this.name = 'RelayShareNotFoundError'
  }
}

export class RelayServerError extends Error {
  readonly code: string

  constructor(message: RelayErrorMessage) {
    super(`relay rejected the session: ${message.code}${serverErrorSuffix(message)}`)
    this.name = 'RelayServerError'
    this.code = message.code
  }
}

function serverErrorSuffix(message: RelayErrorMessage): string {
  return message.message === undefined ? '' : ` (${message.message})`
}

export class RelayProtocolViolation extends Error {
  readonly kind:
    | 'unexpected-message'
    | 'unexpected-binary'
    | 'manifest-sequence'
    | 'foreign-session'
    | 'malformed-message'
    | 'malformed-frame'

  constructor(
    kind: RelayProtocolViolation['kind'],
    message: string,
    options?: ErrorOptions,
  ) {
    super(message, options)
    this.name = 'RelayProtocolViolation'
    this.kind = kind
  }
}

export interface RelayReceiverOptions {
  readonly relayUrl: string
  readonly shareId: string
  readonly socketFactory?: RelaySocketFactory
  readonly joinRetryWindowMs?: number
  readonly retryInitialMs?: number
  readonly retryMaxMs?: number
  readonly keepaliveMs?: number
  readonly now?: () => number
  readonly sleep?: (milliseconds: number, signal: AbortSignal) => Promise<void>
}

interface NormalizedOptions {
  readonly relayUrl: string
  readonly shareId: string
  readonly socketFactory: RelaySocketFactory
  readonly joinRetryWindowMs: number
  readonly retryInitialMs: number
  readonly retryMaxMs: number
  readonly keepaliveMs: number
  readonly now: () => number
  readonly sleep: (milliseconds: number, signal: AbortSignal) => Promise<void>
}

interface JoinedRelay {
  readonly socket: RelaySocket
  readonly reader: ReadableStreamDefaultReader<RelaySocketMessage>
  readonly sessionId: SessionId
  readonly sealedManifest: Uint8Array
}

function normalize(options: RelayReceiverOptions): NormalizedOptions {
  validateShareId(options.shareId)
  const retryInitialMs = relayTimerDuration(
    options.retryInitialMs ?? DEFAULT_RETRY_INITIAL_MS,
    'initial retry delay',
  )
  const retryMaxMs = relayTimerDuration(
    options.retryMaxMs ?? DEFAULT_RETRY_MAX_MS,
    'maximum retry delay',
  )
  if (retryInitialMs > retryMaxMs) {
    throw new RangeError('initial retry delay must not exceed maximum retry delay')
  }
  return {
    relayUrl: options.relayUrl,
    shareId: options.shareId,
    socketFactory: options.socketFactory ?? new BrowserRelaySocketFactory(),
    joinRetryWindowMs: relayTimerDuration(
      options.joinRetryWindowMs ?? DEFAULT_JOIN_RETRY_WINDOW_MS,
      'join retry window',
    ),
    retryInitialMs,
    retryMaxMs,
    keepaliveMs: relayTimerDuration(
      options.keepaliveMs ?? DEFAULT_KEEPALIVE_MS,
      'keepalive interval',
    ),
    now: options.now ?? (() => performance.now()),
    sleep: options.sleep ?? sleepForRelayRetry,
  }
}

async function readMessage(
  reader: ReadableStreamDefaultReader<RelaySocketMessage>,
  signal: AbortSignal,
): Promise<RelaySocketMessage> {
  if (signal.aborted) {
    throw signal.reason
  }
  const cancelled = Symbol('abort wait cancelled')
  let abort!: () => void
  let cancelAbortWait!: () => void
  const aborted = new Promise<typeof cancelled>((resolve, reject) => {
    abort = () => reject(signal.reason)
    cancelAbortWait = () => resolve(cancelled)
    signal.addEventListener('abort', abort, { once: true })
  })
  try {
    const result = await Promise.race([reader.read(), aborted])
    if (result === cancelled) {
      throw new Error('relay abort wait was cancelled before a message arrived')
    }
    if (result.done) {
      throw new Error('relay socket closed while awaiting a message')
    }
    return result.value
  } finally {
    signal.removeEventListener('abort', abort)
    cancelAbortWait()
  }
}

function decodeText(message: RelaySocketMessage): RelaySignalingMessage {
  if (message.type !== 'text') {
    throw new RelayProtocolViolation(
      'unexpected-binary',
      'relay sent binary data before the manifest message',
    )
  }
  try {
    return decodeSignaling(message.data)
  } catch (cause) {
    throw new RelayProtocolViolation('malformed-message', 'relay signaling is invalid', {
      cause,
    })
  }
}

async function awaitManifest(
  socket: RelaySocket,
  shareId: string,
  signal: AbortSignal,
): Promise<JoinedRelay> {
  const reader = socket.messages.getReader()
  let joined = false
  try {
    await socket.sendText(encodeSignaling({ type: 'join', shareId }), signal)
    while (true) {
      const message = decodeText(await readMessage(reader, signal))
      if (message.type === 'keepalive') {
        continue
      }
      if (message.type === 'not_found') {
        throw new RelayShareNotFoundError()
      }
      if (message.type === 'error') {
        throw new RelayServerError(message)
      }
      if (message.type !== 'manifest') {
        throw new RelayProtocolViolation(
          'unexpected-message',
          `relay sent ${message.type} while joining`,
        )
      }
      const sealedManifest = await readManifestFrame(reader, signal)
      joined = true
      return {
        socket,
        reader,
        sessionId: parseSessionId(message.sessionId),
        sealedManifest,
      }
    }
  } finally {
    if (!joined) {
      await reader.cancel().catch(() => undefined)
      reader.releaseLock()
    }
  }
}

async function readManifestFrame(
  reader: ReadableStreamDefaultReader<RelaySocketMessage>,
  signal: AbortSignal,
): Promise<Uint8Array> {
  const manifestFrame = await readMessage(reader, signal)
  if (manifestFrame.type !== 'binary') {
    throw new RelayProtocolViolation(
      'manifest-sequence',
      'manifest signaling was not immediately followed by binary manifest data',
    )
  }
  let envelope
  try {
    envelope = decodeRelayEnvelope(manifestFrame.data)
  } catch (cause) {
    throw new RelayProtocolViolation('malformed-frame', 'manifest envelope is malformed', {
      cause,
    })
  }
  if (envelope.type !== 'manifest') {
    throw new RelayProtocolViolation('manifest-sequence', 'manifest envelope has wrong type')
  }
  if (envelope.sealedManifest.byteLength > MAX_SEALED_MANIFEST_BYTES) {
    throw new RelayProtocolViolation('malformed-frame', 'sealed manifest exceeds its size limit')
  }
  return envelope.sealedManifest
}

function retryable(error: unknown): boolean {
  if (error instanceof RelayShareNotFoundError) {
    return true
  }
  if (error instanceof RelayServerError) {
    return error.code === RELAY_ERROR_RATE_LIMITED
  }
  return !(
    error instanceof RelayProtocolViolation ||
    error instanceof RelaySocketIngressError ||
    error instanceof TypeError
  )
}

async function joinWithRetry(
  options: NormalizedOptions,
  signal?: AbortSignal,
): Promise<JoinedRelay> {
  const endpoint = relayWebSocketUrl(options.relayUrl, options.shareId)
  const expiresAt = options.now() + options.joinRetryWindowMs
  const deadline = relayDeadlineSignal(options.joinRetryWindowMs, signal)
  let delay = options.retryInitialMs
  let lastError: unknown = new RelayShareNotFoundError()
  try {
    while (!deadline.signal.aborted) {
      let socket: RelaySocket | undefined
      try {
        socket = await options.socketFactory.connect(endpoint, deadline.signal)
        return await awaitManifest(socket, options.shareId, deadline.signal)
      } catch (error) {
        lastError = error
        await socket?.close().catch(() => undefined)
        if (!retryable(error)) {
          throw error
        }
      }
      const remaining = expiresAt - options.now()
      if (remaining <= 0) {
        break
      }
      await options.sleep(Math.min(delay, remaining), deadline.signal)
      delay = Math.min(delay * 2, options.retryMaxMs)
    }
  } catch (error) {
    if (signal?.aborted === true) {
      throw signal.reason
    }
    if (!deadline.signal.aborted) {
      throw error
    }
  } finally {
    deadline.cleanup()
  }
  throw new RelayJoinWindowError(lastError)
}

export class RelayReceiverConnection {
  readonly #options: NormalizedOptions
  readonly #socket: RelaySocket
  readonly #reader: ReadableStreamDefaultReader<RelaySocketMessage>
  readonly #sessionId: SessionId
  readonly #sealedManifest: Uint8Array
  readonly channel: RelayFrameChannel
  readonly done: Promise<void>
  #resolveDone!: () => void
  #error: unknown
  #localClose = false
  #keepalivePending = false
  #keepalive: ReturnType<typeof setInterval> | undefined

  constructor(options: NormalizedOptions, joined: JoinedRelay) {
    this.#options = options
    this.#socket = joined.socket
    this.#reader = joined.reader
    this.#sessionId = joined.sessionId
    this.#sealedManifest = joined.sealedManifest.slice()
    this.channel = new RelayFrameChannel(this.#sessionId, this.#socket)
    this.done = new Promise((resolve) => {
      this.#resolveDone = resolve
    })
    this.#keepalive = setInterval(() => {
      this.#queueKeepalive()
    }, options.keepaliveMs)
    this.#run().catch((error) => {
      this.#error = error
      this.#resolveDone()
    })
  }

  get sessionId(): SessionId {
    return this.#sessionId.slice() as SessionId
  }

  get sealedManifest(): Uint8Array {
    return this.#sealedManifest.slice()
  }

  get error(): unknown {
    return this.#error
  }

  async close(): Promise<void> {
    if (!this.#localClose) {
      this.#localClose = true
      this.#stopKeepalive()
      await this.channel.close().catch(() => undefined)
      await this.#socket.close()
    }
    await this.done
  }

  async rejoin(signal?: AbortSignal): Promise<RelayReceiverConnection> {
    await this.close()
    const joined = await joinWithRetry(this.#options, signal)
    return new RelayReceiverConnection(this.#options, joined)
  }

  async #run(): Promise<void> {
    try {
      await this.#serve()
    } catch (error) {
      if (!this.#localClose) {
        this.#error = error
        this.channel.remoteClose(error)
      }
    } finally {
      this.#stopKeepalive()
      await this.#socket.close().catch(() => undefined)
      this.channel.remoteClose(this.#error)
      this.#reader.releaseLock()
      this.#resolveDone()
    }
  }

  async #serve(): Promise<void> {
    while (true) {
      const result = await this.#reader.read()
      if (result.done) {
        if (this.#localClose) {
          return
        }
        if (this.channel.reason !== undefined) {
          throw this.channel.reason
        }
        throw new RelaySocketClosedError('relay socket closed unexpectedly')
      }
      if (result.value.type === 'text') {
        if (this.#handleSignaling(this.#decodeRuntimeSignaling(result.value.data))) {
          return
        }
      } else if (this.#handleEnvelope(this.#decodeRuntimeEnvelope(result.value.data))) {
        return
      }
    }
  }

  #decodeRuntimeSignaling(text: string): RelaySignalingMessage {
    try {
      return decodeSignaling(text)
    } catch (cause) {
      throw new RelayProtocolViolation('malformed-message', 'relay signaling is invalid', {
        cause,
      })
    }
  }

  #decodeRuntimeEnvelope(wire: Uint8Array): ReturnType<typeof decodeRelayEnvelope> {
    try {
      return decodeRelayEnvelope(wire)
    } catch (cause) {
      throw new RelayProtocolViolation('malformed-frame', 'relay envelope is invalid', {
        cause,
      })
    }
  }

  #failKeepalive(error: unknown): void {
    if (this.#localClose) {
      return
    }
    this.channel.remoteClose(error)
    this.#socket.close().catch(() => undefined)
  }

  #queueKeepalive(): void {
    if (this.#keepalivePending || this.#localClose || this.channel.state !== 'open') {
      return
    }
    this.#keepalivePending = true
    this.channel.sendKeepalive()
      .catch((error) => this.#failKeepalive(error))
      .finally(() => {
        this.#keepalivePending = false
      })
  }

  #stopKeepalive(): void {
    if (this.#keepalive !== undefined) {
      clearInterval(this.#keepalive)
      this.#keepalive = undefined
    }
  }

  #handleSignaling(message: RelaySignalingMessage): boolean {
    if (message.type === 'keepalive') {
      return false
    }
    if (message.type === 'signal') {
      this.#requireSession(message.sessionId)
      const result = this.channel.deliverSignal({ kind: message.kind, payload: message.payload })
      if (result === 'overflow') {
        throw this.channel.failIngress('signals')
      }
      return false
    }
    if (message.type === 'bye') {
      this.#requireSession(message.sessionId)
      this.channel.remoteClose()
      return true
    }
    if (message.type === 'error') {
      if (message.sessionId !== undefined) {
        this.#requireSession(message.sessionId)
      }
      throw new RelayServerError(message)
    }
    throw new RelayProtocolViolation(
      'unexpected-message',
      `relay sent ${message.type} after joining`,
    )
  }

  #handleEnvelope(envelope: ReturnType<typeof decodeRelayEnvelope>): boolean {
    if (envelope.type === 'manifest') {
      throw new RelayProtocolViolation('malformed-frame', 'unexpected manifest envelope')
    }
    if (!sessionIdsEqual(envelope.sessionId, this.#sessionId)) {
      throw new RelayProtocolViolation('foreign-session', 'relay routed a foreign session frame')
    }
    const result =
      envelope.type === 'terminal-forward'
        ? this.channel.deliverTerminal(envelope.frame)
        : this.channel.deliverFrame(envelope.frame)
    if (result === 'overflow') {
      throw this.channel.failIngress('frames')
    }
    return envelope.type === 'terminal-forward'
  }

  #requireSession(encoded: string): void {
    if (!sessionIdsEqual(parseSessionId(encoded), this.#sessionId)) {
      throw new RelayProtocolViolation(
        'foreign-session',
        `relay signaling targeted session ${encoded}, expected ${formatSessionId(this.#sessionId)}`,
      )
    }
  }
}

export async function dialRelayReceiver(
  options: RelayReceiverOptions,
  signal?: AbortSignal,
): Promise<RelayReceiverConnection> {
  const normalized = normalize(options)
  const joined = await joinWithRetry(normalized, signal)
  return new RelayReceiverConnection(normalized, joined)
}

const DEFAULT_CAPTURE_BYTES = 16_777_216
const MAXIMUM_CAPTURE_BYTES = 67_108_864
const MAXIMUM_EVENT_COUNT = 4_096

export const OWNED_PROCESS_CAPTURE_LIMITS = Object.freeze({
  defaultStdoutBytes: DEFAULT_CAPTURE_BYTES,
  defaultStderrBytes: DEFAULT_CAPTURE_BYTES,
  maximumStreamBytes: MAXIMUM_CAPTURE_BYTES,
  maximumEvents: MAXIMUM_EVENT_COUNT,
})

export function normalizeOwnedProcessCapture(value) {
  if (value === undefined) {
    return Object.freeze({
      stdoutBytes: DEFAULT_CAPTURE_BYTES,
      stderrBytes: DEFAULT_CAPTURE_BYTES,
      eventCount: 0,
    })
  }
  requireRecord(value, 'owned process capture authority')
  rejectUnknownKeys(value, ['stdoutBytes', 'stderrBytes', 'eventCount'], 'owned process capture authority')
  const stdoutBytes = value.stdoutBytes ?? DEFAULT_CAPTURE_BYTES
  const stderrBytes = value.stderrBytes ?? DEFAULT_CAPTURE_BYTES
  const eventCount = value.eventCount ?? 0
  requireIntegerInRange(stdoutBytes, 1, MAXIMUM_CAPTURE_BYTES, 'owned stdout byte limit')
  requireIntegerInRange(stderrBytes, 1, MAXIMUM_CAPTURE_BYTES, 'owned stderr byte limit')
  requireIntegerInRange(eventCount, 0, MAXIMUM_EVENT_COUNT, 'owned event count limit')
  return Object.freeze({ stdoutBytes, stderrBytes, eventCount })
}

export function createOwnedByteChannel(maximumBytes, label) {
  requireIntegerInRange(maximumBytes, 1, MAXIMUM_CAPTURE_BYTES, `${label} byte limit`)
  requireLabel(label)
  const state = new OwnedByteState(maximumBytes, label)
  return Object.freeze({
    view: state.view(),
    append: (chunk) => state.append(chunk),
    fail: (cause) => state.fail(cause),
    finish: () => state.finish(),
    failure: () => state.failure(),
  })
}

export function createOwnedEventChannel(maximumEvents, label) {
  requireIntegerInRange(maximumEvents, 0, MAXIMUM_EVENT_COUNT, `${label} count limit`)
  requireLabel(label)
  const state = new OwnedEventState(maximumEvents, label)
  return Object.freeze({
    view: state.view(),
    append: (event) => state.append(event),
    fail: (cause) => state.fail(cause),
    finish: () => state.finish(),
    failure: () => state.failure(),
  })
}

export function waitForExactWritableCompletion(stream, label) {
  requireLabel(label)
  return new Promise((resolveCompletion, rejectCompletion) => {
    let settled = false
    let finished = false
    const cleanup = () => {
      stream.off('finish', onFinish)
      stream.off('close', onClose)
      stream.off('error', onError)
    }
    const resolve = () => {
      if (settled) return
      settled = true
      cleanup()
      resolveCompletion()
    }
    const reject = (cause) => {
      if (settled) return
      settled = true
      cleanup()
      const detail = cause instanceof Error ? `: ${cause.message}` : ''
      rejectCompletion(new Error(`${label} failed${detail}`, { cause }))
    }
    const onFinish = () => {
      finished = true
      resolve()
    }
    const onClose = () => {
      if (!finished) reject(new Error(`${label} closed before its bytes finished`))
    }
    const onError = (cause) => reject(cause)
    stream.once('finish', onFinish)
    stream.once('close', onClose)
    stream.once('error', onError)
    // The helper can be installed by a retirement path after the peer has
    // already transitioned the stream; state inspection closes that event gap.
    if (stream.writableFinished) onFinish()
    else if (stream.destroyed) onClose()
  })
}

class OwnedByteState {
  #chunks = []
  #capturedBytes = 0
  #observedBytes = 0
  #failure
  #finished = false
  #waiters = new Set()

  constructor(maximumBytes, label) {
    this.maximumBytes = maximumBytes
    this.label = label
  }

  view() {
    const state = this
    return Object.freeze({
      snapshot: () => state.snapshot(),
      [Symbol.asyncIterator]: () => state.iterator(),
    })
  }

  append(value) {
    if (!(value instanceof Uint8Array)) {
      this.fail(new Error(`${this.label} transport produced non-byte output`))
      return
    }
    if (this.#finished) {
      this.fail(new Error(`${this.label} transport produced bytes after EOF`))
      return
    }
    if (value.byteLength === 0) return
    this.#observedBytes = boundedObservedCount(this.#observedBytes, value.byteLength, this.label, (cause) => {
      this.fail(cause)
    })
    const remaining = this.maximumBytes - this.#capturedBytes
    if (remaining > 0) {
      const accepted = Buffer.from(value.subarray(0, Math.min(remaining, value.byteLength)))
      this.#chunks.push(accepted)
      this.#capturedBytes += accepted.byteLength
    }
    if (value.byteLength > remaining) {
      this.fail(new Error(`${this.label} exceeded its ${this.maximumBytes}-byte capture authority`))
    }
    this.#wake()
  }

  fail(cause) {
    this.#failure ??= asError(cause, `${this.label} failed`)
    this.#wake()
  }

  finish() {
    if (this.#finished) return
    this.#finished = true
    this.#wake()
  }

  failure() {
    return this.#failure
  }

  snapshot() {
    const bytes = Buffer.concat(this.#chunks, this.#capturedBytes)
    return immutableByteSnapshot(
      bytes,
      this.#observedBytes,
      this.#capturedBytes,
      this.#observedBytes > this.#capturedBytes,
      this.#finished,
    )
  }

  iterator() {
    let cursor = 0
    let returned = false
    let active = false
    const cancellation = new AbortController()
    const state = this
    return Object.freeze({
      async next() {
        if (returned) return Object.freeze({ done: true, value: undefined })
        if (active) throw new Error(`${state.label} iterator does not allow concurrent pulls`)
        active = true
        try {
          const result = await state.#read(cursor, cancellation.signal)
          cursor = result.cursor
          return result.entry
        } finally {
          active = false
        }
      },
      async return() {
        returned = true
        cancellation.abort()
        return Object.freeze({ done: true, value: undefined })
      },
      [Symbol.asyncIterator]() { return this },
    })
  }

  async #read(cursor, signal) {
    while (true) {
      if (signal.aborted) {
        return Object.freeze({
          cursor,
          entry: Object.freeze({ done: true, value: undefined }),
        })
      }
      if (cursor < this.#capturedBytes) {
        const bytes = Buffer.concat(this.#chunks, this.#capturedBytes).subarray(cursor)
        const nextCursor = this.#capturedBytes
        return Object.freeze({
          cursor: nextCursor,
          entry: Object.freeze({ done: false, value: Uint8Array.from(bytes) }),
        })
      }
      if (this.#failure !== undefined) throw this.#failure
      if (this.#finished) {
        return Object.freeze({
          cursor,
          entry: Object.freeze({ done: true, value: undefined }),
        })
      }
      await this.#changed(signal)
    }
  }

  #changed(signal) {
    if (signal.aborted) return Promise.resolve()
    return new Promise((resolveChange) => {
      const wake = () => {
        this.#waiters.delete(wake)
        signal.removeEventListener('abort', wake)
        resolveChange()
      }
      this.#waiters.add(wake)
      signal.addEventListener('abort', wake, { once: true })
    })
  }

  #wake() {
    const waiters = [...this.#waiters]
    this.#waiters.clear()
    for (const resolveChange of waiters) resolveChange()
  }
}

class OwnedEventState {
  #events = []
  #observedEvents = 0
  #failure
  #finished = false
  #waiters = new Set()

  constructor(maximumEvents, label) {
    this.maximumEvents = maximumEvents
    this.label = label
  }

  view() {
    const state = this
    return Object.freeze({
      snapshot: () => state.snapshot(),
      [Symbol.asyncIterator]: () => state.iterator(),
    })
  }

  append(event) {
    if (this.#finished) {
      this.fail(new Error(`${this.label} transport produced an event after EOF`))
      return
    }
    this.#observedEvents += 1
    if (this.#events.length < this.maximumEvents) this.#events.push(event)
    else this.fail(new Error(`${this.label} exceeded its ${this.maximumEvents}-event capture authority`))
    this.#wake()
  }

  fail(cause) {
    this.#failure ??= asError(cause, `${this.label} failed`)
    this.#wake()
  }

  finish() {
    if (this.#finished) return
    this.#finished = true
    this.#wake()
  }

  failure() {
    return this.#failure
  }

  snapshot() {
    return Object.freeze({
      events: Object.freeze([...this.#events]),
      observedEvents: this.#observedEvents,
      capturedEvents: this.#events.length,
      truncated: this.#observedEvents > this.#events.length,
      completed: this.#finished,
    })
  }

  iterator() {
    let cursor = 0
    let returned = false
    let active = false
    const cancellation = new AbortController()
    const state = this
    return Object.freeze({
      async next() {
        if (returned) return Object.freeze({ done: true, value: undefined })
        if (active) throw new Error(`${state.label} iterator does not allow concurrent pulls`)
        active = true
        try {
          const result = await state.#read(cursor, cancellation.signal)
          cursor = result.cursor
          return result.entry
        } finally {
          active = false
        }
      },
      async return() {
        returned = true
        cancellation.abort()
        return Object.freeze({ done: true, value: undefined })
      },
      [Symbol.asyncIterator]() { return this },
    })
  }

  async #read(cursor, signal) {
    while (true) {
      if (signal.aborted) {
        return Object.freeze({
          cursor,
          entry: Object.freeze({ done: true, value: undefined }),
        })
      }
      if (cursor < this.#events.length) {
        return Object.freeze({
          cursor: cursor + 1,
          entry: Object.freeze({ done: false, value: this.#events[cursor] }),
        })
      }
      if (this.#failure !== undefined) throw this.#failure
      if (this.#finished) {
        return Object.freeze({
          cursor,
          entry: Object.freeze({ done: true, value: undefined }),
        })
      }
      await this.#changed(signal)
    }
  }

  #changed(signal) {
    if (signal.aborted) return Promise.resolve()
    return new Promise((resolveChange) => {
      const wake = () => {
        this.#waiters.delete(wake)
        signal.removeEventListener('abort', wake)
        resolveChange()
      }
      this.#waiters.add(wake)
      signal.addEventListener('abort', wake, { once: true })
    })
  }

  #wake() {
    const waiters = [...this.#waiters]
    this.#waiters.clear()
    for (const resolveChange of waiters) resolveChange()
  }
}

function immutableByteSnapshot(bytes, observedBytes, capturedBytes, truncated, completed) {
  const retained = Buffer.from(bytes)
  return Object.freeze({
    observedBytes,
    capturedBytes,
    truncated,
    completed,
    bytes: () => Uint8Array.from(retained),
  })
}

function boundedObservedCount(current, additional, label, fail) {
  if (current > Number.MAX_SAFE_INTEGER - additional) {
    fail(new Error(`${label} observed byte count exceeded the safe integer range`))
    return Number.MAX_SAFE_INTEGER
  }
  return current + additional
}

function asError(value, fallback) {
  return value instanceof Error ? value : new Error(fallback, { cause: value })
}

function requireLabel(value) {
  if (typeof value !== 'string' || value.length < 1) throw new Error('owned channel label is invalid')
}

function requireIntegerInRange(value, minimum, maximum, label) {
  if (!Number.isSafeInteger(value) || value < minimum || value > maximum) {
    throw new Error(`${label} must be an integer in [${minimum}, ${maximum}]`)
  }
}

function requireRecord(value, label) {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
}

function rejectUnknownKeys(value, keys, label) {
  const allowed = new Set(keys)
  if (Object.keys(value).some((key) => !allowed.has(key))) {
    throw new Error(`${label} contains unknown keys`)
  }
}

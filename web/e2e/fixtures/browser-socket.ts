import type { Page } from '@playwright/test'

interface BrowserBlockFrame {
  readonly index: number
  readonly last: boolean
}

function socketParserScript(): void {
  const HARNESS_READY_EVENT = 'windshare-e2e-harness-ready'
  const SOCKET_TOOLS_READY_EVENT = 'windshare-e2e-socket-tools-ready'
  if (window.__windshareE2E === undefined) {
    window.addEventListener(HARNESS_READY_EVENT, socketParserScript, { once: true })
    return
  }
  const state = window.__windshareE2E
  const ENVELOPE_FORWARD = 0x02
  const ENVELOPE_TERMINAL_FORWARD = 0x03
  const FRAME_REQUEST = 0x01
  const FRAME_BLOCK = 0x02
  const BLOCK_FLAG_LAST = 0x01
  const ROUTED_ENVELOPE_BYTES = 9
  const REQUEST_COUNT_OFFSET = ROUTED_ENVELOPE_BYTES + 1
  const REQUEST_INDICES_OFFSET = ROUTED_ENVELOPE_BYTES + 5
  const BLOCK_INDEX_OFFSET = ROUTED_ENVELOPE_BYTES + 1
  const BLOCK_FLAGS_OFFSET = ROUTED_ENVELOPE_BYTES + 13
  const UINT64_BYTES = 8
  const asBytes = (value: unknown): Uint8Array | undefined => {
    if (value instanceof ArrayBuffer) return new Uint8Array(value)
    if (ArrayBuffer.isView(value)) {
      return new Uint8Array(value.buffer, value.byteOffset, value.byteLength)
    }
    return undefined
  }
  const signal = (value: unknown) => {
    if (typeof value !== 'string') return
    try {
      const decoded = JSON.parse(value) as { readonly type?: string; readonly kind?: string }
      if (decoded.type !== 'signal') return
      if (decoded.kind === 'offer') state.metrics.signalOffers += 1
      if (decoded.kind === 'candidate') state.metrics.signalIceCandidates += 1
    } catch {
      // Invalid signaling remains production code's responsibility.
    }
  }
  const block = (value: unknown): BrowserBlockFrame | undefined => {
    const bytes = asBytes(value)
    if (
      bytes === undefined ||
      bytes.byteLength <= BLOCK_FLAGS_OFFSET ||
      (bytes[0] !== ENVELOPE_FORWARD && bytes[0] !== ENVELOPE_TERMINAL_FORWARD) ||
      bytes[ROUTED_ENVELOPE_BYTES] !== FRAME_BLOCK
    ) {
      return undefined
    }
    const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength)
    const index = Number(view.getBigUint64(BLOCK_INDEX_OFFSET, true))
    return Number.isSafeInteger(index)
      ? { index, last: bytes[BLOCK_FLAGS_OFFSET] === BLOCK_FLAG_LAST }
      : undefined
  }
  const request = (value: unknown): number[] | undefined => {
    const bytes = asBytes(value)
    if (
      bytes === undefined ||
      bytes.byteLength < REQUEST_INDICES_OFFSET ||
      bytes[0] !== ENVELOPE_FORWARD ||
      bytes[ROUTED_ENVELOPE_BYTES] !== FRAME_REQUEST
    ) return undefined
    const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength)
    const count = view.getUint32(REQUEST_COUNT_OFFSET, true)
    if (count > Math.floor((bytes.byteLength - REQUEST_INDICES_OFFSET) / UINT64_BYTES)) {
      return undefined
    }
    return Array.from({ length: count }, (_, offset) =>
      Number(view.getBigUint64(REQUEST_INDICES_OFFSET + offset * UINT64_BYTES, true)),
    )
  }
  const invoke = (
    listener: EventListenerOrEventListenerObject,
    socket: WebSocket,
    event: Event,
  ) => {
    if ('handleEvent' in listener) listener.handleEvent(event)
    else listener.call(socket, event)
  }
  window.__windshareE2ESocketTools = { signal, block, request, invoke }
  window.dispatchEvent(new Event(SOCKET_TOOLS_READY_EVENT))
}

function socketInterceptionScript(): void {
  const SOCKET_TOOLS_READY_EVENT = 'windshare-e2e-socket-tools-ready'
  if (window.__windshareE2ESocketTools === undefined) {
    window.addEventListener(SOCKET_TOOLS_READY_EVENT, socketInterceptionScript, { once: true })
    return
  }
  const state = window.__windshareE2E
  const NativeWebSocket = window.WebSocket
  const RELAY_ENDPOINT_PREFIX = '/v1/ws/'
  const { signal, block, request, invoke } = window.__windshareE2ESocketTools

  const instrument = (socket: WebSocket, rawUrl: unknown): WebSocket => {
    const relay = new URL(String(rawUrl), window.location.href).pathname.startsWith(
      RELAY_ENDPOINT_PREFIX,
    )
    if (!relay) return socket
    state.metrics.connections += 1
    const connection = state.metrics.connections
    // The oracle needs only ordering, never the capability value itself. Retaining
    // the fragment would turn a failed security assertion into a diagnostic leak.
    state.metrics.fragmentPresentAtFirstSocket ??= window.location.hash !== ''
    const nativeSend = socket.send.bind(socket)
    const nativeAdd = socket.addEventListener.bind(socket) as (
      type: string,
      listener: EventListenerOrEventListenerObject,
      options?: boolean | AddEventListenerOptions,
    ) => void
    const nativeRemove = socket.removeEventListener.bind(socket) as (
      type: string,
      listener: EventListenerOrEventListenerObject,
      options?: boolean | EventListenerOptions,
    ) => void
    const wrapped = new Map<EventListenerOrEventListenerObject, EventListener>()
    const seen = new WeakSet<Event>()
    const firstBlockFrames: MessageEvent[] = []
    const secondBlockFrames: MessageEvent[] = []
    let firstBlockIndex: number | undefined
    let reorderComplete = false
    const recordClose = () => { state.metrics.relayCloseEvents += 1 }
    const recordError = () => { state.metrics.relayErrorEvents += 1 }
    nativeAdd('close', recordClose)
    nativeAdd('error', recordError)

    const deliver = (event: MessageEvent, listener: EventListenerOrEventListenerObject) => {
      const received = block(event.data)
      if (received === undefined) return
      state.metrics.blockDelivery.push(received.index)
      invoke(listener, socket, event)
      if (received.last) state.metrics.completedBlockDelivery.push(received.index)
    }
    const recordArrival = (event: MessageEvent) => {
      if (seen.has(event)) return
      seen.add(event)
      signal(event.data)
      const arrived = block(event.data)
      if (arrived === undefined) return
      state.metrics.blockArrival.push(arrived.index)
      if (arrived.last) state.metrics.completedBlockArrival.push(arrived.index)
    }
    const queueReordered = (
      event: MessageEvent,
      received: BrowserBlockFrame,
      listener: EventListenerOrEventListenerObject,
    ): boolean => {
      if (!state.config.reorderFirstBlockPair || reorderComplete) return false
      firstBlockIndex ??= received.index
      if (received.index === firstBlockIndex) {
        firstBlockFrames.push(event)
        return true
      }
      secondBlockFrames.push(event)
      if (received.last) {
        reorderComplete = true
        for (const frame of secondBlockFrames) deliver(frame, listener)
        for (const frame of firstBlockFrames) deliver(frame, listener)
      }
      return true
    }
    const message = (event: MessageEvent, listener: EventListenerOrEventListenerObject) => {
      recordArrival(event)
      const received = block(event.data)
      if (received === undefined) {
        invoke(listener, socket, event)
        return
      }
      if (queueReordered(event, received, listener)) return
      deliver(event, listener)
    }

    Object.defineProperties(socket, {
      send: {
        configurable: true,
        value: (data: string | ArrayBufferLike | Blob | ArrayBufferView) => {
          signal(data)
          const indices = request(data)
          if (indices !== undefined) state.metrics.requests.push({ connection, indices })
          nativeSend(data as string | ArrayBuffer | Blob | ArrayBufferView<ArrayBuffer>)
        },
      },
      addEventListener: {
        configurable: true,
        value: (
          type: string,
          listener: EventListenerOrEventListenerObject | null,
          options?: boolean | AddEventListenerOptions,
        ) => {
          if (listener === null) return
          if (type !== 'message') {
            nativeAdd(type, listener, options)
            return
          }
          const intercepted: EventListener = (event) => message(event as MessageEvent, listener)
          wrapped.set(listener, intercepted)
          nativeAdd(type, intercepted, options)
        },
      },
      removeEventListener: {
        configurable: true,
        value: (
          type: string,
          listener: EventListenerOrEventListenerObject | null,
          options?: boolean | EventListenerOptions,
        ) => {
          if (listener !== null) nativeRemove(type, wrapped.get(listener) ?? listener, options)
        },
      },
    })
    return socket
  }
  const InstrumentedWebSocket = new Proxy(NativeWebSocket, {
    construct(target, args) {
      return instrument(Reflect.construct(target, args, target) as WebSocket, args[0])
    },
  })
  Object.defineProperty(window, 'WebSocket', {
    configurable: true,
    value: InstrumentedWebSocket,
  })
}

export async function installSocketHarness(page: Page): Promise<void> {
  await page.addInitScript(socketParserScript)
  await page.addInitScript(socketInterceptionScript)
}

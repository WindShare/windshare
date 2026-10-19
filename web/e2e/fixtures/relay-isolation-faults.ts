import type { Page } from '@playwright/test'

export interface RelaySocketSnapshot {
  readonly opened: number
  readonly closed: number
  readonly cuts: number
  readonly receivedBytes: number
  readonly events: readonly { readonly connection: number; readonly at: number; readonly kind: string; readonly code?: number; readonly reason?: string }[]
}

interface RelayIsolationFaults {
  paceFeedback(milliseconds: number): void
  disrupt(bytes: number, count: number): void
  snapshot(): RelaySocketSnapshot
}

declare global {
  interface Window {
    relayIsolationFaults: RelayIsolationFaults
  }
}

export async function installRelayIsolationFaults(page: Page): Promise<void> {
  await page.addInitScript(() => {
    Object.defineProperties(window, {
      RTCPeerConnection: { configurable: true, value: undefined },
      showDirectoryPicker: { configurable: true, value: undefined },
      showSaveFilePicker: { configurable: true, value: undefined },
    })
    Object.defineProperty(navigator.storage, 'getDirectory', { configurable: true, value: undefined })
    const NativeWebSocket = window.WebSocket
    let feedbackDelay = 0
    let threshold = Number.POSITIVE_INFINITY
    let maximumCuts = 0
    const state = { opened: 0, closed: 0, cuts: 0, receivedBytes: 0 }
    const events: { connection: number; at: number; kind: string; code?: number; reason?: string }[] = []
    const binary = (data: unknown): Uint8Array | undefined => {
      if (data instanceof ArrayBuffer) return new Uint8Array(data)
      if (ArrayBuffer.isView(data)) return new Uint8Array(data.buffer, data.byteOffset, data.byteLength)
      return undefined
    }
    const magic = (bytes: Uint8Array | undefined) =>
      bytes === undefined ? '' : String.fromCharCode(...bytes.subarray(0, 4))
    window.WebSocket = class extends NativeWebSocket {
      constructor(url: string | URL, protocols?: string | string[]) {
        super(url, protocols)
        if (new URL(String(url)).pathname !== '/v2/ws') return
        state.opened += 1
        const connection = state.opened
        events.push({ connection, at: Date.now(), kind: 'created' })
        this.addEventListener('open', () => { events.push({ connection, at: Date.now(), kind: 'open' }) })
        let receivedBytes = 0
        let interrupted = false
        this.addEventListener('message', event => {
          const bytes = binary(event.data)
          if (magic(bytes) !== 'WS2O') return
          receivedBytes += bytes!.byteLength
          state.receivedBytes += bytes!.byteLength
          if (!interrupted && state.cuts < maximumCuts && receivedBytes >= threshold) {
            interrupted = true
            state.cuts += 1
            events.push({ connection, at: Date.now(), kind: 'injected-disconnect' })
            this.close(4000, 'Receiver isolation regression')
          }
        })
        this.addEventListener('close', event => {
          state.closed += 1
          events.push({ connection, at: Date.now(), kind: 'close', code: event.code, reason: event.reason })
        })
      }
      send(data: Parameters<WebSocket['send']>[0]) {
        const bytes = binary(data)
        // Pace unchanged consumption reports so the sibling is still downloading
        // across all cuts; heartbeats and settlement remain unmodified.
        if (feedbackDelay > 0 && magic(bytes) === 'WS2B') {
          const retained = bytes!.slice()
          setTimeout(() => {
            if (this.readyState === NativeWebSocket.OPEN) NativeWebSocket.prototype.send.call(this, retained)
          }, feedbackDelay)
          return
        }
        super.send(data)
      }
    }
    window.relayIsolationFaults = {
      paceFeedback: milliseconds => { feedbackDelay = milliseconds },
      disrupt: (bytes, count) => { threshold = bytes; maximumCuts = count },
      snapshot: () => ({ ...state, events: [...events] }),
    }
  })
}

import type { Page } from '@playwright/test'

export interface M1CPathOptions {
  readonly remoteDescriptionDelayMs?: number
}

export interface M1CPathMetrics {
  readonly peerConnections: number
  readonly openedChannels: number
  readonly closedChannels: number
  readonly remoteDescriptions: number
  readonly completedRemoteDescriptions: number
  readonly terminalIntents: number
  readonly rtcRequests: readonly (readonly number[])[]
  readonly rtcCompletedBlocks: readonly number[]
  readonly rtcErrorFrames: number
}

interface MutableM1CPathMetrics {
  peerConnections: number
  openedChannels: number
  closedChannels: number
  remoteDescriptions: number
  completedRemoteDescriptions: number
  terminalIntents: number
  rtcRequests: number[][]
  rtcCompletedBlocks: number[]
  rtcErrorFrames: number
}

declare global {
  interface Window {
    __windshareM1CPath: MutableM1CPathMetrics
  }
}

const DEFAULT_OPTIONS: Required<M1CPathOptions> = Object.freeze({
  remoteDescriptionDelayMs: 0,
})

function pathInstrumentationScript(options: Required<M1CPathOptions>): void {
  const HARNESS_READY_EVENT = 'windshare-e2e-harness-ready'
  if (window.__windshareE2E === undefined) {
    window.addEventListener(
      HARNESS_READY_EVENT,
      () => pathInstrumentationScript(options),
      { once: true },
    )
    return
  }
  if (window.__windshareM1CPath !== undefined) return

  const FRAME_REQUEST = 0x01
  const FRAME_BLOCK = 0x02
  const FRAME_ERROR = 0x03
  const BLOCK_FLAG_LAST = 0x01
  const REQUEST_COUNT_OFFSET = 1
  const REQUEST_INDICES_OFFSET = 5
  const BLOCK_INDEX_OFFSET = 1
  const BLOCK_FLAGS_OFFSET = 13
  const UINT64_BYTES = 8
  const TERMINAL_INTENT = 'terminal-intent'
  const metrics: MutableM1CPathMetrics = {
    peerConnections: 0,
    openedChannels: 0,
    closedChannels: 0,
    remoteDescriptions: 0,
    completedRemoteDescriptions: 0,
    terminalIntents: 0,
    rtcRequests: [],
    rtcCompletedBlocks: [],
    rtcErrorFrames: 0,
  }
  Object.defineProperty(window, '__windshareM1CPath', {
    configurable: false,
    value: metrics,
  })

  const asBytes = (value: unknown): Uint8Array | undefined => {
    if (value instanceof ArrayBuffer) return new Uint8Array(value)
    if (ArrayBuffer.isView(value)) {
      return new Uint8Array(value.buffer, value.byteOffset, value.byteLength)
    }
    return undefined
  }
  const requestIndices = (value: unknown): number[] | undefined => {
    const bytes = asBytes(value)
    if (bytes === undefined || bytes.byteLength < REQUEST_INDICES_OFFSET || bytes[0] !== FRAME_REQUEST) {
      return undefined
    }
    const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength)
    const count = view.getUint32(REQUEST_COUNT_OFFSET, true)
    if (count > Math.floor((bytes.byteLength - REQUEST_INDICES_OFFSET) / UINT64_BYTES)) {
      return undefined
    }
    const indices = Array.from({ length: count }, (_, offset) =>
      Number(view.getBigUint64(REQUEST_INDICES_OFFSET + offset * UINT64_BYTES, true)),
    )
    return indices.every(Number.isSafeInteger) ? indices : undefined
  }
  const inspectInbound = (value: unknown) => {
    if (value === TERMINAL_INTENT) {
      metrics.terminalIntents += 1
      return
    }
    const bytes = asBytes(value)
    if (bytes === undefined || bytes.byteLength === 0) return
    if (bytes[0] === FRAME_ERROR) {
      metrics.rtcErrorFrames += 1
      return
    }
    if (
      bytes[0] === FRAME_BLOCK &&
      bytes.byteLength > BLOCK_FLAGS_OFFSET &&
      bytes[BLOCK_FLAGS_OFFSET] === BLOCK_FLAG_LAST
    ) {
      const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength)
      const index = Number(view.getBigUint64(BLOCK_INDEX_OFFSET, true))
      if (Number.isSafeInteger(index)) metrics.rtcCompletedBlocks.push(index)
    }
  }
  const instrumentChannel = (channel: RTCDataChannel): RTCDataChannel => {
    const nativeSend = channel.send.bind(channel)
    Object.defineProperty(channel, 'send', {
      configurable: true,
      value: (data: string | ArrayBuffer | ArrayBufferView | Blob) => {
        const indices = requestIndices(data)
        if (indices !== undefined) metrics.rtcRequests.push(indices)
        nativeSend(data as ArrayBuffer)
      },
    })
    channel.addEventListener('open', () => { metrics.openedChannels += 1 }, { once: true })
    channel.addEventListener('close', () => { metrics.closedChannels += 1 }, { once: true })
    channel.addEventListener('message', (event) => inspectInbound(event.data))
    return channel
  }

  const NativePeerConnection = window.RTCPeerConnection
  const InstrumentedPeerConnection = new Proxy(NativePeerConnection, {
    construct(target, args) {
      const peer = Reflect.construct(target, args, target) as RTCPeerConnection
      metrics.peerConnections += 1
      const nativeCreateDataChannel = peer.createDataChannel.bind(peer)
      const nativeSetRemoteDescription = peer.setRemoteDescription.bind(peer)
      Object.defineProperties(peer, {
        createDataChannel: {
          configurable: true,
          value: (label: string, dataChannelOptions?: RTCDataChannelInit) =>
            instrumentChannel(nativeCreateDataChannel(label, dataChannelOptions)),
        },
        setRemoteDescription: {
          configurable: true,
          value: async (description: RTCSessionDescriptionInit) => {
            metrics.remoteDescriptions += 1
            if (options.remoteDescriptionDelayMs > 0) {
              await new Promise<void>((resolve) => {
                window.setTimeout(resolve, options.remoteDescriptionDelayMs)
              })
            }
            await nativeSetRemoteDescription(description)
            metrics.completedRemoteDescriptions += 1
          },
        },
      })
      return peer
    },
  })
  Object.defineProperty(window, 'RTCPeerConnection', {
    configurable: true,
    value: InstrumentedPeerConnection,
  })
}

export async function installM1CPathHarness(
  page: Page,
  options: M1CPathOptions = {},
): Promise<void> {
  await page.addInitScript(pathInstrumentationScript, {
    ...DEFAULT_OPTIONS,
    ...options,
  })
}

export async function m1cPathMetrics(page: Page): Promise<M1CPathMetrics> {
  return await page.evaluate(() => {
    const metrics = window.__windshareM1CPath
    return {
      ...metrics,
      rtcRequests: metrics.rtcRequests.map((indices) => [...indices]),
      rtcCompletedBlocks: [...metrics.rtcCompletedBlocks],
    }
  })
}

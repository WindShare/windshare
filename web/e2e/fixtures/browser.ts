import type { Page } from '@playwright/test'

import { installSocketHarness } from './browser-socket'

export interface BrowserHarnessOptions {
  readonly directoryPicker?: boolean
  readonly disablePickers?: boolean
  readonly reorderFirstBlockPair?: boolean
  readonly pauseBeforeWriteCall?: number
  readonly temporaryRemovalFailures?: number
  readonly rejectPeerConnections?: boolean
}

export interface RequestEvent {
  readonly connection: number
  readonly indices: readonly number[]
}

export interface OutputMetrics {
  readonly writeCalls: number
  readonly completedWriteCalls: number
  readonly totalWrittenBytes: number
  readonly maxWriteBytes: number
  readonly maxConcurrentWrites: number
  readonly pickerCalls: number
  readonly objectUrlsCreated: number
  readonly objectUrlsRevoked: number
  readonly temporaryRemovalCalls: number
  readonly temporaryRemovalFailures: number
  readonly completedPositionalWrites: readonly PositionalWrite[]
  readonly writePaused: boolean
}

export interface PositionalWrite {
  readonly position: number
  readonly size: number
}

export interface BrowserMetrics {
  readonly connections: number
  readonly relayCloseEvents: number
  readonly relayErrorEvents: number
  readonly fragmentPresentAtFirstSocket: boolean | null
  readonly requests: readonly RequestEvent[]
  readonly signalOffers: number
  readonly signalIceCandidates: number
  readonly peerConnections: number
  readonly offerCalls: number
  readonly localIceCandidates: number
  readonly blockArrival: readonly number[]
  readonly completedBlockArrival: readonly number[]
  readonly blockDelivery: readonly number[]
  readonly completedBlockDelivery: readonly number[]
  readonly output: OutputMetrics
}

export interface OpfsEntry {
  readonly path: string
  readonly kind: 'file' | 'directory'
  readonly size?: number
  readonly sha256?: string
}

interface MutableBrowserHarness {
  readonly config: Required<BrowserHarnessOptions>
  readonly metrics: {
    connections: number
    relayCloseEvents: number
    relayErrorEvents: number
    fragmentPresentAtFirstSocket: boolean | null
    requests: RequestEvent[]
    signalOffers: number
    signalIceCandidates: number
    peerConnections: number
    offerCalls: number
    localIceCandidates: number
    blockArrival: number[]
    completedBlockArrival: number[]
    blockDelivery: number[]
    completedBlockDelivery: number[]
    output: {
      writeCalls: number
      completedWriteCalls: number
      totalWrittenBytes: number
      maxWriteBytes: number
      activeWrites: number
      maxConcurrentWrites: number
      pickerCalls: number
      objectUrlsCreated: number
      objectUrlsRevoked: number
      temporaryRemovalCalls: number
      temporaryRemovalFailures: number
      completedPositionalWrites: PositionalWrite[]
      writePaused: boolean
    }
  }
  releaseWrites(): void
  readOpfs(): Promise<OpfsEntry[]>
}

interface BrowserBlockFrame {
  readonly index: number
  readonly last: boolean
}

interface BrowserSocketTools {
  signal(value: unknown): void
  block(value: unknown): BrowserBlockFrame | undefined
  request(value: unknown): number[] | undefined
  invoke(listener: EventListenerOrEventListenerObject, socket: WebSocket, event: Event): void
}

declare global {
  interface Window {
    __windshareE2E: MutableBrowserHarness
    __windshareE2ESocketTools: BrowserSocketTools
  }
}

const DEFAULT_OPTIONS: Required<BrowserHarnessOptions> = Object.freeze({
  directoryPicker: false,
  disablePickers: false,
  reorderFirstBlockPair: false,
  pauseBeforeWriteCall: 0,
  temporaryRemovalFailures: 0,
  rejectPeerConnections: false,
})

export async function installBrowserHarness(
  page: Page,
  options: BrowserHarnessOptions = {},
): Promise<void> {
  const config = Object.freeze({ ...DEFAULT_OPTIONS, ...options })
  // Playwright does not specify ordering between multiple init scripts. The
  // readiness event lets every adapter install before application code regardless
  // of which registered script Chromium evaluates first.
  await page.addInitScript((installedConfig) => {
    const HARNESS_READY_EVENT = 'windshare-e2e-harness-ready'
    Object.defineProperty(window, '__windshareE2E', {
      configurable: false,
      value: {
        config: installedConfig,
        metrics: {
          connections: 0,
          relayCloseEvents: 0,
          relayErrorEvents: 0,
          fragmentPresentAtFirstSocket: null,
          requests: [],
          signalOffers: 0,
          signalIceCandidates: 0,
          peerConnections: 0,
          offerCalls: 0,
          localIceCandidates: 0,
          blockArrival: [],
          completedBlockArrival: [],
          blockDelivery: [],
          completedBlockDelivery: [],
          output: {
            writeCalls: 0,
            completedWriteCalls: 0,
            totalWrittenBytes: 0,
            maxWriteBytes: 0,
            activeWrites: 0,
            maxConcurrentWrites: 0,
            pickerCalls: 0,
            objectUrlsCreated: 0,
            objectUrlsRevoked: 0,
            temporaryRemovalCalls: 0,
            temporaryRemovalFailures: 0,
            completedPositionalWrites: [],
            writePaused: false,
          },
        },
        releaseWrites: () => undefined,
      },
    })
    window.dispatchEvent(new Event(HARNESS_READY_EVENT))
  }, config)
  await installStorageHarness(page)
  await installSocketHarness(page)
  await installPeerHarness(page)
}

function objectUrlHarnessScript(): void {
    const HARNESS_READY_EVENT = 'windshare-e2e-harness-ready'
    if (window.__windshareE2E === undefined) {
      window.addEventListener(HARNESS_READY_EVENT, objectUrlHarnessScript, { once: true })
      return
    }
    const output = window.__windshareE2E.metrics.output
    const nativeCreateObjectUrl = URL.createObjectURL.bind(URL)
    const nativeRevokeObjectUrl = URL.revokeObjectURL.bind(URL)
    Object.defineProperties(URL, {
      createObjectURL: {
        configurable: true,
        value: (object: Blob | MediaSource) => {
          output.objectUrlsCreated += 1
          return nativeCreateObjectUrl(object)
        },
      },
      revokeObjectURL: {
        configurable: true,
        value: (url: string) => {
          output.objectUrlsRevoked += 1
          nativeRevokeObjectUrl(url)
        },
      },
    })
}

function storageHarnessScript(): void {
    const HARNESS_READY_EVENT = 'windshare-e2e-harness-ready'
    if (window.__windshareE2E === undefined) {
      window.addEventListener(HARNESS_READY_EVENT, storageHarnessScript, { once: true })
      return
    }
    const state = window.__windshareE2E
    const storage = navigator.storage
    const nativeGetDirectory = storage.getDirectory.bind(storage)
    const TEMPORARY_OUTPUT_PREFIX = '.windshare-download-'
    let releaseWriteGate: () => void = () => undefined
    const writeGate = new Promise<void>((resolve) => {
      releaseWriteGate = resolve
    })
    state.releaseWrites = () => {
      releaseWriteGate()
      state.metrics.output.writePaused = false
    }

    const writeSize = (value: unknown): number => {
      const candidate =
        typeof value === 'object' && value !== null && 'data' in value
          ? (value as { readonly data?: unknown }).data
          : value
      if (candidate instanceof Blob) return candidate.size
      if (candidate instanceof ArrayBuffer) return candidate.byteLength
      if (ArrayBuffer.isView(candidate)) return candidate.byteLength
      return 0
    }

    const trackWrite = async (
      value: unknown,
      operation: (data: unknown) => Promise<void>,
    ): Promise<void> => {
      const size = writeSize(value)
      const position =
        typeof value === 'object' && value !== null && 'position' in value &&
        typeof (value as { readonly position?: unknown }).position === 'number'
          ? (value as { readonly position: number }).position
          : undefined
      const output = state.metrics.output
      output.writeCalls += 1
      const call = output.writeCalls
      output.totalWrittenBytes += size
      output.maxWriteBytes = Math.max(output.maxWriteBytes, size)
      output.activeWrites += 1
      output.maxConcurrentWrites = Math.max(output.maxConcurrentWrites, output.activeWrites)
      try {
        if (call === state.config.pauseBeforeWriteCall) {
          output.writePaused = true
          await writeGate
        }
        await operation(value)
        output.completedWriteCalls += 1
        if (position !== undefined) {
          output.completedPositionalWrites.push({ position, size })
        }
      } finally {
        output.activeWrites -= 1
      }
    }

    const wrapWriter = (writer: WritableStreamDefaultWriter<unknown>) => ({
      get closed() { return writer.closed },
      get desiredSize() { return writer.desiredSize },
      get ready() { return writer.ready },
      abort: (reason?: unknown) => writer.abort(reason),
      close: () => writer.close(),
      releaseLock: () => writer.releaseLock(),
      write: (value: unknown) => trackWrite(value, writer.write.bind(writer)),
    })

    const wrapWritable = (writable: FileSystemWritableFileStream) => ({
      get locked() { return writable.locked },
      abort: (reason?: unknown) => writable.abort(reason),
      close: () => writable.close(),
      getWriter: () => wrapWriter(writable.getWriter()),
      seek: (position: number) => writable.seek(position),
      truncate: (size: number) => writable.truncate(size),
      write: (value: FileSystemWriteChunkType) =>
        trackWrite(
          value,
          writable.write.bind(writable) as (data: unknown) => Promise<void>,
        ),
    })

    const wrapFile = (handle: FileSystemFileHandle): FileSystemFileHandle => ({
      kind: handle.kind,
      name: handle.name,
      createWritable: async (options?: FileSystemCreateWritableOptions) =>
        wrapWritable(await handle.createWritable(options)) as FileSystemWritableFileStream,
      getFile: () => handle.getFile(),
      isSameEntry: (other) => handle.isSameEntry(other),
    })

    const removeEntry = async (
      handle: FileSystemDirectoryHandle,
      name: string,
      options?: FileSystemRemoveOptions,
    ): Promise<void> => {
      if (name.startsWith(TEMPORARY_OUTPUT_PREFIX)) {
        const output = state.metrics.output
        output.temporaryRemovalCalls += 1
        if (output.temporaryRemovalFailures < state.config.temporaryRemovalFailures) {
          output.temporaryRemovalFailures += 1
          throw new DOMException('Injected temporary removal failure', 'InvalidStateError')
        }
      }
      await handle.removeEntry(name, options)
    }

    const wrapDirectory = (handle: FileSystemDirectoryHandle): FileSystemDirectoryHandle => ({
      kind: handle.kind,
      name: handle.name,
      getDirectoryHandle: async (name, options) =>
        wrapDirectory(await handle.getDirectoryHandle(name, options)),
      getFileHandle: async (name, options) =>
        wrapFile(await handle.getFileHandle(name, options)),
      isSameEntry: (other) => handle.isSameEntry(other),
      removeEntry: (name, options) => removeEntry(handle, name, options),
      resolve: (possibleDescendant) => handle.resolve(possibleDescendant),
      entries: () => handle.entries(),
      keys: () => handle.keys(),
      values: () => handle.values(),
      [Symbol.asyncIterator]: () => handle[Symbol.asyncIterator](),
    })

    Object.defineProperty(storage, 'getDirectory', {
      configurable: true,
      value: async () => wrapDirectory(await nativeGetDirectory()),
    })
    if (state.config.disablePickers) {
      Object.defineProperty(window, 'showDirectoryPicker', { configurable: true, value: undefined })
      Object.defineProperty(window, 'showSaveFilePicker', { configurable: true, value: undefined })
    } else if (state.config.directoryPicker) {
      Object.defineProperty(window, 'showDirectoryPicker', {
        configurable: true,
        value: async () => {
          state.metrics.output.pickerCalls += 1
          return await storage.getDirectory()
        },
      })
    }

}

function opfsReaderHarnessScript(): void {
    const HARNESS_READY_EVENT = 'windshare-e2e-harness-ready'
    if (window.__windshareE2E === undefined) {
      window.addEventListener(HARNESS_READY_EVENT, opfsReaderHarnessScript, { once: true })
      return
    }
    const state = window.__windshareE2E
    const getDirectory = navigator.storage.getDirectory.bind(navigator.storage)
    const encodeHex = (bytes: Uint8Array): string => {
      let encoded = ''
      for (const byte of bytes) encoded += byte.toString(16).padStart(2, '0')
      return encoded
    }
    state.readOpfs = async () => {
      const result: OpfsEntry[] = []
      const visit = async (directory: FileSystemDirectoryHandle, prefix: string): Promise<void> => {
        for await (const [name, handle] of directory.entries()) {
          const path = prefix === '' ? name : `${prefix}/${name}`
          if (handle.kind === 'directory') {
            result.push({ path, kind: 'directory' })
            await visit(handle, path)
          } else {
            const file = await handle.getFile()
            const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', await file.arrayBuffer()))
            result.push({
              path,
              kind: 'file',
              size: file.size,
              sha256: encodeHex(digest),
            })
          }
        }
      }
      await visit(await getDirectory(), '')
      return result.sort((left, right) => left.path.localeCompare(right.path))
    }
}

async function installStorageHarness(page: Page): Promise<void> {
  await page.addInitScript(objectUrlHarnessScript)
  await page.addInitScript(storageHarnessScript)
  await page.addInitScript(opfsReaderHarnessScript)
}

function peerHarnessScript(): void {
    const HARNESS_READY_EVENT = 'windshare-e2e-harness-ready'
    if (window.__windshareE2E === undefined) {
      window.addEventListener(HARNESS_READY_EVENT, peerHarnessScript, { once: true })
      return
    }
    const state = window.__windshareE2E
    const NativePeerConnection = window.RTCPeerConnection
    if (NativePeerConnection === undefined) return
    const InstrumentedPeerConnection = new Proxy(NativePeerConnection, {
      construct(target, args, newTarget) {
        state.metrics.peerConnections += 1
        if (state.config.rejectPeerConnections) {
          // M1b is specifically the relay acceptance lane. Throwing at the native
          // capability boundary keeps an attempted pre-gesture connection visible
          // while preventing concurrent M1c policy from carrying C5's payload.
          throw new DOMException('Peer connections disabled by M1b E2E', 'NotSupportedError')
        }
        const peer = Reflect.construct(target, args, newTarget) as RTCPeerConnection
        const createOffer = peer.createOffer
        Object.defineProperty(peer, 'createOffer', {
          configurable: true,
          value: (...offerArgs: unknown[]) => {
            state.metrics.offerCalls += 1
            return Reflect.apply(createOffer, peer, offerArgs)
          },
        })
        peer.addEventListener('icecandidate', (event) => {
          if (event.candidate !== null) state.metrics.localIceCandidates += 1
        })
        return peer
      },
    })
    Object.defineProperty(window, 'RTCPeerConnection', {
      configurable: true,
      value: InstrumentedPeerConnection,
    })
}

async function installPeerHarness(page: Page): Promise<void> {
  await page.addInitScript(peerHarnessScript)
}

export async function browserMetrics(page: Page): Promise<BrowserMetrics> {
  return await page.evaluate(() => {
    const metrics = window.__windshareE2E.metrics
    return {
      ...metrics,
      requests: metrics.requests.map((event) => ({ ...event, indices: [...event.indices] })),
      blockArrival: [...metrics.blockArrival],
      completedBlockArrival: [...metrics.completedBlockArrival],
      blockDelivery: [...metrics.blockDelivery],
      completedBlockDelivery: [...metrics.completedBlockDelivery],
      output: {
        ...metrics.output,
        completedPositionalWrites: metrics.output.completedPositionalWrites.map((write) => ({
          ...write,
        })),
      },
    }
  })
}

export async function readOpfs(page: Page): Promise<readonly OpfsEntry[]> {
  return await page.evaluate(() => window.__windshareE2E.readOpfs())
}

export async function releaseBrowserWrites(page: Page): Promise<void> {
  await page.evaluate(() => window.__windshareE2E.releaseWrites())
}

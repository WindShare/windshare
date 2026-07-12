import { describe, expect, it, vi } from 'vitest'

import type { CapabilityLink, ValidatedManifestV1 } from '../../src/contracts'
import {
  SMALL_SHARE_BYTES,
  type ConnectivityClock,
  type ConnectivitySignal,
  type OfferChannelFactory,
  type PeerChannel,
  type SignalingRoute,
} from '../../src/connectivity'
import { PackedLayout, compileTransferPlan, createSelection } from '../../src/manifest'
import {
  BrowserReceiverGateway,
  type BrowserGatewayRuntime,
} from '../../src/ui/browser-gateway'
import {
  prepareBrowserOutput,
  type BrowserOutputRuntime,
} from '../../src/ui/browser-output'
import type { JoinedShare, ReceiverTransferObserver } from '../../src/ui/model'
import { MockFrameChannel } from '../session/helpers'

const LARGE_FILE_BYTES = SMALL_SHARE_BYTES + 1
const TEMPORARY_NAME = '.windshare-download-ownership'
const LARGE_MANIFEST = Object.freeze({
  version: 1,
  chunkSize: 1024,
  entries: Object.freeze([
    Object.freeze({ kind: 'file', path: 'large.bin', size: LARGE_FILE_BYTES, mtime: 0 }),
  ]),
}) as unknown as ValidatedManifestV1

describe('browser gateway output ownership', () => {
  it('settles production output when joined-share validation fails after preparation', async () => {
    const output = outputProbe()
    const offers = { offer: vi.fn() } as unknown as OfferChannelFactory
    const clock = new DeferredClock()
    const gateway = new BrowserReceiverGateway(
      (plan, choice) => prepareBrowserOutput(plan, choice, output.runtime),
      [],
      {
        dialReceiver: vi.fn(),
        openManifest: vi.fn(),
        offerFactory: offers,
        connectivityClock: clock,
      } as unknown as BrowserGatewayRuntime,
    )
    const plan = await compileTransferPlan(
      new PackedLayout(LARGE_MANIFEST),
      createSelection(null),
    )
    const close = vi.fn(async () => undefined)
    const foreignShare = { manifest: LARGE_MANIFEST, close } as JoinedShare
    const transferObserver = observer()
    const interval = vi.spyOn(globalThis, 'setInterval')

    try {
      await expect(
        gateway.start(
          foreignShare,
          plan,
          'download',
          transferObserver,
          new AbortController().signal,
        ),
      ).rejects.toMatchObject({ code: 'transfer-failed' })
    } finally {
      interval.mockRestore()
    }

    expect(output.abortReasons).toHaveLength(1)
    expect(output.abortReasons[0]).toMatchObject({
      name: 'TypeError',
      message: 'Browser gateway received a foreign joined-share capability',
    })
    expect(output.stream.locked).toBe(false)
    expect(output.removeEntry).toHaveBeenCalledOnce()
    expect(output.removeEntry).toHaveBeenCalledWith(TEMPORARY_NAME)
    expect(close).not.toHaveBeenCalled()
    expect(offers.offer).not.toHaveBeenCalled()
    expect(clock.pending).toBe(false)
    expect(transferObserver.started).not.toHaveBeenCalled()
    expect(transferObserver.progress).not.toHaveBeenCalled()
    expect(interval).not.toHaveBeenCalled()
  })

  it('cancels a locked stream during the large-transfer policy wait', async () => {
    const clock = new DeferredClock()
    const offers = new PendingOfferFactory()
    const output = outputProbe()
    const transfer = await preparedTransfer(offers, clock, output)
    const cancellation = new DOMException('stop before policy settles', 'AbortError')

    await Promise.all([clock.started.promise, offers.started.promise])
    expect(output.stream.locked).toBe(true)
    transfer.controller.abort(cancellation)

    await expect(transfer.completion).rejects.toBe(cancellation)
    expect(output.abortReasons).toEqual([cancellation])
    expect(output.stream.locked).toBe(false)
    expect(output.removeEntry).toHaveBeenCalledOnce()
    expect(output.removeEntry).toHaveBeenCalledWith(TEMPORARY_NAME)
    expect(offers.abortReasons).toEqual([cancellation])
    expect(clock.pending).toBe(false)
    expect(transfer.observer.started).not.toHaveBeenCalled()
    expect(transfer.connection.channel.frames.locked).toBe(false)
    expect(transfer.connection.closeCalls).toBe(1)
  })

  it('settles the same locked resources when policy waiting fails', async () => {
    const clock = new DeferredClock()
    const offers = new PendingOfferFactory()
    const output = outputProbe()
    const transfer = await preparedTransfer(offers, clock, output)
    const policyFailure = new Error('policy clock failed')

    await Promise.all([clock.started.promise, offers.started.promise])
    expect(output.stream.locked).toBe(true)
    clock.fail(policyFailure)

    await expect(transfer.completion).rejects.toMatchObject({ code: 'transfer-failed' })
    expect(output.abortReasons).toEqual([policyFailure])
    expect(output.stream.locked).toBe(false)
    expect(output.removeEntry).toHaveBeenCalledOnce()
    expect(offers.abortReasons).toEqual([policyFailure])
    expect(clock.pending).toBe(false)
    expect(transfer.observer.started).not.toHaveBeenCalled()
    expect(transfer.connection.channel.frames.locked).toBe(false)
  })

  it('hands an observer-triggered boundary cancellation to ReceiveSession once', async () => {
    const clock = new DeferredClock()
    const peer = new PolicyPeerChannel()
    const offers = new ImmediateOfferFactory(peer)
    const output = outputProbe()
    const cancellation = new DOMException('stop at session transfer', 'AbortError')
    const transfer = await preparedTransfer(offers, clock, output, (controller) => ({
      ...observer(),
      started: vi.fn(() => controller.abort(cancellation)),
    }))

    await expect(transfer.completion).rejects.toBe(cancellation)
    expect(transfer.observer.started).toHaveBeenCalledOnce()
    expect(output.abortReasons).toEqual([cancellation])
    expect(output.stream.locked).toBe(false)
    expect(output.removeEntry).toHaveBeenCalledOnce()
    expect(peer.frames.locked).toBe(false)
    expect(transfer.connection.channel.frames.locked).toBe(false)
    expect(clock.pending).toBe(false)
  })

  it('releases the writer but does not report a clean abort when session cleanup fails', async () => {
    const cleanupFailure = new Error('underlying stream abort failed')
    const clock = new DeferredClock()
    const peer = new PolicyPeerChannel()
    const output = outputProbe(cleanupFailure)
    const cancellation = new DOMException('stop after transfer', 'AbortError')
    const transfer = await preparedTransfer(
      new ImmediateOfferFactory(peer),
      clock,
      output,
      (controller) => ({
        ...observer(),
        started: vi.fn(() => controller.abort(cancellation)),
      }),
    )

    await expect(transfer.completion).rejects.toMatchObject({ code: 'transfer-failed' })
    expect(output.abortReasons).toEqual([cancellation])
    expect(output.stream.locked).toBe(false)
    expect(output.removeEntry).toHaveBeenCalledOnce()
    expect(peer.frames.locked).toBe(false)
  })
})

interface OutputProbe {
  readonly runtime: BrowserOutputRuntime
  readonly stream: WritableStream<Uint8Array>
  readonly abortReasons: unknown[]
  readonly removeEntry: ReturnType<typeof vi.fn>
}

function outputProbe(abortFailure?: unknown): OutputProbe {
  const abortReasons: unknown[] = []
  const stream = new WritableStream<Uint8Array>({
    abort: (reason) => {
      abortReasons.push(reason)
      if (abortFailure !== undefined) {
        throw abortFailure
      }
    },
  })
  const handle = {
    createWritable: () => Promise.resolve(stream),
  } as unknown as FileSystemFileHandle
  const removeEntry = vi.fn(async () => undefined)
  const root = {
    getFileHandle: (_name: string, options?: { readonly create?: boolean }) =>
      options?.create === true
        ? Promise.resolve(handle)
        : Promise.reject(new DOMException('missing', 'NotFoundError')),
    removeEntry,
  } as unknown as FileSystemDirectoryHandle
  return {
    stream,
    abortReasons,
    removeEntry,
    runtime: {
      browserWindow: {},
      storage: { getDirectory: () => Promise.resolve(root) },
      randomId: () => 'ownership',
      present: () => undefined,
    } as unknown as BrowserOutputRuntime,
  }
}

interface PreparedTransfer {
  readonly completion: Promise<void>
  readonly connection: FakeRelayConnection
  readonly controller: AbortController
  readonly observer: ReceiverTransferObserver
}

async function preparedTransfer(
  offerFactory: OfferChannelFactory,
  clock: ConnectivityClock,
  output: OutputProbe,
  createObserver: (controller: AbortController) => ReceiverTransferObserver = observer,
): Promise<PreparedTransfer> {
  const connection = new FakeRelayConnection()
  const runtime = {
    dialReceiver: async () => connection,
    openManifest: async () => ({
      manifest: LARGE_MANIFEST,
      fingerprint: new Uint8Array(32),
    }),
    offerFactory,
    connectivityClock: clock,
  } as unknown as BrowserGatewayRuntime
  const gateway = new BrowserReceiverGateway(
    (plan, choice) => prepareBrowserOutput(plan, choice, output.runtime),
    [],
    runtime,
  )
  const controller = new AbortController()
  const joined = await gateway.join(capability(), controller.signal)
  const plan = await gateway.compileSelection(joined, null, controller.signal)
  expect(plan.selectedBytes).toBe(LARGE_FILE_BYTES)
  const transferObserver = createObserver(controller)
  return {
    connection,
    controller,
    observer: transferObserver,
    completion: gateway.start(joined, plan, 'download', transferObserver, controller.signal),
  }
}

function observer(): ReceiverTransferObserver {
  return {
    started: vi.fn(),
    progress: vi.fn(),
    reconnecting: vi.fn(),
    reconnected: vi.fn(),
  }
}

function capability(): CapabilityLink {
  return {
    suite: 1,
    shareId: 'AAECAwQFBgcI',
    readSecret: new Uint8Array(16),
    relayHints: ['https://relay.test'],
  } as unknown as CapabilityLink
}

class PendingOfferFactory implements OfferChannelFactory {
  readonly started = deferred<void>()
  readonly abortReasons: unknown[] = []

  offer(_route: SignalingRoute, signal: AbortSignal): Promise<PeerChannel> {
    this.started.resolve(undefined)
    return new Promise((_resolve, reject) => {
      const abort = () => {
        this.abortReasons.push(signal.reason)
        reject(signal.reason)
      }
      if (signal.aborted) {
        abort()
      } else {
        signal.addEventListener('abort', abort, { once: true })
      }
    })
  }
}

class ImmediateOfferFactory implements OfferChannelFactory {
  readonly #peer: PeerChannel

  constructor(peer: PeerChannel) {
    this.#peer = peer
  }

  offer(): Promise<PeerChannel> {
    return Promise.resolve(this.#peer)
  }
}

class DeferredClock implements ConnectivityClock {
  readonly started = deferred<void>()
  #pending: Deferred<void> | undefined
  #signal: AbortSignal | undefined

  get pending(): boolean {
    return this.#pending !== undefined
  }

  sleep(_milliseconds: number, signal?: AbortSignal): Promise<void> {
    this.started.resolve(undefined)
    this.#pending = deferred<void>()
    this.#signal = signal
    if (signal?.aborted) {
      this.#abort()
    } else {
      signal?.addEventListener('abort', this.#abort, { once: true })
    }
    return this.#pending?.promise ?? Promise.reject(signal?.reason)
  }

  fail(reason: unknown): void {
    const pending = this.#takePending()
    pending?.reject(reason)
  }

  #abort = (): void => {
    const reason = this.#signal?.reason
    const pending = this.#takePending()
    pending?.reject(reason)
  }

  #takePending(): Deferred<void> | undefined {
    const pending = this.#pending
    this.#signal?.removeEventListener('abort', this.#abort)
    this.#pending = undefined
    this.#signal = undefined
    return pending
  }
}

class PolicyPeerChannel extends MockFrameChannel implements PeerChannel {
  readonly opened = Promise.resolve()
  readonly done: Promise<void>
  reason: unknown
  #finish!: () => void

  constructor() {
    super()
    this.done = new Promise((resolve) => {
      this.#finish = resolve
    })
  }

  override async close(): Promise<void> {
    await super.close()
    this.#finish()
  }
}

class FakeRelayConnection {
  readonly channel = new PolicyRelayChannel()
  readonly sealedManifest = new Uint8Array()
  readonly done: Promise<void>
  closeCalls = 0
  #finish!: () => void

  constructor() {
    this.done = new Promise((resolve) => {
      this.#finish = resolve
    })
  }

  rejoin(): Promise<FakeRelayConnection> {
    return Promise.reject(new Error('rejoin is not expected'))
  }

  async close(): Promise<void> {
    this.closeCalls += 1
    await this.channel.close()
    this.#finish()
  }
}

class PolicyRelayChannel extends MockFrameChannel {
  readonly signalMessages = new ReadableStream<ConnectivitySignal>()

  sendSignal(_kind: string, _payload: unknown, signal?: AbortSignal): Promise<void> {
    signal?.throwIfAborted()
    return Promise.resolve()
  }
}

interface Deferred<T> {
  readonly promise: Promise<T>
  readonly resolve: (value: T) => void
  readonly reject: (reason: unknown) => void
}

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((accept, fail) => {
    resolve = accept
    reject = fail
  })
  return { promise, resolve, reject }
}

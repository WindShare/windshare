import { describe, expect, it, vi } from 'vitest'
import type { JoinedBrowserShare } from '../../src/application/browser-share/gateway'
import { ReceiveCoordinator, type ReceiveWindowPort } from '../../src/application/receive/coordinator'
import {
  ScreenWakeLockGuard,
  type ScreenWakeLockSentinelLike,
  type ScreenWakeLockWindowTarget,
} from '../../src/application/receive/wake-lock-guard'
import { SelectionPolicy } from '../../src/catalog/selection'
import { encodeBase64Url } from '../../src/crypto/bytes'
import type { TransferJobResult } from '../../src/transfer/job'

interface FakeSentinel extends ScreenWakeLockSentinelLike {
  released: boolean
  readonly release: ReturnType<typeof vi.fn<() => Promise<void>>>
  emitExternalRelease(): void
}

function createFakeSentinel(): FakeSentinel {
  const listeners = new Set<() => void>()
  const sentinel: FakeSentinel = {
    released: false,
    release: vi.fn(async () => {
      if (sentinel.released) return
      sentinel.released = true
      for (const listener of [...listeners]) listener()
    }),
    addEventListener: (_type, listener) => { listeners.add(listener) },
    removeEventListener: (_type, listener) => { listeners.delete(listener) },
    emitExternalRelease: () => {
      sentinel.released = true
      for (const listener of [...listeners]) listener()
    },
  }
  return sentinel
}

function createFakeWakeLockTarget(options: { readonly initialVisibility?: DocumentVisibilityState } = {}) {
  let visibilityState: DocumentVisibilityState = options.initialVisibility ?? 'visible'
  const visibilityListeners = new Set<() => void>()
  const sentinels: FakeSentinel[] = []
  const pendingRequests: Array<{
    readonly resolve: (sentinel: FakeSentinel) => void
    readonly reject: (reason: unknown) => void
  }> = []
  let deferNextRequest = false

  const request = vi.fn(() => {
    if (deferNextRequest) {
      deferNextRequest = false
      return new Promise<ScreenWakeLockSentinelLike>((resolve, reject) => {
        pendingRequests.push({ resolve, reject })
      })
    }
    const sentinel = createFakeSentinel()
    sentinels.push(sentinel)
    return Promise.resolve(sentinel)
  })

  const documentTarget = {
    get visibilityState() { return visibilityState },
    addEventListener: vi.fn((_type: 'visibilitychange', listener: () => void) => {
      visibilityListeners.add(listener)
    }),
    removeEventListener: vi.fn((_type: 'visibilitychange', listener: () => void) => {
      visibilityListeners.delete(listener)
    }),
  }

  const target: ScreenWakeLockWindowTarget = {
    navigator: { wakeLock: { request } },
    document: documentTarget,
  }

  const setVisibility = (next: DocumentVisibilityState) => {
    visibilityState = next
    for (const listener of [...visibilityListeners]) listener()
  }

  const deferOnce = () => { deferNextRequest = true }

  return {
    target,
    request,
    sentinels,
    pendingRequests,
    visibilityListeners,
    documentTarget,
    setVisibility,
    deferOnce,
  }
}

describe('ScreenWakeLockGuard', () => {
  it('acquires on arm, releases on disarm, and re-acquires when returning to visibility', async () => {
    const fake = createFakeWakeLockTarget()
    const guard = new ScreenWakeLockGuard(fake.target)

    expect(guard.armed).toBe(false)
    expect(fake.request).not.toHaveBeenCalled()
    expect(fake.visibilityListeners.size).toBe(0)

    guard.setArmed(true)
    guard.setArmed(true)
    await vi.waitFor(() => expect(fake.sentinels).toHaveLength(1))
    expect(fake.request).toHaveBeenCalledTimes(1)
    expect(fake.request).toHaveBeenCalledWith('screen')
    expect(fake.visibilityListeners.size).toBe(1)

    // Simulating tab hidden releases the sentinel; returning visible re-acquires while armed.
    fake.sentinels[0]!.emitExternalRelease()
    fake.setVisibility('hidden')
    expect(fake.request).toHaveBeenCalledTimes(1)

    fake.setVisibility('visible')
    await vi.waitFor(() => expect(fake.sentinels).toHaveLength(2))
    expect(fake.request).toHaveBeenCalledTimes(2)

    guard.setArmed(false)
    guard.setArmed(false)
    expect(guard.armed).toBe(false)
    expect(fake.sentinels[1]!.release).toHaveBeenCalledTimes(1)
    expect(fake.visibilityListeners.size).toBe(0)
  })

  it('defers acquisition while hidden and releases an in-flight sentinel if disarmed or disposed first', async () => {
    const fake = createFakeWakeLockTarget({ initialVisibility: 'hidden' })
    const guard = new ScreenWakeLockGuard(fake.target)

    guard.setArmed(true)
    expect(fake.request).not.toHaveBeenCalled()

    fake.deferOnce()
    fake.setVisibility('visible')
    expect(fake.request).toHaveBeenCalledTimes(1)
    expect(fake.pendingRequests).toHaveLength(1)

    // Disarm before the pending wakeLock.request resolves -> resolved sentinel must be released immediately.
    guard.setArmed(false)
    const lateSentinel = createFakeSentinel()
    fake.pendingRequests[0]!.resolve(lateSentinel)
    await vi.waitFor(() => expect(lateSentinel.release).toHaveBeenCalledTimes(1))

    // Dispose ignores later arm attempts.
    guard.dispose()
    guard.setArmed(true)
    expect(guard.armed).toBe(false)
    expect(fake.request).toHaveBeenCalledTimes(1)
  })

  it('degrades silently when wakeLock is absent, throws synchronously, or rejects asynchronously', async () => {
    const unsupported = new ScreenWakeLockGuard(undefined)
    unsupported.setArmed(true)
    expect(unsupported.armed).toBe(false)
    unsupported.dispose()

    const throwingGuard = new ScreenWakeLockGuard({
      navigator: {
        wakeLock: {
          request: () => { throw new DOMException('Denied', 'NotAllowedError') },
        },
      },
    })
    expect(() => throwingGuard.setArmed(true)).not.toThrow()
    throwingGuard.dispose()

    let attempts = 0
    const fakeDoc = createFakeWakeLockTarget()
    const rejectingGuard = new ScreenWakeLockGuard({
      navigator: {
        wakeLock: {
          request: vi.fn(async () => {
            attempts += 1
            throw new DOMException('Low battery', 'NotAllowedError')
          }),
        },
      },
      document: fakeDoc.documentTarget,
    })
    rejectingGuard.setArmed(true)
    await vi.waitFor(() => expect(attempts).toBe(1))
    // After a failed request settles, returning visible can retry cleanly.
    fakeDoc.setVisibility('visible')
    await vi.waitFor(() => expect(attempts).toBe(2))
    rejectingGuard.dispose()
  })

  it('holds screen wake lock only during active ReceiveCoordinator download sessions', async () => {
    const fileId = new Uint8Array(16).fill(11)
    const fileIdText = encodeBase64Url(fileId)
    const fake = createFakeWakeLockTarget()
    let resolvePicker!: (handle: FileSystemDirectoryHandle) => void
    const windowPort: ReceiveWindowPort = {
      URL: { createObjectURL: vi.fn(() => 'blob:wakelock'), revokeObjectURL: vi.fn() },
      document: Object.assign(fake.documentTarget, {
        createElement: vi.fn(() => ({ click: vi.fn(), remove: vi.fn(), style: {} })),
        body: { append: vi.fn() },
      }) as unknown as Document,
      navigator: { wakeLock: { request: fake.request } },
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
      showDirectoryPicker: vi.fn(() => new Promise<FileSystemDirectoryHandle>(resolve => { resolvePicker = resolve })),
    }

    let resolveJob!: (value: TransferJobResult) => void
    let jobCalls = 0
    const joined = {
      descriptor: { shareInstanceId: 'share-wl', syntheticRootId: 'root-wl' },
      selection: new SelectionPolicy(true),
      inspectSelection: async () => ({
        kind: 'single-file' as const,
        file: { kind: 'file' as const, id: fileId, idText: fileIdText, name: 'movie.mp4', expectedSize: 32n },
        sourcePath: ['movie.mp4'],
        exactBytes: 32n,
      }),
      beginDownloadConnectivity: () => ({ routes: [], close: vi.fn() }),
      createTransferJob: () => {
        jobCalls += 1
        return {
          pause: vi.fn(),
          stop: vi.fn(),
          detach: vi.fn(),
          run: () => new Promise(resolve => { resolveJob = resolve }),
        }
      },
    } as unknown as JoinedBrowserShare

    const coordinator = new ReceiveCoordinator({
      windowPort,
      currentJoinedShare: () => joined,
      onActionError: vi.fn(),
    })
    coordinator.refreshOffers(joined)
    await vi.waitFor(() => expect(coordinator.getSnapshot().offers?.kind).toBe('ready'))

    const offers = coordinator.getSnapshot().offers
    if (offers?.kind !== 'ready') throw new Error('expected ready offers')
    const fsaChoice = [offers.primary, ...offers.alternatives].find(choice => choice.backend === 'fsa')
    expect(coordinator.startDownload(fsaChoice!.choiceId)).toBe(true)
    // Waiting on the OS folder picker must not acquire a wake lock yet.
    expect(fake.request).not.toHaveBeenCalled()
    coordinator.cancelActivation()
    resolvePicker({} as FileSystemDirectoryHandle)

    // Active download acquires wake lock and releases it on settlement.
    const portableChoice = [offers.primary, ...offers.alternatives].find(choice => choice.backend === 'portable')
    expect(coordinator.startDownload(portableChoice!.choiceId)).toBe(true)
    await vi.waitFor(() => expect(jobCalls).toBe(1))
    await vi.waitFor(() => expect(fake.sentinels).toHaveLength(1))
    expect(fake.sentinels[0]!.released).toBe(false)

    resolveJob({
      status: 'completed',
      completedFiles: 1n,
      completedBytes: 32n,
      discoveredFiles: 1n,
      discoveredBytes: 32n,
      discoveryComplete: true,
      failedFiles: 0n,
      failedDirectories: 0n,
    })
    await vi.waitFor(() => expect(coordinator.getSnapshot().lifecycle?.kind).toBe('download-started'))
    expect(fake.sentinels[0]!.release).toHaveBeenCalledTimes(1)
    expect(fake.visibilityListeners.size).toBe(0)

    // Disposing mid-transfer also releases the active wake lock immediately.
    coordinator.startNewDownload()
    expect(coordinator.startDownload(portableChoice!.choiceId)).toBe(true)
    await vi.waitFor(() => expect(jobCalls).toBe(2))
    await vi.waitFor(() => expect(fake.sentinels).toHaveLength(2))
    const disposed = coordinator.dispose()
    expect(fake.sentinels[1]!.release).toHaveBeenCalledTimes(1)
    expect(fake.visibilityListeners.size).toBe(0)
    resolveJob({
      status: 'detached', completedFiles: 0n, completedBytes: 0n, discoveredFiles: 1n, discoveredBytes: 32n,
      discoveryComplete: true, failedFiles: 0n, failedDirectories: 0n,
    })
    await disposed
  })
})

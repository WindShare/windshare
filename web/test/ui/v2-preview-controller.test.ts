import { afterEach, describe, expect, it, vi } from 'vitest'
import { SelectionPolicy } from '../../src/catalog/selection'
import type { ReceiveWindowPort } from '../../src/application/receive/coordinator'
import * as outputContract from '../../src/output/backends/contract'
import { ReceiveStore } from '../../src/output/store/database'
import { AutomaticPhotoDeferredError } from '../../src/preview/automatic-photo'
import { ReceiverController } from '../../src/ui/controller'
import { PreviewController } from '../../src/ui/preview-controller'
import { dismissedTaskKey, resolveWorkspaceTaskLayout } from '../../src/ui/experience/retained-match'
import { EMPTY_PREVIEW, type ReceiverFailure } from '../../src/ui/model'
import type { BrowseDirectory, BrowserReceiverGateway, JoinedBrowserShare } from '../../src/application/browser-share/gateway'
import { createMemoryOPFSEnv, createMockDownloadWindow, MemoryReceiveStore } from '../output/opfs-fixture'
import {
  deferred,
  entry,
  FakeJoined,
  identity,
  identityText,
  rootDirectory,
  turn,
  waitFor,
  waitForController,
  directPreviewSnapshot,
  DiagnosticPreviewJoined,
} from './v2-preview-fixture'

afterEach(() => vi.unstubAllGlobals())

describe('automatic photo ownership', () => {
  it('does not fail catalog publication when speculative connectivity admission races replacement', () => {
    const joined = new FakeJoined()
    joined.beginPreviewConnectivity = () => { throw new DOMException('Generation replaced', 'AbortError') }
    let snapshot = directPreviewSnapshot()
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: next => { snapshot = next },
      publicError: () => 'preview failed',
    })
    expect(() => previews.openAutomaticPhoto(joined as unknown as JoinedBrowserShare, entry, rootDirectory)).not.toThrow()
    expect(snapshot.preview).toEqual(EMPTY_PREVIEW)
  })

  it('yields pending speculation synchronously and leaves an explicit preview usable', async () => {
    const joined = new FakeJoined()
    const automaticPhotoPreview = joined.preview.bind(joined)
    const source = Object.assign(joined, { automaticPhotoPreview }) as unknown as JoinedBrowserShare
    let snapshot = directPreviewSnapshot()
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: next => { snapshot = next },
      publicError: () => 'preview failed',
    })
    previews.openAutomaticPhoto(source, entry, rootDirectory)
    expect(snapshot.preview.state).toBe('loading')
    previews.yieldToReceiving()
    expect(joined.previewSignals[0]?.aborted).toBe(true)
    expect(snapshot.preview).toEqual(EMPTY_PREVIEW)
    previews.open(source, entry, rootDirectory)
    previews.yieldToReceiving()
    await turn()
    expect(snapshot.preview.state).toBe('image')
    expect(joined.previewSignals[1]?.aborted).toBe(false)
    await previews.close()
  })

  it('retains decoded automatic photo while receiving and closes it on explicit dismissal', async () => {
    const joined = new FakeJoined()
    joined.previewCount = 1
    const source = Object.assign(joined, { automaticPhotoPreview: joined.preview.bind(joined) }) as unknown as JoinedBrowserShare
    let snapshot = directPreviewSnapshot()
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: next => { snapshot = next },
      publicError: () => 'preview failed',
    })
    previews.openAutomaticPhoto(source, entry, rootDirectory)
    await turn()
    expect(snapshot.preview.state).toBe('image')
    previews.yieldToReceiving()
    expect(snapshot.preview.state).toBe('image')
    expect(joined.sessionCloses).toBe(0)
    previews.cancel()
    await turn()
    expect(joined.sessionCloses).toBe(1)
  })

  it('returns to explicit Preview when automatic header inspection exceeds its budget', async () => {
    const joined = new FakeJoined()
    const source = Object.assign(joined, {
      automaticPhotoPreview: () => Promise.reject(new AutomaticPhotoDeferredError('Use Preview for this larger image')),
    }) as unknown as JoinedBrowserShare
    let snapshot = directPreviewSnapshot()
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: next => { snapshot = next },
      publicError: () => 'preview failed',
    })
    previews.openAutomaticPhoto(source, entry, rootDirectory)
    await turn()
    expect(snapshot.preview).toEqual(EMPTY_PREVIEW)
    expect(joined.events).toEqual(['begin-preview-connectivity', 'close-preview-connectivity'])
  })

  it('bounds stalled speculation and does not activate an oversized candidate', async () => {
    vi.useFakeTimers()
    try {
      const joined = new FakeJoined()
      const source = Object.assign(joined, { automaticPhotoPreview: joined.preview.bind(joined) }) as unknown as JoinedBrowserShare
      let snapshot = directPreviewSnapshot()
      const previews = new PreviewController({
        snapshot: () => snapshot,
        publish: next => { snapshot = next },
        publicError: () => 'preview failed',
      })
      previews.openAutomaticPhoto(source, { ...entry, expectedSize: 5n * 1024n * 1024n }, rootDirectory)
      expect(joined.events).toEqual([])
      previews.openAutomaticPhoto(source, entry, rootDirectory)
      await vi.advanceTimersByTimeAsync(5_000)
      expect(snapshot.preview).toEqual(EMPTY_PREVIEW)
      expect(joined.previewSignals[0]?.aborted).toBe(true)
      await previews.close()
    } finally {
      vi.useRealTimers()
    }
  })
})

describe('v2 preview click controller boundary', () => {
  it('starts each explicit preview immediately, replaces the old preview, and never opens an output picker', async () => {
    const showSaveFilePicker = vi.fn()
    vi.stubGlobal('window', {
      navigator: { storage: {} },
      showSaveFilePicker,
    })
    const joined = new FakeJoined({ ...entry, name: 'photo.bin' })
    const gateway = {
      join: async () => joined as unknown as JoinedBrowserShare,
    } as unknown as BrowserReceiverGateway
    const controller = new ReceiverController(gateway)
    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() => controller.getSnapshot().phase === 'browsing')
    expect(controller.getSnapshot().phase).toBe('browsing')

    controller.previewFile('stale-row')
    expect(joined.events).toEqual([])

    controller.previewFile(entry.idText)
    expect(joined.events.slice(0, 2)).toEqual(['begin-preview-connectivity', 'open-preview'])
    expect(controller.getSnapshot().preview.state).toBe('loading')
    controller.previewFile(entry.idText)
    expect(joined.previewSignals[0]?.aborted).toBe(true)
    await turn()
    expect(controller.getSnapshot().preview).toMatchObject({
      state: 'image',
      url: 'blob:second',
    })
    expect(showSaveFilePicker).not.toHaveBeenCalled()
    controller.previewMediaFailed(Number.MAX_SAFE_INTEGER)
    expect(controller.getSnapshot().preview.state).toBe('image')
    expect(joined.sessionCloses).toBe(0)

    controller.cancelPreview()
    expect(joined.events.slice(-2)).toEqual([
      'close-preview-connectivity',
      'close-preview-session',
    ])
    await turn()
    expect(controller.getSnapshot().preview.state).toBe('idle')
    expect(joined.sessionCloses).toBe(1)
    await controller.dispose()
  })

  it('shows authenticated scan milestones without inventing an exact total', async () => {
    vi.stubGlobal('window', { navigator: { storage: {} } })
    const joined = new FakeJoined()
    let releasePage!: () => void
    joined.progressOnPage = 257n
    joined.pageGate = new Promise<void>((resolve) => { releasePage = resolve })
    const gateway = {
      join: async () => joined as unknown as JoinedBrowserShare,
    } as unknown as BrowserReceiverGateway
    const controller = new ReceiverController(gateway)
    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() => {
      const browse = controller.getSnapshot().browse
      return browse.kind === 'loading' && browse.discoveredEntries === 257n
    })
    expect(controller.getSnapshot().entryCount).toBe(0)
    releasePage()
    await turn()
    expect(controller.getSnapshot().phase).toBe('browsing')
    await controller.dispose()
  })

  it('closes a stale join that resolves after a newer receiver session owns the UI', async () => {
    vi.stubGlobal('window', {
      navigator: { storage: {} },
      showSaveFilePicker: () => Promise.reject(new DOMException('unused', 'AbortError')),
    })
    const first = new FakeJoined({ ...entry, idText: 'first', name: 'first.png' })
    const second = new FakeJoined({ ...entry, idText: 'second', name: 'second.png' })
    const pending: Array<(joined: JoinedBrowserShare) => void> = []
    const gateway = {
      join: () => new Promise<JoinedBrowserShare>((resolve) => pending.push(resolve)),
    } as unknown as BrowserReceiverGateway
    const controller = new ReceiverController(gateway)

    controller.initialize({ capabilityInput: 'first-key', pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() => pending.length === 1)
    controller.submitKey('second-key')
    await waitFor(() => pending.length === 2)
    expect(pending).toHaveLength(2)

    pending[1]?.(second as unknown as JoinedBrowserShare)
    await waitFor(() => controller.getSnapshot().rows[0]?.name === 'second.png')
    expect(controller.getSnapshot().rows[0]?.name).toBe('second.png')
    pending[0]?.(first as unknown as JoinedBrowserShare)
    await waitFor(() => first.closes === 1)
    expect(first.closes).toBe(1)
    expect(controller.getSnapshot().rows[0]?.name).toBe('second.png')
    await controller.dispose()
  })

  it('keeps visible selection facts independent from unresolved artifact projection', async () => {
    vi.stubGlobal('window', {
      navigator: { storage: {} },
      showSaveFilePicker: () => Promise.reject(new DOMException('unused', 'AbortError')),
    })
    const selection = new SelectionPolicy(false)
    const joined = {
      descriptor: {
        shareInstance: identity(7),
        shareInstanceId: identityText(7),
        syntheticRoot: identity(1),
        syntheticRootId: identityText(1),
      },
      recoveryIdentity: 'share.recovery',
      protocolSessionId: identityText(8),
      protocolSessionIdentity: identityText(8),
      selection,
      replaceSelection(this: { selection: SelectionPolicy }, next: SelectionPolicy) { this.selection = next },
      rootDirectory: () => ({
        id: identity(1), idText: identityText(1), name: 'Shared files', path: [], ancestry: [identityText(1)],
      }),
      page: async (directory: BrowseDirectory) => {
        return {
          directory,
          pageIndex: 0,
          pageCount: 2,
          entryCount: 2,
          omittedCount: 0n,
          entries: [entry],
        }
      },
      inspectSelection: () => new Promise<never>(() => undefined),
      subscribeCatalogScanProgress: () => () => undefined,
      subscribePathActivity: () => () => undefined,
      subscribeConnection: (listener: (value: { kind: 'connected' }) => void) => { listener({ kind: 'connected' }); return () => undefined },
      subscribeProtocolGeneration: () => () => undefined,
      close: async () => undefined,
    } as unknown as JoinedBrowserShare
    const gateway = { join: async () => joined } as unknown as BrowserReceiverGateway
    const controller = new ReceiverController(gateway)

    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() => controller.getSnapshot().rows.length === 1)
    expect(controller.getSnapshot().output.offers?.kind).toBe('confirming')
    controller.toggleSelection(entry.idText)
    expect(controller.getSnapshot().rows[0]?.selection).toBe('selected')
    expect(controller.getSnapshot().selectedVisibleFiles).toBe(1)
    expect(controller.getSnapshot().output.offers?.kind).toBe('confirming')
    await controller.dispose()
  })
})

describe('preview target download, single-photo blob reuse, and workspace task dismissal', () => {
  afterEach(() => vi.restoreAllMocks())

  it('retains activeTarget across loading, image, video, and error states and clears only on cancel or close', async () => {
    const blob = new Blob([Uint8Array.of(1, 2, 3)], { type: 'image/png' })
    const imageJoined = {
      beginPreviewConnectivity: () => ({ routes: [], close: vi.fn() }),
      preview: async () => ({
        current: { kind: 'image', name: entry.name, url: 'blob:img', mimeType: 'image/png', width: 20, height: 10 },
        imageBlob: blob,
        seek: async () => { throw new Error('not video') },
        close: async () => undefined,
      }),
    } as unknown as JoinedBrowserShare
    const videoJoined = new DiagnosticPreviewJoined() as unknown as JoinedBrowserShare
    const failingJoined = new DiagnosticPreviewJoined({ openFailure: new Error('decode failed') }) as unknown as JoinedBrowserShare

    let snapshot = directPreviewSnapshot()
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: next => { snapshot = next },
      publicError: () => 'preview failed',
    })

    previews.open(imageJoined, entry, rootDirectory)
    expect(previews.activeTarget()).toEqual({ entry, directory: rootDirectory })
    await turn()
    expect(previews.activeTarget()).toEqual({ entry, directory: rootDirectory, imageBlob: blob })

    previews.open(videoJoined, entry, rootDirectory)
    await turn()
    expect(snapshot.preview.state).toBe('video')
    expect(previews.activeTarget()).toEqual({ entry, directory: rootDirectory })

    previews.open(failingJoined, entry, rootDirectory)
    await turn()
    expect(snapshot.preview.state).toBe('error')
    expect(previews.activeTarget()).toEqual({ entry, directory: rootDirectory })

    previews.cancel()
    expect(previews.activeTarget()).toBeUndefined()

    previews.open(imageJoined, entry, rootDirectory)
    await turn()
    await previews.close()
    expect(previews.activeTarget()).toBeUndefined()
  })

  it('saves decoded preview images during active downloads, closes loading/error only after activation, and keeps video open', async () => {
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: false, fsa: false, opfs: false, portable: true, availableStorageBytes: null,
    }))
    const { windowPort, downloads } = createMockDownloadWindow()
    const photoBlob = new Blob([Uint8Array.of(10, 20, 30)], { type: 'image/png' })
    const photoEntry = { ...entry, id: identity(2), idText: identityText(2), name: 'photo.png', expectedSize: 100n }
    const brokenEntry = { ...entry, id: identity(3), idText: identityText(3), name: 'broken.png', expectedSize: 100n }
    const videoEntry = { ...entry, id: identity(4), idText: identityText(4), name: 'clip.mp4', expectedSize: 100n }
    const bigEntry = { ...entry, id: identity(5), idText: identityText(5), name: 'giant.bin', expectedSize: 80n * 1024n * 1024n }

    let releaseJob!: () => void
    const jobRunning = new Promise<void>(resolve => { releaseJob = resolve })
    const job3Gate = deferred<void>()
    let jobCalls = 0
    const selection = new SelectionPolicy(true)
    const joined = {
      descriptor: {
        shareInstance: identity(7), shareInstanceId: identityText(7),
        syntheticRoot: identity(1), syntheticRootId: identityText(1),
      },
      recoveryIdentity: 'share.recovery',
      protocolSessionId: identityText(8),
      protocolSessionIdentity: identityText(8),
      selection,
      replaceSelection(this: { selection: SelectionPolicy }, next: SelectionPolicy) { this.selection = next },
      rootDirectory: () => rootDirectory,
      page: async (directory: BrowseDirectory) => ({
        directory, pageIndex: 0, pageCount: 1, entryCount: 4, omittedCount: 0n,
        entries: [photoEntry, brokenEntry, videoEntry, bigEntry],
      }),
      inspectSelection: async () => ({
        kind: 'tree' as const, suggestedName: 'Album', rootPath: [], knownFileCount: 4, knownBytes: 400n, complete: true,
      }),
      subscribeCatalogScanProgress: () => () => undefined,
      subscribePathActivity: () => () => undefined,
      subscribeConnection: (listener: (value: { kind: 'connected' }) => void) => { listener({ kind: 'connected' }); return () => undefined },
      subscribeProtocolGeneration: () => () => undefined,
      beginPreviewConnectivity: () => ({ routes: [], close: vi.fn() }),
      beginDownloadConnectivity: () => ({ routes: [], close: vi.fn() }),
      createTransferJob: () => {
        jobCalls += 1
        const callIndex = jobCalls
        return {
          pause: vi.fn(),
          stop: vi.fn(),
          detach: vi.fn(),
          run: async () => {
            if (callIndex === 1) await jobRunning
            if (callIndex === 3) await job3Gate.promise
            return {
              status: 'completed' as const,
              completedFiles: 1n, completedBytes: 100n,
              discoveredFiles: 1n, discoveredBytes: 100n, discoveryComplete: true,
              failedFiles: 0n, failedDirectories: 0n,
            }
          },
        }
      },
      preview: async (target: typeof photoEntry) => {
        if (target.idText === brokenEntry.idText) throw new Error('corrupt image')
        if (target.idText === videoEntry.idText) {
          return {
            current: {
              kind: 'video', name: target.name, url: 'blob:vid', mimeType: 'video/mp4',
              width: 320, height: 180, durationSeconds: 10, keyframeSeconds: 0, decodeSeconds: 0,
            },
            seek: async () => { throw new Error('unused') },
            close: async () => undefined,
          }
        }
        return {
          current: { kind: 'image', name: target.name, url: 'blob:photo', mimeType: 'image/png', width: 20, height: 10 },
          imageBlob: photoBlob,
          seek: async () => { throw new Error('unused') },
          close: async () => undefined,
        }
      },
      close: async () => undefined,
    } as unknown as JoinedBrowserShare

    const controller = new ReceiverController(
      { join: async () => joined } as unknown as BrowserReceiverGateway,
      { windowPort: windowPort as unknown as ReceiveWindowPort },
    )
    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() => controller.getSnapshot().output.capabilitiesState === 'ready' && controller.getSnapshot().rows.length === 4)

    // Start a video preview and download it — modal stays open while transfer runs.
    controller.previewFile(videoEntry.idText)
    await waitFor(() => controller.getSnapshot().preview.state === 'video')
    expect(controller.downloadPreviewedFile()).toEqual({ kind: 'started' })
    await waitFor(() => controller.getSnapshot().output.lifecycle?.kind === 'downloading')
    expect(controller.getSnapshot().preview.state).toBe('video')

    // While the video is actively downloading, switch to a decoded image and save it locally!
    controller.previewFile(photoEntry.idText)
    await waitFor(() => controller.getSnapshot().preview.state === 'image')
    expect(controller.downloadPreviewedFile()).toEqual({ kind: 'saved-image' })
    expect(downloads).toHaveLength(1)
    expect(downloads[0]!.fileName).toBe('photo.png')

    // Finish the background download.
    releaseJob()
    await waitFor(() => controller.getSnapshot().output.lifecycle?.kind === 'download-started')

    // Error preview closes once transfer activates on the same file.
    controller.previewFile(brokenEntry.idText)
    await waitForController(controller, s => s.preview.state === 'error')
    expect(controller.downloadPreviewedFile()).toEqual({ kind: 'started' })
    await waitForController(controller, s => s.preview.state === 'idle' && s.output.activeTaskId === null && jobCalls === 2)
    expect(jobCalls).toBe(2)

    // Switching away before error preview's transfer activates keeps the newly opened preview alive
    // even after onActivated actually executes for job #3.
    controller.previewFile(brokenEntry.idText)
    await waitForController(controller, s => s.preview.state === 'error')
    expect(controller.downloadPreviewedFile()).toEqual({ kind: 'started' })
    expect(jobCalls).toBe(2)
    controller.previewFile(photoEntry.idText)
    await waitForController(controller, s =>
      s.preview.state === 'image' &&
      s.output.activeTaskId !== null &&
      s.output.lifecycle?.kind === 'downloading',
    )
    expect(jobCalls).toBe(3)
    expect(controller.getSnapshot().preview).toMatchObject({ state: 'image', fileId: photoEntry.idText })
    job3Gate.resolve()
    await waitForController(controller, s => s.output.activeTaskId === null)
    expect(controller.getSnapshot().preview).toMatchObject({ state: 'image', fileId: photoEntry.idText })

    // saveLocalBlob failure stays local to preview result without polluting global snapshot.error.
    vi.spyOn(windowPort.URL!, 'createObjectURL').mockImplementationOnce(() => {
      throw new Error('blob url failed')
    })
    expect(controller.downloadPreviewedFile()).toEqual({
      kind: 'failed',
      failure: { kind: 'save-unavailable', reason: 'no-safe-destination' },
    })
    expect(controller.getSnapshot().error).toBeNull()

    // Preview startup failure stays local to preview result without polluting global snapshot.error.
    const bigPreviewFail = vi.spyOn(joined, 'preview').mockRejectedValueOnce(new Error('not previewable'))
    controller.previewFile(bigEntry.idText)
    await waitFor(() => controller.getSnapshot().preview.state === 'error')
    const failedResult = controller.downloadPreviewedFile()
    expect(failedResult).toEqual({
      kind: 'failed',
      failure: { kind: 'save-unavailable', reason: 'portable-limit-exceeded' },
    })
    expect(controller.getSnapshot().error).toBeNull()
    expect(bigPreviewFail).toHaveBeenCalledTimes(1)

    await controller.dispose()
  })
})

describe('single-photo blob reuse, preview picker isolation, and workspace task dismissal', () => {
  afterEach(() => vi.restoreAllMocks())

  it('reuses single-photo imageBlob on default portable save, but starts transfer on redownload or explicit fsa/opfs', async () => {
    const store = new MemoryReceiveStore()
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore)
    const { env } = createMemoryOPFSEnv()
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: true, opfs: true, portable: true, availableStorageBytes: 64n * 1024n * 1024n,
    }))
    const { windowPort: baseWin, downloads } = createMockDownloadWindow()
    const picker = vi.fn(() => new Promise<FileSystemDirectoryHandle>(() => undefined))
    const windowPort = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
      showDirectoryPicker: picker,
    }
    const photoBlob = new Blob([Uint8Array.of(1, 2, 3, 4)], { type: 'image/png' })
    let transferStarts = 0
    const joined = {
      descriptor: {
        shareInstance: identity(7), shareInstanceId: identityText(7),
        syntheticRoot: identity(1), syntheticRootId: identityText(1),
      },
      recoveryIdentity: 'share.recovery',
      protocolSessionId: identityText(8),
      protocolSessionIdentity: identityText(8),
      selection: new SelectionPolicy(true),
      replaceSelection(this: { selection: SelectionPolicy }, next: SelectionPolicy) { this.selection = next },
      rootDirectory: () => rootDirectory,
      page: async (directory: BrowseDirectory) => ({
        directory, pageIndex: 0, pageCount: 1, entryCount: 1, omittedCount: 0n, entries: [entry],
      }),
      inspectSelection: async () => ({
        kind: 'single-file' as const, file: entry, sourcePath: [entry.name], exactBytes: entry.expectedSize,
      }),
      subscribeCatalogScanProgress: () => () => undefined,
      subscribePathActivity: () => () => undefined,
      subscribeConnection: (listener: (value: { kind: 'connected' }) => void) => { listener({ kind: 'connected' }); return () => undefined },
      subscribeProtocolGeneration: () => () => undefined,
      beginPreviewConnectivity: () => ({ routes: [], close: vi.fn() }),
      automaticPhotoPreview: async () => ({
        current: { kind: 'image', name: entry.name, url: 'blob:auto', mimeType: 'image/png', width: 20, height: 10 },
        imageBlob: photoBlob,
        seek: async () => { throw new Error('unused') },
        close: async () => undefined,
      }),
      beginDownloadConnectivity: () => {
        transferStarts += 1
        return { routes: [], close: vi.fn() }
      },
      createTransferJob: (output: Parameters<JoinedBrowserShare['createTransferJob']>[0]) => ({
        pause: vi.fn(),
        stop: vi.fn(),
        detach: vi.fn(),
        run: async () => {
          const writer = await output.openFile({
            fileId: entry.idText,
            path: [entry.name],
            sourcePath: [entry.name],
            revision: 'rev-1',
            exactSize: entry.expectedSize,
          }, new AbortController().signal)
          await writer.writeAt(0n, new Uint8Array(Number(entry.expectedSize)))
          await writer.commit([{ start: 0n, end: entry.expectedSize }])
          return {
            status: 'completed' as const,
            completedFiles: 1n, completedBytes: entry.expectedSize,
            discoveredFiles: 1n, discoveredBytes: entry.expectedSize, discoveryComplete: true,
            failedFiles: 0n, failedDirectories: 0n,
          }
        },
      }),
      close: async () => undefined,
    } as unknown as JoinedBrowserShare

    const controller = new ReceiverController(
      { join: async () => joined } as unknown as BrowserReceiverGateway,
      { windowPort: windowPort as unknown as ReceiveWindowPort, opfsEnv: env },
    )
    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() =>
      controller.getSnapshot().preview.state === 'image' &&
      controller.getSnapshot().output.offers?.kind === 'ready',
    )

    // Default portable save uses in-memory imageBlob with 0 transfer and persists terminal history!
    controller.chooseArtifact('portable:file', 'default')
    expect(transferStarts).toBe(0)
    expect(downloads).toHaveLength(1)
    expect(controller.getSnapshot().output.lifecycle).toMatchObject({ kind: 'download-started', retained: false })
    const localBlobTaskId = controller.getSnapshot().output.lifecycle!.taskId
    await waitFor(() => store.tasks.has(localBlobTaskId))
    expect(controller.presentation.downloads.getSnapshot().history.some(task => task.operationId === localBlobTaskId)).toBe(true)

    // Dismissing a handed-to-browser card hides it in workspace while retaining history.
    const taskSnap = controller.presentation.task.getSnapshot()
    const currentTask = taskSnap.current
    expect(currentTask).not.toBeNull()
    const dismissedLayout = resolveWorkspaceTaskLayout(
      taskSnap,
      new Set([dismissedTaskKey(currentTask!.operationId, currentTask!.generation)]),
    )
    expect(dismissedLayout.primaryTask).toBeNull()
    expect(dismissedLayout.collapseRemoteSaving).toBe(false)

    // Explicit redownload starts a real transfer even for portable and persists an equivalent history shape.
    controller.chooseArtifact('portable:file', 'redownload')
    await waitFor(() => transferStarts === 1)
    await waitFor(() => controller.getSnapshot().output.activeTaskId === null)
    const redownloadTaskId = controller.getSnapshot().output.lifecycle!.taskId
    expect(store.tasks.get(localBlobTaskId)).toMatchObject({
      shareInstance: store.tasks.get(redownloadTaskId)?.shareInstance,
      selectionKey: store.tasks.get(redownloadTaskId)?.selectionKey,
      selection: store.tasks.get(redownloadTaskId)?.selection,
      artifact: store.tasks.get(redownloadTaskId)?.artifact,
      backend: 'portable',
      target: store.tasks.get(redownloadTaskId)?.target,
      discoveryComplete: true,
      display: { destinationLabel: { kind: 'browser-downloads' } },
      lifecycle: { kind: 'download-started', retained: false, fileName: entry.name },
    })

    // Explicit FSA opens the directory picker instead of reusing imageBlob.
    controller.chooseArtifact('fsa:file', 'default')
    expect(picker).toHaveBeenCalledTimes(1)
    expect(controller.getSnapshot().output.activation?.kind).toBe('waiting-picker')
    controller.cancelPreparing()
    await waitFor(() => controller.getSnapshot().output.offers?.kind === 'ready')

    // Explicit OPFS initiates a real transfer, writes/commits bytes, and settles as retained download-started.
    controller.chooseArtifact('opfs:file', 'default')
    await waitForController(controller, s =>
      s.output.backend === 'opfs' &&
      s.output.activeTaskId === null &&
      s.output.lifecycle?.kind === 'download-started',
    )
    expect(transferStarts).toBe(2)
    expect(controller.getSnapshot().output.lifecycle).toMatchObject({
      kind: 'download-started',
      retained: true,
      fileName: entry.name,
    })
    expect(downloads).toHaveLength(3)

    // Active workspace lifecycle action failure routes locally when handled and falls back globally on unmount.
    const lifecycleFailures: ReceiverFailure[] = []
    vi.spyOn(baseWin.URL!, 'createObjectURL').mockImplementationOnce(() => {
      throw new Error('local handoff blocked')
    })
    controller.performLifecycleAction('save', {
      onAsyncFailure: failure => { lifecycleFailures.push(failure); return true },
    })
    await waitFor(() => lifecycleFailures.length === 1)
    expect(controller.getSnapshot().error).toBeNull()

    vi.spyOn(baseWin.URL!, 'createObjectURL').mockImplementationOnce(() => {
      throw new Error('unmounted handoff blocked')
    })
    controller.performLifecycleAction('save', { onAsyncFailure: () => false })
    await waitForController(controller, s => s.error !== null)

    await controller.dispose()
  })

  it('keeps preview open on cancelled picker or item switch, isolates async startup failures, and skips dismissed OPFS cards while keeping ready-to-save', async () => {
    const store = new MemoryReceiveStore()
    const openSpy = vi.spyOn(ReceiveStore, 'open').mockResolvedValue(
      Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore,
    )
    const mib = 1024n * 1024n
    const fsaEntry = { ...entry, id: identity(6), idText: identityText(6), name: 'big-clip.mp4', expectedSize: 80n * mib }
    const otherEntry = { ...entry, id: identity(8), idText: identityText(8), name: 'other.png', expectedSize: 100n }
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: true, opfs: false, portable: true, availableStorageBytes: null,
    }))
    let rejectPicker!: (error: unknown) => void
    let pickerCall = 0
    const { windowPort: baseWin } = createMockDownloadWindow()
    const windowPort = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
      showDirectoryPicker: vi.fn(() => {
        pickerCall += 1
        return new Promise<FileSystemDirectoryHandle>((_res, rej) => { rejectPicker = rej })
      }),
    }
    const joined = {
      descriptor: {
        shareInstance: identity(7), shareInstanceId: identityText(7),
        syntheticRoot: identity(1), syntheticRootId: identityText(1),
      },
      recoveryIdentity: 'share.recovery',
      protocolSessionId: identityText(8),
      protocolSessionIdentity: identityText(8),
      selection: new SelectionPolicy(true),
      replaceSelection(this: { selection: SelectionPolicy }, next: SelectionPolicy) { this.selection = next },
      rootDirectory: () => rootDirectory,
      page: async (directory: BrowseDirectory) => ({
        directory, pageIndex: 0, pageCount: 1, entryCount: 2, omittedCount: 0n, entries: [fsaEntry, otherEntry],
      }),
      inspectSelection: async () => ({
        kind: 'tree' as const, suggestedName: 'Album', rootPath: [], knownFileCount: 2, knownBytes: 80n * mib, complete: true,
      }),
      subscribeCatalogScanProgress: () => () => undefined,
      subscribePathActivity: () => () => undefined,
      subscribeConnection: (listener: (value: { kind: 'connected' }) => void) => { listener({ kind: 'connected' }); return () => undefined },
      subscribeProtocolGeneration: () => () => undefined,
      beginPreviewConnectivity: () => ({ routes: [], close: vi.fn() }),
      preview: async (target: typeof fsaEntry) => {
        if (target.idText === fsaEntry.idText) throw new Error('preview failed')
        return {
          current: { kind: 'image', name: target.name, url: 'blob:other', mimeType: 'image/png', width: 20, height: 10 },
          imageBlob: new Blob([Uint8Array.of(1)], { type: 'image/png' }),
          seek: async () => { throw new Error('unused') },
          close: async () => undefined,
        }
      },
      close: async () => undefined,
    } as unknown as JoinedBrowserShare

    const controller = new ReceiverController(
      { join: async () => joined } as unknown as BrowserReceiverGateway,
      { windowPort: windowPort as unknown as ReceiveWindowPort },
    )
    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await waitForController(controller, s =>
      s.output.capabilitiesState === 'ready' &&
      s.rows.length === 2 &&
      s.output.singleFilePickerPendingFromBytes === null,
    )
    expect(openSpy).toHaveBeenCalled()

    // Cancelling the FSA picker keeps the error preview open with zero errors.
    controller.previewFile(fsaEntry.idText)
    await waitFor(() => controller.getSnapshot().preview.state === 'error')
    const localFailures: ReceiverFailure[] = []
    expect(controller.downloadPreviewedFile({ onAsyncFailure: f => localFailures.push(f) })).toEqual({ kind: 'waiting-picker' })
    rejectPicker(new DOMException('User cancelled', 'AbortError'))
    await turn()
    expect(controller.getSnapshot().preview.state).toBe('error')
    expect(localFailures).toHaveLength(0)
    expect(controller.getSnapshot().error).toBeNull()

    // Non-abort async startup failure while on the same preview goes ONLY to local onAsyncFailure.
    expect(controller.downloadPreviewedFile({ onAsyncFailure: f => localFailures.push(f) })).toEqual({ kind: 'waiting-picker' })
    rejectPicker(new Error('picker denied'))
    await waitFor(() => localFailures.length === 1)
    expect(controller.getSnapshot().error).toBeNull()
    expect(controller.getSnapshot().preview.state).toBe('error')

    // Async startup failure after switching away falls back to global snapshot.error.
    expect(controller.downloadPreviewedFile({ onAsyncFailure: f => localFailures.push(f) })).toEqual({ kind: 'waiting-picker' })
    controller.previewFile(otherEntry.idText)
    await waitFor(() => controller.getSnapshot().preview.state === 'image')
    rejectPicker(new Error('late picker failure'))
    await waitFor(() => controller.getSnapshot().error !== null)
    expect(localFailures).toHaveLength(1)
    expect(pickerCall).toBe(3)

    // Verify ready-to-save is NOT dismissible while a matched OPFS handed-to-browser retained card IS dismissible.
    const baseTaskSnap = controller.presentation.task.getSnapshot()
    const readyTask = {
      operationId: 'opfs-ready',
      generation: 1n,
      stage: 'ready-to-save',
      completeness: 'complete',
      primaryAction: { id: 'save', target: { kind: 'retained', action: 'save' }, disabledReason: null },
      secondaryActions: [],
      destructiveActions: [],
    }
    const readyLayout = resolveWorkspaceTaskLayout(
      { ...baseTaskSnap, current: readyTask as unknown as NonNullable<typeof baseTaskSnap.current> },
      new Set([dismissedTaskKey('opfs-ready', 1n)]),
    )
    expect(readyLayout.primaryTask?.operationId).toBe('opfs-ready')

    const selectionKey = baseTaskSnap.output.offers?.selectionKey ?? 'sel-1'
    const shareInstance = baseTaskSnap.share?.shareInstance ?? identityText(7)
    const opfsHandedTask = {
      operationId: 'opfs-handed',
      generation: 2n,
      stage: 'handed-to-browser',
      completeness: 'complete',
      primaryAction: { id: 'save', target: { kind: 'retained', action: 'save' }, disabledReason: null },
      secondaryActions: [],
      destructiveActions: [],
    }
    const opfsRetainedSnap = {
      ...baseTaskSnap,
      current: null,
      tasks: [opfsHandedTask as unknown as NonNullable<typeof baseTaskSnap.current>],
      retained: {
        kind: 'available' as const,
        pending: null,
        tasks: [{
          taskId: 'opfs-handed',
          shareInstance,
          selectionKey,
          backend: 'opfs' as const,
          lifecycle: { kind: 'download-started' as const, taskId: 'opfs-handed', generation: 2n, updatedAt: 1, retained: true, fileName: 'a.bin' },
          actions: ['save' as const, 'discard' as const],
          activeElsewhere: false,
          canSavePartial: false,
        }],
      },
    }
    expect(resolveWorkspaceTaskLayout(opfsRetainedSnap).primaryTask?.operationId).toBe('opfs-handed')
    expect(resolveWorkspaceTaskLayout(opfsRetainedSnap).collapseRemoteSaving).toBe(true)
    const dismissedKeys = new Set([dismissedTaskKey('opfs-handed', 2n)])
    const dismissedOpfsLayout = resolveWorkspaceTaskLayout(opfsRetainedSnap, dismissedKeys)
    expect(dismissedOpfsLayout.primaryTask).toBeNull()
    expect(dismissedOpfsLayout.collapseRemoteSaving).toBe(false)

    // Re-exporting increments generation so the workspace card resurfaces naturally.
    const reExportedSnap = {
      ...opfsRetainedSnap,
      tasks: [{ ...opfsHandedTask, generation: 3n } as unknown as NonNullable<typeof baseTaskSnap.current>],
    }
    expect(resolveWorkspaceTaskLayout(reExportedSnap, dismissedKeys).primaryTask?.operationId).toBe('opfs-handed')
    expect(resolveWorkspaceTaskLayout(reExportedSnap, dismissedKeys).collapseRemoteSaving).toBe(true)

    await controller.dispose()
  })
})

describe('preview export guards, modal task failure routing, and shareScope visits', () => {
  afterEach(() => vi.restoreAllMocks())

  it('guards late onExported callbacks, routes task action failures locally with unmount fallback, and advances shareScope visitId across switches', async () => {
    const store = new MemoryReceiveStore()
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore)
    const { env } = createMemoryOPFSEnv()
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: false, opfs: true, portable: true, availableStorageBytes: 64n * 1024n * 1024n,
    }))
    const { windowPort: baseWin, downloads } = createMockDownloadWindow()
    const windowPort = {
      ...baseWin,
      indexedDB: { open: vi.fn() } as unknown as IDBFactory,
    }
    const shareAId = identityText(7)
    const shareBId = identityText(9)
    const fileA = { ...entry, id: identity(2), idText: identityText(2), name: 'clip-a.mp4', expectedSize: 20n * 1024n * 1024n }
    const fileB = { ...entry, id: identity(3), idText: identityText(3), name: 'photo-b.png', expectedSize: 100n }
    const makeJoined = (shareId: string, shareBytes: Uint8Array) => ({
      descriptor: {
        shareInstance: shareBytes, shareInstanceId: shareId,
        syntheticRoot: identity(1), syntheticRootId: identityText(1),
      },
      recoveryIdentity: 'share.recovery',
      protocolSessionId: identityText(8),
      protocolSessionIdentity: identityText(8),
      selection: new SelectionPolicy(true),
      replaceSelection(this: { selection: SelectionPolicy }, next: SelectionPolicy) { this.selection = next },
      rootDirectory: () => rootDirectory,
      page: async (directory: BrowseDirectory) => ({
        directory, pageIndex: 0, pageCount: 1, entryCount: 2, omittedCount: 0n, entries: [fileA, fileB],
      }),
      inspectSelection: async () => ({
        kind: 'tree' as const, suggestedName: 'Album', rootPath: [], knownFileCount: 2, knownBytes: fileA.expectedSize + fileB.expectedSize, complete: true,
      }),
      subscribeCatalogScanProgress: () => () => undefined,
      subscribePathActivity: () => () => undefined,
      subscribeConnection: (listener: (value: { kind: 'connected' }) => void) => { listener({ kind: 'connected' }); return () => undefined },
      subscribeProtocolGeneration: () => () => undefined,
      beginPreviewConnectivity: () => ({ routes: [], close: vi.fn() }),
      preview: async (target: typeof fileA) => {
        if (target.idText === fileA.idText) {
          return {
            current: {
              kind: 'video', name: target.name, url: 'blob:a', mimeType: 'video/mp4',
              width: 320, height: 180, durationSeconds: 10, keyframeSeconds: 0, decodeSeconds: 0,
            },
            seek: async () => { throw new Error('unused') },
            close: async () => undefined,
          }
        }
        return {
          current: { kind: 'image', name: target.name, url: 'blob:b', mimeType: 'image/png', width: 20, height: 10 },
          imageBlob: new Blob([Uint8Array.of(1)], { type: 'image/png' }),
          seek: async () => { throw new Error('unused') },
          close: async () => undefined,
        }
      },
      close: async () => undefined,
    }) as unknown as JoinedBrowserShare

    const joinedA1 = makeJoined(shareAId, identity(7))
    const joinedB = makeJoined(shareBId, identity(9))
    const joinedA2 = makeJoined(shareAId, identity(7))
    let joinIndex = 0
    const shares = [joinedA1, joinedB, joinedA2]
    const controller = new ReceiverController(
      { join: async () => shares[joinIndex++]! } as unknown as BrowserReceiverGateway,
      { windowPort: windowPort as unknown as ReceiveWindowPort, opfsEnv: env },
    )

    // Pre-populate a sealed OPFS record for fileA on shareA so preview download triggers async OPFS export.
    const opfsDir = await (await env.getRootDirectory()).getDirectoryHandle('windshare-tasks', { create: true })
    const fileHandle = await opfsDir.getFileHandle('task-a.part', { create: true })
    const nativeIo = await env.openNativeObject!(fileHandle)
    await nativeIo.writeAt(0n, new Uint8Array(8))
    await nativeIo.close()

    const singlePolicy = new SelectionPolicy(false)
    singlePolicy.set(fileA, rootDirectory.ancestry, true)
    const { selectionSpecFromPolicy, selectionKey: computeSelectionKey } = await import('../../src/transfer/selection')
    const specA = selectionSpecFromPolicy(shareAId, identityText(1), singlePolicy.snapshot())
    const keyA = computeSelectionKey(specA)
    await store.putTask({
      taskId: 'task-a',
      shareInstance: shareAId,
      selectionKey: keyA,
      selection: specA,
      artifact: { kind: 'original-file', name: fileA.name, singleFileId: fileA.idText, singleFilePath: [fileA.name] },
      backend: 'opfs',
      lifecycle: { kind: 'download-started', taskId: 'task-a', generation: 2n, retained: true, fileName: fileA.name },
      target: { kind: 'opfs', objectName: 'task-a.part', fileName: fileA.name, isZip: false, allocatedLength: 8n, sealedSize: 8n },
      discoveryComplete: true,
      updatedAt: 1,
    })

    controller.initialize({ capabilityInput: 'share-a', pageUrl: 'https://receiver.invalid/s/a' })
    await waitFor(() =>
      controller.getSnapshot().output.capabilitiesState === 'ready' &&
      controller.getSnapshot().retained.tasks.length === 1,
    )

    // Switching preview from fileA to fileB before OPFS export completes must suppress fileA's late onExported.
    controller.previewFile(fileA.idText)
    await waitFor(() => controller.getSnapshot().preview.state === 'video')
    let exportedCalls = 0
    expect(controller.downloadPreviewedFile({ onExported: () => { exportedCalls += 1 } })).toEqual({ kind: 'exported' })
    controller.previewFile(fileB.idText)
    await waitFor(() => downloads.length === 1 && controller.getSnapshot().retained.pending === null)
    expect(exportedCalls).toBe(0)

    // Staying on fileA receives onExported normally.
    controller.previewFile(fileA.idText)
    await waitFor(() => controller.getSnapshot().preview.state === 'video')
    expect(controller.downloadPreviewedFile({ onExported: () => { exportedCalls += 1 } })).toEqual({ kind: 'exported' })
    await waitFor(() => exportedCalls === 1 && controller.getSnapshot().retained.pending === null)
    expect(downloads).toHaveLength(2)

    // Retained action failure routes locally when onAsyncFailure handles it (returns true/void),
    // and falls back to global snapshot.error when onAsyncFailure returns false (unmounted modal).
    const missingTask = {
      ...controller.getSnapshot().retained.tasks[0]!,
      taskId: 'missing-task',
      lifecycle: { kind: 'ready-to-save' as const, taskId: 'missing-task', generation: 1n, size: 8n, fileName: 'gone.bin' },
      actions: ['save' as const],
    }
    const localFailures: ReceiverFailure[] = []
    controller.performRetainedAction(missingTask, 'save', {
      onAsyncFailure: failure => { localFailures.push(failure); return true },
    })
    await waitFor(() => localFailures.length === 1)
    expect(controller.getSnapshot().error).toBeNull()

    controller.performRetainedAction(missingTask, 'save', {
      onAsyncFailure: () => false,
    })
    await waitFor(() => controller.getSnapshot().error !== null)

    // shareScope.visitId advances monotonically across A -> B -> A and stays stable within a share.
    const scopeA1 = controller.presentation.shareScope.getSnapshot()
    expect(scopeA1.shareInstance).toBe(shareAId)
    let scopeNotifications = 0
    const unsubScope = controller.presentation.shareScope.subscribe(() => { scopeNotifications += 1 })
    controller.toggleSelection(fileB.idText)
    expect(scopeNotifications).toBe(0)
    controller.submitKey('share-b')
    await waitFor(() => controller.presentation.shareScope.getSnapshot().shareInstance === shareBId)
    const scopeB = controller.presentation.shareScope.getSnapshot()
    expect(scopeB.visitId).toBeGreaterThan(scopeA1.visitId)
    controller.submitKey('share-a-again')
    await waitFor(() => controller.presentation.shareScope.getSnapshot().shareInstance === shareAId)
    const scopeA2 = controller.presentation.shareScope.getSnapshot()
    expect(scopeA2.visitId).toBeGreaterThan(scopeB.visitId)
    unsubScope()

    await controller.dispose()
  })

  it('closes loading preview on transfer activation and keeps modal open when loading resolves to image before onActivated', async () => {
    const store = new MemoryReceiveStore()
    vi.spyOn(ReceiveStore, 'open').mockResolvedValue(Object.assign(store, { close: vi.fn() }) as unknown as ReceiveStore)
    const { env } = createMemoryOPFSEnv()
    let openDirGate: ReturnType<typeof deferred<void>> | null = null
    const gatedEnv = {
      ...env,
      getRootDirectory: async () => {
        if (openDirGate !== null) await openDirGate.promise
        return env.getRootDirectory()
      },
    }
    vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
      indexedDb: true, fsa: false, opfs: true, portable: false, availableStorageBytes: 64n * 1024n * 1024n,
    }))
    const { windowPort: baseWin, downloads } = createMockDownloadWindow()
    const windowPort = { ...baseWin, indexedDB: { open: vi.fn() } as unknown as IDBFactory }
    const fileCloseOnLoad = { ...entry, id: identity(2), idText: identityText(2), name: 'slow-a.png', expectedSize: 8n }
    const fileResolveToImage = { ...entry, id: identity(3), idText: identityText(3), name: 'slow-b.png', expectedSize: 8n }

    const previewSignals: AbortSignal[] = []
    const previewGate2 = deferred<Awaited<ReturnType<JoinedBrowserShare['preview']>>>()
    const job2Gate = deferred<void>()
    let jobCalls = 0
    const joined = {
      descriptor: {
        shareInstance: identity(7), shareInstanceId: identityText(7),
        syntheticRoot: identity(1), syntheticRootId: identityText(1),
      },
      recoveryIdentity: 'share.recovery',
      protocolSessionId: identityText(8),
      protocolSessionIdentity: identityText(8),
      selection: new SelectionPolicy(true),
      replaceSelection(this: { selection: SelectionPolicy }, next: SelectionPolicy) { this.selection = next },
      rootDirectory: () => rootDirectory,
      page: async (directory: BrowseDirectory) => ({
        directory, pageIndex: 0, pageCount: 1, entryCount: 2, omittedCount: 0n,
        entries: [fileCloseOnLoad, fileResolveToImage],
      }),
      inspectSelection: async () => ({
        kind: 'tree' as const, suggestedName: 'Album', rootPath: [], knownFileCount: 2, knownBytes: 16n, complete: true,
      }),
      subscribeCatalogScanProgress: () => () => undefined,
      subscribePathActivity: () => () => undefined,
      subscribeConnection: (listener: (value: { kind: 'connected' }) => void) => { listener({ kind: 'connected' }); return () => undefined },
      subscribeProtocolGeneration: () => () => undefined,
      beginPreviewConnectivity: () => ({ routes: [], close: vi.fn() }),
      beginDownloadConnectivity: () => ({ routes: [], close: vi.fn() }),
      createTransferJob: (output: Parameters<JoinedBrowserShare['createTransferJob']>[0]) => {
        jobCalls += 1
        const callIndex = jobCalls
        const file = callIndex === 1 ? fileCloseOnLoad : fileResolveToImage
        return {
          pause: vi.fn(),
          stop: vi.fn(),
          detach: vi.fn(),
          run: async () => {
            if (callIndex === 2) await job2Gate.promise
            const writer = await output.openFile({
              fileId: file.idText,
              path: [file.name],
              sourcePath: [file.name],
              revision: 'rev-1',
              exactSize: file.expectedSize,
            }, new AbortController().signal)
            await writer.writeAt(0n, new Uint8Array(Number(file.expectedSize)))
            await writer.commit([{ start: 0n, end: file.expectedSize }])
            return {
              status: 'completed' as const,
              completedFiles: 1n, completedBytes: file.expectedSize,
              discoveredFiles: 1n, discoveredBytes: file.expectedSize, discoveryComplete: true,
              failedFiles: 0n, failedDirectories: 0n,
            }
          },
        }
      },
      preview: (target: typeof fileCloseOnLoad, _conn: unknown, signal: AbortSignal) => {
        previewSignals.push(signal)
        if (target.idText === fileCloseOnLoad.idText) {
          return new Promise<never>(() => undefined)
        }
        return previewGate2.promise
      },
      close: async () => undefined,
    } as unknown as JoinedBrowserShare

    const controller = new ReceiverController(
      { join: async () => joined } as unknown as BrowserReceiverGateway,
      { windowPort: windowPort as unknown as ReceiveWindowPort, opfsEnv: gatedEnv },
    )
    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await waitForController(controller, s => s.output.capabilitiesState === 'ready' && s.rows.length === 2)

    // Still loading when onActivated fires: modal closes and pending preview aborts.
    controller.previewFile(fileCloseOnLoad.idText)
    expect(controller.getSnapshot().preview).toMatchObject({ state: 'loading', fileId: fileCloseOnLoad.idText })
    expect(controller.downloadPreviewedFile()).toEqual({ kind: 'started' })
    await waitForController(controller, s =>
      s.preview.state === 'idle' && s.output.activeTaskId === null && s.output.lifecycle?.kind === 'download-started',
    )
    expect(previewSignals[0]?.aborted).toBe(true)
    expect(jobCalls).toBe(1)

    // Starts in loading, resolves to image before session opens/activates: onActivated keeps the image modal open.
    openDirGate = deferred<void>()
    controller.previewFile(fileResolveToImage.idText)
    expect(controller.getSnapshot().preview).toMatchObject({ state: 'loading', fileId: fileResolveToImage.idText })
    expect(controller.downloadPreviewedFile()).toEqual({ kind: 'started' })
    previewGate2.resolve({
      current: { kind: 'image', name: fileResolveToImage.name, url: 'blob:slow-b', mimeType: 'image/png', width: 20, height: 10 },
      imageBlob: new Blob([Uint8Array.of(1)], { type: 'image/png' }),
      seek: async () => { throw new Error('unused') },
      close: async () => undefined,
    } as unknown as Awaited<ReturnType<JoinedBrowserShare['preview']>>)
    await waitForController(controller, s => s.preview.state === 'image' && s.preview.fileId === fileResolveToImage.idText)
    expect(jobCalls).toBe(1)

    openDirGate.resolve()
    await waitForController(controller, s => s.output.activeTaskId !== null && s.output.lifecycle?.kind === 'downloading')
    expect(jobCalls).toBe(2)
    expect(controller.getSnapshot().preview).toMatchObject({ state: 'image', fileId: fileResolveToImage.idText })

    job2Gate.resolve()
    await waitForController(controller, s => s.output.activeTaskId === null && s.output.lifecycle?.kind === 'download-started')
    expect(controller.getSnapshot().preview).toMatchObject({ state: 'image', fileId: fileResolveToImage.idText })
    expect(downloads).toHaveLength(2)

    await controller.dispose()
  })
})

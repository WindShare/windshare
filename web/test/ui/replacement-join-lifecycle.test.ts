import { afterEach, describe, expect, it, vi } from 'vitest'
import type {
  BrowseDirectory,
  BrowsePage,
  BrowserReceiverGateway,
  JoinedBrowserShare,
} from '../../src/application/browser-share/gateway'
import type { ReceiveWindowPort } from '../../src/application/receive/coordinator'
import type { CatalogEntry } from '../../src/catalog/records'
import { SelectionPolicy } from '../../src/catalog/selection'
import { encodeSuite02CapabilityKey } from '../../src/crypto/suite02-link'
import * as outputContract from '../../src/output/backends/contract'
import { RELAY_ERROR } from '../../src/transport/relay/protocol'
import { RelayReceiverError } from '../../src/transport/relay/receiver'
import { ReceiverController } from '../../src/ui/controller'
import { createMockDownloadWindow } from '../output/opfs-fixture'
import {
  deferred,
  diagnosticImageJoined,
  entry as photoEntry,
  identity,
  identityText,
  waitForController,
} from './v2-preview-fixture'

const PAGE_URL = 'https://windshare.example/'

async function makeShareLink(seed: number): Promise<string> {
  const key = await encodeSuite02CapabilityKey(new Uint8Array(16).fill(seed), new Uint8Array(16).fill(seed))
  return `${PAGE_URL}s/${key.shareId}?r=relay.invalid#${key.encoded}`
}

function stubPortableOutput() {
  vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
    indexedDb: false, fsa: false, opfs: false, portable: true, availableStorageBytes: null,
  }))
  const { windowPort } = createMockDownloadWindow()
  return windowPort as unknown as ReceiveWindowPort
}

function makeDirectory(first: number, idText: string, path: readonly string[], ancestry: readonly string[]): BrowseDirectory {
  return Object.freeze({
    id: identity(first),
    idText,
    path: Object.freeze([...path]),
    ancestry: Object.freeze([...ancestry]),
  })
}

function makeMultiPageShare(seed: number) {
  const rootId = identityText(seed + 100)
  const folderEntry: CatalogEntry = Object.freeze({
    kind: 'directory',
    id: identity(seed + 101),
    idText: identityText(seed + 101),
    name: 'Docs',
  })
  const firstFile: CatalogEntry = Object.freeze({
    kind: 'file',
    id: identity(seed + 102),
    idText: identityText(seed + 102),
    name: 'readme.txt',
    expectedSize: 128n,
  })
  const nestedFile: CatalogEntry = Object.freeze({
    kind: 'file',
    id: identity(seed + 103),
    idText: identityText(seed + 103),
    name: 'spec.pdf',
    expectedSize: 512n,
  })
  const root = makeDirectory(seed + 100, rootId, [], [rootId])
  let closes = 0
  let inspectCalls = 0
  const share = {
    descriptor: {
      shareInstance: identity(seed),
      shareInstanceId: identityText(seed),
      syntheticRoot: identity(seed + 100),
      syntheticRootId: rootId,
    },
    recoveryIdentity: `multi.${seed}`,
    protocolSessionId: identityText(seed + 50),
    protocolSessionIdentity: identityText(seed + 50),
    selection: new SelectionPolicy(true),
    replaceSelection(this: { selection: SelectionPolicy }, next: SelectionPolicy) { this.selection = next },
    rootDirectory: () => root,
    childDirectory: (parent: BrowseDirectory, child: CatalogEntry): BrowseDirectory => {
      if (child.kind !== 'directory') throw new TypeError('not a directory')
      return Object.freeze({
        id: child.id,
        idText: child.idText,
        path: Object.freeze([...parent.path, child.name]),
        ancestry: Object.freeze([...parent.ancestry, child.idText]),
      })
    },
    page: async (directory: BrowseDirectory, pageIndex: number): Promise<BrowsePage> => {
      if (directory.idText === rootId) {
        return Object.freeze({
          directory,
          pageIndex,
          pageCount: 2,
          entryCount: 2,
          omittedCount: 1n,
          entries: Object.freeze([folderEntry, firstFile]),
        })
      }
      return Object.freeze({
        directory,
        pageIndex,
        pageCount: 2,
        entryCount: 3,
        omittedCount: 2n,
        entries: Object.freeze([nestedFile]),
      })
    },
    inspectSelection: async () => {
      inspectCalls += 1
      return Object.freeze({
        kind: 'single-file' as const,
        file: nestedFile,
        sourcePath: Object.freeze(['Docs', nestedFile.name]),
        exactBytes: nestedFile.expectedSize,
      })
    },
    subscribeCatalogScanProgress: () => () => undefined,
    subscribePathActivity: () => () => undefined,
    subscribeConnection: (listener: (value: { kind: 'connected' }) => void) => {
      listener({ kind: 'connected' })
      return () => undefined
    },
    subscribeProtocolGeneration: () => () => undefined,
    beginDownloadConnectivity: () => ({ routes: [], close: vi.fn() }),
    createTransferJob: () => ({
      pause: vi.fn(),
      stop: vi.fn(),
      detach: vi.fn(),
      run: () => new Promise(() => undefined),
    }),
    close: async () => { closes += 1 },
  } as unknown as JoinedBrowserShare
  return {
    share,
    folderEntry,
    nestedFile,
    get closes() { return closes },
    get inspectCalls() { return inspectCalls },
  }
}

function makeSinglePhotoShare(seed: number) {
  const rootId = identityText(seed + 100)
  const root = makeDirectory(seed + 100, rootId, [], [rootId])
  const preview = diagnosticImageJoined()
  const previewSpy = vi.fn(() => preview.preview())
  const share = {
    descriptor: {
      shareInstance: identity(seed),
      shareInstanceId: identityText(seed),
      syntheticRoot: identity(seed + 100),
      syntheticRootId: rootId,
    },
    recoveryIdentity: `photo.${seed}`,
    protocolSessionId: identityText(seed + 50),
    protocolSessionIdentity: identityText(seed + 50),
    selection: new SelectionPolicy(true),
    replaceSelection(this: { selection: SelectionPolicy }, next: SelectionPolicy) { this.selection = next },
    rootDirectory: () => root,
    page: async (directory: BrowseDirectory): Promise<BrowsePage> => Object.freeze({
      directory,
      pageIndex: 0,
      pageCount: 1,
      entryCount: 1,
      omittedCount: 0n,
      entries: Object.freeze([photoEntry]),
    }),
    inspectSelection: async () => Object.freeze({
      kind: 'single-file' as const,
      file: photoEntry,
      sourcePath: Object.freeze([photoEntry.name]),
      exactBytes: photoEntry.expectedSize,
    }),
    subscribeCatalogScanProgress: () => () => undefined,
    subscribePathActivity: () => () => undefined,
    subscribeConnection: (listener: (value: { kind: 'connected' }) => void) => {
      listener({ kind: 'connected' })
      return () => undefined
    },
    subscribeProtocolGeneration: () => () => undefined,
    beginPreviewConnectivity: preview.beginPreviewConnectivity,
    automaticPhotoPreview: previewSpy,
    preview: previewSpy,
    close: async () => undefined,
  } as unknown as JoinedBrowserShare
  return { share, previewSpy }
}

describe('ReceiverController replacement join lifecycle', () => {
  afterEach(() => vi.restoreAllMocks())

  it('clears breadcrumbs and paging during replacement join and restores share A with ready offers on failure', async () => {
    const windowPort = stubPortableOutput()
    const linkA = await makeShareLink(21)
    const linkB = await makeShareLink(22)
    const linkC = await makeShareLink(23)
    const shareA = makeMultiPageShare(21)
    const pendingB = deferred<JoinedBrowserShare>()
    const pendingC = deferred<JoinedBrowserShare>()
    let joinCStarted!: () => void
    const enteredC = new Promise<void>(resolve => { joinCStarted = resolve })
    const gateway = {
      join: async (input: string) => {
        if (input === linkA) return shareA.share
        if (input === linkB) return await pendingB.promise
        if (input === linkC) {
          joinCStarted()
          return await pendingC.promise
        }
        throw new Error('unexpected link')
      },
    } as unknown as BrowserReceiverGateway

    const controller = new ReceiverController(gateway, { windowPort })
    controller.initialize({ capabilityInput: linkA, pageUrl: PAGE_URL })
    await waitForController(controller, s => s.phase === 'browsing' && s.output.offers?.kind === 'ready')

    controller.openDirectory(shareA.folderEntry.idText)
    await waitForController(controller, s => s.breadcrumbs.length === 2 && s.output.offers?.kind === 'ready')
    controller.showPage(1)
    await waitForController(controller, s => s.pageIndex === 1)
    controller.toggleSelection(shareA.nestedFile.idText)
    await waitForController(controller, s => s.draft.mode === 'selection' && s.output.offers?.kind === 'ready')

    const inspectCountBeforeReplace = shareA.inspectCalls

    // Start replacement join B: browsing fields and paging must clear so InitialJoinLoading is visible.
    controller.openLocation({ capabilityInput: linkB, pageUrl: PAGE_URL })
    const joining = await waitForController(controller, s => s.phase === 'joining')
    expect(joining.share).toBeNull()
    expect(joining.breadcrumbs).toEqual([])
    expect(joining.rows).toEqual([])
    expect(joining.pageIndex).toBe(0)
    expect(joining.pageCount).toBe(0)
    expect(joining.entryCount).toBe(0)
    expect(joining.omittedCount).toBe(0n)
    expect(joining.selectedVisibleFiles).toBe(0)
    expect(joining.selectedVisibleBytes).toBe(0n)
    expect(joining.directoryRetryable).toBe(false)
    expect(controller.presentation.task.getSnapshot().hasContent).toBe(false)
    expect(controller.presentation.surface.getSnapshot()).toBe(true)

    // Supersede B with C while still joining; Share A's suspended state must survive.
    controller.openLocation({ capabilityInput: linkC, pageUrl: PAGE_URL })
    await enteredC

    // Fail C with relay access denied: Share A must restore completely without blocking downloads.
    pendingC.reject(new RelayReceiverError('denied', {
      relayError: { code: RELAY_ERROR.accessDenied, retryAfterMilliseconds: 0 },
    }))
    const restored = await waitForController(
      controller,
      s => s.phase === 'browsing' && s.output.offers?.kind === 'ready',
    )
    expect(shareA.closes).toBe(0)
    expect(shareA.inspectCalls).toBeGreaterThan(inspectCountBeforeReplace)
    expect(restored.connection).toEqual({ kind: 'connected' })
    expect(restored.share?.shareInstance).toBe(shareA.share.descriptor.shareInstanceId)
    expect(restored.breadcrumbs.map(crumb => crumb.name)).toEqual([null, 'Docs'])
    expect(restored.rows.map(row => row.id)).toEqual([shareA.nestedFile.idText])
    expect(restored.pageIndex).toBe(1)
    expect(restored.pageCount).toBe(2)
    expect(restored.omittedCount).toBe(2n)
    expect(restored.draft.mode).toBe('selection')
    expect(restored.selectedVisibleFiles).toBe(1)
    expect(restored.error).toEqual({ kind: 'relayAccessDenied', detail: expect.any(String) })
    expect(restored.startAdmission.allowed).toBe(true)
    expect(controller.presentation.task.getSnapshot().hasContent).toBe(true)
    expect(controller.downloadFile(shareA.nestedFile.idText)).toEqual({ kind: 'started' })

    await controller.dispose()
  })

  it('restores a single-photo share identity, offers, and inline preview after a failed replacement', async () => {
    const windowPort = stubPortableOutput()
    const linkA = await makeShareLink(31)
    const linkB = await makeShareLink(32)
    const photoShare = makeSinglePhotoShare(31)
    const gateway = {
      join: async (input: string) => {
        if (input === linkA) return photoShare.share
        throw new Error('sender offline')
      },
    } as unknown as BrowserReceiverGateway

    const controller = new ReceiverController(gateway, { windowPort })
    controller.initialize({ capabilityInput: linkA, pageUrl: PAGE_URL })
    await waitForController(
      controller,
      s => s.share?.kind === 'photo' && s.preview.state === 'image' && s.output.offers?.kind === 'ready',
    )

    controller.openLocation({ capabilityInput: linkB, pageUrl: PAGE_URL })
    const restored = await waitForController(
      controller,
      s => s.phase === 'browsing' && s.error !== null && s.preview.state === 'image' && s.output.offers?.kind === 'ready',
    )
    expect(restored.share?.kind).toBe('photo')
    expect(restored.error).toEqual({ kind: 'open', detail: 'sender offline' })
    expect(photoShare.previewSpy).toHaveBeenCalledTimes(2)

    await controller.dispose()
  })

  it('restores share A cleanly on cancelJoin during replacement and stays idempotent on repeat cancel', async () => {
    const windowPort = stubPortableOutput()
    const linkA = await makeShareLink(41)
    const linkB = await makeShareLink(42)
    const shareA = makeMultiPageShare(41)
    let joinBSignal: AbortSignal | undefined
    let joinBStarted!: () => void
    const enteredB = new Promise<void>(resolve => { joinBStarted = resolve })
    const gateway = {
      join: async (input: string, _pageUrl: string, signal?: AbortSignal) => {
        if (input === linkA) return shareA.share
        joinBSignal = signal
        joinBStarted()
        return await new Promise<JoinedBrowserShare>((_resolve, reject) => {
          signal?.addEventListener('abort', () => reject(signal.reason), { once: true })
        })
      },
    } as unknown as BrowserReceiverGateway

    const controller = new ReceiverController(gateway, { windowPort })
    controller.initialize({ capabilityInput: linkA, pageUrl: PAGE_URL })
    await waitForController(controller, s => s.phase === 'browsing' && s.output.offers?.kind === 'ready')

    controller.openLocation({ capabilityInput: linkB, pageUrl: PAGE_URL })
    await enteredB
    expect(controller.getSnapshot().phase).toBe('joining')

    controller.cancelJoin()
    controller.cancelJoin()
    expect(joinBSignal?.aborted).toBe(true)

    const restored = await waitForController(
      controller,
      s => s.phase === 'browsing' && s.output.offers?.kind === 'ready',
    )
    expect(shareA.closes).toBe(0)
    expect(restored.error).toBeNull()
    expect(restored.connection).toEqual({ kind: 'connected' })
    expect(restored.share?.shareInstance).toBe(shareA.share.descriptor.shareInstanceId)
    expect(restored.breadcrumbs).toHaveLength(1)
    expect(restored.rows).toHaveLength(2)
    expect(restored.pageCount).toBe(2)

    await controller.dispose()
  })

  it('clears browsing fields and exits receiver surface when cancelling an initial join during root browse', async () => {
    const windowPort = stubPortableOutput()
    const linkA = await makeShareLink(51)
    const rootGate = deferred<BrowsePage>()
    let closed = false
    const rootId = identityText(151)
    const root = makeDirectory(151, rootId, [], [rootId])
    const uncommittedShare = {
      descriptor: {
        shareInstance: identity(51),
        shareInstanceId: identityText(51),
        syntheticRoot: identity(151),
        syntheticRootId: rootId,
      },
      recoveryIdentity: 'initial.51',
      protocolSessionId: identityText(101),
      protocolSessionIdentity: identityText(101),
      selection: new SelectionPolicy(true),
      replaceSelection(this: { selection: SelectionPolicy }, next: SelectionPolicy) { this.selection = next },
      rootDirectory: () => root,
      page: () => rootGate.promise,
      subscribeCatalogScanProgress: () => () => undefined,
      subscribePathActivity: () => () => undefined,
      subscribeConnection: (listener: (value: { kind: 'connected' }) => void) => {
        listener({ kind: 'connected' })
        return () => undefined
      },
      subscribeProtocolGeneration: () => () => undefined,
      close: async () => { closed = true },
    } as unknown as JoinedBrowserShare
    const gateway = {
      join: async () => uncommittedShare,
    } as unknown as BrowserReceiverGateway

    const controller = new ReceiverController(gateway, { windowPort })
    controller.initialize({ capabilityInput: linkA, pageUrl: PAGE_URL })
    await waitForController(controller, s => s.browse.kind === 'loading')

    controller.cancelJoin()
    const cancelled = controller.getSnapshot()
    expect(closed).toBe(true)
    expect(cancelled.phase).toBe('awaiting-key')
    expect(cancelled.status).toBe('cancelled')
    expect(cancelled.share).toBeNull()
    expect(cancelled.breadcrumbs).toEqual([])
    expect(cancelled.pageCount).toBe(0)
    expect(cancelled.omittedCount).toBe(0n)
    expect(controller.presentation.task.getSnapshot().hasContent).toBe(false)
    expect(controller.presentation.surface.getSnapshot()).toBe(false)

    await controller.dispose()
  })
})

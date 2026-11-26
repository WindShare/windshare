import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  BrowserReceiverGateway,
  type BrowseDirectory,
  type BrowserJoinOptions,
  type JoinedBrowserShare,
} from '../../src/application/browser-share/gateway'
import type { ShareReport } from '../../src/application/network/report'
import type { ReceiveWindowPort } from '../../src/application/receive/coordinator'
import { SelectionPolicy } from '../../src/catalog/selection'
import { defaultICEEndpointPool } from '../../src/connectivity/ice-policy/defaults'
import { encodeSuite02CapabilityKey } from '../../src/crypto/suite02-link'
import * as outputContract from '../../src/output/backends/contract'
import { RelayCandidates } from '../../src/receiver/relay-selection/candidates'
import { RelayProtocolError } from '../../src/transport/relay/protocol'
import * as relayReceiver from '../../src/transport/relay/receiver'
import { ReceiverController } from '../../src/ui/controller'
import { createMockDownloadWindow } from '../output/opfs-fixture'
import { diagnosticImageJoined, entry, identity, identityText, waitForController } from './v2-preview-fixture'

const PAGE_URL = 'https://windshare.example/'

async function makeShareLink(seed: number): Promise<string> {
  const key = await encodeSuite02CapabilityKey(new Uint8Array(16).fill(seed), new Uint8Array(16).fill(seed))
  return `${PAGE_URL}s/${key.shareId}?r=relay.invalid#${key.encoded}`
}

class FakeShareReport implements ShareReport {
  #url: string | undefined
  #closed = false
  readonly #listeners = new Set<() => void>()
  closedCount = 0

  constructor(initial?: string) {
    this.#url = initial
  }

  get url(): string | undefined { return this.#url }
  get closed(): boolean { return this.#closed }

  publish(url: string): void {
    if (this.#closed || this.#url === url) return
    this.#url = url
    for (const listener of this.#listeners) listener()
  }

  subscribe(listener: () => void): () => void {
    if (this.#closed) return () => undefined
    this.#listeners.add(listener)
    return () => { this.#listeners.delete(listener) }
  }

  close(): void {
    if (this.#closed) return
    this.#closed = true
    this.closedCount += 1
    this.#url = undefined
    this.#listeners.clear()
  }
}

function makeJoinedShare(seed: number, report?: ShareReport, onClose?: () => void): JoinedBrowserShare {
  const closeReport = () => report?.close()
  const rootId = identityText(seed + 100)
  const root: BrowseDirectory = Object.freeze({
    id: identity(seed + 100),
    idText: rootId,
    path: Object.freeze([]),
    ancestry: Object.freeze([rootId]),
  })
  const preview = diagnosticImageJoined()
  return {
    descriptor: {
      shareInstance: identity(seed), shareInstanceId: identityText(seed),
      syntheticRoot: identity(seed + 100), syntheticRootId: rootId,
    },
    recoveryIdentity: `share.${seed}`,
    report,
    protocolSessionId: identityText(seed + 50),
    protocolSessionIdentity: identityText(seed + 50),
    selection: new SelectionPolicy(true),
    replaceSelection(this: { selection: SelectionPolicy }, next: SelectionPolicy) { this.selection = next },
    rootDirectory: () => root,
    page: async (directory: BrowseDirectory) => ({
      directory, pageIndex: 0, pageCount: 1, entryCount: 1, omittedCount: 0n, entries: [entry],
    }),
    inspectSelection: async () => ({
      kind: 'tree' as const, suggestedName: 'Album', rootPath: [], knownFileCount: 1, knownBytes: 16n, complete: true,
    }),
    subscribeCatalogScanProgress: () => () => undefined,
    subscribePathActivity: () => () => undefined,
    subscribeConnection: (listener: (value: { kind: 'connected' }) => void) => {
      listener({ kind: 'connected' })
      return () => undefined
    },
    subscribeProtocolGeneration: () => () => undefined,
    beginPreviewConnectivity: preview.beginPreviewConnectivity,
    automaticPhotoPreview: preview.preview,
    preview: preview.preview,
    beginDownloadConnectivity: () => ({ routes: [], close: vi.fn() }),
    createTransferJob: () => {
      let resolveRun!: (result: {
        status: 'stopped'
        completedFiles: bigint
        completedBytes: bigint
        discoveredFiles: bigint
        discoveredBytes: bigint
        discoveryComplete: boolean
        failedFiles: bigint
        failedDirectories: bigint
      }) => void
      return {
        pause: vi.fn(),
        stop: vi.fn(() => resolveRun({
          status: 'stopped',
          completedFiles: 0n, completedBytes: 0n,
          discoveredFiles: 1n, discoveredBytes: 16n, discoveryComplete: true,
          failedFiles: 0n, failedDirectories: 0n,
        })),
        detach: vi.fn(),
        run: () => new Promise(resolve => { resolveRun = resolve }),
      }
    },
    close: async () => {
      closeReport()
      onClose?.()
    },
  } as unknown as JoinedBrowserShare
}

function stubPortableOutput() {
  vi.spyOn(outputContract, 'probeBrowserOutputCapabilities').mockResolvedValue(Object.freeze({
    indexedDb: false, fsa: false, opfs: false, portable: true, availableStorageBytes: null,
  }))
  const { windowPort } = createMockDownloadWindow()
  return windowPort as unknown as ReceiveWindowPort
}

describe('ReceiverController share report lifecycle', () => {
  afterEach(() => vi.restoreAllMocks())

  it('preserves the active official report URL across blocked, reused, and failed replacement navigations', async () => {
    const windowPort = stubPortableOutput()
    const linkA = await makeShareLink(1)
    const linkB = await makeShareLink(2)
    const reportA = new FakeShareReport('https://authority.example/report#share=SHARE_A')
    const shareA = makeJoinedShare(1, reportA)
    const reportB = new FakeShareReport('https://authority.example/report#share=SHARE_B')
    let joinCalls = 0
    const intents: string[] = []
    const gateway = {
      join: async (input: string, _pageUrl: string, signal?: AbortSignal, options?: BrowserJoinOptions) => {
        joinCalls += 1
        if (input === linkA) {
          options?.onReport?.(reportA)
          return shareA
        }
        if (input === linkB) {
          const adopted = options?.onReport?.(reportB) === true
          if (!adopted || signal?.aborted) reportB.close()
          throw new Error('sender offline')
        }
        throw new Error('invalid link')
      },
    } as unknown as BrowserReceiverGateway

    const controller = new ReceiverController(gateway, {
      windowPort,
      trace: {
        current: event => {
          if (event.name === 'receiver_experience' && event.transition === 'intent') intents.push(event.action)
        },
      },
    })
    controller.initialize({ capabilityInput: linkA, pageUrl: PAGE_URL })
    await waitForController(controller, s => s.phase === 'browsing' && s.rows.length === 1)
    expect(controller.getSnapshot().reportUrl).toBe('https://authority.example/report#share=SHARE_A')
    expect(controller.presentation.report.getSnapshot()).toBe('https://authority.example/report#share=SHARE_A')

    // Reused navigation (same capability link) must keep the active report URL without re-joining.
    controller.openLocation({ capabilityInput: linkA, pageUrl: PAGE_URL })
    await vi.waitFor(() => expect(intents).toContain('share-navigation-reused'))
    expect(joinCalls).toBe(1)
    expect(controller.getSnapshot().reportUrl).toBe('https://authority.example/report#share=SHARE_A')

    // Blocked navigation during an active download must keep the active report URL.
    expect(controller.downloadFile(entry.idText)).toEqual({ kind: 'started' })
    controller.openLocation({ capabilityInput: linkB, pageUrl: PAGE_URL })
    await waitForController(controller, s => typeof s.error === 'object' && s.error?.kind === 'blocked')
    await vi.waitFor(() => expect(intents).toContain('share-navigation-blocked'))
    expect(joinCalls).toBe(1)
    expect(controller.getSnapshot().reportUrl).toBe('https://authority.example/report#share=SHARE_A')

    // Stop the active download so replacement can be attempted and fail.
    controller.performLifecycleAction('stop')
    await waitForController(controller, s => s.activeReceiveOperationId === null)

    // Failed replacement to another official share must keep share A's report URL and close candidate B.
    controller.openLocation({ capabilityInput: linkB, pageUrl: PAGE_URL })
    await waitForController(controller, s =>
      s.phase === 'browsing' && typeof s.error === 'object' && s.error !== null &&
      'kind' in s.error && s.error.kind === 'open' && s.error.detail === 'sender offline')
    expect(controller.getSnapshot().reportUrl).toBe('https://authority.example/report#share=SHARE_A')
    expect(reportA.closed).toBe(false)
    expect(reportB.closed).toBe(true)

    // Failed replacement with an unparseable link must also keep share A's report URL.
    controller.openLocation({ capabilityInput: 'not-a-valid-link', pageUrl: PAGE_URL })
    await waitForController(controller, s =>
      s.phase === 'browsing' && typeof s.error === 'object' && s.error !== null &&
      'kind' in s.error && s.error.kind === 'open' && s.error.detail === 'invalid link')
    expect(controller.getSnapshot().reportUrl).toBe('https://authority.example/report#share=SHARE_A')

    await controller.dispose()
    expect(reportA.closed).toBe(true)
  })

  it('never leaks an official candidate report URL onto a committed self-hosted share', async () => {
    const windowPort = stubPortableOutput()
    const selfHostedLink = await makeShareLink(3)
    const officialLink = await makeShareLink(4)
    const selfHosted = makeJoinedShare(3, undefined)
    const officialCandidate = new FakeShareReport('https://authority.example/report#share=OFFICIAL_B')
    const gateway = {
      join: async (input: string, _pageUrl: string, signal?: AbortSignal, options?: BrowserJoinOptions) => {
        if (input === selfHostedLink) return selfHosted
        const adopted = options?.onReport?.(officialCandidate) === true
        if (!adopted || signal?.aborted) officialCandidate.close()
        throw new Error('official B unreachable')
      },
    } as unknown as BrowserReceiverGateway

    const controller = new ReceiverController(gateway, { windowPort })
    controller.initialize({ capabilityInput: selfHostedLink, pageUrl: PAGE_URL })
    await waitForController(controller, s => s.phase === 'browsing')
    expect(controller.getSnapshot().reportUrl).toBeNull()

    controller.openLocation({ capabilityInput: officialLink, pageUrl: PAGE_URL })
    await waitForController(controller, s => s.phase === 'browsing' && s.error !== null)
    expect(controller.getSnapshot().reportUrl).toBeNull()
    expect(officialCandidate.closed).toBe(true)

    await controller.dispose()
  })

  it('retains initial official report across connecting and initial failure, and clears it on cancel or next open', async () => {
    const windowPort = stubPortableOutput()
    const offlineLink = await makeShareLink(5)
    const cancelLink = await makeShareLink(6)
    const offlineReport = new FakeShareReport()
    let failOffline!: (error: unknown) => void
    const cancellingReport = new FakeShareReport('https://authority.example/report#share=CANCELLED')
    const gateway = {
      join: async (input: string, _pageUrl: string, signal?: AbortSignal, options?: BrowserJoinOptions) => {
        if (input === offlineLink) {
          options?.onReport?.(offlineReport)
          return await new Promise<JoinedBrowserShare>((_resolve, reject) => { failOffline = reject })
        }
        if (input === cancelLink) {
          options?.onReport?.(cancellingReport)
          return await new Promise<JoinedBrowserShare>((_resolve, reject) => {
            signal?.addEventListener('abort', () => reject(signal.reason), { once: true })
          })
        }
        throw new Error('bad key')
      },
    } as unknown as BrowserReceiverGateway

    const controller = new ReceiverController(gateway, { windowPort })
    const browsingNotified = vi.fn()
    const reportNotified = vi.fn()
    controller.presentation.browsing.subscribe(browsingNotified)
    controller.presentation.report.subscribe(reportNotified)

    controller.initialize({ capabilityInput: offlineLink, pageUrl: PAGE_URL })
    await waitForController(controller, s => s.phase === 'joining')
    expect(controller.getSnapshot().reportUrl).toBeNull()

    // Initial join fails (e.g. sender offline); report watcher remains active and late directory confirmation works.
    failOffline(new Error('sender offline'))
    await waitForController(controller, s => s.phase === 'failed')
    const browsingCallsBeforeLateReport = browsingNotified.mock.calls.length
    offlineReport.publish('https://authority.example/report#share=OFFLINE_SHARE')
    expect(controller.getSnapshot().reportUrl).toBe('https://authority.example/report#share=OFFLINE_SHARE')
    expect(controller.presentation.report.getSnapshot()).toBe('https://authority.example/report#share=OFFLINE_SHARE')
    // Report URL updates must only trigger the report projection region.
    expect(reportNotified).toHaveBeenCalledTimes(1)
    expect(browsingNotified).toHaveBeenCalledTimes(browsingCallsBeforeLateReport)

    // Starting a new join closes the previous failed attempt's report immediately.
    controller.openLocation({ capabilityInput: cancelLink, pageUrl: PAGE_URL })
    await waitForController(controller, s => s.phase === 'joining' && s.reportUrl === 'https://authority.example/report#share=CANCELLED')
    expect(offlineReport.closed).toBe(true)

    // Cancelling returns to awaiting-key with no ghost report button.
    controller.cancelJoin()
    expect(controller.getSnapshot().phase).toBe('awaiting-key')
    expect(controller.getSnapshot().reportUrl).toBeNull()
    expect(cancellingReport.closed).toBe(true)

    // Submitting an invalid key from KeyForm must keep reportUrl null.
    controller.submitKey('invalid-key-input')
    await waitForController(controller, s => s.phase === 'failed')
    expect(controller.getSnapshot().reportUrl).toBeNull()

    await controller.dispose()
  })

  it('preserves late report discovery after commit, swaps cleanly on replacement, and restores committed share report on replacement cancel', async () => {
    const windowPort = stubPortableOutput()
    const linkA = await makeShareLink(10)
    const linkB = await makeShareLink(11)
    const linkC = await makeShareLink(12)
    const hybridReport = new FakeShareReport()
    const shareA = makeJoinedShare(10, hybridReport)
    const reportB = new FakeShareReport('https://authority.example/report#share=SHARE_B')
    const shareB = makeJoinedShare(11, reportB)
    const reportC = new FakeShareReport('https://authority.example/report#share=SHARE_C')
    let pendingCStarted!: () => void
    const enteredPendingC = new Promise<void>(resolve => { pendingCStarted = resolve })
    const gateway = {
      join: async (input: string, _pageUrl: string, signal?: AbortSignal, options?: BrowserJoinOptions) => {
        if (input === linkA) {
          options?.onReport?.(hybridReport)
          return shareA
        }
        if (input === linkB) {
          options?.onReport?.(reportB)
          return shareB
        }
        const adopted = options?.onReport?.(reportC) === true
        pendingCStarted()
        return await new Promise<JoinedBrowserShare>((_resolve, reject) => {
          signal?.addEventListener('abort', () => {
            if (!adopted || signal.aborted) reportC.close()
            reject(signal.reason)
          }, { once: true })
        })
      },
    } as unknown as BrowserReceiverGateway

    const controller = new ReceiverController(gateway, { windowPort })
    controller.initialize({ capabilityInput: linkA, pageUrl: PAGE_URL })
    await waitForController(controller, s => s.phase === 'browsing')
    expect(controller.getSnapshot().reportUrl).toBeNull()

    // Late directory discovery after JoinedBrowserShare committed must surface immediately.
    hybridReport.publish('https://authority.example/report#share=HYBRID_A')
    expect(controller.getSnapshot().reportUrl).toBe('https://authority.example/report#share=HYBRID_A')
    expect(hybridReport.closed).toBe(false)

    // Successful replacement swaps to share B and closes share A's report.
    controller.openLocation({ capabilityInput: linkB, pageUrl: PAGE_URL })
    await waitForController(controller, s => s.phase === 'browsing' && s.reportUrl === 'https://authority.example/report#share=SHARE_B')
    expect(hybridReport.closed).toBe(true)

    // Cancelling while replacing B with C closes candidate C's report and restores committed share B.
    controller.openLocation({ capabilityInput: linkC, pageUrl: PAGE_URL })
    await enteredPendingC
    expect(controller.getSnapshot().reportUrl).toBe('https://authority.example/report#share=SHARE_B')
    controller.cancelJoin()
    expect(controller.getSnapshot().phase).toBe('browsing')
    expect(controller.getSnapshot().share?.shareInstance).toBe(shareB.descriptor.shareInstanceId)
    expect(controller.getSnapshot().reportUrl).toBe('https://authority.example/report#share=SHARE_B')
    expect(reportB.closed).toBe(false)
    expect(reportC.closed).toBe(true)

    await controller.dispose()
    expect(reportB.closed).toBe(true)
  })

  it('closes resolved report handles in BrowserReceiverGateway when unadopted join fails or aborts', async () => {
    vi.spyOn(relayReceiver, 'dialRelayReceiver').mockRejectedValue(new RelayProtocolError('malformed', 'terminal failure'))
    const link = await makeShareLink(9)
    const unadoptedReport = new FakeShareReport('https://authority.example/report#share=UNADOPTED')
    const adoptedFailureReport = new FakeShareReport('https://authority.example/report#share=ADOPTED_FAIL')
    const abortedReport = new FakeShareReport('https://authority.example/report#share=ABORTED')
    let nextReport = unadoptedReport
    const gateway = new BrowserReceiverGateway({
      network: {
        icePool: defaultICEEndpointPool,
        resolve: () => ({
          candidates: new RelayCandidates(['https://relay.invalid']),
          report: nextReport,
        }),
      },
    })

    await expect(gateway.join(link, PAGE_URL)).rejects.toThrow()
    expect(unadoptedReport.closed).toBe(true)

    nextReport = adoptedFailureReport
    await expect(gateway.join(link, PAGE_URL, undefined, { onReport: () => true })).rejects.toThrow()
    expect(adoptedFailureReport.closed).toBe(false)
    adoptedFailureReport.close()

    nextReport = abortedReport
    const abort = new AbortController()
    const adoptedButAborted = gateway.join(link, PAGE_URL, abort.signal, {
      onReport: () => {
        abort.abort(new DOMException('cancelled', 'AbortError'))
        return true
      },
    })
    await expect(adoptedButAborted).rejects.toThrow()
    expect(abortedReport.closed).toBe(true)
  })
})

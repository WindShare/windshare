import { createElement } from 'react'
import { createRoot, type Root } from 'react-dom/client'

import {
  createChunkSet,
  type CapabilityLink,
  type ManifestEntry,
  type TransferPlan,
  type ValidatedManifestV1,
} from '../../src/contracts'
import { ReceiverApp } from '../../src/ui/ReceiverApp'
import { ReceiverController } from '../../src/ui/controller'
import {
  ReceiverPublicError,
  SELECTION_PAGE_ROWS,
  emptyProgress,
  type JoinedShare,
  type ReceiverGateway,
  type ReceiverTransferObserver,
} from '../../src/ui/model'

const SHARE_ID = 'AAECAwQFBgcI'
const BARE_URL = `https://windshare.test/v1/ws/${SHARE_ID}?r=https%3A%2F%2Frelay.test`
const ENTRIES = [
  { kind: 'directory', path: 'docs', mtime: 0 },
  { kind: 'file', path: 'docs/a.txt', size: 3, mtime: 0 },
  { kind: 'file', path: 'docs/b.txt', size: 5, mtime: 0 },
] as unknown as readonly ManifestEntry[]
const WIDE_ENTRIES = Array.from({ length: SELECTION_PAGE_ROWS + 5 }, (_, index) => ({
  kind: 'file' as const,
  path: `wide-${index.toString().padStart(4, '0')}.bin`,
  size: 1,
  mtime: 0,
})) as unknown as readonly ManifestEntry[]
const CAPABILITY = {
  suite: 1,
  shareId: SHARE_ID,
  readSecret: new Uint8Array(16),
  relayHints: ['https://relay.test'],
} as unknown as CapabilityLink

export type HarnessMode = 'ready' | 'key' | 'key-pending' | 'wide'

class Deferred {
  readonly promise: Promise<void>
  resolve!: () => void
  reject!: (reason: unknown) => void

  constructor() {
    this.promise = new Promise<void>((resolve, reject) => {
      this.resolve = resolve
      this.reject = reject
    })
  }
}

class HarnessShare implements JoinedShare {
  readonly manifest: ValidatedManifestV1

  constructor(entries: readonly ManifestEntry[]) {
    this.manifest = {
      version: 1,
      chunkSize: 1024,
      entries,
    } as unknown as ValidatedManifestV1
  }

  async close(): Promise<void> {}
}

function planFor(
  manifestEntries: readonly ManifestEntry[],
  selectors: readonly string[] | null,
): TransferPlan {
  const entries =
    selectors === null
      ? manifestEntries
      : manifestEntries.filter((entry) =>
          selectors.some(
            (selector) => entry.path === selector || entry.path.startsWith(`${selector}/`),
          ),
        )
  const selectedBytes = entries.reduce(
    (bytes, entry) => bytes + (entry.kind === 'file' ? entry.size : 0),
    0,
  )
  return {
    planId: new Uint8Array(32),
    selectedEntries: entries,
    selectedBytes,
    chunks: createChunkSet(selectedBytes === 0 ? [] : [{ first: 0, end: 2 }]),
  } as unknown as TransferPlan
}

class HarnessGateway implements ReceiverGateway {
  readonly outputChoices = Object.freeze([
    Object.freeze({
      id: 'folder' as const,
      label: 'Files and folders',
      description: 'Folder output',
      available: true,
    }),
    Object.freeze({
      id: 'download' as const,
      label: 'Browser download',
      description: 'Download output',
      available: true,
    }),
  ])
  readonly share: HarnessShare
  startCalls = 0
  aborted = false
  abortCleanupFails = false
  cancelOutput = false
  observer: ReceiverTransferObserver | undefined
  transfer: Deferred | undefined
  pendingJoin: {
    readonly promise: Promise<JoinedShare>
    reject(reason: unknown): void
  } | undefined
  joinedCapability: CapabilityLink | undefined

  constructor(delayJoin: boolean, entries: readonly ManifestEntry[]) {
    this.share = new HarnessShare(entries)
    if (delayJoin) {
      let rejectJoin!: (reason: unknown) => void
      const promise = new Promise<JoinedShare>((_resolve, reject) => {
        rejectJoin = reject
      })
      this.pendingJoin = { promise, reject: rejectJoin }
    }
  }

  async join(capability: CapabilityLink, signal: AbortSignal): Promise<JoinedShare> {
    this.joinedCapability = capability
    signal.throwIfAborted()
    if (this.pendingJoin !== undefined) {
      return this.pendingJoin.promise
    }
    return this.share
  }

  async compileSelection(
    _share: JoinedShare,
    selectors: readonly string[] | null,
    signal: AbortSignal,
  ): Promise<TransferPlan> {
    signal.throwIfAborted()
    return planFor(this.share.manifest.entries, selectors)
  }

  start(
    _share: JoinedShare,
    plan: TransferPlan,
    _choice: 'folder' | 'download',
    observer: ReceiverTransferObserver,
    signal: AbortSignal,
  ): Promise<void> {
    this.startCalls += 1
    if (this.cancelOutput) {
      this.cancelOutput = false
      return Promise.reject(
        new ReceiverPublicError('output-cancelled', 'The save prompt was canceled.'),
      )
    }
    this.observer = observer
    this.transfer = new Deferred()
    observer.started(Object.freeze({
      ...emptyProgress(),
      totalBytes: plan.selectedBytes,
      totalBlocks: plan.chunks.count,
      channels: 1,
    }))
    signal.addEventListener('abort', () => {
      this.aborted = true
      this.transfer?.reject(
        this.abortCleanupFails
          ? new ReceiverPublicError(
              'transfer-failed',
              'The download could not be completed safely.',
            )
          : signal.reason,
      )
    }, { once: true })
    return this.transfer.promise
  }
}

let root: Root | undefined
let controller: ReceiverController | undefined
let gateway: HarnessGateway | undefined

function requireHarness(): { controller: ReceiverController; gateway: HarnessGateway } {
  if (controller === undefined || gateway === undefined) {
    throw new Error('C4 harness is not mounted')
  }
  return { controller, gateway }
}

export async function mountC4Harness(mode: HarnessMode = 'ready'): Promise<void> {
  await controller?.dispose()
  root?.unmount()
  document.getElementById('root')?.remove()
  const container = document.createElement('div')
  container.id = 'root'
  document.body.append(container)

  gateway = new HarnessGateway(
    mode === 'key-pending',
    mode === 'wide' ? WIDE_ENTRIES : ENTRIES,
  )
  controller = new ReceiverController(gateway)
  controller.initialize(
    mode === 'key' || mode === 'key-pending'
      ? { kind: 'needs-key', bareUrl: BARE_URL }
      : { kind: 'ready', capability: CAPABILITY },
  )
  root = createRoot(container)
  root.render(createElement(ReceiverApp, { controller }))
}

export function harnessState() {
  const harness = requireHarness()
  return {
    startCalls: harness.gateway.startCalls,
    aborted: harness.gateway.aborted,
    phase: harness.controller.getSnapshot().phase,
    joinedCapabilityCleared:
      harness.gateway.joinedCapability?.readSecret.every((byte) => byte === 0) ?? false,
  }
}

export function failPendingJoin(): void {
  const { gateway: active } = requireHarness()
  active.pendingJoin?.reject(new Error('PRIVATE-LATE-JOIN-FAILURE'))
}

export function emitProgress(): void {
  const { gateway: active } = requireHarness()
  active.observer?.progress(Object.freeze({
    writtenBytes: 3,
    totalBytes: 8,
    completedBlocks: 1,
    totalBlocks: 2,
    retryBlocks: 1,
    channels: 1,
    bufferedBlocks: 1,
    maxBufferedBlocks: 2,
  }))
}

export function cancelNextOutput(): void {
  requireHarness().gateway.cancelOutput = true
}

export function failAbortCleanup(): void {
  requireHarness().gateway.abortCleanupFails = true
}

export function emitReconnect(): void {
  requireHarness().gateway.observer?.reconnecting(2)
}

export function emitReconnected(): void {
  const { gateway: active } = requireHarness()
  active.observer?.reconnected(Object.freeze({
    writtenBytes: 3,
    totalBytes: 8,
    completedBlocks: 1,
    totalBlocks: 2,
    retryBlocks: 0,
    channels: 1,
    bufferedBlocks: 0,
    maxBufferedBlocks: 2,
  }))
}

export function completeTransfer(): void {
  const { gateway: active } = requireHarness()
  active.observer?.progress(Object.freeze({
    writtenBytes: 8,
    totalBytes: 8,
    completedBlocks: 2,
    totalBlocks: 2,
    retryBlocks: 0,
    channels: 1,
    bufferedBlocks: 0,
    maxBufferedBlocks: 2,
  }))
  // Resolve after the Playwright evaluation turn; otherwise Chromium can retire
  // that isolated evaluation realm while React flushes the completion update.
  setTimeout(() => active.transfer?.resolve(), 0)
}

export function failTerminal(): void {
  requireHarness().gateway.transfer?.reject(
    new ReceiverPublicError(
      'peer-terminal',
      'The sender stopped the transfer: source file changed',
    ),
  )
}

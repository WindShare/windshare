import { describe, expect, it } from 'vitest'

import {
  createChunkSet,
  type CapabilityLink,
  type ManifestEntry,
  type TransferPlan,
  type ValidatedManifestV1,
} from '../../src/contracts'
import { encodeCapabilityKey, parseCapabilityLink } from '../../src/crypto'
import {
  ReceiverController,
} from '../../src/ui/controller'
import {
  ReceiverPublicError,
  SELECTION_PAGE_ROWS,
  emptyProgress,
  type JoinedShare,
  type ReceiverGateway,
  type ReceiverTransferObserver,
} from '../../src/ui/model'

const SHARE_ID = 'AAECAwQFBgcI'
const OTHER_SHARE_ID = 'AAECAwQFBgcJ'
const KEY = encodeCapabilityKey(1, Uint8Array.from({ length: 16 }, (_, index) => index + 1))
const BARE_URL = `https://windshare.test/v1/ws/${SHARE_ID}?r=https%3A%2F%2Frelay.test`
const ENTRIES = [
  { kind: 'directory', path: 'docs', mtime: 0 },
  { kind: 'file', path: 'docs/a.txt', size: 3, mtime: 0 },
  { kind: 'file', path: 'docs/b.txt', size: 5, mtime: 0 },
] as unknown as readonly ManifestEntry[]
const MANIFEST = {
  version: 1,
  chunkSize: 1024,
  entries: ENTRIES,
} as unknown as ValidatedManifestV1

function freshCapability(): CapabilityLink {
  return parseCapabilityLink(`${BARE_URL}#${KEY}`)
}

class Deferred<T> {
  readonly promise: Promise<T>
  resolve!: (value: T) => void
  reject!: (reason: unknown) => void

  constructor() {
    this.promise = new Promise<T>((resolve, reject) => {
      this.resolve = resolve
      this.reject = reject
    })
  }
}

class FakeShare implements JoinedShare {
  readonly manifest: ValidatedManifestV1
  closed = 0

  constructor(manifest: ValidatedManifestV1 = MANIFEST) {
    this.manifest = manifest
  }

  async close(): Promise<void> {
    this.closed += 1
  }
}

function selectedEntries(selectors: readonly string[] | null): readonly ManifestEntry[] {
  if (selectors === null) {
    return ENTRIES
  }
  return ENTRIES.filter((entry) =>
    selectors.some((selector) => entry.path === selector || entry.path.startsWith(`${selector}/`)),
  )
}

function planFor(selectors: readonly string[] | null): TransferPlan {
  const entries = selectedEntries(selectors)
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

class FakeGateway implements ReceiverGateway {
  readonly outputChoices = Object.freeze([
    Object.freeze({
      id: 'folder' as const,
      label: 'Folder',
      description: 'Folder output',
      available: true,
    }),
    Object.freeze({
      id: 'download' as const,
      label: 'Download',
      description: 'Download output',
      available: true,
    }),
  ])
  readonly share = new FakeShare()
  startCalls = 0
  joinCalls = 0
  observer: ReceiverTransferObserver | undefined
  transfer: Deferred<void> | undefined
  signal: AbortSignal | undefined

  async join(capability: CapabilityLink, signal: AbortSignal): Promise<JoinedShare> {
    this.joinCalls += 1
    expect(capability.shareId).toBe(SHARE_ID)
    signal.throwIfAborted()
    return this.share
  }

  async compileSelection(
    share: JoinedShare,
    selectors: readonly string[] | null,
    signal: AbortSignal,
  ): Promise<TransferPlan> {
    expect(share.manifest).toBe(MANIFEST)
    signal.throwIfAborted()
    return planFor(selectors)
  }

  start(
    _share: JoinedShare,
    _plan: TransferPlan,
    _choice: 'folder' | 'download',
    observer: ReceiverTransferObserver,
    signal: AbortSignal,
  ): Promise<void> {
    this.startCalls += 1
    this.observer = observer
    this.signal = signal
    this.transfer = new Deferred<void>()
    signal.addEventListener('abort', () => this.transfer?.reject(signal.reason), { once: true })
    return this.transfer.promise
  }
}

async function settle(): Promise<void> {
  for (let count = 0; count < 8; count += 1) {
    await Promise.resolve()
  }
}

async function readyController(gateway = new FakeGateway()) {
  const controller = new ReceiverController(gateway)
  controller.initialize({ kind: 'ready', capability: freshCapability() })
  await settle()
  expect(controller.getSnapshot().phase).toBe('ready')
  return { controller, gateway }
}

describe('receiver controller', () => {
  it('keeps separate keys out of snapshots for raw, hash, and full-link input', async () => {
    for (const input of [KEY, `#${KEY}`, `${BARE_URL}#${KEY}`]) {
      const gateway = new FakeGateway()
      const controller = new ReceiverController(gateway)
      controller.initialize({ kind: 'needs-key', bareUrl: BARE_URL })
      controller.submitKey(input)
      expect(JSON.stringify(controller.getSnapshot())).not.toContain(KEY)
      await controller.dispose()
    }

    const controller = new ReceiverController(new FakeGateway())
    const invalidSecret = 'DO-NOT-REFLECT-IN-SNAPSHOT'
    controller.initialize({ kind: 'needs-key', bareUrl: BARE_URL })
    controller.submitKey(invalidSecret)
    expect(controller.getSnapshot().error).toBe('The separate key is invalid.')
    expect(JSON.stringify(controller.getSnapshot())).not.toContain(invalidSecret)
    await controller.dispose()
  })

  it('rejects a complete key link that names a different share', async () => {
    const gateway = new FakeGateway()
    const controller = new ReceiverController(gateway)
    controller.initialize({ kind: 'needs-key', bareUrl: BARE_URL })
    controller.submitKey(
      `https://windshare.test/v1/ws/${OTHER_SHARE_ID}?r=https%3A%2F%2Frelay.test#${KEY}`,
    )
    await settle()

    expect(controller.getSnapshot()).toMatchObject({
      phase: 'awaiting-key',
      error: 'The separate key is invalid.',
    })
    expect(gateway.joinCalls).toBe(0)
    expect(JSON.stringify(controller.getSnapshot())).not.toContain(KEY)
    await controller.dispose()
  })

  it('zeroizes submitted capability bytes before a late join failure offers a retry', async () => {
    const gateway = new FakeGateway()
    const joins = [new Deferred<JoinedShare>(), new Deferred<JoinedShare>()]
    const submitted: CapabilityLink[] = []
    gateway.join = (capability) => {
      submitted.push(capability)
      const join = joins[submitted.length - 1]
      if (join === undefined) throw new Error('unexpected join')
      return join.promise
    }
    const controller = new ReceiverController(gateway)
    controller.initialize({ kind: 'needs-key', bareUrl: BARE_URL })

    controller.submitKey(KEY)
    expect(controller.getSnapshot().phase).toBe('joining')
    expect(submitted[0]?.readSecret).toEqual(new Uint8Array(16))

    joins[0]?.reject(new Error('PRIVATE-LATE-JOIN-FAILURE'))
    await settle()
    expect(controller.getSnapshot()).toMatchObject({
      phase: 'awaiting-key',
      error: 'Could not connect to this share.',
    })
    expect(JSON.stringify(controller.getSnapshot())).not.toContain(KEY)

    controller.submitKey(KEY)
    expect(submitted[1]?.readSecret).toEqual(new Uint8Array(16))
    joins[1]?.resolve(gateway.share)
    await settle()
    expect(controller.getSnapshot().phase).toBe('ready')
    await controller.dispose()
  })

  it('zeroizes submitted capability bytes when join throws synchronously', async () => {
    const gateway = new FakeGateway()
    let submitted: CapabilityLink | undefined
    gateway.join = (capability) => {
      submitted = capability
      throw new Error('PRIVATE-SYNCHRONOUS-JOIN-FAILURE')
    }
    const controller = new ReceiverController(gateway)
    controller.initialize({ kind: 'needs-key', bareUrl: BARE_URL })

    controller.submitKey(KEY)

    expect(submitted?.readSecret).toEqual(new Uint8Array(16))
    expect(controller.getSnapshot()).toMatchObject({
      phase: 'awaiting-key',
      error: 'Could not connect to this share.',
    })
    await controller.dispose()
  })

  it('does not cross the transfer boundary before an explicit start call', async () => {
    const { controller, gateway } = await readyController()

    expect(gateway.startCalls).toBe(0)
    controller.toggleSelection('docs/a.txt')
    await settle()
    expect(gateway.startCalls).toBe(0)

    controller.startDownload()
    expect(gateway.startCalls).toBe(1)
    expect(controller.getSnapshot().phase).toBe('preparing-output')
    await controller.dispose()
  })

  it('keeps the rendered selection window bounded while every entry remains reachable', async () => {
    const entryCount = SELECTION_PAGE_ROWS * 2 + 17
    const wideEntries = Array.from({ length: entryCount }, (_, index) => ({
      kind: 'file' as const,
      path: `wide-${index.toString().padStart(4, '0')}.bin`,
      size: 1,
      mtime: 0,
    })) as unknown as readonly ManifestEntry[]
    const wideManifest = {
      version: 1,
      chunkSize: 1024,
      entries: wideEntries,
    } as unknown as ValidatedManifestV1
    const wideShare = new FakeShare(wideManifest)
    const gateway = new FakeGateway()
    gateway.join = async () => wideShare
    gateway.compileSelection = async () => ({
      planId: new Uint8Array(32),
      selectedEntries: wideEntries,
      selectedBytes: entryCount,
      chunks: createChunkSet([{ first: 0, end: 1 }]),
    }) as unknown as TransferPlan
    const controller = new ReceiverController(gateway)
    controller.initialize({ kind: 'ready', capability: freshCapability() })
    await settle()

    expect(controller.getSnapshot()).toMatchObject({
      manifestEntryCount: entryCount,
      selectionPageIndex: 0,
      selectionPageCount: 3,
    })
    expect(controller.getSnapshot().entries).toHaveLength(SELECTION_PAGE_ROWS)

    controller.showSelectionPage(2)
    expect(controller.getSnapshot().selectionPageIndex).toBe(2)
    expect(controller.getSnapshot().entries).toHaveLength(17)
    expect(controller.getSnapshot().entries[0]?.path).toBe('wide-0400.bin')
    await controller.dispose()
  })

  it('publishes progress, retries, reconnect state, and completion', async () => {
    const { controller, gateway } = await readyController()
    controller.startDownload()
    const started = Object.freeze({
      ...emptyProgress(),
      totalBytes: 8,
      totalBlocks: 2,
      channels: 1,
    })
    gateway.observer?.started(started)
    gateway.observer?.progress(Object.freeze({
      ...started,
      writtenBytes: 3,
      completedBlocks: 1,
      retryBlocks: 1,
      bufferedBlocks: 1,
      maxBufferedBlocks: 2,
    }))
    expect(controller.getSnapshot()).toMatchObject({
      phase: 'transferring',
      progress: { writtenBytes: 3, retryBlocks: 1, bufferedBlocks: 1 },
    })

    gateway.observer?.reconnecting(2)
    expect(controller.getSnapshot()).toMatchObject({
      phase: 'reconnecting',
      reconnectAttempt: 2,
    })
    gateway.observer?.reconnected(started)
    expect(controller.getSnapshot().phase).toBe('transferring')

    gateway.transfer?.resolve()
    await settle()
    expect(controller.getSnapshot().phase).toBe('completed')
    gateway.observer?.reconnecting(3)
    gateway.observer?.progress(emptyProgress())
    expect(controller.getSnapshot().phase).toBe('completed')
    await controller.dispose()
  })

  it('waits for abort cleanup before publishing the aborted state', async () => {
    const { controller, gateway } = await readyController()
    controller.startDownload()
    gateway.observer?.started(Object.freeze({
      ...emptyProgress(),
      totalBytes: 8,
      totalBlocks: 2,
      channels: 1,
    }))

    controller.abortDownload()
    expect(controller.getSnapshot().phase).toBe('aborting')
    expect(gateway.signal?.aborted).toBe(true)
    await settle()
    expect(controller.getSnapshot().phase).toBe('aborted')
    await controller.dispose()
  })

  it('reports abort cleanup failure instead of claiming partial output was cleaned', async () => {
    const { controller, gateway } = await readyController()
    gateway.start = (_share, _plan, _choice, _observer, signal) =>
      new Promise<void>((_resolve, reject) => {
        signal.addEventListener(
          'abort',
          () => reject(
            new ReceiverPublicError(
              'transfer-failed',
              'The download could not be completed safely.',
            ),
          ),
          { once: true },
        )
      })

    controller.startDownload()
    controller.abortDownload()
    await settle()

    expect(controller.getSnapshot()).toMatchObject({
      phase: 'failed',
      error: 'The download could not be completed safely.',
    })
    await controller.dispose()
  })

  it('shows only bounded public terminal errors', async () => {
    const { controller, gateway } = await readyController()
    controller.startDownload()
    gateway.transfer?.reject(
      new ReceiverPublicError('peer-terminal', 'The sender stopped the transfer: changed file'),
    )
    await settle()

    expect(controller.getSnapshot()).toMatchObject({
      phase: 'failed',
      error: 'The sender stopped the transfer: changed file',
    })
    await controller.dispose()
  })

  it('returns to the ready state when a save prompt is canceled', async () => {
    const { controller, gateway } = await readyController()
    gateway.start = () => Promise.reject(
      new ReceiverPublicError('output-cancelled', 'The save prompt was canceled.'),
    )
    controller.chooseOutput('download')
    controller.startDownload()
    await settle()

    expect(controller.getSnapshot()).toMatchObject({
      phase: 'ready',
      outputChoice: 'download',
      error: 'The save prompt was canceled.',
      canStart: true,
    })
    await controller.dispose()
  })

  it('does not expose arbitrary join failure details', async () => {
    const gateway = new FakeGateway()
    const secret = 'INTERNAL-SECRET-ERROR'
    gateway.join = () => Promise.reject(new Error(secret))
    const controller = new ReceiverController(gateway)
    controller.initialize({ kind: 'ready', capability: freshCapability() })
    await settle()

    expect(controller.getSnapshot()).toMatchObject({
      phase: 'failed',
      error: 'Could not connect to this share.',
    })
    expect(JSON.stringify(controller.getSnapshot())).not.toContain(secret)
    await controller.dispose()
  })

  it('does not expose arbitrary transfer failure details', async () => {
    const { controller, gateway } = await readyController()
    const secret = 'PRIVATE-TRANSFER-FAILURE'
    controller.startDownload()
    gateway.transfer?.reject(new Error(secret))
    await settle()

    expect(controller.getSnapshot()).toMatchObject({
      phase: 'failed',
      error: 'The download could not be completed.',
    })
    expect(JSON.stringify(controller.getSnapshot())).not.toContain(secret)
    await controller.dispose()
  })
})

describe('selection compilation scheduling', () => {
  it('coalesces rapid changes and never computes two plan IDs concurrently', async () => {
    const gateway = new FakeGateway()
    const pending: Array<{ selectors: readonly string[] | null; task: Deferred<TransferPlan> }> = []
    let active = 0
    let maximumActive = 0
    gateway.compileSelection = (_share, selectors) => {
      active += 1
      maximumActive = Math.max(maximumActive, active)
      const task = new Deferred<TransferPlan>()
      pending.push({ selectors, task })
      return task.promise.finally(() => {
        active -= 1
      })
    }
    const controller = new ReceiverController(gateway)
    controller.initialize({ kind: 'ready', capability: freshCapability() })
    await settle()
    expect(pending).toHaveLength(1)

    controller.toggleSelection('docs/a.txt')
    controller.toggleSelection('docs/b.txt')
    expect(pending).toHaveLength(1)
    pending[0]?.task.resolve(planFor(null))
    await settle()

    expect(pending).toHaveLength(2)
    expect(pending[1]?.selectors).toEqual([])
    pending[1]?.task.resolve(planFor([]))
    await settle()
    expect(maximumActive).toBe(1)
    expect(controller.getSnapshot()).toMatchObject({
      phase: 'ready',
      selectedBytes: 0,
      canStart: false,
    })
    await controller.dispose()
  })

  it('suppresses a joined share that resolves after controller disposal', async () => {
    const gateway = new FakeGateway()
    const joined = new Deferred<JoinedShare>()
    gateway.join = () => joined.promise
    const controller = new ReceiverController(gateway)
    controller.initialize({ kind: 'ready', capability: freshCapability() })

    await controller.dispose()
    joined.resolve(gateway.share)
    await settle()

    expect(gateway.share.closed).toBe(1)
    expect(controller.getSnapshot().entries).toEqual([])
  })
})

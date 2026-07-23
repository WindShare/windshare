import { createElement } from 'react'
import { flushSync } from 'react-dom'
import { createRoot } from 'react-dom/client'

import { V2ReceiverApp } from '../../src/ui/V2ReceiverApp'
import { V2ReceiverController } from '../../src/ui/v2-controller'
import {
  V2BrowserReceiverGateway,
  V2JoinedBrowserShare,
} from '../../src/ui/v2-gateway'
import { IndexedDbV2CatalogPageStore } from '../../src/catalog/v2-page-store'
import type { V2ReceiverReconnectSupervisor } from '../../src/receiver/v2-supervisor'
import {
  createR8WideDirectoryFixture,
  ObservedR8CatalogPageStore,
  R8_WIDE_PROGRESS_PREFIX,
  type R8WideDirectoryProgress,
  type R8WideDirectoryProbeSnapshot,
} from './r8-wide-directory-source'

export interface R8WideDirectoryMeasurement {
  readonly domNodeCounts: readonly number[]
  readonly heapBytes: readonly number[]
  readonly renderMilliseconds: readonly number[]
  readonly renderedRowCounts: readonly number[]
  readonly pageCount: number
  readonly pageEntries: number
  readonly probe: R8WideDirectoryProbeSnapshot
}

export async function measureR8WideDirectoryUi(
  directoryEntries: number,
  sampleCount: number,
): Promise<R8WideDirectoryMeasurement> {
  const fixture = await createR8WideDirectoryFixture(directoryEntries, reportProgress)
  const databaseName = `windshare-r8-wide-${crypto.randomUUID()}`
  const storageIdentity = `r8-wide-${crypto.randomUUID()}`
  const persistedStore = await IndexedDbV2CatalogPageStore.open(storageIdentity, databaseName)
  const observedStore = new ObservedR8CatalogPageStore(persistedStore, fixture.probe)
  const catalog = fixture.createClient(observedStore, storageIdentity)
  const supervisor = {
    close: async () => undefined,
  } as unknown as V2ReceiverReconnectSupervisor
  const joined = new V2JoinedBrowserShare({
    descriptor: fixture.descriptor,
    supervisor,
    catalog,
    recoveryIdentity: storageIdentity,
  })
  class R8WideDirectoryGateway extends V2BrowserReceiverGateway {
    override async join(
      _input: string,
      _pageUrl: string,
      signal?: AbortSignal,
    ): Promise<V2JoinedBrowserShare> {
      signal?.throwIfAborted()
      return joined
    }
  }
  const controller = new V2ReceiverController(new R8WideDirectoryGateway())
  const domNodeCounts: number[] = []
  const heapBytes: number[] = []
  const renderMilliseconds: number[] = []
  const renderedRowCounts: number[] = []

  try {
    const browsing = waitForBrowsing(controller)
    controller.initialize({
      capabilityInput: 'r8-wide-directory',
      pageUrl: 'https://receiver.invalid/s/r8-wide-directory',
    })
    await browsing
    observeControllerOwnership(controller, fixture.probe)
    await walkCommittedPages(controller, fixture.pageCount, fixture.probe)
    const snapshot = controller.getSnapshot()

    for (let sample = 0; sample < sampleCount; sample += 1) {
      const container = document.createElement('div')
      document.body.append(container)
      const root = createRoot(container)
      const startedAt = performance.now()
      flushSync(() => {
        root.render(createElement(V2ReceiverApp, { controller }))
      })
      renderMilliseconds.push(performance.now() - startedAt)
      const renderedRows = container.querySelectorAll('.selection-list > li').length
      const domNodes = container.querySelectorAll('*').length
      renderedRowCounts.push(renderedRows)
      domNodeCounts.push(domNodes)
      fixture.probe.observeDom(renderedRows, domNodes)
      const memory = performance as Performance & { memory?: { readonly usedJSHeapSize: number } }
      if (memory.memory !== undefined) heapBytes.push(memory.memory.usedJSHeapSize)
      root.unmount()
      container.remove()
    }

    return Object.freeze({
      domNodeCounts: Object.freeze(domNodeCounts),
      heapBytes: Object.freeze(heapBytes),
      renderMilliseconds: Object.freeze(renderMilliseconds),
      renderedRowCounts: Object.freeze(renderedRowCounts),
      pageCount: snapshot.pageCount,
      pageEntries: snapshot.rows.length,
      probe: fixture.probe.snapshot(),
    })
  } finally {
    await controller.dispose()
    fixture.close()
    await deleteDatabase(databaseName)
  }
}

async function walkCommittedPages(
  controller: V2ReceiverController,
  pageCount: number,
  probe: import('./r8-wide-directory-source').R8WideDirectoryProbe,
): Promise<void> {
  for (let pageIndex = 1; pageIndex < pageCount; pageIndex += 1) {
    const loaded = waitForPage(controller, pageIndex)
    controller.showPage(pageIndex)
    await loaded
    observeControllerOwnership(controller, probe)
  }
  const returned = waitForPage(controller, 0)
  controller.showPage(0)
  await returned
  observeControllerOwnership(controller, probe)
}

function observeControllerOwnership(
  controller: V2ReceiverController,
  probe: import('./r8-wide-directory-source').R8WideDirectoryProbe,
): void {
  probe.observeControllerRows(
    controller.getSnapshot().rows.length,
    controller.getOwnershipSnapshot(),
  )
}

function reportProgress(progress: R8WideDirectoryProgress): void {
  console.info(`${R8_WIDE_PROGRESS_PREFIX}${JSON.stringify(progress)}`)
}

function waitForBrowsing(controller: V2ReceiverController): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    const observe = () => {
      const snapshot = controller.getSnapshot()
      if (snapshot.phase === 'browsing') {
        unsubscribe()
        resolve()
      } else if (snapshot.phase === 'failed') {
        unsubscribe()
        reject(new Error(snapshot.error ?? 'Wide-directory controller failed without an error'))
      }
    }
    const unsubscribe = controller.subscribe(observe)
    observe()
  })
}

function waitForPage(controller: V2ReceiverController, pageIndex: number): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    const observe = () => {
      const snapshot = controller.getSnapshot()
      if (snapshot.phase === 'browsing' && snapshot.pageIndex === pageIndex) {
        unsubscribe()
        resolve()
      } else if (snapshot.phase === 'failed') {
        unsubscribe()
        reject(new Error(snapshot.error ?? 'Wide-directory page walk failed without an error'))
      }
    }
    const unsubscribe = controller.subscribe(observe)
    observe()
  })
}

function deleteDatabase(databaseName: string): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(databaseName)
    request.addEventListener('success', () => resolve(), { once: true })
    request.addEventListener('error', () => reject(request.error), { once: true })
    request.addEventListener('blocked', () => {
      reject(new Error('Wide-directory catalog database remained open during cleanup'))
    }, { once: true })
  })
}

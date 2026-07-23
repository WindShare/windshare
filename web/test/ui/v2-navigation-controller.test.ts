import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import type { V2CatalogEntry } from '../../src/catalog/v2-records'
import { V2ReceiverController } from '../../src/ui/v2-controller'
import type {
  V2BrowseDirectory,
  V2BrowsePage,
  V2BrowserReceiverGateway,
  V2JoinedBrowserShare,
} from '../../src/ui/v2-gateway'

const firstChild = directoryEntry(2, 'first', 'First')
const secondChild = directoryEntry(3, 'second', 'Second')

class NavigableJoinedShare {
  readonly descriptor = { syntheticRootId: 'root' }
  readonly recoveryIdentity = 'navigation-share'
  readonly selection = new V2SelectionPolicy(true)
  readonly requests: Array<{
    readonly directory: V2BrowseDirectory
    readonly signal: AbortSignal | undefined
    readonly result: Deferred<V2BrowsePage>
  }> = []

  rootDirectory(): V2BrowseDirectory {
    return directory(1, 'root', 'Shared files', [], ['root'])
  }

  childDirectory(parent: V2BrowseDirectory, entry: V2CatalogEntry): V2BrowseDirectory {
    if (entry.kind !== 'directory') throw new TypeError('not a directory')
    return directory(
      entry.id[0] ?? 0,
      entry.idText,
      entry.name,
      [...parent.path, entry.name],
      [...parent.ancestry, entry.idText],
    )
  }

  async page(
    directory: V2BrowseDirectory,
    _pageIndex: number,
    options: { readonly signal?: AbortSignal } = {},
  ): Promise<V2BrowsePage> {
    if (directory.idText === 'root') {
      return browsePage(directory, [firstChild, secondChild])
    }
    const result = deferred<V2BrowsePage>()
    this.requests.push({ directory, signal: options.signal, result })
    // The fake deliberately ignores AbortSignal so tests exercise the stale
    // completion fence rather than relying on cooperative I/O cancellation.
    return result.promise
  }

  subscribeCatalogScanProgress(): () => void {
    return () => undefined
  }

  async close(): Promise<void> {}
}

beforeEach(() => {
  vi.stubGlobal('window', { navigator: { storage: {} } })
})

afterEach(() => vi.unstubAllGlobals())

describe('v2 receiver child navigation publication', () => {
  it('deduplicates a repeated pending click and publishes its breadcrumb only with the page', async () => {
    const { controller, joined } = await readyController()

    controller.openDirectory(firstChild.idText)
    controller.openDirectory(firstChild.idText)
    expect(joined.requests).toHaveLength(1)
    expect(controller.getSnapshot().breadcrumbs.map((item) => item.name)).toEqual(['Shared files'])
    expect(controller.getSnapshot().rows.map((item) => item.id)).toEqual(['first', 'second'])

    const request = joined.requests[0]
    request?.result.resolve(browsePage(request.directory, [fileEntry(4, 'inside', 'inside.txt')]))
    await turns()
    expect(controller.getSnapshot().breadcrumbs.map((item) => item.name)).toEqual([
      'Shared files',
      'First',
    ])
    expect(controller.getSnapshot().rows.map((item) => item.id)).toEqual(['inside'])
    await controller.dispose()
  })

  it('keeps the committed route after failure and ignores a cancelled late success', async () => {
    const { controller, joined } = await readyController()

    controller.openDirectory(firstChild.idText)
    const failed = joined.requests[0]
    failed?.result.reject(new Error('listing failed'))
    await turns()
    expect(controller.getSnapshot()).toMatchObject({
      phase: 'browsing',
      error: 'listing failed',
    })
    expect(controller.getSnapshot().breadcrumbs.map((item) => item.name)).toEqual(['Shared files'])
    expect(controller.getSnapshot().rows.map((item) => item.id)).toEqual(['first', 'second'])

    controller.openDirectory(firstChild.idText)
    const stale = joined.requests[1]
    controller.openDirectory(secondChild.idText)
    const current = joined.requests[2]
    expect(stale?.signal?.aborted).toBe(true)
    current?.result.resolve(browsePage(current.directory, [fileEntry(5, 'current', 'current.txt')]))
    await turns()
    stale?.result.resolve(browsePage(stale.directory, [fileEntry(6, 'stale', 'stale.txt')]))
    await turns()

    expect(controller.getSnapshot().breadcrumbs.map((item) => item.name)).toEqual([
      'Shared files',
      'Second',
    ])
    expect(controller.getSnapshot().rows.map((item) => item.id)).toEqual(['current'])
    await controller.dispose()
  })
})

async function readyController(): Promise<{
  readonly controller: V2ReceiverController
  readonly joined: NavigableJoinedShare
}> {
  const joined = new NavigableJoinedShare()
  const gateway = {
    join: async () => joined as unknown as V2JoinedBrowserShare,
  } as unknown as V2BrowserReceiverGateway
  const controller = new V2ReceiverController(gateway)
  controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
  await turns()
  expect(controller.getSnapshot().phase).toBe('browsing')
  return { controller, joined }
}

function browsePage(
  browseDirectory: V2BrowseDirectory,
  entries: readonly V2CatalogEntry[],
): V2BrowsePage {
  return Object.freeze({
    directory: browseDirectory,
    pageIndex: 0,
    pageCount: 1,
    entryCount: entries.length,
    omittedCount: 0n,
    entries: Object.freeze([...entries]),
  })
}

function directoryEntry(first: number, idText: string, name: string): V2CatalogEntry {
  return Object.freeze({ kind: 'directory', id: identity(first), idText, name })
}

function fileEntry(first: number, idText: string, name: string): V2CatalogEntry {
  return Object.freeze({ kind: 'file', id: identity(first), idText, name, expectedSize: 1n })
}

function directory(
  first: number,
  idText: string,
  name: string,
  path: readonly string[],
  ancestry: readonly string[],
): V2BrowseDirectory {
  return Object.freeze({
    id: identity(first),
    idText,
    name,
    path: Object.freeze([...path]),
    ancestry: Object.freeze([...ancestry]),
  })
}

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

interface Deferred<T> {
  readonly promise: Promise<T>
  resolve(value: T): void
  reject(error: unknown): void
}

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void
  let reject!: (error: unknown) => void
  const promise = new Promise<T>((complete, fail) => {
    resolve = complete
    reject = fail
  })
  return { promise, resolve, reject }
}

async function turns(): Promise<void> {
  for (let index = 0; index < 12; index += 1) await Promise.resolve()
}

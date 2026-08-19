import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import type { V2CatalogEntry } from '../../src/catalog/v2-records'
import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  createFailureIdentity,
  createIncidentScopeIssuer,
  createProtocolFailure,
  type FailureFact,
  type FailureFactRelation,
  type IncidentScopeHandle,
  type IncidentScopeIdentity,
  type IncidentScopeKind,
  type IncidentScopeOwner,
  type PresentationDecision,
} from '../../src/diagnostics/incident'
import { V2RemoteOperationError } from '../../src/content/v2-session-operations'
import type {
  AuthenticatedDiscoveryRequest,
  AuthenticatedDiscoverySource,
} from '../../src/transfer/projection'
import { BrowserNavigationCoordinator } from '../../src/ui/controller/navigation'
import { captureV2Location, V2ReceiverController } from '../../src/ui/v2-controller'
import type {
  V2BrowseDirectory,
  V2BrowsePage,
  V2BrowserReceiverGateway,
  V2JoinedBrowserShare,
} from '../../src/ui/v2-gateway'
import type { V2ReceiverSnapshot } from '../../src/ui/v2-model'
import { INERT_TEST_RECEIVE_COMPOSITION } from './v2-receive-fixture'

const firstChild = directoryEntry(2, 'first', 'First')
const secondChild = directoryEntry(3, 'second', 'Second')

class NavigableJoinedShare {
  readonly descriptor = {
    shareInstance: identity(7),
    shareInstanceId: identityText(7),
    syntheticRoot: identity(1),
    syntheticRootId: identityText(1),
  }
  readonly recoveryIdentity = 'navigation-share'
  readonly protocolSessionId = identityText(8)
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

  projectionSource(): AuthenticatedDiscoverySource {
    return Object.freeze({
      discover: async function* (request: AuthenticatedDiscoveryRequest) {
        yield* []
        return Object.freeze({ settledTargets: request.unsettledTargets })
      },
    })
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

describe('browser navigation incident ownership', () => {
  it('submits the browse decision before publishing a visible failure and closes its owner', async () => {
    const joined = new NavigableJoinedShare()
    const incidents = new RecordingIncidentPort()
    let snapshot = directNavigationSnapshot()
    const publicationOrder: string[] = []
    const navigation = new BrowserNavigationCoordinator({
      currentJoinedShare: () => joined as unknown as V2JoinedBrowserShare,
      isDisposed: () => false,
      snapshot: () => snapshot,
      publish: (next) => {
        snapshot = next
        if (next.error !== null) publicationOrder.push('visible-failure')
      },
      publicError: (error) => error instanceof Error ? error.message : 'failed',
      incidents,
    })
    incidents.onDecision = () => publicationOrder.push('decision')

    const root = joined.rootDirectory()
    await navigation.loadPage(root, 0, Object.freeze([root]))
    navigation.openDirectory(firstChild.idText)
    joined.requests[0]?.result.reject(new Error('listing failed'))
    await turns()

    expect(incidents.owners.map((owner) => owner.identity.scopeKind)).toEqual([
      'browse',
      'browse',
    ])
    expect(incidents.decisions).toMatchObject([
      { decision: { kind: 'excluded', boundary: 'browse', reason: 'success' } },
      { decision: { kind: 'incident', boundary: 'browse', outcome: 'failed' } },
    ])
    expect(incidents.facts).toMatchObject([
      {
        fact: {
          kind: 'unclassified',
          stage: 'browse',
          recoveryDisposition: 'none',
        },
        relation: 'contributor',
      },
    ])
    expect(publicationOrder.slice(-2)).toEqual(['decision', 'visible-failure'])
    expect(incidents.owners.every((owner) => owner.isClosed())).toBe(true)
  })

  it('keeps the browse result unchanged when diagnostic submission throws', async () => {
    const joined = new NavigableJoinedShare()
    const incidents = new RecordingIncidentPort()
    let snapshot = directNavigationSnapshot()
    const navigation = new BrowserNavigationCoordinator({
      currentJoinedShare: () => joined as unknown as V2JoinedBrowserShare,
      isDisposed: () => false,
      snapshot: () => snapshot,
      publish: (next) => { snapshot = next },
      publicError: (error) => error instanceof Error ? error.message : 'failed',
      incidents,
    })
    incidents.onDecision = () => { throw new Error('diagnostic submit failed') }
    const requested = directory(2, 'first', 'First', ['First'], ['first'])

    const loading = navigation.loadPage(
      requested,
      0,
      Object.freeze([requested]),
    )
    joined.requests[0]?.result.reject(new Error('listing failed'))
    await loading

    expect(snapshot.error).toBe('listing failed')
    expect(incidents.decisions[0]?.decision).toMatchObject({
      kind: 'incident',
      boundary: 'browse',
      outcome: 'failed',
    })
    expect(incidents.owners[0]?.isClosed()).toBe(true)
  })

  it('gives replaced and successful page requests distinct closed scopes', async () => {
    const joined = new NavigableJoinedShare()
    const incidents = new RecordingIncidentPort()
    let snapshot = directNavigationSnapshot()
    const navigation = new BrowserNavigationCoordinator({
      currentJoinedShare: () => joined as unknown as V2JoinedBrowserShare,
      isDisposed: () => false,
      snapshot: () => snapshot,
      publish: (next) => { snapshot = next },
      publicError: () => 'failed',
      incidents,
    })
    const first = directory(2, 'first', 'First', ['First'], ['first'])
    const second = directory(3, 'second', 'Second', ['Second'], ['second'])

    const stale = navigation.loadPage(first, 0, Object.freeze([first]))
    const current = navigation.loadPage(second, 0, Object.freeze([second]))
    joined.requests[1]?.result.resolve(browsePage(second, []))
    await current
    joined.requests[0]?.result.resolve(browsePage(first, []))
    await stale

    expect(incidents.decisions).toMatchObject([
      {
        scope: { scopeKind: 'browse', scopeSequence: 1n },
        decision: {
          kind: 'excluded',
          boundary: 'browse',
          reason: 'stale_replacement',
        },
      },
      {
        scope: { scopeKind: 'browse', scopeSequence: 2n },
        decision: { kind: 'excluded', boundary: 'browse', reason: 'success' },
      },
    ])
    expect(incidents.owners.every((owner) => owner.isClosed())).toBe(true)
  })

  it('retains a reviewed authenticated operation fact without reclassifying it', async () => {
    const joined = new NavigableJoinedShare()
    const incidents = new RecordingIncidentPort()
    let snapshot = directNavigationSnapshot()
    const navigation = new BrowserNavigationCoordinator({
      currentJoinedShare: () => joined as unknown as V2JoinedBrowserShare,
      isDisposed: () => false,
      snapshot: () => snapshot,
      publish: (next) => { snapshot = next },
      publicError: () => 'failed',
      incidents,
    })
    const requested = directory(2, 'first', 'First', ['First'], ['first'])
    const failure = authenticatedRemoteOperationError()

    const loading = navigation.loadPage(
      requested,
      0,
      Object.freeze([requested]),
    )
    joined.requests[0]?.result.reject(failure)
    await loading

    expect(incidents.facts).toHaveLength(1)
    expect(incidents.facts[0]?.fact).toBe(failure.failureFact)
    expect(incidents.facts[0]).toMatchObject({
      scope: { scopeKind: 'browse' },
      fact: {
        kind: 'protocol_failure',
        stage: 'protocol_operation',
        recoveryDisposition: 'retryable',
      },
      relation: 'contributor',
    })
    expect(incidents.owners[0]?.isClosed()).toBe(true)
  })

  it('names explicit cancellation without waiting for an uncooperative page source', async () => {
    const joined = new NavigableJoinedShare()
    const incidents = new RecordingIncidentPort()
    let snapshot = directNavigationSnapshot()
    const navigation = new BrowserNavigationCoordinator({
      currentJoinedShare: () => joined as unknown as V2JoinedBrowserShare,
      isDisposed: () => false,
      snapshot: () => snapshot,
      publish: (next) => { snapshot = next },
      publicError: () => 'failed',
      incidents,
    })
    const requested = directory(2, 'first', 'First', ['First'], ['first'])

    const loading = navigation.loadPage(
      requested,
      0,
      Object.freeze([requested]),
    )
    navigation.cancel(new DOMException('cancelled', 'AbortError'))

    expect(incidents.decisions).toMatchObject([
      {
        decision: {
          kind: 'excluded',
          boundary: 'browse',
          reason: 'cancelled',
        },
      },
    ])
    expect(incidents.owners[0]?.isClosed()).toBe(true)

    joined.requests[0]?.result.resolve(browsePage(requested, []))
    await loading
  })
})

describe('v2 receiver capability lifecycle', () => {
  it('erases the location before publishing the location-cleared milestone', () => {
    const completeUrl = 'https://receiver.invalid/s/share#actual-location-key'
    const events: string[] = []
    const history = {
      state: { route: 'receiver' },
      replaceState: (_state: unknown, _unused: string, url: URL | string | null | undefined) => {
        events.push('history-replaced')
        expect(new URL(String(url)).hash).toBe('')
      },
    }
    const windowPort = {
      location: { href: completeUrl },
      history,
    } as unknown as Window

    const captured = captureV2Location(windowPort, {
      onSecurityMilestone: (milestone) => events.push(milestone),
    })

    expect(events).toEqual(['history-replaced', 'location-cleared'])
    expect(captured).toEqual({
      capabilityInput: completeUrl,
      pageUrl: 'https://receiver.invalid/s/share',
    })
    expect(Object.isFrozen(captured)).toBe(true)
  })

  it('publishes key clearing synchronously and redacts nested gateway failures before UI state', async () => {
    const key = 'actual-separate-key-for-this-invocation'
    const milestones: string[] = []
    const gatewayInputs: string[] = []
    const gateway = {
      join: async (input: string) => {
        gatewayInputs.push(input)
        throw new AggregateError(
          [new Error(`nested capability ${input}`)],
          `gateway rejected ${input}`,
        )
      },
    } as unknown as V2BrowserReceiverGateway
    const controller = new V2ReceiverController(gateway, {
      receive: INERT_TEST_RECEIVE_COMPOSITION,
      onSecurityMilestone: (milestone) => milestones.push(milestone),
    })
    controller.initialize({ capabilityInput: null, pageUrl: 'https://receiver.invalid/s/share' })

    controller.submitKey(key)
    expect(milestones).toEqual(['key-cleared'])
    expect(gatewayInputs).toEqual([])
    await turns()

    const failure = controller.getSnapshot()
    expect(gatewayInputs).toEqual([key])
    expect(failure.phase).toBe('failed')
    expect(failure.error).toContain('[separate-key redacted]')
    expect(failure.error).not.toContain(key)
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
  const controller = new V2ReceiverController(gateway, {
    receive: INERT_TEST_RECEIVE_COMPOSITION,
  })
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

function identityText(first: number): string {
  return encodeBase64Url(identity(first))
}

class RecordingIncidentPort {
  readonly #issuer = createIncidentScopeIssuer()
  readonly owners: IncidentScopeOwner[] = []
  readonly decisions: Array<{
    readonly scope: IncidentScopeIdentity
    readonly decision: PresentationDecision
  }> = []
  readonly facts: Array<{
    readonly scope: IncidentScopeIdentity
    readonly fact: FailureFact
    readonly relation: FailureFactRelation
  }> = []
  onDecision: (() => void) | undefined

  openScope(kind: IncidentScopeKind): IncidentScopeOwner {
    const owner = this.#issuer.open(kind, {
      factRecorded: (observation) => {
        this.facts.push({
          scope: observation.ref.scope,
          fact: observation.fact,
          relation: observation.relation,
        })
      },
    })
    this.owners.push(owner)
    return owner
  }

  submitDecision(
    scope: IncidentScopeHandle,
    decision: PresentationDecision,
  ): void {
    this.decisions.push({ scope: scope.identity, decision })
    this.onDecision?.()
  }
}

function directNavigationSnapshot(): V2ReceiverSnapshot {
  return {
    phase: 'browsing',
    status: 'ready',
    error: null,
    rows: Object.freeze([]),
    breadcrumbs: Object.freeze([]),
    pageIndex: 0,
    pageCount: 1,
    entryCount: 0,
    omittedCount: 0n,
    selectedVisibleFiles: 0,
    selectedVisibleBytes: 0n,
    directoryRetryable: false,
  } as unknown as V2ReceiverSnapshot
}

function authenticatedRemoteOperationError(): V2RemoteOperationError {
  return new V2RemoteOperationError(createProtocolFailure({
    requestKind: 'list_children',
    wireScope: 'directory',
    wireCode: 0x21,
    retryable: true,
    retryAfterMilliseconds: 250,
    settlement: Object.freeze({ kind: 'received_authenticated' }),
    correlation: Object.freeze({
      protocolSessionId: createFailureIdentity('protocol_session', identity(9)),
      protocolOperationId: createFailureIdentity('protocol_operation', identity(10)),
    }),
  }))
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

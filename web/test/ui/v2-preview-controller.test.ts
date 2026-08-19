import { afterEach, describe, expect, it, vi } from 'vitest'

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
import type { V2CatalogScanProgressListener } from '../../src/catalog/v2-client'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import {
  type V2ConnectivityActivation,
  V2ConnectivityRouteAuthority,
} from '../../src/connectivity/v2-receiver-policy'
import type { V2FilePreview } from '../../src/preview/v2-preview'
import type {
  AuthenticatedDiscoveryRequest,
  AuthenticatedDiscoverySource,
} from '../../src/transfer/projection'
import { V2ReceiverController } from '../../src/ui/v2-controller'
import { V2PreviewController } from '../../src/ui/v2-preview-controller'
import { EMPTY_V2_PREVIEW, type V2ReceiverSnapshot } from '../../src/ui/v2-model'
import type {
  V2BrowseDirectory,
  V2BrowserReceiverGateway,
  V2JoinedBrowserShare,
} from '../../src/ui/v2-gateway'
import { INERT_TEST_RECEIVE_COMPOSITION } from './v2-receive-fixture'

const entry: Extract<V2CatalogEntry, { kind: 'file' }> = Object.freeze({
  kind: 'file',
  id: identity(2),
  idText: identityText(2),
  name: 'photo.png',
  expectedSize: 100n,
})

class FakeJoined {
  readonly descriptor = {
    shareInstance: identity(7),
    shareInstanceId: identityText(7),
    syntheticRoot: identity(1),
    syntheticRootId: identityText(1),
  }
  readonly recoveryIdentity = 'share.recovery'
  readonly protocolSessionId = identityText(8)
  readonly selection = new V2SelectionPolicy(true)
  readonly entry: Extract<V2CatalogEntry, { kind: 'file' }>
  readonly events: string[] = []
  readonly previewSignals: AbortSignal[] = []
  previewCount = 0
  sessionCloses = 0
  closes = 0
  progressOnPage: bigint | undefined
  pageGate: Promise<void> | undefined
  #scanProgress: V2CatalogScanProgressListener | undefined

  constructor(fileEntry = entry) {
    this.entry = fileEntry
  }

  rootDirectory(): V2BrowseDirectory {
    return {
      id: identity(1),
      idText: identityText(1),
      name: 'Shared files',
      path: [],
      ancestry: [identityText(1)],
    }
  }

  async page(directory: V2BrowseDirectory) {
    if (this.progressOnPage !== undefined) {
      this.#scanProgress?.({
        directoryId: directory.id,
        attemptId: identity(9),
        discoveredEntries: this.progressOnPage,
      })
    }
    await this.pageGate
    return {
      directory,
      pageIndex: 0,
      pageCount: 1,
      entryCount: 1,
      omittedCount: 0n,
      entries: [this.entry],
    }
  }

  subscribeCatalogScanProgress(listener: V2CatalogScanProgressListener): () => void {
    this.#scanProgress = listener
    return () => {
      if (this.#scanProgress === listener) this.#scanProgress = undefined
    }
  }

  projectionSource(): AuthenticatedDiscoverySource {
    return completedProjectionSource()
  }

  beginPreviewConnectivity(): V2ConnectivityActivation {
    this.events.push('begin-preview-connectivity')
    let closed = false
    const routes = new V2ConnectivityRouteAuthority()
    return {
      routes,
      close: () => {
        if (closed) return
        closed = true
        routes.close()
        this.events.push('close-preview-connectivity')
      },
    }
  }

  preview(
    _entry: V2CatalogEntry,
    _connectivity: V2ConnectivityActivation,
    signal: AbortSignal,
  ): Promise<V2FilePreview> {
    this.events.push('open-preview')
    this.previewSignals.push(signal)
    this.previewCount += 1
    if (this.previewCount === 1) {
      return new Promise((_resolve, reject) => {
        const abort = () => reject(signal.reason)
        signal.addEventListener('abort', abort, { once: true })
      })
    }
    return Promise.resolve({
      current: {
        kind: 'image',
        name: entry.name,
        url: 'blob:second',
        mimeType: 'image/png',
        width: 20,
        height: 10,
      },
      seek: async () => { throw new Error('not a video') },
      close: async () => {
        this.events.push('close-preview-session')
        this.sessionCloses += 1
      },
    } as unknown as V2FilePreview)
  }

  async close(): Promise<void> { this.closes += 1 }
}

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

function identityText(first: number): string {
  return encodeBase64Url(identity(first))
}

function authenticatedRemoteOperationError(): V2RemoteOperationError {
  return new V2RemoteOperationError(createProtocolFailure({
    requestKind: 'open_revisions',
    wireScope: 'revision',
    wireCode: 0x22,
    retryable: true,
    retryAfterMilliseconds: 250,
    settlement: Object.freeze({ kind: 'received_authenticated' }),
    correlation: Object.freeze({
      protocolSessionId: createFailureIdentity('protocol_session', identity(9)),
      protocolOperationId: createFailureIdentity('protocol_operation', identity(10)),
    }),
  }))
}

async function turn(): Promise<void> {
  for (let index = 0; index < 8; index += 1) await Promise.resolve()
}

afterEach(() => vi.unstubAllGlobals())

describe('v2 preview incident ownership', () => {
  it('owns browser media failure independently from a successful preview open', async () => {
    const joined = new DiagnosticPreviewJoined()
    const incidents = new RecordingPreviewIncidents()
    let snapshot = directPreviewSnapshot()
    const order: string[] = []
    const previews = new V2PreviewController({
      snapshot: () => snapshot,
      publish: (next) => {
        snapshot = next
        if (next.preview.state === 'error') order.push('visible-failure')
      },
      publicError: () => 'preview failed',
      incidents,
    })
    incidents.onDecision = (decision) => {
      if (decision.kind === 'incident') order.push('decision')
    }

    previews.open(joined as unknown as V2JoinedBrowserShare, entry)
    await turn()
    if (snapshot.preview.state !== 'image' && snapshot.preview.state !== 'video') {
      throw new TypeError('expected media preview')
    }
    previews.mediaFailed(snapshot.preview.presentationId)
    await turn()

    expect(incidents.owners.map((owner) => owner.identity.scopeKind)).toEqual([
      'preview_open',
      'preview_media',
    ])
    expect(incidents.decisions).toMatchObject([
      {
        decision: {
          kind: 'excluded',
          boundary: 'preview',
          reason: 'success',
        },
      },
      {
        decision: {
          kind: 'incident',
          boundary: 'preview',
          outcome: 'failed',
        },
      },
    ])
    expect(incidents.facts).toMatchObject([
      {
        fact: {
          kind: 'unclassified',
          stage: 'preview_media',
          recoveryDisposition: 'none',
        },
        relation: 'contributor',
      },
    ])
    expect(order).toEqual(['decision', 'visible-failure'])
    expect(incidents.owners.every((owner) => owner.isClosed())).toBe(true)
  })

  it('closes successful image and video media attempts without closing the preview', async () => {
    for (const joined of [
      new DiagnosticPreviewJoined(),
      diagnosticImageJoined(),
    ]) {
      const incidents = new RecordingPreviewIncidents()
      let snapshot = directPreviewSnapshot()
      const previews = new V2PreviewController({
        snapshot: () => snapshot,
        publish: next => {
          snapshot = next
        },
        publicError: () => 'preview failed',
        incidents,
      })

      previews.open(joined as unknown as V2JoinedBrowserShare, entry)
      await turn()
      const presented = snapshot.preview
      if (presented.state !== 'image' && presented.state !== 'video') {
        throw new TypeError('expected media preview')
      }
      previews.mediaPresented(presented.presentationId)

      const mediaOwner = incidents.owners.find(
        owner => owner.identity.scopeKind === 'preview_media',
      )
      expect(mediaOwner?.isClosed()).toBe(true)
      expect(incidents.decisions.at(-1)).toMatchObject({
        decision: {
          kind: 'excluded',
          boundary: 'preview',
          reason: 'success',
        },
      })
      expect(snapshot.preview.state).toBe(presented.state)

      previews.mediaFailed(presented.presentationId)
      expect(incidents.decisions).toHaveLength(2)
      await previews.close()
    }
  })

  it('fences post-seek readiness by presentation generation', async () => {
    const joined = new DiagnosticPreviewJoined()
    const incidents = new RecordingPreviewIncidents()
    let snapshot = directPreviewSnapshot()
    const previews = new V2PreviewController({
      snapshot: () => snapshot,
      publish: next => {
        snapshot = next
      },
      publicError: () => 'preview failed',
      incidents,
    })

    previews.open(joined as unknown as V2JoinedBrowserShare, entry)
    await turn()
    if (snapshot.preview.state !== 'video') throw new TypeError('expected video preview')
    const initialPresentation = snapshot.preview.presentationId
    previews.mediaPresented(initialPresentation)

    previews.seek(12)
    await turn()
    if (snapshot.preview.state !== 'video') throw new TypeError('expected seek preview')
    const seekPresentation = snapshot.preview.presentationId
    expect(seekPresentation).not.toBe(initialPresentation)

    previews.mediaFailed(initialPresentation)
    previews.mediaPresented(initialPresentation)
    expect(snapshot.preview.state).toBe('video')
    previews.mediaPresented(seekPresentation)

    const mediaDecisions = incidents.decisions.filter(({ scope }) =>
      scope.scopeKind === 'preview_media')
    expect(mediaDecisions).toHaveLength(2)
    expect(mediaDecisions.map(({ decision }) => decision)).toMatchObject([
      { kind: 'excluded', reason: 'success' },
      { kind: 'excluded', reason: 'success' },
    ])
    expect(incidents.owners.filter(
      owner => owner.identity.scopeKind === 'preview_media',
    ).every(owner => owner.isClosed())).toBe(true)
    await previews.close()
  })

  it('keeps the preview result unchanged when diagnostic submission throws', async () => {
    const joined = new DiagnosticPreviewJoined({
      openFailure: new Error('open failed'),
    })
    const incidents = new RecordingPreviewIncidents()
    let snapshot = directPreviewSnapshot()
    const previews = new V2PreviewController({
      snapshot: () => snapshot,
      publish: (next) => { snapshot = next },
      publicError: () => 'preview failed',
      incidents,
    })
    incidents.onDecision = () => { throw new Error('diagnostic submit failed') }

    previews.open(joined as unknown as V2JoinedBrowserShare, entry)
    await turn()

    expect(snapshot.preview).toMatchObject({
      state: 'error',
      message: 'preview failed',
    })
    expect(incidents.decisions[0]?.decision).toMatchObject({
      kind: 'incident',
      boundary: 'preview',
      outcome: 'failed',
    })
    expect(incidents.owners[0]?.isClosed()).toBe(true)
  })

  it('keeps seek failure primary and records session-close failure as its consequence', async () => {
    const joined = new DiagnosticPreviewJoined({
      seekFailure: new Error('seek failed'),
      closeFailure: new Error('close failed'),
    })
    const incidents = new RecordingPreviewIncidents()
    let snapshot = directPreviewSnapshot()
    const previews = new V2PreviewController({
      snapshot: () => snapshot,
      publish: (next) => { snapshot = next },
      publicError: () => 'preview failed',
      incidents,
    })

    previews.open(joined as unknown as V2JoinedBrowserShare, entry)
    await turn()
    previews.seek(12)
    await turn()

    expect(incidents.owners.map((owner) => owner.identity.scopeKind)).toEqual([
      'preview_open',
      'preview_media',
      'preview_seek',
    ])
    expect(incidents.decisions).toMatchObject([
      { decision: { kind: 'excluded', reason: 'success' } },
      {
        decision: {
          kind: 'incident',
          boundary: 'preview',
          outcome: 'failed',
        },
      },
      {
        decision: {
          kind: 'excluded',
          boundary: 'preview',
          reason: 'stale_replacement',
        },
      },
    ])
    expect(incidents.facts).toMatchObject([
      {
        scope: { scopeKind: 'preview_seek' },
        fact: { kind: 'unclassified', stage: 'preview_seek' },
        relation: 'contributor',
      },
      {
        scope: { scopeKind: 'preview_seek' },
        fact: { kind: 'unclassified', stage: 'cleanup' },
        relation: 'consequence',
      },
    ])
    expect(incidents.owners.every((owner) => owner.isClosed())).toBe(true)
  })

  it('retains a reviewed authenticated open failure without reclassifying it', async () => {
    const failure = authenticatedRemoteOperationError()
    const joined = new DiagnosticPreviewJoined({ openFailure: failure })
    const incidents = new RecordingPreviewIncidents()
    let snapshot = directPreviewSnapshot()
    const previews = new V2PreviewController({
      snapshot: () => snapshot,
      publish: (next) => { snapshot = next },
      publicError: () => 'preview failed',
      incidents,
    })

    previews.open(joined as unknown as V2JoinedBrowserShare, entry)
    await turn()

    expect(incidents.facts).toHaveLength(1)
    expect(incidents.facts[0]?.fact).toBe(failure.failureFact)
    expect(incidents.facts[0]).toMatchObject({
      scope: { scopeKind: 'preview_open' },
      fact: {
        kind: 'protocol_failure',
        stage: 'protocol_operation',
        recoveryDisposition: 'retryable',
      },
      relation: 'contributor',
    })
    expect(incidents.owners[0]?.isClosed()).toBe(true)
  })

  it('records an open failure without creating a media scope', async () => {
    const joined = new DiagnosticPreviewJoined({
      openFailure: new Error('open failed'),
    })
    const incidents = new RecordingPreviewIncidents()
    let snapshot = directPreviewSnapshot()
    const previews = new V2PreviewController({
      snapshot: () => snapshot,
      publish: (next) => { snapshot = next },
      publicError: () => 'preview failed',
      incidents,
    })

    previews.open(joined as unknown as V2JoinedBrowserShare, entry)
    await turn()

    expect(incidents.owners.map((owner) => owner.identity.scopeKind)).toEqual([
      'preview_open',
    ])
    expect(incidents.decisions).toMatchObject([
      {
        decision: {
          kind: 'incident',
          boundary: 'preview',
          outcome: 'failed',
        },
      },
    ])
    expect(incidents.facts[0]).toMatchObject({
      fact: { kind: 'unclassified', stage: 'preview_open' },
      relation: 'contributor',
    })
    expect(incidents.owners[0]?.isClosed()).toBe(true)
  })

  it('closes a pending open with an explicit cancellation decision', async () => {
    const joined = new DiagnosticPreviewJoined({ openPending: true })
    const incidents = new RecordingPreviewIncidents()
    let snapshot = directPreviewSnapshot()
    const previews = new V2PreviewController({
      snapshot: () => snapshot,
      publish: (next) => { snapshot = next },
      publicError: () => 'preview failed',
      incidents,
    })

    previews.open(joined as unknown as V2JoinedBrowserShare, entry)
    previews.cancel()
    await turn()

    expect(incidents.decisions).toMatchObject([
      {
        decision: {
          kind: 'excluded',
          boundary: 'preview',
          reason: 'cancelled',
        },
      },
    ])
    expect(incidents.owners[0]?.isClosed()).toBe(true)
    expect(snapshot.preview).toEqual(EMPTY_V2_PREVIEW)
  })
})

describe('v2 preview click controller boundary', () => {
  it('starts each explicit preview immediately, replaces the old preview, and never opens an output picker', async () => {
    const showSaveFilePicker = vi.fn()
    vi.stubGlobal('window', {
      navigator: { storage: {} },
      showSaveFilePicker,
    })
    const joined = new FakeJoined()
    const gateway = {
      join: async () => joined as unknown as V2JoinedBrowserShare,
    } as unknown as V2BrowserReceiverGateway
    const controller = new V2ReceiverController(gateway, { receive: INERT_TEST_RECEIVE_COMPOSITION })
    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await turn()
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
      join: async () => joined as unknown as V2JoinedBrowserShare,
    } as unknown as V2BrowserReceiverGateway
    const controller = new V2ReceiverController(gateway, { receive: INERT_TEST_RECEIVE_COMPOSITION })
    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await turn()
    expect(controller.getSnapshot().status).toContain('257 entries discovered')
    expect(controller.getSnapshot().status).toContain('total still unknown')
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
    const pending: Array<(joined: V2JoinedBrowserShare) => void> = []
    const gateway = {
      join: () => new Promise<V2JoinedBrowserShare>((resolve) => pending.push(resolve)),
    } as unknown as V2BrowserReceiverGateway
    const controller = new V2ReceiverController(gateway, { receive: INERT_TEST_RECEIVE_COMPOSITION })

    controller.initialize({ capabilityInput: 'first-key', pageUrl: 'https://receiver.invalid/s/share' })
    await turn()
    controller.submitKey('second-key')
    await turn()
    expect(pending).toHaveLength(2)

    pending[1]?.(second as unknown as V2JoinedBrowserShare)
    await turn()
    expect(controller.getSnapshot().rows[0]?.name).toBe('second.png')
    pending[0]?.(first as unknown as V2JoinedBrowserShare)
    await turn()
    expect(first.closes).toBe(1)
    expect(controller.getSnapshot().rows[0]?.name).toBe('second.png')
    await controller.dispose()
  })

  it('keeps visible selection facts independent from unresolved artifact projection', async () => {
    vi.stubGlobal('window', {
      navigator: { storage: {} },
      showSaveFilePicker: () => Promise.reject(new DOMException('unused', 'AbortError')),
    })
    const selection = new V2SelectionPolicy(false)
    const joined = {
      descriptor: {
        shareInstance: identity(7),
        shareInstanceId: identityText(7),
        syntheticRoot: identity(1),
        syntheticRootId: identityText(1),
      },
      recoveryIdentity: 'share.recovery',
      protocolSessionId: identityText(8),
      selection,
      rootDirectory: () => ({
        id: identity(1), idText: identityText(1), name: 'Shared files', path: [], ancestry: [identityText(1)],
      }),
      page: async (directory: V2BrowseDirectory) => {
        return {
          directory,
          pageIndex: 0,
          pageCount: 2,
          entryCount: 2,
          omittedCount: 0n,
          entries: [entry],
        }
      },
      projectionSource: () => completedProjectionSource(),
      subscribeCatalogScanProgress: () => () => undefined,
      close: async () => undefined,
    } as unknown as V2JoinedBrowserShare
    const gateway = { join: async () => joined } as unknown as V2BrowserReceiverGateway
    const controller = new V2ReceiverController(gateway, {
      receive: {
        retained: {
          list: () => Promise.resolve(Object.freeze({
            operations: Object.freeze([]),
            presentationFailures: Object.freeze([]),
            act: () => Promise.reject(new DOMException('No retained operation', 'InvalidStateError')),
            close: () => undefined,
          })),
        },
        environment: () => new Promise<never>(() => undefined),
        startArtifactAuthority: () => {
          throw new Error('output authority must not start from selection projection')
        },
      },
    })

    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await turn()
    expect(controller.getSnapshot().output.projection).toBeNull()
    controller.toggleSelection(entry.idText)
    expect(controller.getSnapshot().rows[0]?.selection).toBe('selected')
    expect(controller.getSnapshot().selectedVisibleFiles).toBe(1)
    expect(controller.getSnapshot().output.projection).toBeNull()
    await controller.dispose()
  })
})

function diagnosticImageJoined(): Readonly<{
  beginPreviewConnectivity(): V2ConnectivityActivation
  preview(): Promise<V2FilePreview>
}> {
  return Object.freeze({
    beginPreviewConnectivity: () => {
      const routes = new V2ConnectivityRouteAuthority()
      return { routes, close: () => routes.close() }
    },
    preview: () => Promise.resolve({
      current: {
        kind: 'image',
        name: entry.name,
        url: 'blob:diagnostic-image',
        mimeType: 'image/png',
        width: 20,
        height: 10,
      },
      seek: async () => {
        throw new Error('image preview cannot seek')
      },
      close: async () => undefined,
    } as unknown as V2FilePreview),
  })
}

interface DiagnosticPreviewOptions {
  readonly openFailure?: unknown
  readonly openPending?: boolean
  readonly seekFailure?: unknown
  readonly closeFailure?: unknown
}

class DiagnosticPreviewJoined {
  readonly #options: DiagnosticPreviewOptions
  readonly #presentation = Object.freeze({
    kind: 'video' as const,
    name: entry.name,
    url: 'blob:diagnostic-preview',
    mimeType: 'video/mp4',
    width: 20,
    height: 10,
    durationSeconds: 30,
    positionSeconds: 0,
  })
  readonly #session: V2FilePreview

  constructor(options: DiagnosticPreviewOptions = {}) {
    this.#options = options
    this.#session = {
      current: this.#presentation,
      seek: async () => {
        if (this.#options.seekFailure !== undefined) {
          throw this.#options.seekFailure
        }
        return this.#presentation
      },
      close: async () => {
        if (this.#options.closeFailure !== undefined) {
          throw this.#options.closeFailure
        }
      },
    } as unknown as V2FilePreview
  }

  beginPreviewConnectivity(): V2ConnectivityActivation {
    const routes = new V2ConnectivityRouteAuthority()
    return {
      routes,
      close: () => routes.close(),
    }
  }

  preview(): Promise<V2FilePreview> {
    if (this.#options.openFailure !== undefined) {
      return Promise.reject(this.#options.openFailure)
    }
    if (this.#options.openPending === true) {
      return new Promise(() => undefined)
    }
    return Promise.resolve(this.#session)
  }
}

class RecordingPreviewIncidents {
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
  onDecision: ((decision: PresentationDecision) => void) | undefined

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
    this.onDecision?.(decision)
  }
}

function directPreviewSnapshot(): V2ReceiverSnapshot {
  return {
    preview: EMPTY_V2_PREVIEW,
  } as V2ReceiverSnapshot
}

function completedProjectionSource(): AuthenticatedDiscoverySource {
  return Object.freeze({
    discover: async function* (request: AuthenticatedDiscoveryRequest) {
      yield* []
      return Object.freeze({ settledTargets: request.unsettledTargets })
    },
  })
}

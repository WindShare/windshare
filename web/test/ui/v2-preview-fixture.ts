import type { CatalogEntry } from '../../src/catalog/records'
import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  createFailureIdentity, createIncidentScopeIssuer, createReceivedProtocolError,
  type FailureFact, type FailureFactRelation, type IncidentScopeHandle,
  type IncidentScopeIdentity, type IncidentScopeKind, type IncidentScopeOwner,
  type PresentationDecision,
} from '../../src/diagnostics/incident'
import { RemoteOperationError } from '../../src/content/session-operations'
import { RemoteRevisionError, RevisionCapacityBusyError } from '../../src/content/session-services'
import { REVISION_CODE_QUOTA } from '../../src/content/flow'
import type { CatalogScanProgressListener } from '../../src/catalog/client'
import { SelectionPolicy } from '../../src/catalog/selection'
import { type ConnectivityActivation, ConnectivityRouteAuthority } from '../../src/connectivity/receiver-policy'
import type { FilePreview } from '../../src/preview/preview'
import type {
  AuthenticatedDiscoveryRequest,
  AuthenticatedDiscoverySource,
} from '../../src/transfer/projection/coordinator'
import { EMPTY_PREVIEW, type ReceiverSnapshot } from '../../src/ui/model'
import type { BrowseDirectory } from '../../src/application/browser-share/gateway'

export const entry: Extract<CatalogEntry, { kind: 'file' }> = Object.freeze({
  kind: 'file',
  id: identity(2),
  idText: identityText(2),
  name: 'photo.png',
  expectedSize: 100n,
})

export class FakeJoined {
  readonly descriptor = {
    shareInstance: identity(7),
    shareInstanceId: identityText(7),
    syntheticRoot: identity(1),
    syntheticRootId: identityText(1),
  }
  readonly recoveryIdentity = 'share.recovery'
  readonly protocolSessionId = identityText(8)
  selection = new SelectionPolicy(true)
  replaceSelection(selection: SelectionPolicy): void { this.selection = selection }
  readonly entry: Extract<CatalogEntry, { kind: 'file' }>
  readonly events: string[] = []
  readonly previewSignals: AbortSignal[] = []
  previewCount = 0
  sessionCloses = 0
  closes = 0
  progressOnPage: bigint | undefined
  pageGate: Promise<void> | undefined
  #scanProgress: CatalogScanProgressListener | undefined

  constructor(fileEntry = entry) {
    this.entry = fileEntry
  }

  rootDirectory(): BrowseDirectory {
    return {
      id: identity(1),
      idText: identityText(1),
      name: 'Shared files',
      path: [],
      ancestry: [identityText(1)],
    }
  }

  async page(directory: BrowseDirectory) {
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

  subscribeCatalogScanProgress(listener: CatalogScanProgressListener): () => void {
    this.#scanProgress = listener
    return () => {
      if (this.#scanProgress === listener) this.#scanProgress = undefined
    }
  }

  get protocolSessionIdentity(): string {
    return this.protocolSessionId
  }

  subscribePathActivity(): () => void { return () => undefined }
  subscribeConnection(listener: (value: { kind: 'connected' }) => void): () => void { listener({ kind: 'connected' }); return () => undefined }

  subscribeProtocolGeneration(): () => void {
    return () => undefined
  }

  projectionSource(): AuthenticatedDiscoverySource {
    return completedProjectionSource()
  }

  beginPreviewConnectivity(): ConnectivityActivation {
    this.events.push('begin-preview-connectivity')
    let closed = false
    const routes = new ConnectivityRouteAuthority()
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
    _entry: CatalogEntry,
    _connectivity: ConnectivityActivation,
    signal: AbortSignal,
  ): Promise<FilePreview> {
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
    } as unknown as FilePreview)
  }

  async close(): Promise<void> { this.closes += 1 }
}

export function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

export function identityText(first: number): string {
  return encodeBase64Url(identity(first))
}

export function authenticatedRemoteOperationError(): RemoteOperationError {
  return new RemoteOperationError(createReceivedProtocolError({
    requestKind: 'open_revisions', correlation: Object.freeze({
      protocolSessionId: createFailureIdentity('protocol_session', identity(9)),
      protocolOperationId: createFailureIdentity('protocol_operation', identity(10)),
    }), content: {
      scope: 'revision', code: 0x22, retryable: true, retryAfterMilliseconds: 250
    }
  }))
}

export function remoteOperationError(retryable: boolean): RemoteOperationError {
  return new RemoteOperationError(createReceivedProtocolError({
    requestKind: 'request_blocks', correlation: Object.freeze({
      protocolSessionId: createFailureIdentity('protocol_session', identity(13)),
      protocolOperationId: createFailureIdentity('protocol_operation', identity(14)),
    }), content: retryable
      ? { scope: 'block', code: 0x31, retryable, retryAfterMilliseconds: 250 }
      : { scope: 'block', code: 0x31, retryable },
  }))
}

export function nonRetryableRemoteRevisionError(): RemoteRevisionError {
  return new RemoteRevisionError(createReceivedProtocolError({
    requestKind: 'open_revisions', correlation: Object.freeze({
      protocolSessionId: createFailureIdentity('protocol_session', identity(15)),
      protocolOperationId: createFailureIdentity('protocol_operation', identity(16)),
    }), content: { scope: 'revision', code: 0x21, retryable: false },
  }))
}

export function authenticatedRevisionCapacityError(): RevisionCapacityBusyError {
  const failure = Object.freeze({
    code: REVISION_CODE_QUOTA,
    retryable: true as const,
    retryAfterMilliseconds: 250,
  })
  return new RevisionCapacityBusyError(createReceivedProtocolError({
    requestKind: 'open_revisions', correlation: Object.freeze({
      protocolSessionId: createFailureIdentity('protocol_session', identity(11)),
      protocolOperationId: createFailureIdentity('protocol_operation', identity(12)),
    }), content: {
      scope: 'revision', code: failure.code, retryable: failure.retryable, retryAfterMilliseconds: failure.retryAfterMilliseconds
    }
  }))
}

export async function turn(): Promise<void> {
  for (let index = 0; index < 8; index += 1) await Promise.resolve()
}

export async function waitFor(predicate: () => boolean): Promise<void> {
  for (let attempt = 0; attempt < 200; attempt += 1) {
    if (predicate()) return
    await new Promise<void>(resolve => setTimeout(resolve, 1))
  }
  throw new Error('timed out waiting for controller state')
}

export function diagnosticImageJoined(): Readonly<{
  beginPreviewConnectivity(): ConnectivityActivation
  preview(): Promise<FilePreview>
}> {
  return Object.freeze({
    beginPreviewConnectivity: () => {
      const routes = new ConnectivityRouteAuthority()
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
    } as unknown as FilePreview),
  })
}

// A video seek result for a long-GOP source with a keyframe every 8 s: the
// presented keyframe position differs from the requested seconds unless the
// request lands on a keyframe.
export function keyframePresentation(seconds: number, keyframe = Math.floor(seconds / 8) * 8) {
  return Object.freeze({
    kind: 'video' as const, name: entry.name, mimeType: 'video/mp4', width: 20, height: 10,
    durationSeconds: 30, keyframeSeconds: keyframe, decodeSeconds: keyframe + 0.2, url: `blob:at-${seconds}`,
  })
}

interface DiagnosticPreviewOptions {
  readonly openFailure?: unknown
  readonly openPending?: boolean
  readonly seekFailure?: unknown
  readonly closeFailure?: unknown
  readonly openPresentation?: ReturnType<typeof keyframePresentation>
}

export class DiagnosticPreviewJoined {
  readonly #options: DiagnosticPreviewOptions
  readonly #presentation = Object.freeze({
    kind: 'video' as const,
    name: entry.name,
    url: 'blob:diagnostic-preview',
    mimeType: 'video/mp4',
    width: 20,
    height: 10,
    durationSeconds: 30,
    keyframeSeconds: 0,
    decodeSeconds: 0,
  })
  readonly #session: FilePreview
  closed = 0
  get presentation() { return this.#presentation }
  // Replaces the default seek result: return a presentation or throw.
  seekResult: ((seconds: number) => unknown) | undefined

  constructor(options: DiagnosticPreviewOptions = {}) {
    this.#options = options
    this.#session = {
      current: options.openPresentation ?? this.#presentation,
      seek: async (seconds: number) => {
        if (this.seekResult !== undefined) return this.seekResult(seconds)
        if (this.#options.seekFailure !== undefined) {
          throw this.#options.seekFailure
        }
        return this.#presentation
      },
      close: async () => {
        this.closed += 1
        if (this.#options.closeFailure !== undefined) {
          throw this.#options.closeFailure
        }
      },
    } as unknown as FilePreview
  }

  beginPreviewConnectivity(): ConnectivityActivation {
    const routes = new ConnectivityRouteAuthority()
    return {
      routes,
      close: () => routes.close(),
    }
  }

  preview(): Promise<FilePreview> {
    if (this.#options.openFailure !== undefined) {
      return Promise.reject(this.#options.openFailure)
    }
    if (this.#options.openPending === true) {
      return new Promise(() => undefined)
    }
    return Promise.resolve(this.#session)
  }
}

export class RecordingPreviewIncidents {
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

export function directPreviewSnapshot(): ReceiverSnapshot {
  return {
    preview: EMPTY_PREVIEW,
  } as ReceiverSnapshot
}

export function completedProjectionSource(): AuthenticatedDiscoverySource {
  return Object.freeze({
    discover: async function* (request: AuthenticatedDiscoveryRequest) {
      yield* []
      return Object.freeze({ kind: 'complete' as const, settledTargets: request.unsettledTargets })
    },
  })
}

import { afterEach, describe, expect, it, vi } from 'vitest'

import type { CatalogEntry } from '../../src/catalog/records'
import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  createFailureIdentity,
  createIncidentScopeIssuer,
  createReceivedProtocolError,
  type FailureFact,
  type FailureFactRelation,
  type IncidentScopeHandle,
  type IncidentScopeIdentity,
  type IncidentScopeKind,
  type IncidentScopeOwner,
  type PresentationDecision,
} from '../../src/diagnostics/incident'
import { createBrowserDiagnosticsComposition } from '../../src/application/diagnostics/composition'
import { browserBuildSnapshot } from '../../src/diagnostics/build-identity'
import { FakeTraceTime } from '../diagnostics/trace/test-support'
import { RemoteOperationError } from '../../src/content/session-operations'
import { RevisionCapacityBusyError } from '../../src/content/session-services'
import { REVISION_CODE_QUOTA } from '../../src/content/flow'
import type { CatalogScanProgressListener } from '../../src/catalog/client'
import { SelectionPolicy } from '../../src/catalog/selection'
import {
  type ConnectivityActivation,
  ConnectivityRouteAuthority,
} from '../../src/connectivity/receiver-policy'
import { PreviewRuntimeError, type FilePreview } from '../../src/preview/preview'
import { AutomaticPhotoDeferredError } from '../../src/preview/automatic-photo'
import type {
  AuthenticatedDiscoveryRequest,
  AuthenticatedDiscoverySource,
} from '../../src/transfer/projection'
import { ReceiverController } from '../../src/ui/controller'
import { PreviewController } from '../../src/ui/preview-controller'
import { EMPTY_PREVIEW, type ReceiverSnapshot } from '../../src/ui/model'
import type {
  BrowseDirectory,
  BrowserReceiverGateway,
  JoinedBrowserShare,
} from '../../src/application/browser-share/gateway'
import { INERT_TEST_RECEIVE_COMPOSITION } from './v2-receive-fixture'

const entry: Extract<CatalogEntry, { kind: 'file' }> = Object.freeze({
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

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

function identityText(first: number): string {
  return encodeBase64Url(identity(first))
}

function authenticatedRemoteOperationError(): RemoteOperationError {
  return new RemoteOperationError(createReceivedProtocolError({
    requestKind: 'open_revisions', correlation: Object.freeze({
      protocolSessionId: createFailureIdentity('protocol_session', identity(9)),
      protocolOperationId: createFailureIdentity('protocol_operation', identity(10)),
    }), content: {
      scope: 'revision', code: 0x22, retryable: true, retryAfterMilliseconds: 250
    }
  }))
}

function authenticatedRevisionCapacityError(): RevisionCapacityBusyError {
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

async function turn(): Promise<void> {
  for (let index = 0; index < 8; index += 1) await Promise.resolve()
}

async function waitFor(predicate: () => boolean): Promise<void> {
  for (let attempt = 0; attempt < 200; attempt += 1) {
    if (predicate()) return
    await new Promise<void>(resolve => setTimeout(resolve, 1))
  }
  throw new Error('timed out waiting for controller state')
}

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
    expect(() => previews.openAutomaticPhoto(joined as unknown as JoinedBrowserShare, entry)).not.toThrow()
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
    previews.openAutomaticPhoto(source, entry)
    expect(snapshot.preview.state).toBe('loading')
    previews.yieldToReceiving()
    expect(joined.previewSignals[0]?.aborted).toBe(true)
    expect(snapshot.preview).toEqual(EMPTY_PREVIEW)
    previews.open(source, entry)
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
    previews.openAutomaticPhoto(source, entry)
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
    previews.openAutomaticPhoto(source, entry)
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
      previews.openAutomaticPhoto(source, { ...entry, expectedSize: 5n * 1024n * 1024n })
      expect(joined.events).toEqual([])
      previews.openAutomaticPhoto(source, entry)
      await vi.advanceTimersByTimeAsync(5_000)
      expect(snapshot.preview).toEqual(EMPTY_PREVIEW)
      expect(joined.previewSignals[0]?.aborted).toBe(true)
      await previews.close()
    } finally {
      vi.useRealTimers()
    }
  })
})

describe('v2 preview incident ownership', () => {
  it('owns browser media failure independently from a successful preview open', async () => {
    const joined = new DiagnosticPreviewJoined()
    const incidents = new RecordingPreviewIncidents()
    let snapshot = directPreviewSnapshot()
    const order: string[] = []
    const previews = new PreviewController({
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

    previews.open(joined as unknown as JoinedBrowserShare, entry)
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
          payload: { unclassified: {} },
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
      const previews = new PreviewController({
        snapshot: () => snapshot,
        publish: next => {
          snapshot = next
        },
        publicError: () => 'preview failed',
        incidents,
      })

      previews.open(joined as unknown as JoinedBrowserShare, entry)
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
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: next => {
        snapshot = next
      },
      publicError: () => 'preview failed',
      incidents,
    })

    previews.open(joined as unknown as JoinedBrowserShare, entry)
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
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: (next) => { snapshot = next },
      publicError: () => 'preview failed',
      incidents,
    })
    incidents.onDecision = () => { throw new Error('diagnostic submit failed') }

    previews.open(joined as unknown as JoinedBrowserShare, entry)
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
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: (next) => { snapshot = next },
      publicError: () => 'preview failed',
      incidents,
    })

    previews.open(joined as unknown as JoinedBrowserShare, entry)
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
        fact: {
          kind: 'unclassified', stage: 'preview_seek',
          evidence: { exception: { message: 'seek failed', stack: expect.any(String) } },
        },
        relation: 'contributor',
      },
      {
        scope: { scopeKind: 'preview_seek' },
        fact: {
          kind: 'unclassified', stage: 'cleanup',
          evidence: { exception: { message: 'close failed', stack: expect.any(String) } },
        },
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
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: (next) => { snapshot = next },
      publicError: () => 'preview failed',
      incidents,
    })

    previews.open(joined as unknown as JoinedBrowserShare, entry)
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

  it('retains authenticated revision-capacity evidence when preview open surfaces it', async () => {
    const failure = authenticatedRevisionCapacityError()
    const joined = new DiagnosticPreviewJoined({ openFailure: failure })
    const incidents = new RecordingPreviewIncidents()
    let snapshot = directPreviewSnapshot()
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: (next) => { snapshot = next },
      publicError: () => 'preview failed',
      incidents,
    })

    previews.open(joined as unknown as JoinedBrowserShare, entry)
    await turn()

    expect(incidents.facts).toHaveLength(1)
    expect(incidents.facts[0]?.fact).toBe(failure.failureFact)
    expect(incidents.facts[0]).toMatchObject({
      scope: { scopeKind: 'preview_open' },
      fact: {
        kind: 'protocol_failure',
        correlation: failure.failureFact.correlation,
        payload: {
          protocolFailure: { correlation: failure.protocolFailure.correlation, content: { code: REVISION_CODE_QUOTA, retryable: true, retryAfterMilliseconds: 250 } },
        },
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
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: (next) => { snapshot = next },
      publicError: () => 'preview failed',
      incidents,
    })

    previews.open(joined as unknown as JoinedBrowserShare, entry)
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
      fact: {
        kind: 'unclassified', stage: 'preview_open',
        evidence: { exception: { message: 'open failed', stack: expect.any(String) } },
      },
      relation: 'contributor',
    })
    expect(incidents.owners[0]?.isClosed()).toBe(true)
  })

  it('closes a pending open with an explicit cancellation decision', async () => {
    const joined = new DiagnosticPreviewJoined({ openPending: true })
    const incidents = new RecordingPreviewIncidents()
    let snapshot = directPreviewSnapshot()
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: (next) => { snapshot = next },
      publicError: () => 'preview failed',
      incidents,
    })

    previews.open(joined as unknown as JoinedBrowserShare, entry)
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
    expect(snapshot.preview).toEqual(EMPTY_PREVIEW)
  })
})

describe('preview diagnostics continuity', () => {
  it('exports a failed preview and subsequent download evidence in the same capture', async () => {
    const time = new FakeTraceTime()
    const diagnostics = createBrowserDiagnosticsComposition({
      build: browserBuildSnapshot(),
      secureContext: true,
      clock: {
        nowMilliseconds: () => time.nowMilliseconds(),
        captureTime: () => new Date(time.nowMilliseconds()).toISOString(),
      },
      scheduler: time,
      consoleSink: { error: vi.fn() },
      randomBytes: length => new Uint8Array(length).fill(1),
    })
    const enabled = diagnostics.runtime.enable()
    const failure = new PreviewRuntimeError('File is not a supported preview image or MP4 video')
    const joined = new DiagnosticPreviewJoined({ openFailure: failure })
    let snapshot = directPreviewSnapshot()
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: next => { snapshot = next },
      publicError: error => String(error),
      incidents: diagnostics.incidents,
    })
    try {
      previews.open(joined as unknown as JoinedBrowserShare, { ...entry, name: 'setup.exe' })
      await turn()
      expect(snapshot.preview.state).toBe('error')
      expect(diagnostics.runtime.inspectLastFailure()?.payload.trigger).toMatchObject({
        stage: 'preview_open',
        evidence: { exception: {
          errorName: 'PreviewRuntimeError', message: failure.message, stack: failure.stack,
        } },
      })
      // This pause exceeds both former failure deadlines without delaying the test.
      time.advance(60_000)
      diagnostics.trace.current?.({
        eventName: 'content_scheduling',
        correlation: { protocol_session_id: identityText(7), lane_id: 2, lane_epoch: 1 },
        payload: {
          dispatch_sequence: '1', file_id: entry.idText, block_index: '0',
          route: 'direct', purpose: 'content', expected_ms: 1,
          pending_bytes: '100', bytes_per_second: 100,
        },
      })
      diagnostics.trace.current?.({
        eventName: 'transfer_progress',
        payload: {
          operation_id: identityText(5), transfer_job_id: identityText(6),
          discovered_files: '1', discovered_bytes: '100', received_object_bytes: '100',
          source_omitted_entries: '0',
          written_bytes: '100', completed_files: '1', completed_bytes: '100',
          file_errors: '0', selection_errors: '0', failed_directories: '0',
          content_lanes: 1, capacity_waiting_files: '0', capacity_accumulated_wait_ms: '0',
          capacity_wait_attempts: '0', capacity_wait_visible: false, discovery: 'complete', partial: false,
        },
      })
      diagnostics.trace.current?.({
        eventName: 'receive_transition',
        payload: {
          transition: 'materialization_completed', entry_count: '1',
          file_count: '1', directory_count: '0', raw_bytes: '100',
        },
      })
      const lines = diagnostics.runtime.export().trim().split('\n').map(line => JSON.parse(line))
      const events = lines.filter(line => line.line_type === 'trace_event').map(line => line.record)
      expect(events.map(record => record.event)).toEqual([
        'incident_marker', 'content_scheduling', 'transfer_progress', 'receive_transition',
      ])
      expect(events.at(-1).payload.transition).toBe('materialization_completed')
      expect(diagnostics.runtime.status()).toMatchObject({
        enabled: true, state: 'recording', capture_generation: enabled.capture_generation,
        expires_at: enabled.expires_at,
      })
      expect(diagnostics.runtime.status().health.trace_dropped_count).toBe('0')
    } finally {
      await previews.close()
      diagnostics.runtime.disable()
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
    const controller = new ReceiverController(gateway, { receive: INERT_TEST_RECEIVE_COMPOSITION })
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
    const controller = new ReceiverController(gateway, { receive: INERT_TEST_RECEIVE_COMPOSITION })
    controller.initialize({ capabilityInput: 'key', pageUrl: 'https://receiver.invalid/s/share' })
    await waitFor(() => controller.getSnapshot().browse.status.includes('257 entries discovered'))
    expect(controller.getSnapshot().browse.status).toContain('257 entries discovered')
    expect(controller.getSnapshot().browse.status).toContain('total still unknown')
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
    const controller = new ReceiverController(gateway, { receive: INERT_TEST_RECEIVE_COMPOSITION })

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
      projectionSource: () => completedProjectionSource(),
      subscribeCatalogScanProgress: () => () => undefined,
      subscribePathActivity: () => () => undefined,
      subscribeConnection: (listener: (value: { kind: 'connected' }) => void) => { listener({ kind: 'connected' }); return () => undefined },
      subscribeProtocolGeneration: () => () => undefined,
      close: async () => undefined,
    } as unknown as JoinedBrowserShare
    const gateway = { join: async () => joined } as unknown as BrowserReceiverGateway
    const controller = new ReceiverController(gateway, {
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
    await waitFor(() => controller.getSnapshot().rows.length === 1)
    expect(controller.getSnapshot().output.projection).toBeNull()
    controller.toggleSelection(entry.idText)
    expect(controller.getSnapshot().rows[0]?.selection).toBe('selected')
    expect(controller.getSnapshot().selectedVisibleFiles).toBe(1)
    expect(controller.getSnapshot().output.projection).toBeNull()
    await controller.dispose()
  })
})

function diagnosticImageJoined(): Readonly<{
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
  readonly #session: FilePreview

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

function directPreviewSnapshot(): ReceiverSnapshot {
  return {
    preview: EMPTY_PREVIEW,
  } as ReceiverSnapshot
}

function completedProjectionSource(): AuthenticatedDiscoverySource {
  return Object.freeze({
    discover: async function* (request: AuthenticatedDiscoveryRequest) {
      yield* []
      return Object.freeze({ kind: 'complete' as const, settledTargets: request.unsettledTargets })
    },
  })
}

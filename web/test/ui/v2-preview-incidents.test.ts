import { describe, expect, it, vi } from 'vitest'
import { createBrowserDiagnosticsComposition } from '../../src/application/diagnostics/composition'
import { browserBuildSnapshot } from '../../src/diagnostics/build-identity'
import { FakeTraceTime } from '../diagnostics/trace/test-support'
import { PreviewRuntimeError } from '../../src/preview/preview'
import { REVISION_CODE_QUOTA } from '../../src/content/flow'
import { EMPTY_PREVIEW } from '../../src/ui/model'
import { ReceiveSourceInvalidatedError } from '../../src/application/receive/errors'
import { VideoPreviewError } from '../../src/preview/mp4-range'
import { PreviewController } from '../../src/ui/preview-controller'
import type { JoinedBrowserShare } from '../../src/application/browser-share/gateway'
import {
  entry,
  identityText,
  authenticatedRemoteOperationError,
  authenticatedRevisionCapacityError,
  turn,
  diagnosticImageJoined,
  DiagnosticPreviewJoined,
  RecordingPreviewIncidents,
  directPreviewSnapshot
} from './v2-preview-fixture'

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

describe('v2 preview local failure incidents', () => {
  function localFailurePreview() {
    const joined = new DiagnosticPreviewJoined({
      seekFailure: new VideoPreviewError('MP4 sample timing exceeds the preview limits'),
    })
    const incidents = new RecordingPreviewIncidents()
    let snapshot = directPreviewSnapshot()
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: (next) => { snapshot = next },
      publicError: () => 'preview failed',
      incidents,
    })
    const preview = () => snapshot.preview
    const presentationId = () => {
      const current = snapshot.preview
      if (current.state !== 'video') throw new TypeError('expected video preview')
      return current.presentationId
    }
    return { joined, incidents, previews, preview, presentationId }
  }

  it('reports one incident per preview session for repeated local failures', async () => {
    const { joined, incidents, previews, preview, presentationId } = localFailurePreview()
    previews.open(joined as unknown as JoinedBrowserShare, entry)
    await turn()
    previews.mediaPresented(presentationId())
    for (const seconds of [3, 6, 9]) {
      previews.seek(seconds)
      await turn()
    }
    joined.seekResult = () => joined.presentation
    previews.seek(12)
    await turn()
    previews.mediaFailed(presentationId())
    expect(preview()).toMatchObject({ state: 'video', failure: expect.any(String) })

    const reported = incidents.decisions.filter(({ decision }) => decision.kind === 'incident')
    expect(reported).toHaveLength(1)
    expect(reported[0]?.scope.scopeKind).toBe('preview_seek')
    expect(incidents.facts.map(({ scope, fact, relation }) => [scope, fact.stage, relation])).toEqual([
      [reported[0]?.scope, 'preview_seek', 'contributor'],
      [reported[0]?.scope, 'preview_seek', 'consequence'],
      [reported[0]?.scope, 'preview_seek', 'consequence'],
      [reported[0]?.scope, 'preview_media', 'consequence'],
    ])
    const open = incidents.owners.filter((owner) => !owner.isClosed())
    expect(open.map((owner) => owner.identity)).toEqual([reported[0]?.scope])
    await previews.close()
    expect(incidents.owners.every((owner) => owner.isClosed())).toBe(true)
  })

  it('still reports a closing failure after a local incident', async () => {
    const { joined, incidents, previews, preview, presentationId } = localFailurePreview()
    previews.open(joined as unknown as JoinedBrowserShare, entry)
    await turn()
    previews.mediaPresented(presentationId())
    previews.seek(3)
    await turn()
    joined.seekResult = () => { throw new ReceiveSourceInvalidatedError(new Error('revision changed')) }
    previews.seek(6)
    await turn()

    expect(preview().state).toBe('error')
    expect(joined.closed).toBe(1)
    expect(incidents.decisions.filter(({ decision }) => decision.kind === 'incident')).toHaveLength(2)
    expect(incidents.owners.every((owner) => owner.isClosed())).toBe(true)
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

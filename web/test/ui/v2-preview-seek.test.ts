import { describe, expect, it } from 'vitest'
import { PreviewRuntimeError } from '../../src/preview/preview'
import { VideoPreviewError } from '../../src/preview/mp4-range'
import { ReceiveSourceInvalidatedError } from '../../src/application/receive/errors'
import { PreviewController } from '../../src/ui/preview-controller'
import type { JoinedBrowserShare } from '../../src/application/browser-share/gateway'
import {
  entry,
  remoteOperationError,
  nonRetryableRemoteRevisionError,
  turn,
  keyframePresentation,
  DiagnosticPreviewJoined,
  RecordingPreviewIncidents,
  directPreviewSnapshot
} from './v2-preview-fixture'

describe('v2 preview seek failure presentation', () => {
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
    if (snapshot.preview.state !== 'video') throw new TypeError('expected video preview')
    previews.mediaPresented(snapshot.preview.presentationId)
    previews.seek(12)
    await turn()

    // An unclassified seek failure is not known to be local to one position.
    expect(snapshot.preview).toEqual({
      state: 'error', fileId: entry.idText, name: entry.name, message: 'preview failed',
    })
    expect(joined.closed).toBe(1)
    expect(incidents.owners.map((owner) => owner.identity.scopeKind)).toEqual([
      'preview_open',
      'preview_media',
      'preview_seek',
    ])
    expect(incidents.decisions).toMatchObject([
      { decision: { kind: 'excluded', reason: 'success' } },
      { decision: { kind: 'excluded', reason: 'success' } },
      {
        scope: { scopeKind: 'preview_seek' },
        decision: {
          kind: 'incident',
          boundary: 'preview',
          outcome: 'failed',
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

  it.each([
    ['a short broker range read', () => new PreviewRuntimeError('Preview range ended before its exact length')],
    ['a source invalidation', () => new ReceiveSourceInvalidatedError(new Error('revision changed'))],
    ['a non-retryable remote operation error', () => remoteOperationError(false)],
    ['a retryable remote operation error', () => remoteOperationError(true)],
    ['a non-retryable remote revision error', () => nonRetryableRemoteRevisionError()],
    ['a dead peer session', () => new DOMException('Peer session closed', 'NetworkError')],
  ])('closes the preview into the error state after %s', async (_name, failure) => {
    const error = failure()
    const joined = new DiagnosticPreviewJoined({ seekFailure: error })
    const incidents = new RecordingPreviewIncidents()
    let snapshot = directPreviewSnapshot()
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: (next) => { snapshot = next },
      publicError: (cause) => cause instanceof Error ? cause.message : 'preview failed',
      incidents,
    })

    previews.open(joined as unknown as JoinedBrowserShare, entry)
    await turn()
    if (snapshot.preview.state !== 'video') throw new TypeError('expected video preview')
    previews.mediaPresented(snapshot.preview.presentationId)
    previews.seek(12)
    await turn()

    expect(snapshot.preview).toEqual({
      state: 'error', fileId: entry.idText, name: entry.name, message: error.message,
    })
    expect(joined.closed).toBe(1)
    expect(incidents.decisions.filter(({ decision }) => decision.kind === 'incident')).toMatchObject([
      { scope: { scopeKind: 'preview_seek' }, decision: { boundary: 'preview', outcome: 'failed' } },
    ])
    expect(incidents.owners.every((owner) => owner.isClosed())).toBe(true)
  })

  it('uses the error state when a local seek failure precedes the first presented frame', async () => {
    const joined = new DiagnosticPreviewJoined({
      seekFailure: new VideoPreviewError('MP4 sample geometry exceeds the preview limits'),
    })
    let snapshot = directPreviewSnapshot()
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: (next) => { snapshot = next },
      publicError: () => 'preview failed',
    })

    previews.open(joined as unknown as JoinedBrowserShare, entry)
    await turn()
    // The opening frame has not presented, so there is no earlier frame to keep.
    previews.seek(12)
    await turn()

    expect(snapshot.preview).toEqual({
      state: 'error', fileId: entry.idText, name: entry.name, message: 'preview failed',
    })
    expect(joined.closed).toBe(1)
  })

  it('keeps the preview open on a local seek failure and records session-close failure as its consequence', async () => {
    const joined = new DiagnosticPreviewJoined({
      seekFailure: new VideoPreviewError('seek failed'),
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
    const opened = snapshot.preview
    if (opened.state !== 'video') throw new TypeError('expected video preview')
    previews.mediaPresented(opened.presentationId)
    previews.seek(12)
    await turn()

    // One position failed: the session, the last frame and the download stay.
    expect(snapshot.preview).toEqual({ ...opened, seeking: false, failure: 'preview failed' })
    expect(joined.closed).toBe(0)
    expect(incidents.owners.map((owner) => owner.identity.scopeKind)).toEqual([
      'preview_open',
      'preview_media',
      'preview_seek',
    ])
    expect(incidents.decisions).toMatchObject([
      { decision: { kind: 'excluded', reason: 'success' } },
      { decision: { kind: 'excluded', reason: 'success' } },
      {
        scope: { scopeKind: 'preview_seek' },
        decision: {
          kind: 'incident',
          boundary: 'preview',
          outcome: 'failed',
        },
      },
    ])

    previews.seek(13)
    expect(snapshot.preview).toMatchObject({ state: 'video', seeking: true, failure: null })
    await turn()
    expect(snapshot.preview).toMatchObject({ state: 'video', seeking: false, failure: 'preview failed' })
    // The first local failure keeps the session's one incident open; the
    // repeat adds to it and its own scope closes without a second incident.
    const seekScopes = incidents.owners.filter((owner) => owner.identity.scopeKind === 'preview_seek')
    expect(seekScopes.map((owner) => owner.isClosed())).toEqual([false, true])
    expect(incidents.decisions.filter(({ decision }) => decision.kind === 'incident')).toHaveLength(1)

    await previews.close()
    expect(joined.closed).toBe(1)
    expect(incidents.facts).toMatchObject([
      {
        scope: seekScopes[0]!.identity,
        fact: {
          kind: 'unclassified', stage: 'preview_seek',
          evidence: { exception: { message: 'seek failed', stack: expect.any(String) } },
        },
        relation: 'contributor',
      },
      {
        scope: seekScopes[0]!.identity,
        fact: {
          kind: 'unclassified', stage: 'preview_seek',
          evidence: { exception: { message: 'seek failed', stack: expect.any(String) } },
        },
        relation: 'consequence',
      },
      {
        scope: seekScopes[0]!.identity,
        fact: {
          kind: 'unclassified', stage: 'cleanup',
          evidence: { exception: { message: 'close failed', stack: expect.any(String) } },
        },
        relation: 'consequence',
      },
    ])
    expect(incidents.owners.every((owner) => owner.isClosed())).toBe(true)
  })

  it('keeps an earlier frame when a later seek frame fails to present', async () => {
    const joined = new DiagnosticPreviewJoined()
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
    if (snapshot.preview.state !== 'video') throw new TypeError('expected video preview')
    previews.mediaPresented(snapshot.preview.presentationId)
    previews.seek(12)
    await turn()
    if (snapshot.preview.state !== 'video') throw new TypeError('expected seek preview')
    const seekPresentation = snapshot.preview.presentationId
    previews.mediaFailed(seekPresentation)

    expect(snapshot.preview).toMatchObject({
      state: 'video',
      presentationId: seekPresentation,
      seeking: false,
      failure: 'The browser could not decode this bounded media preview.',
    })
    expect(joined.closed).toBe(0)
    expect(incidents.decisions.at(-1)).toMatchObject({
      scope: { scopeKind: 'preview_media' },
      decision: { kind: 'incident', boundary: 'preview', outcome: 'failed' },
    })
    previews.mediaFailed(seekPresentation)
    expect(incidents.decisions.filter(({ decision }) => decision.kind === 'incident')).toHaveLength(1)
    await previews.close()
    expect(incidents.owners.every((owner) => owner.isClosed())).toBe(true)
  })
})

describe('v2 preview seek failure rollback', () => {
  function rollbackPreview() {
    const joined = new DiagnosticPreviewJoined()
    let snapshot = directPreviewSnapshot()
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: (next) => { snapshot = next },
      publicError: () => 'preview failed',
    })
    const video = () => {
      if (snapshot.preview.state !== 'video') throw new TypeError('expected video preview')
      return snapshot.preview
    }
    // Long-GOP source: a seek presents the keyframe at or before the request
    // (every 8 s), so the requested position and the keyframe differ.
    const presentAt = (seconds: number) => keyframePresentation(seconds)
    return { joined, previews, video, presentAt }
  }

  it('rolls back to the requested position, not the presented keyframe', async () => {
    const { joined, previews, video, presentAt } = rollbackPreview()
    previews.open(joined as unknown as JoinedBrowserShare, entry)
    await turn()
    previews.mediaPresented(video().presentationId)
    joined.seekResult = presentAt
    previews.seek(7.9)
    await turn()
    // The preview layer presents keyframe 0 for 7.9 s; the slider keeps 7.9.
    expect(video()).toMatchObject({ requestedSeconds: 7.9, decodeSeconds: 0.2 })
    previews.mediaPresented(video().presentationId)

    joined.seekResult = () => { throw new VideoPreviewError('MP4 sample geometry exceeds the preview limits') }
    previews.seek(9)
    await turn()
    expect(video()).toMatchObject({ requestedSeconds: 7.9, seeking: false, failure: 'preview failed' })

    // A decode failure of a later frame also returns to 7.9, not keyframe 8.
    joined.seekResult = presentAt
    previews.seek(10)
    await turn()
    previews.mediaFailed(video().presentationId)
    expect(video()).toMatchObject({ requestedSeconds: 7.9, seeking: false,
      failure: 'The browser could not decode this bounded media preview.' })
    await previews.close()
  })

  it('rolls back to the opening keyframe position when no seek has presented', async () => {
    const joined = new DiagnosticPreviewJoined({ openPresentation: keyframePresentation(0, 0.5) })
    const { previews, video } = rollbackPreview()
    previews.open(joined as unknown as JoinedBrowserShare, entry)
    await turn()
    // A head-trimmed file opens with the slider at the keyframe seek(0) chose.
    expect(video().requestedSeconds).toBe(0.5)
    previews.mediaPresented(video().presentationId)
    joined.seekResult = () => { throw new VideoPreviewError('MP4 sample timing exceeds the preview limits') }
    previews.seek(3)
    await turn()
    expect(video()).toMatchObject({ requestedSeconds: 0.5, failure: 'preview failed' })
    await previews.close()
  })

  it('returns the position to the last presented frame after a local seek failure', async () => {
    const { joined, previews, video, presentAt } = rollbackPreview()
    previews.open(joined as unknown as JoinedBrowserShare, entry)
    await turn()
    previews.mediaPresented(video().presentationId)
    joined.seekResult = presentAt
    previews.seek(5)
    await turn()
    previews.mediaPresented(video().presentationId)
    const presented = video()

    joined.seekResult = () => { throw new VideoPreviewError('MP4 sample geometry exceeds the preview limits') }
    previews.seek(9)
    await turn()
    expect(video()).toEqual({ ...presented, seeking: false, failure: 'preview failed' })

    // A later seek whose frame fails to decode also returns to 5 s, while the
    // canvas keeps that frame.
    joined.seekResult = presentAt
    previews.seek(12)
    await turn()
    expect(video().requestedSeconds).toBe(12)
    previews.mediaFailed(video().presentationId)
    expect(video()).toMatchObject({ requestedSeconds: 5, seeking: false,
      failure: 'The browser could not decode this bounded media preview.' })
    expect(joined.closed).toBe(0)
    await previews.close()
  })

  it('follows a frame that presents after a later seek has failed', async () => {
    const { joined, previews, video, presentAt } = rollbackPreview()
    previews.open(joined as unknown as JoinedBrowserShare, entry)
    await turn()
    previews.mediaPresented(video().presentationId)
    joined.seekResult = presentAt
    previews.seek(5)
    await turn()
    const pending = video().presentationId
    joined.seekResult = () => { throw new VideoPreviewError('MP4 sample timing exceeds the preview limits') }
    previews.seek(9)
    await turn()
    // 5 s has not presented yet: the canvas still shows the opening frame.
    expect(video()).toMatchObject({ requestedSeconds: 0, failure: 'preview failed' })
    previews.mediaPresented(pending)
    expect(video()).toMatchObject({ requestedSeconds: 5, failure: 'preview failed' })
    await previews.close()
  })
})

describe('v2 preview stale media failure during a newer seek', () => {
  function racingPreview() {
    const joined = new DiagnosticPreviewJoined()
    const incidents = new RecordingPreviewIncidents()
    let snapshot = directPreviewSnapshot()
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: (next) => { snapshot = next },
      publicError: () => 'preview failed',
      incidents,
    })
    const video = () => {
      if (snapshot.preview.state !== 'video') throw new TypeError('expected video preview')
      return snapshot.preview
    }
    const presentAt = (seconds: number) => keyframePresentation(seconds)
    return { joined, incidents, previews, video, presentAt }
  }

  it('keeps seek B pending when the previous frame fails, then shows B', async () => {
    const { joined, incidents, previews, video, presentAt } = racingPreview()
    previews.open(joined as unknown as JoinedBrowserShare, entry)
    await turn()
    previews.mediaPresented(video().presentationId)       // frame at 0 on canvas
    joined.seekResult = presentAt
    previews.seek(5)                                      // seek A
    await turn()
    const a = video().presentationId                      // A decoding
    let releaseB!: () => void
    joined.seekResult = (s) => new Promise((resolve) => { releaseB = () => resolve(presentAt(s)) })
    previews.seek(20)                                     // seek B: range read in flight
    await turn()
    expect(video()).toMatchObject({ seeking: true, failure: null, presentationId: a })
    previews.mediaFailed(a)                               // A's decode error or 10 s deadline fires
    expect(video()).toMatchObject({ seeking: true, failure: null, requestedSeconds: 20, presentationId: a })
    // The stale frame is still evidence: it reports the session's local incident.
    const reported = incidents.decisions.filter(({ decision }) => decision.kind === 'incident')
    expect(reported.map(({ scope }) => scope.scopeKind)).toEqual(['preview_media'])

    releaseB()
    await turn()
    expect(video()).toMatchObject({ seeking: false, failure: null, requestedSeconds: 20 })
    expect(video().presentationId).not.toBe(a)
    previews.mediaPresented(video().presentationId)
    expect(video()).toMatchObject({ seeking: false, failure: null, requestedSeconds: 20 })
    expect(joined.closed).toBe(0)
    await previews.close()
    expect(incidents.owners.every((owner) => owner.isClosed())).toBe(true)
  })

  it('reports seek B\'s own local failure after the stale frame failure', async () => {
    const { joined, incidents, previews, video, presentAt } = racingPreview()
    previews.open(joined as unknown as JoinedBrowserShare, entry)
    await turn()
    previews.mediaPresented(video().presentationId)
    joined.seekResult = presentAt
    previews.seek(5)
    await turn()
    const a = video().presentationId
    let failB!: () => void
    joined.seekResult = () => new Promise((_resolve, reject) => {
      failB = () => reject(new VideoPreviewError('MP4 sample timing exceeds the preview limits'))
    })
    previews.seek(20)
    await turn()
    previews.mediaFailed(a)
    expect(video()).toMatchObject({ seeking: true, failure: null })
    failB()
    await turn()
    // The canvas still shows the opening frame, so B's failure rolls back to it.
    expect(video()).toMatchObject({ seeking: false, failure: 'preview failed', requestedSeconds: 0 })
    const reported = incidents.decisions.filter(({ decision }) => decision.kind === 'incident')
    expect(reported).toHaveLength(1)
    expect(incidents.facts.map(({ fact, relation }) => [fact.stage, relation])).toEqual([
      ['preview_media', 'contributor'],
      ['preview_seek', 'consequence'],
    ])
    await previews.close()
  })
})

import { describe, expect, it, vi } from 'vitest'
import type { JoinedBrowserShare } from '../../src/application/browser-share/gateway'
import { createBrowserDiagnosticsComposition } from '../../src/application/diagnostics/composition'
import { createReceiverTraceSource, projectReceiverTraceEvent } from '../../src/application/diagnostics/receive-trace'
import { browserBuildSnapshot } from '../../src/diagnostics/build-identity'
import { snapshotTraceEventObservationV2 } from '../../src/diagnostics/export/trace-event-v2'
import type { TraceEventObservationV2 } from '../../src/diagnostics/trace/model'
import { AutomaticPhotoDeferredError } from '../../src/preview/automatic-photo'
import { ImagePreviewError } from '../../src/preview/image-header'
import { PreviewController } from '../../src/ui/preview-controller'
import { directPreviewSnapshot, entry, FakeJoined, RecordingPreviewIncidents, turn } from '../ui/v2-preview-fixture'
import { FakeTraceTime } from './trace/test-support'

function diagnostics() {
  const time = new FakeTraceTime()
  return createBrowserDiagnosticsComposition({
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
}

function deferredShare(error: AutomaticPhotoDeferredError) {
  return Object.assign(new FakeJoined(), {
    automaticPhotoPreview: () => Promise.reject(error),
  })
}

describe('automatic preview diagnostic evidence', () => {
  it('exports the original exception chain without reporting a user-visible incident', async () => {
    const cause = new DOMException('Invalid image data', 'InvalidStateError')
    const format = new ImagePreviewError('Browser rejected the image payload', { cause })
    const failure = new AutomaticPhotoDeferredError('Use Preview to inspect this file', { cause: format })
    const joined = deferredShare(failure)
    const composition = diagnostics()
    let snapshot = directPreviewSnapshot()
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: next => { snapshot = next },
      publicError: String,
      incidents: composition.incidents,
      trace: createReceiverTraceSource(composition.trace),
    })
    composition.runtime.enable()
    try {
      previews.openAutomaticPhoto(joined as unknown as JoinedBrowserShare, entry)
      await turn()
      const events = composition.runtime.export().trim().split('\n')
        .map(line => JSON.parse(line))
        .filter(line => line.line_type === 'trace_event')
        .map(line => line.record)
      expect(events).toHaveLength(1)
      expect(events[0]).toMatchObject({
        event: 'preview_transition',
        payload: {
          attempt: 'open', transition: 'deferred', intent: 'automatic_photo', preview_id: '1',
          share_instance_id: joined.descriptor.shareInstanceId, file_id: entry.idText,
          exception: {
            errorName: failure.name,
            cause: { errorName: format.name, message: format.message,
              cause: { errorName: cause.name, message: cause.message } },
          },
        },
      })
      expect(composition.runtime.inspectLastFailure()).toBeNull()
      expect(composition.runtime.status().health.trace_dropped_count).toBe('0')
      expect(joined.events).toEqual(['begin-preview-connectivity', 'close-preview-connectivity'])
    } finally {
      await previews.close()
      composition.runtime.disable()
    }
  })

  it('does not inspect exception evidence while detailed tracing is disabled', async () => {
    const readCause = vi.fn(() => new ImagePreviewError('Invalid PNG'))
    const failure = new AutomaticPhotoDeferredError('Use Preview')
    Object.defineProperty(failure, 'cause', { get: readCause })
    const composition = diagnostics()
    const joined = deferredShare(failure)
    let snapshot = directPreviewSnapshot()
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: next => { snapshot = next },
      publicError: String,
      trace: createReceiverTraceSource(composition.trace),
    })
    try {
      previews.openAutomaticPhoto(joined as unknown as JoinedBrowserShare, entry)
      await turn()
      expect(readCause).not.toHaveBeenCalled()
      expect(joined.events).toEqual(['begin-preview-connectivity', 'close-preview-connectivity'])
    } finally {
      await previews.close()
    }
  })

  it('keeps cleanup and incident exclusion independent of a failing trace sink', async () => {
    const joined = deferredShare(new AutomaticPhotoDeferredError('Use Preview'))
    const incidents = new RecordingPreviewIncidents()
    let snapshot = directPreviewSnapshot()
    const trace = vi.fn(() => { throw new Error('trace sink failed') })
    const previews = new PreviewController({
      snapshot: () => snapshot,
      publish: next => { snapshot = next },
      publicError: String,
      incidents,
      trace: { current: trace },
    })
    try {
      previews.openAutomaticPhoto(joined as unknown as JoinedBrowserShare, entry)
      await turn()
      expect(trace).toHaveBeenCalledOnce()
      expect(incidents.decisions.map(({ decision }) => decision)).toEqual([
        { kind: 'excluded', boundary: 'preview', reason: 'not_user_visible' },
      ])
      expect(incidents.owners.every(owner => owner.isClosed())).toBe(true)
      expect(joined.events).toEqual(['begin-preview-connectivity', 'close-preview-connectivity'])
    } finally {
      await previews.close()
    }
  })

  it.each([{ attempt: 'seek' }, { file_id: 'invalid' }, { exception: {} }])(
    'rejects malformed deferred trace evidence: %o', (invalid) => {
      const joined = new FakeJoined()
      const event = projectReceiverTraceEvent({
        name: 'preview_transition', transition: 'deferred', previewId: 1,
        shareInstanceId: joined.descriptor.shareInstanceId, fileId: entry.idText,
        error: new AutomaticPhotoDeferredError('Use Preview'),
      })
      expect(() => snapshotTraceEventObservationV2({
        ...event, payload: { ...event.payload, ...invalid },
      } as TraceEventObservationV2)).toThrow()
    },
  )
})

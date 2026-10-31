import { unclassifiedFailureFact } from '../../../src/diagnostics/incident'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createBrowserDiagnosticsComposition } from '../../../src/application/diagnostics/composition'
import { BrowserDiagnosticsSession, DIAGNOSTICS_CHECKPOINT_INTERVAL_MS } from '../../../src/diagnostics/browser/session'
import {
  DIAGNOSTICS_ARCHIVE_MAX_AGE_MS,
  retainDiagnosticCaptures,
  summarizeDiagnosticCapture,
  type DiagnosticsArchiveStore,
  type SavedDiagnosticCapture,
} from '../../../src/diagnostics/browser/archive'
import { DEFAULT_TRACE_CAPTURE_EXPIRY_MS } from '../../../src/diagnostics/trace/capacity'
import { FakeTraceTime } from '../trace/test-support'

describe('browser diagnostic evidence lifetime', () => {
  beforeEach(() => { vi.spyOn(console, 'debug').mockImplementation(() => undefined) })

  it('saves failures with trace off, coalesces bursts, and restores the last successful archive after reload', async () => {
    const harness = setup()
    const failure = (message: string) => harness.composition.failures.record(unclassifiedFailureFact({
      stage: 'content_read', recoveryDisposition: 'retryable', error: new Error(message),
    }))
    failure('initial cause')
    await settle()
    expect(harness.archive.captures[0]?.file.text).toContain('initial cause')
    expect(harness.session.status().state).toBe('idle')
    expect(harness.session.inspectLastFailure()).toBeNull()
    for (let index = 0; index < 30; index++) { failure('latest cause'); await settle() }
    expect(harness.archive.save).toHaveBeenCalledTimes(1)
    harness.time.advance(DIAGNOSTICS_CHECKPOINT_INTERVAL_MS)
    await settle()
    expect(harness.archive.save).toHaveBeenCalledTimes(2)
    expect(harness.archive.captures[0]?.file.text).toContain('latest cause')
    harness.session.dispose()
    const reopened = setup({ archive: harness.archive, runSeed: 2, time: harness.time })
    await settle()
    const previous = reopened.session.getSnapshot().previousCaptures[0]!
    expect((await reopened.session.readSavedFile(previous.id))?.text).toContain('latest cause')
    reopened.session.dispose()
  })

  it('retries trace-off saves autonomously with a bounded attempt budget', async () => {
    const archive = memoryArchive()
    archive.save.mockRejectedValue(new Error('unavailable'))
    const harness = setup({ archive })
    harness.composition.failures.record(unclassifiedFailureFact({
      stage: 'content_read', recoveryDisposition: 'retryable', error: new Error('retained in memory'),
    }))
    await settle()
    for (let index = 0; index < 6; index++) { harness.time.advance(5000); await settle() }
    expect(archive.save).toHaveBeenCalledTimes(3)
    expect(harness.session.export()).toContain('retained in memory')
    expect(harness.session.getSnapshot().archiveUnavailable).toBe(true)
    harness.session.dispose()
  })

  it('starts before producers, keeps repeated link intake idempotent, and exports without changing capture', async () => {
    const harness = setup()
    harness.session.enableFromLink()
    const before = harness.session.status()
    record(harness)
    harness.session.enableFromLink()
    const file = harness.session.exportFile()
    expect(file.text).toContain('"line_type":"trace_event"')
    expect(harness.session.status()).toMatchObject({
      enabled: true, capture_generation: before.capture_generation, expires_at: before.expires_at,
    })
    expect(harness.session.getSnapshot()).toBe(harness.session.getSnapshot())
    await harness.session.checkpoint()
    expect(harness.archive.captures).toHaveLength(1)
    harness.session.dispose()
  })

  it('batches ongoing evidence and saves a sealed capture without discarding it', async () => {
    const harness = setup()
    harness.session.enable()
    record(harness)
    await settle()
    const initialSaves = harness.archive.save.mock.calls.length
    record(harness)
    harness.time.advance(DIAGNOSTICS_CHECKPOINT_INTERVAL_MS - 1)
    await settle()
    expect(harness.archive.save).toHaveBeenCalledTimes(initialSaves)
    harness.time.advance(1)
    await settle()
    expect(harness.archive.save).toHaveBeenCalledTimes(initialSaves + 1)
    harness.session.disable()
    await settle()
    expect(harness.session.getSnapshot().capture).toMatchObject({ state: 'sealed', seal_reason: 'manual_disable' })
    expect(harness.archive.captures[0]?.file.text).toContain('"seal_reason":"manual_disable"')
    expect(harness.session.exportFile().text).toContain('"line_type":"trace_event"')
    harness.session.dispose()
  })

  it('skips unchanged evidence without renewing its saved time, but persists a final seal', async () => {
    const harness = setup()
    harness.session.enable()
    record(harness)
    await harness.session.checkpoint()
    await settle()
    const saves = harness.archive.save.mock.calls.length
    const savedAt = harness.session.getSnapshot().savedAt
    harness.time.advance(DIAGNOSTICS_CHECKPOINT_INTERVAL_MS)
    await settle()
    await harness.session.checkpoint()
    expect(harness.archive.save).toHaveBeenCalledTimes(saves)
    expect(harness.session.getSnapshot().savedAt).toBe(savedAt)
    harness.session.disable()
    await settle()
    expect(harness.archive.save).toHaveBeenCalledTimes(saves + 1)
    await harness.session.checkpoint()
    expect(harness.archive.save).toHaveBeenCalledTimes(saves + 1)
    harness.session.dispose()
  })

  it('does not archive an empty recording that would displace earlier evidence', async () => {
    const harness = setup()
    harness.session.enable()
    harness.time.advance(DIAGNOSTICS_CHECKPOINT_INTERVAL_MS)
    await settle()
    harness.session.disable()
    await harness.session.checkpoint()
    await settle()
    expect(harness.archive.save).not.toHaveBeenCalled()
    expect(harness.session.getSnapshot().hasCurrentEvidence).toBe(false)
    harness.session.dispose()
  })

  it('retries a failed checkpoint even when the evidence has not changed', async () => {
    const archive = memoryArchive()
    archive.save.mockRejectedValueOnce(new Error('temporarily unavailable'))
    const harness = setup({ archive })
    harness.session.enable()
    record(harness)
    await settle()
    expect(harness.session.getSnapshot().archiveUnavailable).toBe(true)
    await harness.session.checkpoint()
    expect(archive.save).toHaveBeenCalledTimes(2)
    expect(archive.captures).toHaveLength(1)
    expect(harness.session.getSnapshot().archiveUnavailable).toBe(false)
    harness.session.dispose()
  })

  it('shows prior evidence separately after reload, isolated by share path', async () => {
    const first = setup()
    first.session.enable()
    record(first)
    first.session.disable()
    await first.session.checkpoint()
    const text = first.archive.captures[0]!.file.text
    first.session.dispose()
    const reopened = setup({ archive: first.archive, runSeed: 2 })
    await settle()
    expect(reopened.session.status().state).toBe('idle')
    expect(reopened.session.getSnapshot().hasCurrentEvidence).toBe(false)
    expect(reopened.archive.readFile).not.toHaveBeenCalled()
    const previous = reopened.session.getSnapshot().previousCaptures[0]!
    expect((await reopened.session.readSavedFile(previous.id))?.text).toBe(text)
    expect(reopened.session.exportFile().text).not.toContain('"line_type":"trace_event"')
    const unrelated = setup({ archive: first.archive, pageUrl: 'https://receiver.invalid/other' })
    await settle()
    expect(unrelated.session.getSnapshot().previousCaptures).toEqual([])
    reopened.session.dispose()
    unrelated.session.dispose()
  })

  it('keeps newer silent failures and older recordings available together after reload', async () => {
    const { archive, time, recording, failure } = await archivedRecordingAndFailure()
    const reopened = setup({ archive, time, runSeed: 3 })
    await settle()
    const previousCaptures = reopened.session.getSnapshot().previousCaptures
    expect(previousCaptures).toEqual([failure, recording].map(summarizeDiagnosticCapture))
    expect(archive.readFile).not.toHaveBeenCalled()
    expect((await reopened.session.readSavedFile(recording.id))?.text).toBe(recording.file.text)
    expect((await reopened.session.readSavedFile(failure.id))?.text).toBe(failure.file.text)

    time.advance(1)
    reopened.composition.failures.record(unclassifiedFailureFact({
      stage: 'content_read', recoveryDisposition: 'retryable', error: new Error('current failure'),
    }))
    await reopened.session.checkpoint()
    await settle()
    expect(archive.captures).toHaveLength(3)
    expect(reopened.session.getSnapshot().previousCaptures).toEqual(previousCaptures)
    reopened.session.dispose()
  })

  it.each([false, true])('expires each historical file on time while recording is %s', async recordingEnabled => {
    const { archive, time, recording, failure } = await archivedRecordingAndFailure()
    time.advance(DIAGNOSTICS_ARCHIVE_MAX_AGE_MS - (time.nowMilliseconds() - recording.savedAt) - 1)
    const reopened = setup({ archive, time, runSeed: 3 })
    if (recordingEnabled) reopened.session.enable()
    await settle()
    expect(reopened.session.getSnapshot().previousCaptures).toHaveLength(2)
    const listener = vi.fn()
    reopened.session.subscribe(listener)

    time.advance(1)
    await settle()
    expect(reopened.session.getSnapshot().previousCaptures).toEqual([summarizeDiagnosticCapture(failure)])
    expect(listener).toHaveBeenCalled()
    time.advance(failure.savedAt - recording.savedAt)
    await settle()
    expect(reopened.session.getSnapshot().previousCaptures).toEqual([])
    expect(archive.readFile).not.toHaveBeenCalled()
    reopened.session.dispose()
  })

  it('checkpoints a failure without replacing capture when the same diagnostic intent is received again', async () => {
    const harness = setup()
    harness.session.enableFromLink()
    record(harness)
    const scope = { scopeKind: 'join' as const, scopeSequence: 1n }
    harness.composition.trace.signal({ kind: 'incident_sealed', incident: { scope, incidentSequence: 1n }, elapsedMs: 0n })
    await settle()
    expect(harness.session.status().state).toBe('recording')
    expect(harness.archive.captures[0]?.file.text).toContain('incident_marker')
    const status = harness.session.status()
    const activation = harness.session.activation()
    harness.session.enableFromLink()
    expect(harness.session.status()).toEqual(status)
    expect(harness.session.activation()).toEqual(activation)
    harness.session.disable()
    await settle()
    expect(harness.session.getSnapshot().activation).toEqual({ kind: 'off' })
    harness.session.dispose()
  })

  it('reacts to timer-driven expiry and console/core capture changes', async () => {
    const harness = setup()
    const listener = vi.fn()
    const stop = harness.session.subscribe(listener)
    harness.composition.runtime.enable()
    await settle()
    expect(harness.session.getSnapshot().capture.enabled).toBe(true)
    harness.time.advance(DEFAULT_TRACE_CAPTURE_EXPIRY_MS)
    await settle()
    expect(harness.session.getSnapshot().capture).toMatchObject({ enabled: false, seal_reason: 'expired' })
    expect(listener).toHaveBeenCalled()
    stop()
    harness.session.dispose()
  })

  it('keeps live export usable when local storage fails', async () => {
    const archive = memoryArchive()
    archive.list.mockRejectedValue(new Error('denied'))
    archive.save.mockRejectedValue(new Error('quota'))
    const harness = setup({ archive })
    harness.session.enable()
    record(harness)
    await harness.session.checkpoint()
    expect(harness.session.getSnapshot().archiveUnavailable).toBe(true)
    expect(harness.session.exportFile().text).toContain('"line_type":"trace_event"')
    expect(harness.session.status().enabled).toBe(true)
    harness.session.dispose()
  })

  it('preserves a previous recording final snapshot while a new recording checkpoints behind a slow write', async () => {
    const archive = memoryArchive()
    let release: () => void = () => undefined
    const blocked = new Promise<void>(resolve => { release = resolve })
    const normalSave = archive.save.getMockImplementation()!
    archive.save.mockImplementationOnce(async capture => {
      await blocked
      return normalSave(capture)
    })
    const harness = setup({ archive })
    const first = harness.session.enable()
    record(harness)
    const pending = harness.session.checkpoint()
    await settle()
    record(harness)
    harness.session.disable()
    await settle()
    expect(harness.session.status()).toMatchObject({ state: 'sealed', retained_event_count: '2' })
    harness.session.enable()
    record(harness)
    harness.time.advance(DIAGNOSTICS_CHECKPOINT_INTERVAL_MS)
    await settle()
    release()
    await pending
    await settle()
    const previous = archive.captures.find(capture =>
      capture.id === `${harness.session.runtimeRunId}.${first.capture_generation}`)!
    const events = previous.file.text.trim().split('\n')
      .filter(line => JSON.parse(line).line_type === 'trace_event')
    expect(events).toHaveLength(2)
    expect(previous.captureState).toBe('sealed')
    expect(previous.file.text).toContain('"seal_reason":"manual_disable"')
    expect(archive.captures).toHaveLength(2)
    expect(harness.session.getSnapshot().previousCaptures).toEqual([summarizeDiagnosticCapture(previous)])
    expect(harness.session.status().state).toBe('recording')
    harness.session.dispose()
  })

  it('serializes clear after an in-flight write so old evidence cannot return', async () => {
    const archive = memoryArchive()
    let release: () => void = () => undefined
    const blocked = new Promise<void>(resolve => { release = resolve })
    const normalSave = archive.save.getMockImplementation()!
    archive.save.mockImplementationOnce(async capture => {
      await blocked
      return normalSave(capture)
    })
    const harness = setup({ archive })
    harness.session.enable()
    record(harness)
    const pending = harness.session.checkpoint()
    await settle()
    harness.session.clear()
    release()
    await pending
    await settle()
    expect(archive.captures).toEqual([])
    expect(harness.session.status().retained_event_count).toBe('0')
    harness.session.dispose()
  })
})

function setup(options: { time?: FakeTraceTime; archive?: ReturnType<typeof memoryArchive>; runSeed?: number; pageUrl?: string } = {}) {
  const time = options.time ?? new FakeTraceTime()
  const archive = options.archive ?? memoryArchive()
  const composition = createBrowserDiagnosticsComposition({
    build: { version: '0.0.0', mode: 'test' },
    secureContext: true,
    consoleSink: { error: () => undefined },
    randomBytes: length => new Uint8Array(length).fill(options.runSeed ?? 1),
    scheduler: time,
    clock: { nowMilliseconds: () => time.nowMilliseconds(), captureTime: () => new Date(time.nowMilliseconds()).toISOString() },
  })
  const session = new BrowserDiagnosticsSession({
    runtime: composition.runtime,
    observeCapture: composition.observeEvidence,
    archive,
    pageUrl: options.pageUrl ?? 'https://receiver.invalid/share',
    scheduler: time,
    now: () => time.nowMilliseconds(),
  })
  return { time, archive, composition, session }
}

async function archivedRecordingAndFailure() {
  const first = setup()
  first.session.enable()
  record(first)
  first.session.disable()
  await first.session.checkpoint()
  const recording = first.archive.captures[0]!
  first.session.dispose()
  first.time.advance(DIAGNOSTICS_CHECKPOINT_INTERVAL_MS * 2)
  const second = setup({ archive: first.archive, time: first.time, runSeed: 2 })
  second.composition.failures.record(unclassifiedFailureFact({
    stage: 'content_read', recoveryDisposition: 'retryable', error: new Error('saved silent failure'),
  }))
  await second.session.checkpoint()
  const failure = second.archive.captures[0]!
  second.session.dispose()
  return { archive: first.archive, time: first.time, recording, failure }
}

function record(harness: ReturnType<typeof setup>) {
  harness.composition.trace.current?.(Object.freeze({
    eventName: 'cleanup', payload: Object.freeze({ backend: 'portable', transition: 'completed' }),
  }))
}

function memoryArchive() {
  let captures: readonly SavedDiagnosticCapture[] = []
  return {
    get captures() { return captures },
    list: vi.fn<DiagnosticsArchiveStore['list']>(async () => captures.map(summarizeDiagnosticCapture)),
    readFile: vi.fn<DiagnosticsArchiveStore['readFile']>(async id => captures.find(capture => capture.id === id)?.file ?? null),
    save: vi.fn<DiagnosticsArchiveStore['save']>(async capture => {
      const candidates = [capture, ...captures.filter(saved => saved.id !== capture.id)]
      const summaries = retainDiagnosticCaptures(candidates.map(summarizeDiagnosticCapture), capture.savedAt)
      captures = candidates.filter(saved => summaries.some(summary => summary.id === saved.id))
      return summaries
    }),
    remove: vi.fn<DiagnosticsArchiveStore['remove']>(async id => {
      captures = captures.filter(capture => capture.id !== id)
      return captures.map(summarizeDiagnosticCapture)
    }),
  }
}

async function settle() {
  for (let turn = 0; turn < 12; turn++) await Promise.resolve()
}

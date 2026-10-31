import { describe, expect, it, vi } from 'vitest'
import {
  DiagnosticsArchiveWriter,
  DIAGNOSTICS_SAVE_MAX_ATTEMPTS,
  type DiagnosticArchiveWriteEvent,
} from '../../../src/diagnostics/browser/archive-writer'
import {
  DIAGNOSTICS_ARCHIVE_MAX_AGE_MS,
  DIAGNOSTICS_ARCHIVE_MAX_CAPTURES,
  DIAGNOSTICS_ARCHIVE_MAX_TOTAL_BYTES,
  type DiagnosticsArchiveStore,
  type SavedDiagnosticCapture,
} from '../../../src/diagnostics/browser/archive'
import { FakeTraceTime } from '../trace/test-support'

const RUN_ID = 'AQAAAAAAAAAAAAAAAAAAAA'

describe('diagnostic archive write ownership', () => {
  it('coalesces checkpoints within a capture while keeping other captures and final seals', async () => {
    const harness = setup()
    const release = blockNextSave(harness)
    harness.writer.save(capture('first', harness, 'initial', 'recording'))
    const draining = harness.writer.flush()
    harness.writer.save(capture('first', harness, 'intermediate', 'recording'))
    harness.writer.save(capture('first', harness, 'final', 'sealed'))
    harness.writer.save(capture('second', harness, 'next recording', 'recording'))
    release()
    await draining
    expect(writes(harness).map(saved => [saved.id, saved.file.text, saved.captureState])).toEqual([
      ['first', 'initial', 'recording'],
      ['first', 'final', 'sealed'],
      ['second', 'next recording', 'recording'],
    ])
    harness.writer.dispose()
  })

  it('bounds pending capture count and prefers newer admissions when timestamps are equal', async () => {
    const harness = setup()
    const release = blockNextSave(harness)
    harness.writer.save(capture('in-flight', harness))
    const draining = harness.writer.flush()
    const ids = Array.from({ length: DIAGNOSTICS_ARCHIVE_MAX_CAPTURES + 2 }, (_, index) => `capture-${index}`)
    for (const id of ids) harness.writer.save(capture(id, harness))
    release()
    await draining
    expect(writes(harness).map(saved => saved.id)).toEqual(['in-flight', ...ids.slice(-DIAGNOSTICS_ARCHIVE_MAX_CAPTURES)])
    expect(harness.trace).toHaveBeenCalledWith(expect.objectContaining({
      event: 'diagnostic_archive_evicted', captureId: ids[0],
    }))
    harness.writer.dispose()
  })

  it('bounds pending UTF-8 bytes even when the capture count would fit', async () => {
    const harness = setup()
    const release = blockNextSave(harness)
    harness.writer.save(capture('in-flight', harness))
    const draining = harness.writer.flush()
    const bytesPerCapture = Math.floor(DIAGNOSTICS_ARCHIVE_MAX_TOTAL_BYTES / DIAGNOSTICS_ARCHIVE_MAX_CAPTURES) + 1
    const text = '界'.repeat(Math.ceil(bytesPerCapture / 3))
    for (const id of ['oldest', 'middle', 'newest']) harness.writer.save(capture(id, harness, text))
    release()
    await draining
    expect(writes(harness).map(saved => saved.id)).toEqual(['in-flight', 'middle', 'newest'])
    harness.writer.dispose()
  })

  it('drops expired pending snapshots using the same retention deadline as stored captures', async () => {
    const harness = setup()
    const release = blockNextSave(harness)
    harness.writer.save(capture('in-flight', harness))
    const draining = harness.writer.flush()
    harness.writer.save(capture('expired', harness))
    harness.time.advance(DIAGNOSTICS_ARCHIVE_MAX_AGE_MS)
    harness.writer.save(capture('fresh', harness))
    release()
    await draining
    expect(writes(harness).map(saved => saved.id)).toEqual(['in-flight', 'fresh'])
    harness.writer.dispose()
  })

  it('retries an older final capture after a newer capture has already saved', async () => {
    const harness = setup()
    const release = blockNextSave(harness, new Error('temporarily unavailable'))
    harness.writer.save(capture('previous-final', harness))
    const draining = harness.writer.flush()
    harness.writer.save(capture('current', harness))
    release()
    await draining
    expect(writes(harness).map(saved => saved.id)).toEqual(['previous-final', 'current'])
    harness.time.advance(1_000)
    await settle()
    expect(writes(harness).map(saved => saved.id)).toEqual(['previous-final', 'current', 'previous-final'])
    expect(harness.onFailure).toHaveBeenCalledTimes(1)
    expect(harness.trace).toHaveBeenCalledWith(expect.objectContaining({
      event: 'diagnostic_archive_write', captureId: 'previous-final', attempt: 2, outcome: 'saved',
    }))
    harness.writer.dispose()
  })

  it('keeps retry budgets per snapshot when other captures arrive or checkpoints repeat', async () => {
    const harness = setup()
    harness.archive.save.mockRejectedValue(new Error('unavailable'))
    harness.writer.save(capture('previous-final', harness))
    await harness.writer.flush()
    harness.writer.save(capture('current', harness))
    await harness.writer.flush()
    for (let attempt = 0; attempt < DIAGNOSTICS_SAVE_MAX_ATTEMPTS + 1; attempt++) {
      harness.time.advance(5_000)
      await settle()
      await harness.writer.flush()
    }
    for (const id of ['previous-final', 'current']) {
      expect(writes(harness).filter(saved => saved.id === id)).toHaveLength(DIAGNOSTICS_SAVE_MAX_ATTEMPTS)
    }
    harness.writer.dispose()
  })

  it('revokes a replaced snapshot retry even if its cancelled callback still runs', async () => {
    const harness = setup()
    harness.archive.save.mockRejectedValueOnce(new Error('unavailable'))
    harness.writer.save(capture('same', harness, 'obsolete'))
    await harness.writer.flush()
    harness.writer.save(capture('same', harness, 'final'))
    await harness.writer.flush()
    harness.time.runEvenIfCancelled(0)
    await settle()
    expect(writes(harness).map(saved => saved.file.text)).toEqual(['obsolete', 'final'])
    harness.writer.dispose()
  })

  it('keeps clear ordered after an in-flight write and exempts deletion from capture eviction', async () => {
    const harness = setup()
    const release = blockNextSave(harness, new Error('unavailable'))
    harness.writer.save(capture('cleared', harness))
    const draining = harness.writer.flush()
    harness.writer.remove('cleared')
    for (let index = 0; index <= DIAGNOSTICS_ARCHIVE_MAX_CAPTURES; index++) {
      harness.writer.save(capture(`other-${index}`, harness))
    }
    expect(harness.archive.remove).not.toHaveBeenCalled()
    release()
    await draining
    harness.time.advance(5_000)
    await settle()
    expect(harness.archive.remove).toHaveBeenCalledExactlyOnceWith('cleared')
    expect(writes(harness).filter(saved => saved.id === 'cleared')).toHaveLength(1)
    expect(writes(harness)).toHaveLength(1 + DIAGNOSTICS_ARCHIVE_MAX_CAPTURES)
    harness.writer.dispose()
  })

  it('finishes queued final saves on disposal without scheduling new retries', async () => {
    const harness = setup()
    const release = blockNextSave(harness, new Error('unavailable'))
    harness.writer.save(capture('in-flight', harness))
    const draining = harness.writer.flush()
    harness.writer.save(capture('final', harness))
    harness.writer.dispose()
    release()
    await draining
    harness.time.advance(5_000)
    await settle()
    expect(writes(harness).map(saved => saved.id)).toEqual(['in-flight', 'final'])
  })

  it('does not let a throwing trace observer interrupt archival', async () => {
    const harness = setup()
    harness.trace.mockImplementation(() => { throw new Error('observer failed') })
    harness.writer.save(capture('final', harness))
    await harness.writer.flush()
    expect(harness.onSaved).toHaveBeenCalledTimes(1)
    expect(harness.onFailure).not.toHaveBeenCalled()
    harness.writer.dispose()
  })
})

function setup() {
  const time = new FakeTraceTime()
  const archive = {
    save: vi.fn<DiagnosticsArchiveStore['save']>(async () => []),
    remove: vi.fn<DiagnosticsArchiveStore['remove']>(async () => []),
  }
  const onSaved = vi.fn()
  const onFailure = vi.fn()
  const trace = vi.fn<(event: DiagnosticArchiveWriteEvent) => void>()
  const writer = new DiagnosticsArchiveWriter({
    archive, scheduler: time, now: () => time.nowMilliseconds(), onSaved, onFailure, trace,
  })
  return { time, archive, onSaved, onFailure, trace, writer }
}

function capture(
  id: string, harness: ReturnType<typeof setup>, text = id,
  captureState: SavedDiagnosticCapture['captureState'] = 'sealed',
): SavedDiagnosticCapture {
  return { id, savedAt: harness.time.nowMilliseconds(), scope: '/share', captureState,
    file: { name: 'diagnostics.ndjson', runtimeRunId: RUN_ID, text } }
}

function blockNextSave(harness: ReturnType<typeof setup>, error?: Error): () => void {
  let release: () => void = () => undefined
  const blocked = new Promise<void>(resolve => { release = resolve })
  harness.archive.save.mockImplementationOnce(async () => {
    await blocked
    if (error !== undefined) throw error
    return []
  })
  return release
}

function writes(harness: ReturnType<typeof setup>): SavedDiagnosticCapture[] {
  return harness.archive.save.mock.calls.map(([saved]) => saved)
}

async function settle() {
  for (let turn = 0; turn < 12; turn++) await Promise.resolve()
}

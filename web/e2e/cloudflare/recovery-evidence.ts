import { setTimeout as delay } from 'node:timers/promises'
import { expect, type Page } from '@playwright/test'

import type { TraceEventRecordV2 } from '../../src/diagnostics/trace/model'
import type { WindShareDiagnostics } from '../../src/diagnostics/export/developer-api'

export const EVIDENCE_POLL_MILLISECONDS = 250

interface SenderTraceRecord {
  readonly sequence: string
  readonly event: string
  readonly payload: {
    readonly state?: string
    readonly available?: number
    readonly details?: { readonly connection_generation: string; readonly resume_registration: boolean }
  }
}

function senderRecords(encoded: string): readonly SenderTraceRecord[] {
  return encoded.slice(0, encoded.lastIndexOf('\n')).split('\n').filter(Boolean)
    .map(line => JSON.parse(line) as SenderTraceRecord)
}

export function senderTraceSequence(encoded: string): string {
  return senderRecords(encoded).at(-1)!.sequence
}

export function senderRecoveryEvidence(encoded: string, afterSequence: string) {
  const records = senderRecords(encoded).filter(record => BigInt(record.sequence) > BigInt(afterSequence))
  const events = records.filter(record => record.event === 'relay_availability' || record.event === 'relay_recovering')
  if (events.length === 0) return { outcome: 'uninterrupted' as const, events }
  const lost = records.find(record => record.event === 'relay_availability' && record.payload.available === 0)
  expect(lost, 'Sender must observe loss of publication').toBeDefined()
  const resumed = records.find(record => record.event === 'relay_recovering' && record.payload.state === 'succeeded')
  expect(resumed, 'Sender must automatically recover publication').toBeDefined()
  expect(resumed!.payload.details?.resume_registration).toBe(true)
  expect(Number(resumed!.payload.details?.connection_generation)).toBeGreaterThan(1)
  expect(records.some(record => record.event === 'relay_availability' && record.payload.available === 1 &&
    BigInt(record.sequence) > BigInt(lost!.sequence))).toBe(true)
  return { outcome: 'resumed' as const, events }
}

type ProgressRecord = Extract<TraceEventRecordV2, { event: 'transfer_progress' }>

export interface DownloadCut {
  readonly sequence: string
  readonly progress: ProgressRecord['payload']
  readonly completedBlocks: number
  readonly blockBytes: number
}

/** Polling preserves evidence as the product's bounded diagnostic ring advances. */
export class RecoveryEvidence {
  readonly #records = new Map<string, TraceEventRecordV2>()
  #bundle = ''
  #runtimeRunId: string | undefined

  async capture(page: Page): Promise<void> {
    this.#bundle = await page.evaluate(() => (window as Window & {
      windshareDiagnostics: WindShareDiagnostics
    }).windshareDiagnostics.export())
    for (const line of this.#bundle.trim().split('\n')) {
      const value = JSON.parse(line) as { line_type: string; runtime_run_id?: string; record?: TraceEventRecordV2 }
      if (value.line_type === 'bundle_header') {
        this.#runtimeRunId ??= value.runtime_run_id
        expect(value.runtime_run_id, 'Browser runtime must survive without refresh').toBe(this.#runtimeRunId)
      }
      if (value.line_type === 'trace_event' && value.record) this.#records.set(value.record.sequence, value.record)
    }
  }

  async during<T>(page: Page, operation: () => Promise<T>): Promise<T> {
    let pending = true
    const completion = operation().then(
      value => ({ ok: true as const, value }),
      error => ({ ok: false as const, error: error as unknown }),
    ).finally(() => { pending = false })
    try {
      // Cloud deployment can outlive the diagnostic ring. Keep observing while
      // Wrangler runs, and join it before any failure tears down the Worker.
      while (pending) {
        await this.capture(page)
        await delay(EVIDENCE_POLL_MILLISECONDS)
      }
    } finally {
      await completion
    }
    const result = await completion
    if (!result.ok) throw result.error
    await this.capture(page)
    return result.value
  }

  records(): readonly TraceEventRecordV2[] {
    return [...this.#records.values()].sort((a, b) => Number(BigInt(a.sequence) - BigInt(b.sequence)))
  }

  bundle(): string { return this.#bundle }

  collected(): string {
    return this.records().map(record => JSON.stringify({ line_type: 'trace_event', record })).join('\n') + '\n'
  }

  writtenBytes(): number {
    return Number(this.#progress().at(-1)?.payload.written_bytes ?? 0)
  }

  cut(blockBytes: number, fileBytes: number): DownloadCut {
    const progress = this.#progress().at(-1)!
    expect(progress, 'Interruption requires accepted output progress').toBeDefined()
    const written = Number(progress.payload.written_bytes)
    expect(written).toBeGreaterThanOrEqual(blockBytes)
    expect(written, 'Interruption must occur before the file finishes').toBeLessThan(fileBytes)
    return {
      sequence: this.records().at(-1)!.sequence,
      progress: progress.payload,
      completedBlocks: Math.floor(written / blockBytes),
      blockBytes,
    }
  }

  verify(cut: DownloadCut, fileBytes: number) {
    const records = this.records()
    const progress = this.#progress()
    expect(progress.length).toBeGreaterThan(1)
    let previous = 0
    for (const { payload } of progress) {
      expect(payload.operation_id).toBe(cut.progress.operation_id)
      expect(payload.transfer_job_id).toBe(cut.progress.transfer_job_id)
      expect(Number(payload.written_bytes), 'Accepted output progress cannot reset').toBeGreaterThanOrEqual(previous)
      previous = Number(payload.written_bytes)
      expect(previous).toBeLessThanOrEqual(fileBytes)
      expect(payload.file_errors).toBe('0')
      expect(payload.partial).toBe(false)
    }
    const later = records.filter(record => BigInt(record.sequence) > BigInt(cut.sequence))
    expect(later.some(record => record.event === 'transfer_progress' &&
      Number(record.payload.written_bytes) > Number(cut.progress.written_bytes))).toBe(true)
    const dispatches = later.filter(record => record.event === 'content_scheduling')
    expect(dispatches.length, 'Recovery must dispatch remaining content').toBeGreaterThan(0)
    let completedBlocks = cut.completedBlocks
    for (const record of later) {
      if (record.event === 'transfer_progress') {
        completedBlocks = Math.floor(Number(record.payload.written_bytes) / cut.blockBytes)
      }
      if (record.event === 'content_scheduling') {
        // Progress can advance while the deployment command is still running.
        expect(Number(record.payload.block_index), 'Already written blocks must not be requested again')
          .toBeGreaterThanOrEqual(completedBlocks)
        expect(record.payload.route).toBe('application-relay')
      }
    }
    const recovered = later.filter(record => record.event === 'connection_recovery' && record.payload.transition === 'connected')
    const completed = records.filter(record => record.event === 'receive_transition' &&
      record.payload.transition === 'download_connectivity')
    expect(completed).toHaveLength(1)
    const final = completed[0]!
    if (final.event !== 'receive_transition' || final.payload.transition !== 'download_connectivity') {
      throw new Error('Missing final connectivity evidence')
    }
    expect(final.payload.transfer_job_id).toBe(cut.progress.transfer_job_id)
    expect(final.payload.connectivity).toMatchObject({
      application_relay_bytes: String(fileBytes), direct_bytes: '0', final: true, incomplete: false,
    })
    return {
      operationId: cut.progress.operation_id,
      transferJobId: cut.progress.transfer_job_id,
      writtenBytesBeforeInterruption: Number(cut.progress.written_bytes),
      completedBlocksBeforeInterruption: cut.completedBlocks,
      firstSubsequentBlock: Number(dispatches[0]!.payload.block_index),
      minimumSubsequentBlock: Math.min(...dispatches.map(record => Number(record.payload.block_index))),
      browserRecovery: recovered.map(record => record.payload),
      protocolSessions: [...new Set(records.filter(record => record.event === 'content_scheduling')
        .map(record => record.correlation?.protocol_session_id))],
    }
  }

  #progress(): readonly ProgressRecord[] {
    return this.records().filter(record => record.event === 'transfer_progress')
  }
}

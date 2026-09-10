import { afterEach, describe, expect, it, vi } from 'vitest'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import type { ReceiveLifecycleState } from '../../src/output/workspace/state'
import type { DirectZipIntent } from '../../src/transfer/direct-zip'
import { SelectionMeasureTracker } from '../../src/transfer/measure'
import { EMPTY_TRANSFER_FAILURE_SUMMARY, transferWorkerSettlement } from '../../src/transfer/outcome'
import {
  TransferPauseRequestedError,
  type DirectResumableZipExecution,
  type MaterializationSummary,
} from '../../src/transfer/output-session'
import { V2TransferProgressLedger } from '../../src/transfer/progress/v2-ledger'
import { pauseFailedV2Execution } from '../../src/transfer/settlement/v2-output'
import { V2JobFailureAuthority } from '../../src/transfer/v2-job-failure-authority'
import { TransferJobSettlement } from '../../src/transfer/v2-job-settlement'
import { createWriterHarness, fileAdmission } from '../output/direct-zip/writer/fault-model'
import { deferred, manualSettlementDeadline } from './settlement-deadline'
import { planAuthorityFixture } from './v2-job-fixture'

const PAYLOAD = Uint8Array.of(1, 2, 3, 4, 5, 6)
const DEADLINE_MILLISECONDS = 1

afterEach(() => vi.useRealTimers())

describe('Direct ZIP durable settlement ownership', () => {
  it('keeps a slow final close in finishing until publication wins over its deadline', async () => {
    vi.useFakeTimers()
    const fixture = await settlementFixture(true)
    // Match the job's failure boundary so a detached timeout would attempt Pause.
    const running = fixture.settlement.completeWorkers(fixture.measure.snapshot())
      .catch(error => fixture.settlement.settleRunFailure(error))
    let exposed = false
    const observed = running.then(() => { exposed = true })
    await fixture.closing.promise
    fixture.deadline.expire()
    await vi.advanceTimersByTimeAsync(0)

    expect(exposed).toBe(false)
    expect(fixture.progress.snapshot(fixture.measure.snapshot()).phase).toBe('finishing')
    expect(fixture.pause).not.toHaveBeenCalled()
    expect(fixture.unknown).not.toHaveBeenCalled()
    expect(fixture.harness.cuts.promoted).toHaveLength(0)

    fixture.releaseClose.resolve()
    const result = await running
    await observed
    expect(result.worker.status).toBe('Succeeded')
    expect(result.lifecycle.kind).toBe('published')
    expect(result.abortReason).toBeUndefined()
    expect(fixture.pause).not.toHaveBeenCalled()
    expect(fixture.unknown).not.toHaveBeenCalled()
    expect(fixture.harness.target.openEpochCount).toBe(1)
    expect(fixture.harness.target.closeAttemptCount).toBe(1)
    expect(fixture.harness.cuts.promoted[0]?.checkpoint.safeResumeBytes).toBe(6n)
  })

  it.each(['pause', 'admission recovery'] as const)(
    'drains a slow %s close before returning its durable checkpoint',
    async route => {
      vi.useFakeTimers()
      const fixture = await settlementFixture(false)
      const running = pauseFailedV2Execution({
        intent: fixture.intent,
        ...(route === 'pause' ? { execution: fixture.execution } : {}),
        authority: fixture.plans,
        worker: transferWorkerSettlement('Paused', EMPTY_TRANSFER_FAILURE_SUMMARY),
        materialization: fixture.summary,
        selectionFacts: { discoveredFileCount: 1n, discoveredBytes: 6n, discovery: 'complete' },
        reason: new TransferPauseRequestedError(),
        timeoutMilliseconds: DEADLINE_MILLISECONDS,
        deadline: fixture.deadline,
      })
      let exposed = false
      const observed = running.then(() => { exposed = true })
      await fixture.closing.promise
      fixture.deadline.expire()
      await vi.advanceTimersByTimeAsync(0)

      expect(exposed).toBe(false)
      expect(fixture.unknown).not.toHaveBeenCalled()
      expect(fixture.harness.cuts.promoted).toHaveLength(0)
      fixture.releaseClose.resolve()
      expect(await running).toMatchObject({
        kind: 'resumable-receive', payloadKind: 'direct-zip', safeSelectedPayloadBytes: 3n,
      })
      await observed
      expect(fixture.unknown).not.toHaveBeenCalled()
      expect(fixture.harness.target.closeAttemptCount).toBe(1)
      expect(fixture.harness.cuts.promoted[0]?.checkpoint.member?.payloadOffset).toBe(3n)
    },
  )
})

async function settlementFixture(complete: boolean) {
  const harness = createWriterHarness()
  const closing = deferred()
  const releaseClose = deferred()
  const openEpoch = harness.target.openEpoch.bind(harness.target)
  harness.target.openEpoch = async () => {
    const opened = await openEpoch()
    if (opened.kind !== 'opened') return opened
    return { kind: 'opened', writable: { ...opened.writable,
      closeOnce: async () => {
        closing.resolve()
        await releaseClose.promise
        return opened.writable.closeOnce()
      },
    } }
  }
  const writer = harness.writer()
  const member = await writer.beginFile(fileAdmission(harness.checkpoint))
  await member.write(complete ? PAYLOAD : PAYLOAD.subarray(0, 3))
  if (complete) await member.close()
  const intent = { operationId: 'operation-1', digest: 'intent-1',
    plan: { kind: 'direct-resumable-zip' } } as DirectZipIntent
  const identity = { operationId: intent.operationId, receiveIntentDigest: intent.digest, generation: 2n }
  const summary: MaterializationSummary = {
    entryCount: complete ? 2n : 1n, directoryCount: 1n,
    fileCount: complete ? 1n : 0n, rawBytes: complete ? 6n : 0n,
  }
  const pause = vi.fn(async (): Promise<ReceiveLifecycleState> => {
    const cut = await writer.pause()
    if (cut.kind === 'replay-required') throw new Error('Pause failed to persist its prefix')
    return { ...identity, kind: 'resumable-receive', payloadKind: 'direct-zip',
      directZipCheckpointDigest: 'checkpoint-2',
      safeSelectedPayloadBytes: cut.checkpoint.safeResumeBytes,
      committedArchiveLength: cut.checkpoint.committedLength,
      checkpointPhase: cut.checkpoint.phase }
  })
  const execution: DirectResumableZipExecution = {
    planKind: 'direct-resumable-zip',
    output: {
      identity: { backend: 'file_system_access', outputSessionId: 'output-1' },
      capabilities: { durability: 'ProcessRestart', randomWrite: false,
        fileFailureIsolation: false, modificationTime: true },
      beginFile: async () => { throw new Error('Content was already received') },
    },
    ordered: {
      beginTraversal: async () => undefined,
      visit: async () => { throw new Error('Traversal was already completed') },
      finishTraversal: async () => undefined,
      materializationSummary: () => summary,
    },
    pause,
    settle: async () => {
      const pages = await harness.pages.snapshot()
      await writer.closeArchive({
        entryCount: 2n, centralDirectoryBytes: pages.centralBytes,
        layoutRoot: pages.layoutRoot, centralRoot: pages.centralRoot,
        predecessorEpochRoot: writer.committedCheckpoint.epochRoot,
      })
      return { ...identity, kind: 'published', receiptDigest: 'publication', cleanupState: 'clean' }
    },
  }
  const unknown = vi.fn(async (): Promise<Extract<ReceiveLifecycleState, { kind: 'needs-attention' }>> =>
    ({ ...identity, kind: 'needs-attention', reason: 'publication-unknown', lastVerifiedRecordDigest: 'checkpoint-1' }))
  const plans = { ...planAuthorityFixture(), settleExecutionAdmissionFailure: pause, recordSettlementUnknown: unknown }
  const deadline = manualSettlementDeadline()
  const measure = new SelectionMeasureTracker()
  measure.observeUniqueFile(6n)
  measure.complete()
  const progress = new V2TransferProgressLedger()
  if (complete) progress.completeFile(6n)
  const lifetime = new AbortController()
  const observers = () => undefined
  const emitProgress = () => undefined
  const failures = new V2JobFailureAuthority({
    selection: new V2SelectionPolicy(true).snapshot(), signal: lifetime.signal,
    measure, progress, observers, emitProgress,
  })
  const settlement = new TransferJobSettlement({
    options: { plans, outputSettlementDeadline: deadline },
    lifetime, measure, progress, failures, transferJobId: 'job-1',
    outputSettlementTimeoutMilliseconds: DEADLINE_MILLISECONDS,
    intent: () => intent,
    execution: () => execution,
    preparation: () => undefined,
    observers, emitProgress,
    externalCancellationRequested: () => false,
    materializationSummary: () => summary,
  })
  return { harness, closing, releaseClose, intent, summary, pause, execution, unknown,
    plans, deadline, measure, progress, settlement }
}

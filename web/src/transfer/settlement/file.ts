import type { ByteRangeSet } from '../../content/geometry'
import type { RevisionReopenedDuringRecoveryError } from '../../content/session-services'
import type { IncidentScopeHandle } from '../../diagnostics/incident'
import { OutputCapacityBlockedError } from '../capacity-pressure/drain'
import {
  FaultDomain, FaultScope, OutputFaultCode, SourceFaultCode,
  authorizeFileRetirement, outputFault, promoteFaultScope, sourceFault, type Fault,
} from '../failure/fault'
import type { OutputFileTransaction, OutputSession, VerifiedDurableRanges } from '../output/session'
import { retireOutputFileTransaction } from '../output/transaction'
import { withOutputSettlementTimeout } from './output'
import type { FileTransferSettlement } from '../job/contract'
import {
  findDeclaredOutputFailure, normalizeFileTransferFailure, normalizedFileTransferFault, type NormalizedFileTransferFailure,
} from '../job/failures'
import {
  observeRevisionReadmission, rangeBytes,
  type FileRevisionReadmission, type FileRevisionReadmissionObservation,
} from '../job/file-authority'

export const UNCONFIRMED_FILE_SETTLEMENT = Object.freeze({ kind: 'unconfirmed' })

interface FileSettlementOptions {
  readonly output: Pick<OutputSession, 'capabilities' | 'readmitFile'>
  readonly signal: AbortSignal
  readonly outputSettlementTimeoutMilliseconds: number
  readonly incidentScope?: IncidentScopeHandle
  readonly onRevisionReadmission?: (event: FileRevisionReadmissionObservation) => void
}

type FileFault = Extract<NormalizedFileTransferFailure, { kind: 'fault' }>
type PausedFileSettlement = Extract<FileTransferSettlement, { kind: 'paused' }>

interface SettledFileFailure {
  readonly kind: 'failed'
  readonly failure: NormalizedFileTransferFailure
  readonly settlement: FileTransferSettlement
  readonly capacityBlocked?: OutputCapacityBlockedError
}

export interface FileContinuationRequired {
  readonly kind: 'continuation-required'
  readonly settlement: Extract<FileTransferSettlement, { kind: 'paused' | 'unconfirmed' }>
  readonly failure: FileFault
}

export type FileAttemptInterruption = SettledFileFailure | FileContinuationRequired | {
  readonly kind: 'readmit'
  readonly settlement: PausedFileSettlement
  readonly readmission: FileRevisionReadmission
}

// Allow one readmission with zero newly persisted bytes; stop once a second
// consecutive reopen still fails to advance durable coverage.
const MAX_STALLED_REVISION_READMISSIONS = 1

export async function settleReopenedFile(
  options: FileSettlementOptions, transaction: OutputFileTransaction,
  error: RevisionReopenedDuringRecoveryError, initialDurable: ByteRangeSet, started: number, priorStalledReopens: number,
): Promise<FileAttemptInterruption> {
  const { previous, current, comparison } = error
  const readmit = options.output.readmitFile?.bind(options.output)
  const initialDecision = readmissionDecision(options.signal, comparison, readmit !== undefined)
  const failureOptions = {
    signal: options.signal,
    ...(options.incidentScope === undefined ? {} : { incidentScope: options.incidentScope }),
  }

  let paused: VerifiedDurableRanges
  try {
    // Verify the retained cut while it still describes the active revision.
    // Recording a source failure can withdraw these ranges from reuse.
    paused = await pauseOutputFile(options, transaction, error)
  } catch (reason) {
    observeRevisionReadmission(options, previous, current, comparison, 'settlement-failed', 0n, 0n, started)
    return unconfirmedPause(options, reason)
  }

  const settlement = pausedFileSettlement(paused)
  const stalledReopens = nextStalledReopens(settlement.retainedBytes, initialDurable, priorStalledReopens)
  const decision = resolveReopenedDecision(initialDecision, stalledReopens)
  const failure = classifyReopenedFileFailure(error, decision, failureOptions)
  const persistError = await persistReopenedFailure(options, transaction, failure)
  if (persistError !== undefined) {
    observeRevisionReadmission(options, previous, current, comparison, 'settlement-failed', 0n, 0n, started)
    return unconfirmedPause(options, persistError.reason)
  }

  const finalDecision = options.signal.aborted ? 'canceled' : decision
  observeRevisionReadmission(options, previous, current, comparison, finalDecision, settlement.retainedBytes, 0n, started)
  if (finalDecision === 'readmit' && readmit !== undefined) {
    return {
      kind: 'readmit',
      settlement,
      readmission: {
        previous, current, comparison, started, paused, initialDurable, stalledReopens,
        beginFile: (request, signal) => readmit(request, paused, signal),
      },
    }
  }
  return finishReopenedFailure(options, error, finalDecision, settlement, failure, failureOptions)
}

function nextStalledReopens(retainedBytes: bigint, initialDurable: ByteRangeSet, priorStalledReopens: number): number {
  return retainedBytes > rangeBytes(initialDurable) ? 0 : priorStalledReopens + 1
}

function resolveReopenedDecision(
  initial: FileRevisionReadmissionObservation['decision'],
  stalledReopens: number,
): FileRevisionReadmissionObservation['decision'] {
  return initial === 'readmit' && stalledReopens > MAX_STALLED_REVISION_READMISSIONS ? 'retry-limit' : initial
}

async function persistReopenedFailure(
  options: FileSettlementOptions,
  transaction: OutputFileTransaction,
  failure: NormalizedFileTransferFailure | undefined,
): Promise<Readonly<{ reason: unknown }> | undefined> {
  if (options.signal.aborted || failure?.kind !== 'fault') return undefined
  try {
    await persistSourceFailure(options, transaction, failure.fault)
    return undefined
  } catch (reason) {
    return { reason }
  }
}

function finishReopenedFailure(
  options: FileSettlementOptions,
  error: RevisionReopenedDuringRecoveryError,
  finalDecision: FileRevisionReadmissionObservation['decision'],
  settlement: PausedFileSettlement,
  failure: NormalizedFileTransferFailure | undefined,
  failureOptions: Pick<FileSettlementOptions, 'signal' | 'incidentScope'>,
): SettledFileFailure | FileContinuationRequired {
  const terminalFailure = finalDecision === 'canceled'
    ? normalizeFileTransferFailure(options.signal.reason, failureOptions)
    : failure ?? normalizeFileTransferFailure(error, failureOptions)
  if (terminalFailure.kind === 'fault') {
    if (!options.output.capabilities.fileFailureIsolation) {
      return { kind: 'failed', settlement, failure: promoteFileFaultToOutputPause(options, terminalFailure) }
    }
    if (finalDecision === 'retry-limit') {
      return { kind: 'continuation-required', settlement, failure: terminalFailure }
    }
  }
  return { kind: 'failed', settlement, failure: terminalFailure }
}

function readmissionDecision(
  signal: AbortSignal, comparison: RevisionReopenedDuringRecoveryError['comparison'], supported: boolean,
): FileRevisionReadmissionObservation['decision'] {
  if (signal.aborted) return 'canceled'
  if (comparison.kind === 'observed-change') return 'observed-change'
  if (comparison.kind === 'unavailable') return 'evidence-unavailable'
  return supported ? 'readmit' : 'readmission-unsupported'
}

function classifyReopenedFileFailure(
  error: RevisionReopenedDuringRecoveryError,
  decision: FileRevisionReadmissionObservation['decision'],
  options: Pick<FileSettlementOptions, 'signal' | 'incidentScope'>,
): NormalizedFileTransferFailure | undefined {
  if (decision === 'readmit' || decision === 'canceled') return undefined
  if (decision === 'retry-limit') {
    return normalizedFileTransferFault(sourceFault(FaultScope.FileLocal, SourceFaultCode.Unavailable), {
      ...(options.incidentScope === undefined ? {} : { incidentScope: options.incidentScope }),
      error,
      recoveryDisposition: 'resumable_receive',
    })
  }
  return normalizeFileTransferFailure(error, options)
}

export async function settleTransferError(
  options: FileSettlementOptions,
  transaction: OutputFileTransaction | undefined,
  error: unknown,
): Promise<SettledFileFailure | FileContinuationRequired> {
  if (error instanceof OutputCapacityBlockedError && !options.signal.aborted) {
    // Release the write lease before waiting so admitted files retain capacity
    // to drain the payload buffers.
    try {
      const settlement = transaction === undefined ? UNCONFIRMED_FILE_SETTLEMENT : pausedFileSettlement(
        await withOutputSettlementTimeout('checkpoint capacity-blocked file',
          options.outputSettlementTimeoutMilliseconds, () => transaction.pause(error)),
      )
      return {
        kind: 'failed', capacityBlocked: error, settlement,
        failure: normalizeFileTransferFailure(error, { stage: 'output_write' }),
      }
    } catch (settlementFailure) {
      return {
        kind: 'failed', settlement: UNCONFIRMED_FILE_SETTLEMENT,
        failure: normalizeFileTransferFailure(settlementFailure, { stage: 'settlement' }),
      }
    }
  }

  const failure = normalizeFileTransferFailure(error, {
    signal: options.signal,
    ...(options.incidentScope === undefined ? {} : { incidentScope: options.incidentScope }),
  })
  if (transaction === undefined) {
    if (failure.kind === 'fault' && failure.fault.scope === FaultScope.FileLocal &&
        options.output.capabilities.fileFailureIsolation && options.output.capabilities.durability !== 'None' &&
        findDeclaredOutputFailure(error)?.kind === 'deferred-file-admission') {
      return { kind: 'continuation-required', failure, settlement: UNCONFIRMED_FILE_SETTLEMENT }
    }
    return { kind: 'failed', failure, settlement: UNCONFIRMED_FILE_SETTLEMENT }
  }
  if (failure.kind === 'canceled') return pauseCanceledFileTransfer(options, transaction, failure)
  try {
    await persistSourceFailure(options, transaction, failure.fault)
  } catch (error) {
    return {
      kind: 'failed',
      failure: normalizeFileTransferFailure(error, { stage: 'settlement' }),
      settlement: UNCONFIRMED_FILE_SETTLEMENT,
    }
  }
  return {
    kind: 'failed',
    failure: await settleFileFault(options, transaction, failure),
    settlement: UNCONFIRMED_FILE_SETTLEMENT,
  }
}

async function persistSourceFailure(
  options: FileSettlementOptions,
  transaction: OutputFileTransaction,
  fault: Fault,
): Promise<void> {
  if (fault.domain !== FaultDomain.Source || transaction.recordSourceFailure === undefined) return
  await withOutputSettlementTimeout('persist source revision failure', options.outputSettlementTimeoutMilliseconds,
    () => transaction.recordSourceFailure!(fault))
}

async function settleFileFault(
  options: FileSettlementOptions, transaction: OutputFileTransaction, failure: FileFault,
): Promise<NormalizedFileTransferFailure> {
  const authorization = authorizeFileRetirement(failure.fault)
  if (authorization === undefined) return promoteFileFaultToOutputPause(options, failure)
  try {
    const disposition = await withOutputSettlementTimeout(
      'retire output file transaction',
      options.outputSettlementTimeoutMilliseconds,
      () => retireOutputFileTransaction(transaction, authorization),
    )
    if (disposition === 'FileIsolated') return failure
  } catch (retirementFailure) {
    normalizeFileTransferFailure(retirementFailure, {
      ...(options.incidentScope === undefined ? {} : { incidentScope: options.incidentScope }),
      relation: 'consequence',
      stage: 'settlement',
    })
  }
  return ambiguousOutputMutation(options, failure.diagnostic, failure.materializationFailureReason)
}

function promoteFileFaultToOutputPause(options: FileSettlementOptions, failure: FileFault): FileFault {
  if (failure.fault.scope === FaultScope.OutputPause || failure.fault.scope === FaultScope.SessionTerminal) return failure
  return normalizedFileTransferFault(promoteFaultScope(failure.fault, FaultScope.OutputPause), {
    ...(options.incidentScope === undefined ? {} : { incidentScope: options.incidentScope }),
    stage: 'settlement',
    error: failure.diagnostic,
    materializationFailureReason: failure.materializationFailureReason,
  })
}

async function pauseCanceledFileTransfer(
  options: FileSettlementOptions,
  transaction: OutputFileTransaction,
  failure: Extract<NormalizedFileTransferFailure, { kind: 'canceled' }>,
): Promise<SettledFileFailure> {
  try {
    const durable = await pauseOutputFile(options, transaction, failure.diagnostic)
    return { kind: 'failed', failure, settlement: pausedFileSettlement(durable) }
  } catch (pauseFailure) {
    normalizeFileTransferFailure(pauseFailure, {
      ...(options.incidentScope === undefined ? {} : { incidentScope: options.incidentScope }),
      relation: 'consequence',
      stage: 'settlement',
    })
    return unconfirmedPause(options, pauseFailure)
  }
}

function pauseOutputFile(
  options: FileSettlementOptions, transaction: OutputFileTransaction, reason: unknown,
): Promise<VerifiedDurableRanges> {
  return withOutputSettlementTimeout(
    'pause output file transaction', options.outputSettlementTimeoutMilliseconds, () => transaction.pause(reason),
  )
}

function pausedFileSettlement(durable: VerifiedDurableRanges): PausedFileSettlement {
  return Object.freeze({ kind: 'paused', retainedBytes: rangeBytes(durable.asRangeSet()) })
}

function unconfirmedPause(options: FileSettlementOptions, reason: unknown): SettledFileFailure {
  return {
    kind: 'failed',
    failure: ambiguousOutputMutation(options, reason, 'output-write-failed'),
    settlement: UNCONFIRMED_FILE_SETTLEMENT,
  }
}

function ambiguousOutputMutation(
  options: FileSettlementOptions, reason: unknown, materializationFailureReason: FileFault['materializationFailureReason'],
): FileFault {
  return normalizedFileTransferFault(outputFault(FaultScope.OutputPause, OutputFaultCode.MutationAmbiguous), {
    ...(options.incidentScope === undefined ? {} : { incidentScope: options.incidentScope }),
    stage: 'settlement',
    error: reason,
    materializationFailureReason,
  })
}

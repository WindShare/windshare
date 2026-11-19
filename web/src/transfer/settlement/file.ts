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
  normalizeFileTransferFailure, normalizedFileTransferFault, type NormalizedFileTransferFailure,
} from '../job/failures'
import {
  observeRevisionReadmission, rangeBytes,
  type FileRevisionReadmission, type FileRevisionReadmissionObservation,
} from '../job/file-authority'

export const UNCONFIRMED_FILE_SETTLEMENT: FileTransferSettlement = Object.freeze({ kind: 'unconfirmed' })

interface FileSettlementOptions {
  readonly output: Pick<OutputSession, 'capabilities' | 'readmitFile'>
  readonly signal: AbortSignal
  readonly outputSettlementTimeoutMilliseconds: number
  readonly incidentScope?: IncidentScopeHandle
  readonly onRevisionReadmission?: (event: FileRevisionReadmissionObservation) => void
}

type FileFault = Extract<NormalizedFileTransferFailure, { kind: 'fault' }>

interface SettledFileFailure {
  readonly failure: NormalizedFileTransferFailure
  readonly settlement: FileTransferSettlement
  readonly capacityBlocked?: OutputCapacityBlockedError
}

export async function settleReopenedFile(
  options: FileSettlementOptions, transaction: OutputFileTransaction,
  error: RevisionReopenedDuringRecoveryError, initialDurable: ByteRangeSet, started: number, retried: boolean,
): Promise<SettledFileFailure | {
  readonly settlement: FileTransferSettlement
  readonly retry: FileRevisionReadmission
}> {
  const { previous, current, comparison } = error
  const readmit = options.output.readmitFile?.bind(options.output)
  const decision = readmissionDecision(options.signal, comparison, retried, readmit !== undefined)
  const failureOptions = {
    signal: options.signal,
    ...(options.incidentScope === undefined ? {} : { incidentScope: options.incidentScope }),
  }
  const failure = classifyReopenedFileFailure(error, decision, failureOptions)

  let paused: VerifiedDurableRanges
  try {
    // Verify the retained cut while it still describes the active revision.
    // Recording a source failure can withdraw these ranges from reuse.
    paused = await pauseOutputFile(options, transaction, error)
    if (failure?.kind === 'fault') await persistSourceFailure(options, transaction, failure.fault)
  } catch (reason) {
    observeRevisionReadmission(options, previous, current, comparison, 'settlement-failed', 0n, 0n, started)
    return unconfirmedPause(options, reason)
  }

  const settlement = pausedFileSettlement(paused)
  const finalDecision = options.signal.aborted ? 'canceled' : decision
  observeRevisionReadmission(options, previous, current, comparison, finalDecision, settlement.retainedBytes, 0n, started)
  if (finalDecision === 'readmit' && readmit !== undefined) {
    return { settlement, retry: {
      previous, current, comparison, started, paused, initialDurable,
      beginFile: (request, signal) => readmit(request, paused, signal),
    } }
  }

  const terminalFailure = finalDecision === 'canceled'
    ? normalizeFileTransferFailure(options.signal.reason, failureOptions)
    : failure ?? normalizeFileTransferFailure(error, failureOptions)
  return {
    settlement,
    failure: terminalFailure.kind === 'fault' && !options.output.capabilities.fileFailureIsolation
      ? promoteFileFaultToOutputPause(options, terminalFailure) : terminalFailure,
  }
}

function readmissionDecision(
  signal: AbortSignal, comparison: RevisionReopenedDuringRecoveryError['comparison'], retried: boolean, supported: boolean,
): FileRevisionReadmissionObservation['decision'] {
  if (signal.aborted) return 'canceled'
  if (comparison.kind === 'observed-change') return 'observed-change'
  if (comparison.kind === 'unavailable') return 'evidence-unavailable'
  if (retried) return 'retry-limit'
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
      fileOutcomeEvidence: { kind: 'deferred-revision-readmission' },
    })
  }
  return normalizeFileTransferFailure(error, options)
}

export async function settleTransferError(
  options: FileSettlementOptions,
  transaction: OutputFileTransaction | undefined,
  error: unknown,
): Promise<SettledFileFailure> {
  if (error instanceof OutputCapacityBlockedError && !options.signal.aborted) {
    // Release the write lease before waiting so admitted files retain capacity
    // to drain the payload buffers.
    try {
      const settlement = transaction === undefined ? UNCONFIRMED_FILE_SETTLEMENT : pausedFileSettlement(
        await withOutputSettlementTimeout('checkpoint capacity-blocked file',
          options.outputSettlementTimeoutMilliseconds, () => transaction.pause(error)),
      )
      return { capacityBlocked: error, settlement,
        failure: normalizeFileTransferFailure(error, { stage: 'output_write' }) }
    } catch (settlementFailure) {
      return { failure: normalizeFileTransferFailure(settlementFailure, { stage: 'settlement' }),
        settlement: UNCONFIRMED_FILE_SETTLEMENT }
    }
  }

  const failure = normalizeFileTransferFailure(error, {
    signal: options.signal,
    ...(options.incidentScope === undefined ? {} : { incidentScope: options.incidentScope }),
  })
  if (transaction === undefined) return { failure, settlement: UNCONFIRMED_FILE_SETTLEMENT }
  if (failure.kind === 'canceled') return pauseCanceledFileTransfer(options, transaction, failure)
  try {
    await persistSourceFailure(options, transaction, failure.fault)
  } catch (error) {
    return {
      failure: normalizeFileTransferFailure(error, { stage: 'settlement' }),
      settlement: UNCONFIRMED_FILE_SETTLEMENT,
    }
  }
  return {
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
    return { failure, settlement: pausedFileSettlement(durable) }
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

function pausedFileSettlement(durable: VerifiedDurableRanges): Extract<FileTransferSettlement, { kind: 'paused' }> {
  return Object.freeze({ kind: 'paused', retainedBytes: rangeBytes(durable.asRangeSet()) })
}

function unconfirmedPause(options: FileSettlementOptions, reason: unknown): SettledFileFailure {
  return {
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

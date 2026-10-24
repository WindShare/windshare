import {
  faultFailureFact,
  unclassifiedFailureFact,
  type FailureStage,
} from '../../diagnostics/incident'
import {
  CheckpointFaultCode,
  FaultScope,
  OutputFaultCode,
  SourceFaultCode,
  checkpointFault,
  outputFault,
  sourceFault,
  type Fault,
} from '../../transfer/failure/fault'
import type { TransferFileOutcomeEvidence } from '../../transfer/failure/file-outcome'
import {
  declareOutputFailure,
  type OutputFailure,
} from '../../transfer/failure/output'

export function outputInitializationFailure(error: Error): OutputFailure {
  return outputExceptionFailure(error, 'output_initialization', 'retryable')
}

export function outputCapacityStateFailure(error: DOMException): OutputFailure {
  return outputExceptionFailure(error, 'output_reservation', 'resumable_receive')
}

function outputExceptionFailure(
  error: Error | DOMException,
  stage: FailureStage,
  recoveryDisposition: 'retryable' | 'resumable_receive',
): OutputFailure {
  return declareOutputFailure(Object.freeze({
    fault: outputFault(FaultScope.OutputPause, OutputFaultCode.StateIO),
    fact: unclassifiedFailureFact({ stage, recoveryDisposition, error }),
    materializationFailureReason: 'output-write-failed',
  }))
}

export function checkpointLineageFailure(
  decision: 'revision-conflict' | 'ownership-conflict' | 'invalid',
): OutputFailure {
  const codes = {
    'revision-conflict': CheckpointFaultCode.UnsafeInstall,
    'ownership-conflict': CheckpointFaultCode.OwnershipMismatch,
    invalid: CheckpointFaultCode.CorruptRecord,
  } as const
  return persistentFileFailure(
    checkpointFault(FaultScope.FileLocal, codes[decision]),
    { kind: 'checkpoint-decision', decision },
  )
}

export function destinationCollisionFailure(): OutputFailure {
  return persistentFileFailure(
    outputFault(FaultScope.FileLocal, OutputFaultCode.NamespaceUnsafe),
    { kind: 'occupied-unbound-destination' },
  )
}

export function persistedSourceRevisionFailure(): OutputFailure {
  return persistentFileFailure(
    sourceFault(FaultScope.FileLocal, SourceFaultCode.RevisionChanged),
    { kind: 'authenticated-source-drift' },
  )
}

function persistentFileFailure(fault: Fault, evidence: TransferFileOutcomeEvidence): OutputFailure {
  return declareOutputFailure(Object.freeze({
    fault,
    fact: faultFailureFact({ fault, stage: 'output_write', recoveryDisposition: 'none' }),
    // File-operation callers supply the specific materialization stage. Preserve
    // the existing default for unwrapped failures without inventing a write.
    materializationFailureReason: 'content-read-failed',
  }), evidence)
}

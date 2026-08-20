import {
  nativeOutputFailureFact,
  type FailureFactRef,
  type FailureFactRelation,
  type FailureFactSink,
  type NativeFailureClass,
  type RecoveryDisposition,
} from '../../diagnostics/incident'
import {
  CheckpointFaultCode,
  OutputFaultCode,
  type CheckpointFaultCode as CheckpointFaultCodeValue,
  type OutputFaultCode as OutputFaultCodeValue,
} from '../../transfer/fault'
import { FileCheckpointError } from '../persistence/checkpoint-lifecycle'
import {
  PersistentOutputError,
  TargetOwnershipUnknownError,
} from '../persistent-tree/errors'
import type { LocalOutputFailureAttemptSource } from './local-output-failure'

export const OUTPUT_FAILURE_STAGES = Object.freeze([
  'output_reservation',
  'output_write',
  'output_commit',
  'checkpoint',
  'settlement',
  'publication',
  'continuation',
  'reopen',
  'cleanup',
] as const)

export type OutputFailureStage = (typeof OUTPUT_FAILURE_STAGES)[number]

export type OutputFailureCode<Stage extends OutputFailureStage> =
  Stage extends 'checkpoint' ? CheckpointFaultCodeValue : OutputFaultCodeValue

export interface OutputFailureObservation<Stage extends OutputFailureStage> {
  readonly nativeClass: NativeFailureClass
  readonly recoveryDisposition?: RecoveryDisposition
  readonly code?: OutputFailureCode<Stage>
}

export interface OutputFailureSink<Stage extends OutputFailureStage> {
  readonly stage: Stage
  record(observation: OutputFailureObservation<Stage>): FailureFactRef | undefined
}

export interface OutputFailureSinks {
  /** Correlation-only authority; native output code never reads or mutates it. */
  readonly attempt?: LocalOutputFailureAttemptSource
  readonly outputReservation?: OutputFailureSink<'output_reservation'>
  readonly outputWrite?: OutputFailureSink<'output_write'>
  readonly outputCommit?: OutputFailureSink<'output_commit'>
  readonly checkpoint?: OutputFailureSink<'checkpoint'>
  readonly settlement?: OutputFailureSink<'settlement'>
  readonly publication?: OutputFailureSink<'publication'>
  readonly continuation?: OutputFailureSink<'continuation'>
  readonly reopen?: OutputFailureSink<'reopen'>
  readonly cleanup?: OutputFailureSink<'cleanup'>
}

export function createOutputFailureSink<Stage extends OutputFailureStage>(input: {
  readonly facts: FailureFactSink
  readonly stage: Stage
  readonly relation: FailureFactRelation
  readonly recoveryDisposition: RecoveryDisposition
  readonly code?: OutputFailureCode<Stage>
}): OutputFailureSink<Stage> {
  return Object.freeze({
    stage: input.stage,
    record: (observation: OutputFailureObservation<Stage>): FailureFactRef | undefined => {
      try {
        return input.facts.record(nativeOutputFailureFact({
          stage: input.stage,
          recoveryDisposition:
            observation.recoveryDisposition ?? input.recoveryDisposition,
          nativeClass: observation.nativeClass,
          ...((observation.code ?? input.code) === undefined
            ? {}
            : { code: (observation.code ?? input.code) as OutputFailureCode<Stage> }),
        }), input.relation)
      } catch {
        // A rejected diagnostic fact cannot change the native operation result.
        return undefined
      }
    },
  })
}

export function recordOutputException<Stage extends OutputFailureStage>(
  sink: OutputFailureSink<Stage> | undefined,
  error: unknown,
  input: Readonly<{
    recoveryDisposition?: RecoveryDisposition
    code?: OutputFailureCode<Stage>
  }> = {},
): FailureFactRef | undefined {
  if (sink === undefined) return undefined
  const classifiedCode = input.code ?? outputFailureCode(error, sink.stage)
  try {
    return sink.record({
      nativeClass: nativeFailureClass(error),
      ...(input.recoveryDisposition === undefined
        ? {}
        : { recoveryDisposition: input.recoveryDisposition }),
      ...(classifiedCode === undefined ? {} : { code: classifiedCode }),
    })
  } catch {
    // Injected sinks are observational and cannot replace the native output result.
    return undefined
  }
}

export function nativeFailureClass(error: unknown): NativeFailureClass {
  if (!(typeof DOMException === 'function' && error instanceof DOMException)) {
    return 'unknown'
  }
  switch (error.name) {
    case 'AbortError': return 'abort'
    case 'TimeoutError': return 'timeout'
    case 'NotAllowedError': return 'not_allowed'
    case 'NotFoundError': return 'not_found'
    case 'InvalidStateError': return 'invalid_state'
    case 'QuotaExceededError': return 'quota_exceeded'
    case 'DataError': return 'data'
    case 'SecurityError': return 'security'
    case 'NotSupportedError': return 'not_supported'
    case 'NoModificationAllowedError': return 'no_modification_allowed'
    default: return 'unknown'
  }
}

export function outputFailureCode<Stage extends OutputFailureStage>(
  error: unknown,
  stage: Stage,
): OutputFailureCode<Stage> | undefined {
  if (stage === 'checkpoint') {
    return checkpointFailureCode(error) as OutputFailureCode<Stage> | undefined
  }
  return nativeOutputCode(error) as OutputFailureCode<Stage> | undefined
}

function checkpointFailureCode(error: unknown): CheckpointFaultCodeValue | undefined {
  if (error instanceof TargetOwnershipUnknownError) {
    return CheckpointFaultCode.OwnershipMismatch
  }
  if (!(error instanceof FileCheckpointError)) return undefined
  switch (error.code) {
    case 'checksum':
    case 'non-canonical':
    case 'binding':
    case 'invalid':
      return CheckpointFaultCode.CorruptRecord
    case 'ownership':
      return CheckpointFaultCode.OwnershipMismatch
    case 'generation':
    case 'crash-boundary':
      return CheckpointFaultCode.UnsafeInstall
    case 'recovery':
      return CheckpointFaultCode.StateIO
  }
}

function nativeOutputCode(error: unknown): OutputFaultCodeValue | undefined {
  if (error instanceof TargetOwnershipUnknownError) return OutputFaultCode.Ownership
  if (!(error instanceof PersistentOutputError)) return undefined
  switch (error.kind) {
    case 'authorization': return OutputFaultCode.Ownership
    case 'collision': return OutputFaultCode.NamespaceUnsafe
    case 'incomplete-file': return OutputFaultCode.Contract
    case 'output-state': return OutputFaultCode.StateIO
    case 'source-revision-changed': return OutputFaultCode.StateIO
    case 'target-ownership-unknown': return OutputFaultCode.Ownership
  }
}

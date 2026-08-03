import {
  NETWORK_MATRIX_ORCHESTRATION_FAILURE_CODES,
  NETWORK_MATRIX_SAMPLE_FAILURE_CODES,
  type NetworkMatrixExecutionMode,
} from '../vocabulary.ts'
import type {
  LoadedNetworkMatrixRegistry,
  NetworkMatrixIdentity,
  NetworkMatrixProfileReference,
} from '../manifest.ts'
import type { NetworkTopologyProfile } from '../profile.ts'
import type {
  NetworkOrchestrationFailure,
  NetworkRunResult,
  NetworkSampleFailure,
  NetworkSampleResult,
} from '../result.ts'
import type { NetworkMatrixSampleObservation } from '../run-collector.ts'
import type {
  NetworkMatrixAuthorityResolver,
  NetworkMatrixExecutionAuthority,
  PreparedNetworkMatrixAuthority,
} from '../runtime-authority.ts'
import { requireEnum } from '../contract-support.ts'
import type {
  NetworkMatrixDeadlineScheduler,
  NetworkMatrixOwnedOperation,
  NetworkMatrixOwnershipRegistrar,
  NetworkMatrixOwnershipRegistration,
} from '../owned-operation.ts'
import type { NetworkRuntimeAttestation } from '../attestation.ts'
import type {
  NetworkMatrixTraceChannel,
  NetworkMatrixTraceEvent,
} from '../trace/index.ts'

export const NETWORK_MATRIX_AUTHORITY_PREPARATION_DEADLINE_MS = 30_000 as const
export const NETWORK_MATRIX_SAMPLE_EXECUTION_DEADLINE_MS = 180_000 as const
export const NETWORK_MATRIX_AUTHORITY_CLOSE_DEADLINE_MS = 15_000 as const
export const NETWORK_MATRIX_MAXIMUM_TRACE_EVENTS = 512 as const
export const NETWORK_MATRIX_MAXIMUM_TRACE_BYTES = 4_194_304 as const

export type OrchestrationFailureCode = (typeof NETWORK_MATRIX_ORCHESTRATION_FAILURE_CODES)[number]
export type RunnerCleanupOutcome = 'not-required' | 'completed' | 'failed'

export interface NetworkMatrixRunnerDeadlines {
  readonly authorityPreparationMs: number
  readonly sampleExecutionMs: number
  readonly authorityCloseMs: number
}

export const NETWORK_MATRIX_RUNNER_DEADLINES: NetworkMatrixRunnerDeadlines = Object.freeze({
  authorityPreparationMs: NETWORK_MATRIX_AUTHORITY_PREPARATION_DEADLINE_MS,
  sampleExecutionMs: NETWORK_MATRIX_SAMPLE_EXECUTION_DEADLINE_MS,
  authorityCloseMs: NETWORK_MATRIX_AUTHORITY_CLOSE_DEADLINE_MS,
})

export interface NetworkMatrixSampleExecutionContext {
  readonly runId: string
  readonly manifestSha256: string
  readonly identity: NetworkMatrixIdentity
  readonly profile: NetworkTopologyProfile
  readonly authority: NetworkMatrixExecutionAuthority
  readonly operationId: string
}

export interface NetworkMatrixSampleExecution {
  readonly processInstanceId: string
  readonly observation: NetworkMatrixSampleObservation
}

export interface NetworkMatrixSampleExecutor {
  execute(
    context: NetworkMatrixSampleExecutionContext,
  ): NetworkMatrixOwnedOperation<NetworkMatrixSampleExecution>
}

/** Consumer-side boundary keeps authority cleanup testable when evidence storage fails. */
export interface NetworkMatrixRunCollectorPort {
  recordAttestation(attestation: NetworkRuntimeAttestation): NetworkRuntimeAttestation
  recordSample(
    identity: NetworkMatrixIdentity,
    processInstanceId: string | null,
    observation: NetworkMatrixSampleObservation,
  ): NetworkSampleResult
  finalize(orchestrationFailure: NetworkOrchestrationFailure | null): NetworkRunResult
}

export type NetworkMatrixRunTrace = NetworkMatrixTraceEvent

export interface NetworkMatrixRunExecution {
  readonly result: Promise<NetworkRunResult>
  readonly traces: NetworkMatrixTraceChannel
}

export interface NetworkMatrixRunnerOptions {
  readonly registry: LoadedNetworkMatrixRegistry
  readonly runId: string
  readonly executionMode: NetworkMatrixExecutionMode
  readonly authorities: NetworkMatrixAuthorityResolver
  readonly samples: NetworkMatrixSampleExecutor
  readonly collector?: NetworkMatrixRunCollectorPort
  readonly deadlines?: NetworkMatrixRunnerDeadlines
  readonly deadlineScheduler?: NetworkMatrixDeadlineScheduler
  readonly ownershipRegistrar?: NetworkMatrixOwnershipRegistrar
}

export class NetworkMatrixSampleExecutionError extends Error {
  readonly failureCode: NetworkSampleFailure['failureCode']

  constructor(failureCode: NetworkSampleFailure['failureCode'], message: string) {
    super(message)
    this.name = 'NetworkMatrixSampleExecutionError'
    this.failureCode = requireEnum(
      failureCode,
      NETWORK_MATRIX_SAMPLE_FAILURE_CODES,
      'network matrix sample execution failure code',
    )
  }
}

export class NetworkMatrixOrchestrationError extends Error {
  readonly failureCode: OrchestrationFailureCode

  constructor(failureCode: OrchestrationFailureCode, message: string) {
    super(message)
    this.name = 'NetworkMatrixOrchestrationError'
    this.failureCode = requireEnum(
      failureCode,
      NETWORK_MATRIX_ORCHESTRATION_FAILURE_CODES,
      'network matrix orchestration failure code',
    )
  }
}

export interface PreparedProfile {
  readonly reference: NetworkMatrixProfileReference
  readonly profile: NetworkTopologyProfile
  readonly authority: PreparedNetworkMatrixAuthority
  readonly ownership?: NetworkMatrixOwnershipRegistration
  closed: boolean
}

export interface RunnerContext {
  readonly options: NetworkMatrixRunnerOptions
  readonly runId: string
  readonly appendTrace: (trace: NetworkMatrixRunTrace) => void
  readonly deadlines: NetworkMatrixRunnerDeadlines
  readonly scheduler: NetworkMatrixDeadlineScheduler
  readonly collector: NetworkMatrixRunCollectorPort
}

export interface RunnerState {
  readonly processInstances: Set<string>
  readonly terminalSampleTraceIds: Set<string>
  orchestrationFailure: OrchestrationFailureCode | null
}

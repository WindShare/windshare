import { isProxy } from 'node:util/types'

import type { NetworkMatrixProfileId } from '../vocabulary.ts'
import {
  networkMatrixIdentities,
  sha256,
  type NetworkMatrixIdentity,
} from '../manifest.ts'
import { networkMatrixSampleOperationId } from '../sample-authority.ts'
import {
  requireOperationId,
  requireRunId,
} from '../contract-support.ts'
import {
  NetworkMatrixDeadlineExceeded,
  NetworkMatrixOwnershipCleanupError,
} from '../owned-operation.ts'
import {
  networkMatrixTrace,
  type NetworkMatrixTraceIdentity,
  type NetworkMatrixTraceOutcome,
} from '../trace/index.ts'
import {
  NetworkMatrixOrchestrationError,
  NetworkMatrixSampleExecutionError,
  type NetworkMatrixRunnerOptions,
  type NetworkMatrixRunTrace,
} from './contract.ts'

const NETWORK_MATRIX_AUTHORITY_OPERATION_DOMAIN =
  'windshare.browser-network-matrix.authority-operation/v1' as const

export function expectedRunnerTraceIdentities(
  options: NetworkMatrixRunnerOptions,
  runId: string,
): readonly NetworkMatrixTraceIdentity[] {
  const profiles = options.registry.manifest.profiles
    .filter(({ executionMode }) => executionMode === options.executionMode)
    .map(({ profileId }) => profileTraceIdentity(runId, profileId))
  const samples = networkMatrixIdentities(options.registry.manifest, options.executionMode)
    .map((identity) => sampleTraceIdentity(networkMatrixSampleOperationId(runId, identity), runId, identity))
  return Object.freeze([
    runTraceIdentity(runId),
    ...profiles,
    ...samples,
  ])
}

/**
 * Authority ownership outlives individual calls, so its process identifier must
 * stay globally reconstructible while remaining bounded at the maximum run ID.
 */
export function authorityOperationId(
  phase: 'prepare' | 'live',
  runId: string,
  profileId: NetworkMatrixProfileId,
): string {
  const digest = sha256(`${JSON.stringify([
    NETWORK_MATRIX_AUTHORITY_OPERATION_DOMAIN,
    phase,
    requireRunId(runId, 'network matrix authority operation run ID'),
    profileId,
  ])}\n`)
  return requireOperationId(
    `authority-${phase}-${digest}`,
    'network matrix authority operation ID',
  )
}

export function runTraceIdentity(runId: string): NetworkMatrixTraceIdentity {
  return Object.freeze({
    component: 'browser-network-matrix-runner',
    scenario: 'network-matrix-run',
    operationId: runId,
    runId,
  })
}

export function profileTraceIdentity(
  runId: string,
  profileId: NetworkMatrixIdentity['profileId'],
): NetworkMatrixTraceIdentity {
  return Object.freeze({
    component: 'browser-network-matrix-runner',
    scenario: 'network-matrix-profile',
    operationId: `${runId}-${profileId}`,
    runId,
    profileId,
  })
}

export function sampleTraceIdentity(
  operationId: string,
  runId: string,
  identity: NetworkMatrixIdentity,
): NetworkMatrixTraceIdentity {
  return Object.freeze({
    component: 'browser-network-matrix-runner',
    scenario: 'network-matrix-sample',
    operationId,
    runId,
    profileId: identity.profileId,
    browser: identity.browser,
    sampleOrdinal: identity.sampleOrdinal,
  })
}

export function emitTrace(
  appendTrace: (trace: NetworkMatrixRunTrace) => void,
  identity: NetworkMatrixTraceIdentity,
  milestone: string,
  outcome: NetworkMatrixTraceOutcome,
  context?: Readonly<Record<string, unknown>>,
): void {
  appendTrace(networkMatrixTrace(identity, milestone, outcome, context))
}

type RunnerTraceFailureCode =
  | 'authority-close-failed'
  | 'authority-preparation-failed'
  | 'profile-execution-failed'
  | 'result-finalization-failed'
  | 'run-cleanup-failed'
  | 'run-terminal-failed'
  | 'sample-cleanup-failed'
  | 'sample-execution-failed'
  | 'sample-operation-failed'

/**
 * Dependency causes are deliberately opaque. A thrown value may own hostile
 * accessors or Proxy traps, while the phase-specific code remains a stable and
 * sufficient reconstruction key alongside the lifecycle milestone.
 */
export function opaqueFailureContext(
  failureCode: RunnerTraceFailureCode,
): Readonly<Record<string, unknown>> {
  return Object.freeze({ failureCode })
}

export function isOwnershipCleanupError(
  value: unknown,
): value is NetworkMatrixOwnershipCleanupError {
  return typeof value === 'object' &&
    value !== null &&
    !isProxy(value) &&
    value instanceof NetworkMatrixOwnershipCleanupError
}

export function isDeadlineExceeded(value: unknown): value is NetworkMatrixDeadlineExceeded {
  return typeof value === 'object' &&
    value !== null &&
    !isProxy(value) &&
    value instanceof NetworkMatrixDeadlineExceeded
}

export function isOrchestrationError(value: unknown): value is NetworkMatrixOrchestrationError {
  return typeof value === 'object' &&
    value !== null &&
    !isProxy(value) &&
    value instanceof NetworkMatrixOrchestrationError
}

export function isSampleExecutionError(value: unknown): value is NetworkMatrixSampleExecutionError {
  return typeof value === 'object' &&
    value !== null &&
    !isProxy(value) &&
    value instanceof NetworkMatrixSampleExecutionError
}

export function combinedFailure(first: unknown, second: unknown): unknown {
  return first === undefined
    ? second
    : new AggregateError([first, second], 'network matrix execution and cleanup both failed')
}

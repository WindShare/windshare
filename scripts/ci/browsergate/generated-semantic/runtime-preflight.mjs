import {
  GENERATED_SEMANTIC_ALLOWED_EXTERNAL_IMPORTS,
  GENERATED_SEMANTIC_DIGEST,
  GENERATED_SEMANTIC_EXPORTS,
} from './build/artifact-policy.mjs'
import { GENERATED_SEMANTIC_FILENAME } from './build/config.mjs'
import { generatedSemanticCauseMessage } from './build/failure.mjs'
import { parseGeneratedSemanticResultRecord } from './build/result-contract.mjs'
import { GENERATED_SEMANTIC_REQUIRED_TOOL_VERSIONS } from './build/tool-authorization.mjs'

export const GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_OPERATION_ID =
  'browser-runtime-generated-semantic-preflight'
export const GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_MODE = 'verify'

export class GeneratedSemanticRuntimePreflightError extends Error {
  constructor(code, message, { result = null, cause } = {}) {
    super(message, cause === undefined ? undefined : { cause })
    this.name = 'GeneratedSemanticRuntimePreflightError'
    this.code = code
    this.result = result
  }
}

export function validateGeneratedSemanticRuntimePreflight({
  execution,
  platform,
  expectedNodeVersion,
  expectedArtifact,
}) {
  const result = requireGeneratedSemanticRuntimeExecution({ execution, platform })
  validateGeneratedSemanticRuntimeEvidence({
    result,
    expectedNodeVersion,
    expectedArtifact,
  })
  return result
}

export function requireGeneratedSemanticRuntimeExecution({ execution, platform }) {
  let result
  try {
    result = parseGeneratedSemanticResultRecord(execution?.stdout)
  } catch (cause) {
    throw new GeneratedSemanticRuntimePreflightError(
      'result-record-invalid',
      'generated semantic verifier emitted an invalid result record',
      { cause },
    )
  }

  if (result.outcome === 'failed') {
    throw new GeneratedSemanticRuntimePreflightError(
      'reported-failure',
      'generated semantic verifier reported a typed failure',
      { result },
    )
  }
  if (execution.stderr !== '') {
    throw new GeneratedSemanticRuntimePreflightError(
      'stderr-not-empty',
      'generated semantic verifier emitted unexpected stderr',
      { result },
    )
  }
  requireSuccessfulOwnedExecution(execution, platform, result)
  if (result.mode !== GENERATED_SEMANTIC_RUNTIME_PREFLIGHT_MODE) {
    throw new GeneratedSemanticRuntimePreflightError(
      'mode-mismatch',
      'generated semantic verifier result mode differs from runtime preflight policy',
      { result },
    )
  }
  if (result.outcome !== 'current') {
    throw new GeneratedSemanticRuntimePreflightError(
      'outcome-mismatch',
      'generated semantic verifier did not report the committed artifact as current',
      { result },
    )
  }
  return result
}

export function validateGeneratedSemanticRuntimeEvidence({
  result,
  expectedNodeVersion,
  expectedArtifact,
}) {
  requireToolEvidence(result, expectedNodeVersion)
  requireArtifactEvidence(result, expectedArtifact)
  return result
}

export function generatedSemanticResultTraceContext(result) {
  const context = {}
  if (result?.tools !== null && result?.tools !== undefined) {
    context.nodeVersion = result.tools.node
    context.viteVersion = result.tools.vite
    context.rolldownVersion = result.tools.rolldown
  }
  if (result?.artifact !== null && result?.artifact !== undefined) {
    context.artifactByteLength = result.artifact.byteLength
    context.artifactSha256 = result.artifact.sha256
    context.semanticDigest = result.artifact.semanticDigest
  }
  if (Array.isArray(result?.failures) && result.failures.length > 0) {
    context.failures = result.failures
  }
  return Object.freeze(context)
}

export function generatedSemanticPreflightFailureContext(cause) {
  try {
    if (cause instanceof GeneratedSemanticRuntimePreflightError) {
      const result = cause.result
      return Object.freeze({
        failureCode: cause.code,
        failureMessage: generatedSemanticCauseMessage(
          cause,
          'generated semantic runtime preflight failed',
        ),
        ...(result?.mode === null || result?.mode === undefined
          ? {}
          : { reportedMode: result.mode }),
        ...(typeof result?.outcome === 'string' ? { reportedOutcome: result.outcome } : {}),
        ...generatedSemanticResultTraceContext(result),
      })
    }
  } catch {
    // Failure reporting itself must not be controllable by hostile causes.
  }
  return Object.freeze({
    failureCode: 'unexpected-preflight-failure',
    failureMessage: generatedSemanticCauseMessage(
      cause,
      'generated semantic runtime preflight failed unexpectedly',
    ),
  })
}

function requireSuccessfulOwnedExecution(execution, platform, result) {
  if (
    execution === null || typeof execution !== 'object' ||
    execution.launched !== true || execution.timedOut !== false ||
    execution.treeEmpty !== true || execution.processEvidence?.terminal !== 'exited' ||
    execution.processEvidence.exitCode !== 0
  ) {
    throw new GeneratedSemanticRuntimePreflightError(
      'process-evidence-rejected',
      'generated semantic verifier did not prove a successful empty process tree',
      { result },
    )
  }
  if (
    execution.inputEvidence === undefined || (
      execution.inputEvidence.outcome !== 'not-requested' ||
      execution.inputEvidence.failureCode !== '' ||
      execution.inputEvidence.failureMessage !== ''
    )
  ) rejectOwnershipEvidence(result, 'generated semantic verifier input evidence is not successful')
  if (
    execution.clientIoEvidence === undefined || (
      execution.clientIoEvidence.requestOutcome !== 'delivered' ||
      execution.clientIoEvidence.rawInputOutcome !== 'not-requested' ||
      !['not-requested', 'delivered'].includes(execution.clientIoEvidence.controlOutcome) ||
      execution.clientIoEvidence.outputOutcome !== 'delivered' ||
      execution.clientIoEvidence.failureCode !== '' ||
      execution.clientIoEvidence.failureMessage !== ''
    )
  ) rejectOwnershipEvidence(result, 'generated semantic verifier client I/O evidence is not successful')
  requireOwnershipEvidence(execution.ownershipEvidence, platform, result)
}

function requireOwnershipEvidence(ownership, platform, result) {
  if (platform === 'win32') {
    if (
      ownership?.supervisionOutcome === 'tree-empty' &&
      ownership.terminationReason === 'natural' &&
      ownership.activeProcessCount === 0 &&
      Number.isSafeInteger(ownership.root?.pid) && ownership.root.pid > 0 &&
      ownership.root.exitCode === 0 && ownership.spawnFailure === null
    ) return
  } else if (platform === 'linux') {
    if (
      Number.isSafeInteger(ownership?.ownerPid) && ownership.ownerPid > 0 &&
      Number.isSafeInteger(ownership.rootPid) && ownership.rootPid > 0 &&
      typeof ownership.rootStartTimeTicks === 'string' &&
      /^[1-9][0-9]*$/u.test(ownership.rootStartTimeTicks) &&
      ownership.controlOutcome === 'target-terminal' &&
      ownership.cleanupOutcome === 'completed' &&
      ownership.failureCode === '' && ownership.failureMessage === ''
    ) return
  }
  rejectOwnershipEvidence(
    result,
    'generated semantic verifier owner did not prove complete process-tree settlement',
  )
}

function rejectOwnershipEvidence(result, message) {
  throw new GeneratedSemanticRuntimePreflightError(
    'process-evidence-rejected',
    message,
    { result },
  )
}

function requireToolEvidence(result, expectedNodeVersion) {
  if (
    result.tools?.node !== expectedNodeVersion ||
    result.tools?.vite !== GENERATED_SEMANTIC_REQUIRED_TOOL_VERSIONS.vite ||
    result.tools?.rolldown !== GENERATED_SEMANTIC_REQUIRED_TOOL_VERSIONS.rolldown
  ) {
    throw new GeneratedSemanticRuntimePreflightError(
      'tool-evidence-mismatch',
      'generated semantic verifier tool evidence differs from repository authority',
      { result },
    )
  }
}

function requireArtifactEvidence(result, expectedArtifact) {
  const artifact = result.artifact
  if (
    artifact?.fileName !== GENERATED_SEMANTIC_FILENAME ||
    artifact?.byteLength !== expectedArtifact?.byteLength ||
    artifact?.sha256 !== expectedArtifact?.sha256 ||
    artifact?.semanticDigest !== GENERATED_SEMANTIC_DIGEST ||
    !equalStringArrays(artifact?.exports, GENERATED_SEMANTIC_EXPORTS) ||
    !approvedCanonicalStringSubset(
      artifact?.externalImports,
      GENERATED_SEMANTIC_ALLOWED_EXTERNAL_IMPORTS,
    )
  ) {
    throw new GeneratedSemanticRuntimePreflightError(
      'artifact-evidence-mismatch',
      'generated semantic verifier artifact evidence differs from committed authority',
      { result },
    )
  }
}

function equalStringArrays(left, right) {
  return Array.isArray(left) && left.length === right.length &&
    left.every((value, index) => value === right[index])
}

function approvedCanonicalStringSubset(observed, approved) {
  return Array.isArray(observed) && new Set(observed).size === observed.length &&
    observed.every((value) => approved.includes(value)) &&
    observed.every((value, index) => index === 0 || observed[index - 1] < value)
}

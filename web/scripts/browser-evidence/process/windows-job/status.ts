import type { RunnerProcessEvidence } from '../../execution-evidence.ts'
import {
  requireBoolean,
  requireEnum,
  requireExactKeys,
  requireLiteral,
  requireRecord,
  requireSafeInteger,
  requireString,
} from '../../contract/json.ts'
import { parseCanonicalJsonText } from '../../contract/strict-json.ts'
import {
  WINDOWS_JOB_MAXIMUM_OPERATION_BYTES,
  WINDOWS_JOB_NONCE_BYTES,
  WINDOWS_JOB_ROOT_KEYS,
  WINDOWS_JOB_SCHEMA_VERSION,
  WINDOWS_JOB_STATUS_KEYS,
  WINDOWS_JOB_SUPERVISION_OUTCOMES,
  WINDOWS_JOB_TERMINATION_REASONS,
  WINDOWS_UINT32_MAXIMUM,
  type WindowsJobStatus,
  type WindowsJobStatusRoot,
} from './contract.ts'

const WINDOWS_JOB_MAXIMUM_NONCE_TEXT_BYTES = WINDOWS_JOB_NONCE_BYTES * 2
const WINDOWS_JOB_MAXIMUM_SPAWN_FAILURE_BYTES = 512

export function parseWindowsJobAuthorityStatus(
  encoded: Uint8Array,
  expectedOperationId: string,
  expectedNonce: string,
  expectedInputRequested: boolean,
): WindowsJobStatus {
  let text: string
  try {
    text = new TextDecoder('utf-8', { fatal: true }).decode(encoded)
  } catch {
    throw new Error('Windows Job authority status is not valid UTF-8')
  }
  const parsed = parseCanonicalJsonText(text, 'Windows Job authority status')
  if (text !== JSON.stringify(parsed)) {
    throw new Error('Windows Job authority status is not exact canonical JSON')
  }
  const status = requireRecord(parsed, 'Windows Job authority status')
  requireExactKeys(status, WINDOWS_JOB_STATUS_KEYS, [], 'Windows Job authority status')
  requireKeyOrder(status, WINDOWS_JOB_STATUS_KEYS, 'Windows Job authority status')
  const operationId = requireString(
    status.operationId,
    'Windows Job status operation ID',
    WINDOWS_JOB_MAXIMUM_OPERATION_BYTES,
  )
  const nonce = requireString(
    status.nonce,
    'Windows Job status nonce',
    WINDOWS_JOB_MAXIMUM_NONCE_TEXT_BYTES,
  )
  if (operationId !== expectedOperationId || nonce !== expectedNonce) {
    throw new Error('Windows Job authority status identity does not match its private request')
  }
  const supervisionOutcome = requireEnum(
    status.supervisionOutcome,
    WINDOWS_JOB_SUPERVISION_OUTCOMES,
    'Windows Job supervision outcome',
  )
  const terminationReason = requireEnum(
    status.terminationReason,
    WINDOWS_JOB_TERMINATION_REASONS,
    'Windows Job termination reason',
  )
  const timedOut = requireBoolean(status.timedOut, 'Windows Job timed-out field')
  requireLiteral(status.activeProcessCount, 0, 'Windows Job active process count')
  const inputOutcome = requireEnum(
    status.inputOutcome,
    ['not-started', 'not-requested', 'delivered'] as const,
    'Windows Job input outcome',
  )
  const root = status.root === null ? null : parseStatusRoot(status.root)
  const spawnFailure = status.spawnFailure === null
    ? null
    : requireString(
      status.spawnFailure,
      'Windows Job spawn failure',
      WINDOWS_JOB_MAXIMUM_SPAWN_FAILURE_BYTES,
    )
  validateStatusCombination(
    supervisionOutcome,
    terminationReason,
    timedOut,
    inputOutcome,
    root,
    spawnFailure,
  )
  const expectedInputOutcome = expectedInputRequested ? 'delivered' : 'not-requested'
  if (root !== null && inputOutcome !== expectedInputOutcome) {
    throw new Error('Windows Job input outcome contradicts its private start request')
  }
  return Object.freeze({
    schemaVersion: requireLiteral(
      status.schemaVersion,
      WINDOWS_JOB_SCHEMA_VERSION,
      'Windows Job schema version',
    ),
    operationId,
    nonce,
    supervisionOutcome,
    terminationReason,
    timedOut,
    activeProcessCount: 0,
    inputOutcome,
    root,
    spawnFailure,
  })
}

export function statusProcessEvidence(status: WindowsJobStatus): RunnerProcessEvidence {
  if (status.root !== null) {
    return Object.freeze({ terminal: 'exited', exitCode: status.root.exitCode })
  }
  return Object.freeze({
    terminal: 'spawn-failed',
    errorCode: 'WINDOWS_TARGET_SPAWN_FAILED',
    errorMessage: status.spawnFailure ?? 'Windows target spawn failed',
  })
}

function parseStatusRoot(value: unknown): WindowsJobStatusRoot {
  const root = requireRecord(value, 'Windows Job root process')
  requireExactKeys(root, WINDOWS_JOB_ROOT_KEYS, [], 'Windows Job root process')
  requireKeyOrder(root, WINDOWS_JOB_ROOT_KEYS, 'Windows Job root process')
  return Object.freeze({
    pid: requireSafeInteger(root.pid, 1, WINDOWS_UINT32_MAXIMUM, 'Windows Job root PID'),
    exitCode: requireSafeInteger(
      root.exitCode,
      0,
      WINDOWS_UINT32_MAXIMUM,
      'Windows Job root exit code',
    ),
  })
}

function validateStatusCombination(
  outcome: WindowsJobStatus['supervisionOutcome'],
  reason: WindowsJobStatus['terminationReason'],
  timedOut: boolean,
  inputOutcome: WindowsJobStatus['inputOutcome'],
  root: WindowsJobStatusRoot | null,
  spawnFailure: string | null,
): void {
  if (timedOut !== (reason === 'deadline')) {
    throw new Error('Windows Job deadline reason and timed-out field disagree')
  }
  if (outcome === 'tree-empty') {
    if (
      root === null || spawnFailure !== null || inputOutcome === 'not-started' ||
      !['natural', 'deadline', 'parent-request'].includes(reason)
    ) {
      throw new Error('Windows Job tree-empty status has contradictory root evidence')
    }
    return
  }
  if (
    root !== null || spawnFailure === null || reason !== 'target-spawn-failed' ||
    inputOutcome !== 'not-started'
  ) {
    throw new Error('Windows Job spawn-failed status has contradictory root evidence')
  }
}

function requireKeyOrder(
  value: Readonly<Record<string, unknown>>,
  expected: readonly string[],
  label: string,
): void {
  if (Object.keys(value).some((key, index) => key !== expected[index])) {
    throw new Error(`${label} fields are not in canonical order`)
  }
}

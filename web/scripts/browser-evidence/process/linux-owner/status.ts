import type { RunnerProcessEvidence } from '../../execution-evidence.ts'
import {
  LINUX_PROCESS_OWNER_STATUS_SCHEMA_VERSION,
  type LinuxProcessInputEvidence,
  type LinuxProcessOwnerExecution,
  type LinuxProcessOwnershipEvidence,
} from './contract.ts'

const MAXIMUM_FAILURE_MESSAGE_BYTES = 512
const MAXIMUM_PORTABLE_TOKEN_BYTES = 256
const MAXIMUM_ROOT_START_TIME_BYTES = 32
const REQUIRED_QUIET_INVENTORY_COUNT = 2
const UINT64_MAXIMUM = 0xffff_ffff_ffff_ffffn

type ParsedLinuxProcessOwnerStatus = Omit<LinuxProcessOwnerExecution, 'clientIoEvidence'>

export function parseLinuxProcessOwnerStatus(
  encoded: string,
  operationId: string,
): ParsedLinuxProcessOwnerStatus {
  const record = parseCanonicalLinuxProcessOwnerStatus(encoded)
  exactKeys(record, [
    'schemaVersion', 'operationId', 'processEvidence', 'inputEvidence', 'timedOut', 'launched',
    'treeEmpty', 'ownershipEvidence',
  ], 'Linux process owner status')
  if (
    record.schemaVersion !== LINUX_PROCESS_OWNER_STATUS_SCHEMA_VERSION ||
    record.operationId !== operationId
  ) throw new Error('Linux process owner status differs from its request authority')
  const status = Object.freeze({
    processEvidence: parseProcessEvidence(record.processEvidence),
    inputEvidence: parseLinuxProcessInputEvidence(record.inputEvidence),
    timedOut: requireBoolean(record.timedOut, 'Linux timeout evidence'),
    launched: requireBoolean(record.launched, 'Linux launch evidence'),
    treeEmpty: requireBoolean(record.treeEmpty, 'Linux tree-empty evidence'),
    ownershipEvidence: parseLinuxProcessOwnershipEvidence(record.ownershipEvidence),
  })
  validateLinuxProcessOwnerStatus(status)
  return status
}

export function parseLinuxProcessOwnerStatusOutput(
  encodedOutput: string,
  operationId: string,
): ParsedLinuxProcessOwnerStatus {
  return parseLinuxProcessOwnerStatus(oneCanonicalStatusLine(encodedOutput), operationId)
}

function parseCanonicalLinuxProcessOwnerStatus(encoded: string): Record<string, unknown> {
  let value: unknown
  try {
    value = JSON.parse(encoded)
  } catch (cause) {
    throw new Error('Linux process owner status is invalid JSON', { cause })
  }
  if (JSON.stringify(value) !== encoded) {
    throw new Error('Linux process owner status is not canonical JSON')
  }
  return requireRecord(value, 'Linux process owner status')
}

function oneCanonicalStatusLine(value: string): string {
  if (!value.endsWith('\n')) throw new Error('Linux process owner status is not newline terminated')
  const lines = value.slice(0, -1).split(/\r?\n/u)
  const [line] = lines
  if (lines.length !== 1 || line === undefined || line === '') {
    throw new Error('Linux process owner status must contain exactly one record')
  }
  return line
}

function parseLinuxProcessInputEvidence(value: unknown): LinuxProcessInputEvidence {
  const input = requireRecord(value, 'Linux input evidence')
  exactKeys(input, ['outcome', 'failureCode', 'failureMessage'], 'Linux input evidence')
  const evidence = Object.freeze({
    outcome: requireEnum(input.outcome, [
      'not-started', 'not-requested', 'delivered', 'failed',
    ] as const, 'Linux input outcome'),
    failureCode: requireOptionalPortableToken(input.failureCode, 'Linux input failure code'),
    failureMessage: requireString(
      input.failureMessage,
      'Linux input failure message',
      MAXIMUM_FAILURE_MESSAGE_BYTES,
    ),
  })
  const completeFailure = evidence.failureCode !== '' && evidence.failureMessage !== ''
  const noFailure = evidence.failureCode === '' && evidence.failureMessage === ''
  if (evidence.outcome === 'failed' ? !completeFailure : !noFailure) {
    throw new Error('Linux input outcome contradicts its bounded failure evidence')
  }
  return evidence
}

function parseLinuxProcessOwnershipEvidence(value: unknown): LinuxProcessOwnershipEvidence {
  const ownership = requireRecord(value, 'Linux ownership evidence')
  exactKeys(ownership, [
    'ownerPid', 'rootPid', 'rootStartTimeTicks', 'inventoryScans',
    'maximumObservedDescendants', 'quietInventoryCount', 'controlOutcome', 'cleanupOutcome',
    'failureCode', 'failureMessage',
  ], 'Linux ownership evidence')
  return Object.freeze({
    ownerPid: requirePositiveInteger(ownership.ownerPid, 'Linux owner PID'),
    rootPid: ownership.rootPid === null
      ? null
      : requirePositiveInteger(ownership.rootPid, 'Linux root PID'),
    rootStartTimeTicks: requireString(
      ownership.rootStartTimeTicks,
      'Linux root starttime',
      MAXIMUM_ROOT_START_TIME_BYTES,
    ),
    inventoryScans: requireNonnegativeInteger(
      ownership.inventoryScans,
      'Linux inventory scan count',
    ),
    maximumObservedDescendants: requireNonnegativeInteger(
      ownership.maximumObservedDescendants,
      'Linux maximum descendant count',
    ),
    quietInventoryCount: requireNonnegativeInteger(
      ownership.quietInventoryCount,
      'Linux quiet inventory count',
    ),
    controlOutcome: requireEnum(ownership.controlOutcome, [
      'not-started', 'target-terminal', 'parent-request', 'parent-eof', 'control-failure',
      'control-closed', 'deadline', 'ownership-evidence-failure',
    ] as const, 'Linux control outcome'),
    cleanupOutcome: requireEnum(
      ownership.cleanupOutcome,
      ['completed', 'failed'] as const,
      'Linux cleanup outcome',
    ),
    failureCode: requireOptionalPortableToken(
      ownership.failureCode,
      'Linux ownership failure code',
    ),
    failureMessage: requireString(
      ownership.failureMessage,
      'Linux ownership failure message',
      MAXIMUM_FAILURE_MESSAGE_BYTES,
    ),
  })
}

function validateLinuxProcessOwnerStatus(status: ParsedLinuxProcessOwnerStatus): void {
  validateLinuxCleanupEvidence(status.treeEmpty, status.ownershipEvidence)
  if (status.launched) validateLaunchedLinuxProcess(status)
  else validateUnlaunchedLinuxProcess(status)
  if (
    status.treeEmpty &&
    status.ownershipEvidence.controlOutcome === 'ownership-evidence-failure'
  ) throw new Error('Linux owner claimed tree quiescence after losing ownership evidence')
  if (status.timedOut !== (status.ownershipEvidence.controlOutcome === 'deadline')) {
    throw new Error('Linux timeout evidence contradicts its control outcome')
  }
}

function validateLinuxCleanupEvidence(
  treeEmpty: boolean,
  ownership: LinuxProcessOwnershipEvidence,
): void {
  if (treeEmpty !== (ownership.cleanupOutcome === 'completed')) {
    throw new Error('Linux tree-empty evidence contradicts its cleanup outcome')
  }
  if (treeEmpty) {
    if (
      ownership.quietInventoryCount !== REQUIRED_QUIET_INVENTORY_COUNT ||
      ownership.inventoryScans < ownership.quietInventoryCount ||
      ownership.failureCode !== '' ||
      ownership.failureMessage !== ''
    ) throw new Error('Linux owner claimed tree quiescence without its exact quiet proof')
    return
  }
  if (ownership.quietInventoryCount !== 0 || ownership.cleanupOutcome !== 'failed') {
    throw new Error('failed Linux cleanup contains contradictory quiet evidence')
  }
  if (ownership.failureCode === '' || ownership.failureMessage === '') {
    throw new Error('failed Linux cleanup lacks bounded failure evidence')
  }
}

function validateLaunchedLinuxProcess(status: ParsedLinuxProcessOwnerStatus): void {
  if (
    status.ownershipEvidence.rootPid === null ||
    !canonicalUint64(status.ownershipEvidence.rootStartTimeTicks) ||
    status.processEvidence.terminal === 'spawn-failed'
  ) throw new Error('launched Linux target lacks its exact root identity or terminal')
  if (
    status.ownershipEvidence.controlOutcome === 'not-started' ||
    status.inputEvidence.outcome === 'not-started'
  ) throw new Error('launched Linux target claims a pre-launch control or input outcome')
}

function validateUnlaunchedLinuxProcess(status: ParsedLinuxProcessOwnerStatus): void {
  if (
    status.ownershipEvidence.rootPid !== null ||
    status.ownershipEvidence.rootStartTimeTicks !== '' ||
    status.processEvidence.terminal !== 'spawn-failed'
  ) throw new Error('unlaunched Linux target claims root identity or terminal execution')
  if (status.treeEmpty || status.ownershipEvidence.controlOutcome === 'target-terminal') {
    throw new Error('unlaunched Linux target claims terminal control or tree quiescence')
  }
}

function parseProcessEvidence(value: unknown): RunnerProcessEvidence {
  const record = requireRecord(value, 'Linux root process evidence')
  if (record.terminal === 'exited') {
    exactKeys(record, ['terminal', 'exitCode'], 'Linux exited process evidence')
    return Object.freeze({
      terminal: 'exited',
      exitCode: requireNonnegativeInteger(record.exitCode, 'Linux root exit code'),
    })
  }
  if (record.terminal === 'signaled') {
    exactKeys(record, ['terminal', 'signal'], 'Linux signaled process evidence')
    return Object.freeze({
      terminal: 'signaled',
      signal: requirePortableToken(record.signal, 'Linux root signal'),
    })
  }
  if (record.terminal === 'spawn-failed') {
    exactKeys(
      record,
      ['terminal', 'errorCode', 'errorMessage'],
      'Linux spawn-failed process evidence',
    )
    return Object.freeze({
      terminal: 'spawn-failed',
      errorCode: requirePortableToken(record.errorCode, 'Linux owner error code'),
      errorMessage: requireString(
        record.errorMessage,
        'Linux owner error message',
        MAXIMUM_FAILURE_MESSAGE_BYTES,
      ),
    })
  }
  throw new Error('Linux root process terminal is invalid')
}

export function requireRecord(value: unknown, label: string): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as Record<string, unknown>
}

function exactKeys(record: Record<string, unknown>, keys: readonly string[], label: string): void {
  const actual = Object.keys(record)
  if (actual.length !== keys.length || keys.some((key) => !Object.hasOwn(record, key))) {
    throw new Error(`${label} does not have exact keys`)
  }
}

function requireBoolean(value: unknown, label: string): boolean {
  if (typeof value !== 'boolean') throw new Error(`${label} must be boolean`)
  return value
}

function requirePositiveInteger(value: unknown, label: string): number {
  if (!Number.isSafeInteger(value) || (value as number) < 1) {
    throw new Error(`${label} must be a positive safe integer`)
  }
  return value as number
}

export function requireBoundedPositiveInteger(
  value: unknown,
  maximum: number,
  label: string,
): number {
  const result = requirePositiveInteger(value, label)
  if (result > maximum) throw new Error(`${label} exceeds its bounded maximum`)
  return result
}

function requireNonnegativeInteger(value: unknown, label: string): number {
  if (!Number.isSafeInteger(value) || (value as number) < 0) {
    throw new Error(`${label} must be a nonnegative safe integer`)
  }
  return value as number
}

function requireString(value: unknown, label: string, maximumBytes: number): string {
  if (
    typeof value !== 'string' || Buffer.byteLength(value, 'utf8') > maximumBytes ||
    value.includes('\0')
  ) throw new Error(`${label} is invalid text`)
  return value
}

export function requirePortableToken(value: unknown, label: string): string {
  const result = requireString(value, label, MAXIMUM_PORTABLE_TOKEN_BYTES)
  if (!/^[A-Za-z0-9._-]+$/u.test(result)) throw new Error(`${label} is not portable`)
  return result
}

function requireOptionalPortableToken(value: unknown, label: string): string {
  const result = requireString(value, label, MAXIMUM_PORTABLE_TOKEN_BYTES)
  if (result !== '' && !/^[A-Za-z0-9._-]+$/u.test(result)) {
    throw new Error(`${label} is not portable`)
  }
  return result
}

function canonicalUint64(value: string): boolean {
  if (!/^[1-9]\d*$/u.test(value)) return false
  try {
    const parsed = BigInt(value)
    return parsed <= UINT64_MAXIMUM && parsed.toString(10) === value
  } catch {
    return false
  }
}

function requireEnum<const T extends string>(
  value: unknown,
  values: readonly T[],
  label: string,
): T {
  if (!values.includes(value as T)) throw new Error(`${label} is invalid`)
  return value as T
}

export function requireOperationId(value: string): void {
  if (!/^[A-Za-z0-9._-]{1,256}$/u.test(value)) throw new Error('Linux operation ID is invalid')
}

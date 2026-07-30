import { lstat } from 'node:fs/promises'
import { win32 } from 'node:path'

import type { RunnerProcessEvidence } from '../../execution-evidence.ts'

export const WINDOWS_JOB_CONTROL_MAXIMUM_BYTES = 1 * 1024 * 1024
export const WINDOWS_JOB_STDIN_MAXIMUM_BYTES = 1 * 1024 * 1024
export const WINDOWS_JOB_STATUS_MAXIMUM_BYTES = 16_384 as const
export const WINDOWS_JOB_NONCE_BYTES = 32 as const
export const WINDOWS_JOB_WATCHDOG_SLACK_MS = 5_000 as const
export const WINDOWS_JOB_POST_KILL_LEASE_MS = 5_000 as const
export const WINDOWS_JOB_MAXIMUM_DEADLINE_MS = 86_400_000 as const
export const WINDOWS_JOB_MAXIMUM_TERMINATION_GRACE_MS = 60_000 as const
export const WINDOWS_JOB_MAXIMUM_OPERATION_BYTES = 256 as const

export const WINDOWS_JOB_SCHEMA_VERSION = 2 as const
export const WINDOWS_JOB_STATUS_KEYS = Object.freeze([
  'schemaVersion',
  'operationId',
  'nonce',
  'supervisionOutcome',
  'terminationReason',
  'timedOut',
  'activeProcessCount',
  'inputOutcome',
  'root',
  'spawnFailure',
])
export const WINDOWS_JOB_ROOT_KEYS = Object.freeze(['pid', 'exitCode'])
export const WINDOWS_JOB_SUPERVISION_OUTCOMES = Object.freeze([
  'tree-empty',
  'spawn-failed',
] as const)
export const WINDOWS_JOB_TERMINATION_REASONS = Object.freeze([
  'natural',
  'target-spawn-failed',
  'deadline',
  'parent-request',
] as const)
export const WINDOWS_UINT32_MAXIMUM = 0xffff_ffff

export interface WindowsJobCommand {
  readonly executable: string
  readonly executableSha256?: string
  readonly arguments: readonly string[]
  readonly cwd?: string
  readonly environment?: Readonly<Record<string, string>>
  readonly stdin?: Uint8Array
  readonly stdinAuthority?: WindowsJobStdinAuthority
}

export interface WindowsJobStdinAuthority {
  readonly channelId: string
  readonly runId: string
  readonly profileId: string
  readonly attemptId: string
}

export interface WindowsJobExecutionOptions {
  readonly helperPath: string
  readonly operationId: string
  readonly command: WindowsJobCommand
  readonly inheritedEnvironment: NodeJS.ProcessEnv
  readonly injectedEnvironment: Readonly<Record<string, string>>
  readonly deadlineMs: number
  readonly terminationGraceMs: number
  readonly terminationSignal?: AbortSignal
  readonly stdout: (chunk: Uint8Array) => void
  readonly stderr: (chunk: Uint8Array) => void
}

type WindowsJobOptionalClientIoOutcome = 'not-requested' | 'delivered' | 'failed'

export interface WindowsJobExecution {
  readonly processEvidence: RunnerProcessEvidence
  readonly timedOut: boolean
  readonly launched: boolean
  readonly treeEmpty: boolean
  readonly inputEvidence: {
    readonly outcome: 'not-started' | 'not-requested' | 'delivered' | 'failed'
    readonly failureCode: string
    readonly failureMessage: string
  }
  readonly clientIoEvidence: {
    readonly requestOutcome: 'delivered' | 'failed'
    readonly rawInputOutcome: WindowsJobOptionalClientIoOutcome
    readonly controlOutcome: WindowsJobOptionalClientIoOutcome
    readonly outputOutcome: 'delivered' | 'failed'
    readonly failureCode: string
    readonly failureMessage: string
  }
  readonly ownershipEvidence: {
    readonly supervisionOutcome: WindowsJobStatus['supervisionOutcome']
    readonly terminationReason: WindowsJobStatus['terminationReason']
    readonly activeProcessCount: 0
    readonly root: WindowsJobStatusRoot | null
    readonly spawnFailure: string | null
  }
}

export interface WindowsJobEnvironmentEntry {
  readonly name: string
  readonly value: string
}

export interface WindowsJobStatusRoot {
  readonly pid: number
  readonly exitCode: number
}

export interface WindowsJobStatus {
  readonly schemaVersion: typeof WINDOWS_JOB_SCHEMA_VERSION
  readonly operationId: string
  readonly nonce: string
  readonly supervisionOutcome: typeof WINDOWS_JOB_SUPERVISION_OUTCOMES[number]
  readonly terminationReason: typeof WINDOWS_JOB_TERMINATION_REASONS[number]
  readonly timedOut: boolean
  readonly activeProcessCount: 0
  readonly inputOutcome: 'not-started' | 'not-requested' | 'delivered'
  readonly root: WindowsJobStatusRoot | null
  readonly spawnFailure: string | null
}

export interface PreparedWindowsJobInput {
  readonly bytes: Buffer
  readonly metadata: ReturnType<typeof canonicalWindowsJobStdinMetadata> | null
}

interface WindowsJobRawInputPipe {
  once(event: 'error', listener: (cause: unknown) => void): unknown
  once(event: 'close', listener: () => void): unknown
  end(bytes: Uint8Array, callback: () => void): unknown
}

export async function preflightWindowsJobExecution(
  options: WindowsJobExecutionOptions,
): Promise<void> {
  requireWireText(
    options.operationId,
    WINDOWS_JOB_MAXIMUM_OPERATION_BYTES,
    'Windows Job operation ID',
  )
  requireCanonicalWindowsPath(options.helperPath, 'Windows Job helper')
  requireCanonicalWindowsPath(options.command.executable, 'Windows Job target executable')
  if (options.command.cwd !== undefined) {
    requireCanonicalWindowsPath(options.command.cwd, 'Windows Job target working directory')
  }
  requireBoundedPositiveInteger(
    options.deadlineMs,
    WINDOWS_JOB_MAXIMUM_DEADLINE_MS,
    'Windows Job deadline',
  )
  requireBoundedPositiveInteger(
    options.terminationGraceMs,
    WINDOWS_JOB_MAXIMUM_TERMINATION_GRACE_MS,
    'Windows Job termination grace',
  )
  await requireRegularHelper(options.helperPath)
}

export function canonicalWindowsJobStdinMetadata(
  byteLength: number,
  authority: WindowsJobStdinAuthority,
): Readonly<{
  kind: 'anonymous-pipe'
  descriptor: 0
  byteLength: number
  channelId: string
  runId: string
  profileId: string
  attemptId: string
}> {
  requireBoundedPositiveInteger(
    byteLength,
    WINDOWS_JOB_STDIN_MAXIMUM_BYTES,
    'Windows Job target stdin byte length',
  )
  return Object.freeze({
    kind: 'anonymous-pipe' as const,
    descriptor: 0 as const,
    byteLength,
    channelId: requirePortableScope(authority.channelId, 'Windows Job stdin channel ID'),
    runId: requirePortableScope(authority.runId, 'Windows Job stdin run ID'),
    profileId: requirePortableScope(authority.profileId, 'Windows Job stdin profile ID'),
    attemptId: requirePortableScope(authority.attemptId, 'Windows Job stdin attempt ID'),
  })
}

export function prepareWindowsJobRawInput(command: WindowsJobCommand): PreparedWindowsJobInput {
  let bytes = Buffer.alloc(0)
  try {
    if ((command.stdin === undefined) !== (command.stdinAuthority === undefined)) {
      throw new Error('Windows Job stdin bytes and nonsecret authority must appear together')
    }
    if (command.stdin === undefined || command.stdinAuthority === undefined) {
      return Object.freeze({ bytes, metadata: null })
    }
    const metadata = canonicalWindowsJobStdinMetadata(
      command.stdin.byteLength,
      command.stdinAuthority,
    )
    bytes = Buffer.from(command.stdin)
    return Object.freeze({ bytes, metadata })
  } catch (cause) {
    bytes.fill(0)
    throw cause
  } finally {
    command.stdin?.fill(0)
  }
}

export function deliverAndEraseWindowsJobRawInput(
  pipe: WindowsJobRawInputPipe,
  bytes: Buffer,
): Promise<void> {
  return new Promise((resolveDelivery, rejectDelivery) => {
    let settled = false
    const settle = (failure?: unknown): void => {
      if (settled) return
      settled = true
      bytes.fill(0)
      if (failure === undefined) resolveDelivery()
      else rejectDelivery(failure)
    }
    try {
      pipe.once('error', settle)
      pipe.once('close', () => settle(new Error('Windows Job raw stdin closed before delivery')))
      pipe.end(bytes, () => settle())
    } catch (cause) {
      settle(cause)
    }
  })
}

export function canonicalWindowsJobEnvironment(
  inherited: NodeJS.ProcessEnv,
  command: Readonly<Record<string, string>> | undefined,
  injected: Readonly<Record<string, string>>,
): readonly WindowsJobEnvironmentEntry[] {
  const values = new Map<string, WindowsJobEnvironmentEntry>()
  for (const layer of [inherited, command ?? {}, injected]) {
    for (const [name, value] of Object.entries(layer)) {
      if (value === undefined) continue
      validateEnvironmentEntry(name, value)
      values.set(windowsEnvironmentIdentityFold(name), Object.freeze({ name, value }))
    }
  }
  return Object.freeze([...values.values()].sort((left, right) => {
    const folded = compareUtf8Ordinal(
      windowsEnvironmentSortFold(left.name),
      windowsEnvironmentSortFold(right.name),
    )
    return folded === 0 ? compareUtf8Ordinal(left.name, right.name) : folded
  }))
}

export function windowsJobSupervisorEnvironment(): Readonly<Record<string, string>> {
  // The native helper is addressed by an absolute path and receives every
  // capability through its control frame. An empty control-plane environment
  // prevents ambient host state from becoming a second process authority.
  return Object.freeze({})
}

export function requireExecutableSha256(value: string): string {
  if (!/^[0-9a-f]{64}$/u.test(value)) {
    throw new Error('Windows Job target executable SHA-256 must be lowercase 64-hex')
  }
  return value
}

export function encodeWindowsJobControlFrame(value: unknown): Uint8Array {
  const body = Buffer.from(JSON.stringify(value), 'utf8')
  if (body.byteLength > WINDOWS_JOB_CONTROL_MAXIMUM_BYTES) {
    throw new Error('Windows Job start request exceeds its control-frame limit')
  }
  const header = Buffer.allocUnsafe(4)
  header.writeUInt32BE(body.byteLength)
  return Buffer.concat([header, body])
}

export function requireBoundedPositiveInteger(
  value: number,
  maximum: number,
  label: string,
): void {
  if (!Number.isSafeInteger(value) || value < 1 || value > maximum) {
    throw new Error(`${label} must be an integer in [1, ${maximum}]`)
  }
}

export function errorMessage(cause: unknown): string {
  return cause instanceof Error ? cause.message : String(cause)
}

function validateEnvironmentEntry(name: string, value: string): void {
  if (
    name.length === 0 || name.includes('=') || name.includes('\0') ||
    !isWellFormedUnicode(name)
  ) throw new Error('Windows Job target environment contains an invalid name')
  if (value.includes('\0') || !isWellFormedUnicode(value)) {
    throw new Error(`Windows Job target environment ${name} contains an invalid value`)
  }
}

function isWellFormedUnicode(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    const unit = value.charCodeAt(index)
    if (unit >= 0xd800 && unit <= 0xdbff) {
      const next = value.charCodeAt(index + 1)
      if (next < 0xdc00 || next > 0xdfff) return false
      index += 1
    } else if (unit >= 0xdc00 && unit <= 0xdfff) {
      return false
    }
  }
  return !value.includes('\ufffd')
}

function windowsEnvironmentIdentityFold(name: string): string {
  return name.toUpperCase()
}

function windowsEnvironmentSortFold(name: string): string {
  return name.replace(/[A-Z]/gu, (character) => character.toLowerCase())
}

function compareUtf8Ordinal(left: string, right: string): number {
  return Buffer.compare(Buffer.from(left, 'utf8'), Buffer.from(right, 'utf8'))
}

async function requireRegularHelper(path: string): Promise<void> {
  try {
    const metadata = await lstat(path)
    if (!metadata.isFile() || metadata.isSymbolicLink()) {
      throw new Error('Windows Job helper must be a regular file')
    }
  } catch (cause) {
    throw new Error(`Windows Job helper cannot be opened: ${errorMessage(cause)}`, { cause })
  }
}

function requireCanonicalWindowsPath(path: string, label: string): void {
  if (!win32.isAbsolute(path) || win32.normalize(path) !== path || path.includes('\0')) {
    throw new Error(`${label} must be an absolute canonical Windows path`)
  }
}

function requireWireText(value: string, maximumBytes: number, label: string): void {
  if (
    value.length === 0 || value.includes('\0') || value.normalize('NFC') !== value ||
    !isWellFormedUnicode(value) ||
    Buffer.byteLength(value, 'utf8') > maximumBytes
  ) throw new Error(`${label} must be non-empty well-formed text within ${maximumBytes} bytes`)
}

function requirePortableScope(value: string, label: string): string {
  requireWireText(value, WINDOWS_JOB_MAXIMUM_OPERATION_BYTES, label)
  if (!/^[A-Za-z0-9._-]+$/u.test(value)) throw new Error(`${label} is not portable`)
  return value
}

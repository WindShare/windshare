import { randomBytes } from 'node:crypto'
import { spawn, type ChildProcessWithoutNullStreams } from 'node:child_process'
import { link, lstat, mkdtemp, open, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join, win32 } from 'node:path'

import type { RunnerProcessEvidence } from '../execution-evidence.ts'
import { readStableRegularFileSnapshot } from '../filesystem/snapshot.ts'
import {
  requireBoolean,
  requireEnum,
  requireExactKeys,
  requireLiteral,
  requireRecord,
  requireSafeInteger,
  requireString,
} from '../contract/json.ts'
import { parseCanonicalJsonText } from '../contract/strict-json.ts'

export const WINDOWS_JOB_CONTROL_MAXIMUM_BYTES = 1 * 1024 * 1024
export const WINDOWS_JOB_STDIN_MAXIMUM_BYTES = 1 * 1024 * 1024
export const WINDOWS_JOB_STATUS_MAXIMUM_BYTES = 16_384 as const
export const WINDOWS_JOB_NONCE_BYTES = 32 as const
export const WINDOWS_JOB_WATCHDOG_SLACK_MS = 5_000 as const
export const WINDOWS_JOB_POST_KILL_LEASE_MS = 5_000 as const
export const WINDOWS_JOB_MAXIMUM_DEADLINE_MS = 86_400_000 as const
export const WINDOWS_JOB_MAXIMUM_TERMINATION_GRACE_MS = 60_000 as const
export const WINDOWS_JOB_MAXIMUM_OPERATION_BYTES = 256 as const

const WINDOWS_JOB_SCHEMA_VERSION = 2 as const
const WINDOWS_JOB_STATUS_KEYS = Object.freeze([
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
const WINDOWS_JOB_ROOT_KEYS = Object.freeze(['pid', 'exitCode'])
const WINDOWS_JOB_SUPERVISION_OUTCOMES = Object.freeze([
  'tree-empty',
  'spawn-failed',
] as const)
const WINDOWS_JOB_TERMINATION_REASONS = Object.freeze([
  'natural',
  'target-spawn-failed',
  'deadline',
  'parent-request',
] as const)
const WINDOWS_UINT32_MAXIMUM = 0xffff_ffff

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

export type WindowsJobHelperKillOutcome = 'not-attempted' | 'accepted' | 'rejected' | 'threw'

export interface WindowsJobHelperTerminal {
  readonly code: number | null
  readonly signal: NodeJS.Signals | null
  readonly spawnError: unknown
  readonly watchdogExpired: boolean
  readonly postKillLeaseExpired: boolean
  readonly postKillLeaseMs: number
  readonly killOutcome: WindowsJobHelperKillOutcome
  readonly killError: unknown
  readonly handleReleaseErrors: readonly string[]
}

export interface WindowsJobHelperLeaseTarget {
  readonly onError: (listener: (cause: unknown) => void) => () => void
  readonly onClose: (
    listener: (code: number | null, signal: NodeJS.Signals | null) => void,
  ) => () => void
  readonly kill: () => boolean
  readonly releaseHandles: () => readonly string[]
}

export interface WindowsJobHelperLeaseClock {
  readonly setReferencedTimeout: (callback: () => void, delayMs: number) => unknown
  readonly clearTimeout: (handle: unknown) => void
}

export interface WindowsJobHelperLease {
  readonly terminal: Promise<WindowsJobHelperTerminal>
  readonly terminateRejectedStart: () => void
}

const SYSTEM_WINDOWS_JOB_LEASE_CLOCK: WindowsJobHelperLeaseClock = Object.freeze({
  // Node timers are referenced by default. The watchdog is authority, so allowing
  // it to disappear merely because no other handle is live would reopen the hang.
  setReferencedTimeout: (callback: () => void, delayMs: number) => {
    const timer = setTimeout(callback, delayMs)
    timer.ref()
    return timer
  },
  clearTimeout: (handle: unknown) => clearTimeout(handle as ReturnType<typeof setTimeout>),
})

export async function executeWindowsJob(
  options: WindowsJobExecutionOptions,
): Promise<WindowsJobExecution> {
  const externalTermination = observeWindowsJobTermination(options.terminationSignal)
  try {
    const preparedInput = prepareWindowsJobRawInput(options.command)
    try {
      await preflightWindowsJobExecution(options)
      return await executePreparedWindowsJob(options, externalTermination, preparedInput)
    } finally {
      preparedInput.bytes.fill(0)
    }
  } finally {
    externalTermination.close()
  }
}

async function preflightWindowsJobExecution(options: WindowsJobExecutionOptions): Promise<void> {
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

interface PreparedWindowsJobInput {
  readonly bytes: Buffer
  readonly metadata: ReturnType<typeof canonicalWindowsJobStdinMetadata> | null
}

interface WindowsJobWorkspace {
  readonly nonce: string
  readonly statusPath: string
  readonly requestPath: string
  readonly controlPath: string
}

async function executePreparedWindowsJob(
  options: WindowsJobExecutionOptions,
  externalTermination: WindowsJobTerminationObservation,
  preparedInput: PreparedWindowsJobInput,
): Promise<WindowsJobExecution> {
  // The nonce stays exclusively in the invocation-private request file. Its
  // pathname can grant denial, but cannot forge the create-new status record.
  const nonce = randomBytes(WINDOWS_JOB_NONCE_BYTES).toString('hex')
  const statusWorkspace = await mkdtemp(join(tmpdir(), 'windshare-windows-job-'))
  const workspace = Object.freeze({
    nonce,
    statusPath: join(statusWorkspace, 'status.json'),
    requestPath: join(statusWorkspace, 'request.bin'),
    controlPath: join(statusWorkspace, 'control.bin'),
  })
  try {
    await publishWindowsJobStartRequest(options, preparedInput, workspace)
    return await superviseWindowsJob(options, externalTermination, preparedInput, workspace)
  } finally {
    // The authenticated status and request are control-plane state outside the
    // evidence tree. Cleanup cannot revoke authority after exact acceptance.
    await rm(statusWorkspace, { recursive: true, force: true }).catch(() => undefined)
  }
}

async function publishWindowsJobStartRequest(
  options: WindowsJobExecutionOptions,
  preparedInput: PreparedWindowsJobInput,
  workspace: WindowsJobWorkspace,
): Promise<void> {
  const request = {
    schemaVersion: WINDOWS_JOB_SCHEMA_VERSION,
    type: 'start',
    operationId: options.operationId,
    nonce: workspace.nonce,
    executable: options.command.executable,
    arguments: [...options.command.arguments],
    cwd: options.command.cwd ?? '',
    environment: canonicalWindowsJobEnvironment(
      options.inheritedEnvironment,
      options.command.environment,
      options.injectedEnvironment,
    ),
    deadlineMs: options.deadlineMs,
    terminationGraceMs: options.terminationGraceMs,
    ...(options.command.executableSha256 === undefined
      ? {}
      : { executableSha256: requireExecutableSha256(options.command.executableSha256) }),
    ...(preparedInput.metadata === null ? {} : { stdin: preparedInput.metadata }),
  }
  const controlFrame = encodeControlFrame(request)
  try {
    await writeFile(workspace.requestPath, controlFrame, { flag: 'wx', mode: 0o400 })
  } finally {
    controlFrame.fill(0)
  }
}

async function superviseWindowsJob(
  options: WindowsJobExecutionOptions,
  externalTermination: WindowsJobTerminationObservation,
  preparedInput: PreparedWindowsJobInput,
  workspace: WindowsJobWorkspace,
): Promise<WindowsJobExecution> {
  const helper = spawn(options.helperPath, [
    'supervise',
    '--status', workspace.statusPath,
    '--request', workspace.requestPath,
    '--control', workspace.controlPath,
  ], {
    env: windowsJobSupervisorEnvironment(),
    shell: false,
    stdio: ['pipe', 'pipe', 'pipe'],
    windowsHide: true,
  })
  let outputSinkFailure: Error | undefined
  const forwardStdout = nonAuthoritativeByteSink('stdout', options.stdout, (failure) => {
    outputSinkFailure ??= failure
  })
  const forwardStderr = nonAuthoritativeByteSink('stderr', options.stderr, (failure) => {
    outputSinkFailure ??= failure
  })
  const ignoreStdinError = () => undefined
  helper.stdout.on('data', forwardStdout)
  helper.stderr.on('data', forwardStderr)
  // Standard input is the only raw secret channel. Metadata and parent
  // liveness have independent authorities, so these bytes never enter JSON.
  helper.stdin.on('error', ignoreStdinError)
  const helperLease = createWindowsJobHelperLease(
    windowsJobHelperLeaseTarget(helper),
    options.deadlineMs + options.terminationGraceMs + WINDOWS_JOB_WATCHDOG_SLACK_MS,
  )
  const controlDelivery = deliverWindowsJobTermination(
    externalTermination,
    helperLease.terminal,
    workspace.controlPath,
    windowsJobTerminationControl(options.operationId, workspace.nonce),
  )
  let execution: WindowsJobExecution | undefined
  let authorityFailure: unknown
  try {
    await deliverWindowsJobStartInput(helper, helperLease, preparedInput.bytes)
    const terminal = await helperLease.terminal
    const controlEvidence = await controlDelivery
    requireSuccessfulWindowsJobHelper(terminal)
    execution = await readWindowsJobExecution(
      options,
      preparedInput,
      workspace,
      controlEvidence,
      outputSinkFailure,
    )
  } catch (cause) {
    authorityFailure = cause
  } finally {
    helper.stdin.off('error', ignoreStdinError)
    helper.stdout.off('data', forwardStdout)
    helper.stderr.off('data', forwardStderr)
  }
  return requireWindowsJobExecution(execution, authorityFailure, outputSinkFailure)
}

function windowsJobTerminationControl(
  operationId: string,
  nonce: string,
): Readonly<Record<string, unknown>> {
  return Object.freeze({
    schemaVersion: WINDOWS_JOB_SCHEMA_VERSION,
    type: 'terminate',
    operationId,
    nonce,
    reason: 'parent-request',
  })
}

async function deliverWindowsJobStartInput(
  helper: ChildProcessWithoutNullStreams,
  helperLease: WindowsJobHelperLease,
  bytes: Buffer,
): Promise<void> {
  try {
    await deliverAndEraseWindowsJobRawInput(helper.stdin, bytes)
  } catch (cause) {
    helper.stdin.destroy()
    helperLease.terminateRejectedStart()
    const terminal = await helperLease.terminal
    if (terminal.spawnError !== undefined) {
      throw new Error(windowsJobHelperFailureMessage(terminal), { cause })
    }
    const cleanupFailure = terminal.postKillLeaseExpired
      ? `; ${windowsJobHelperFailureMessage(terminal)}`
      : ''
    throw new Error(
      `Windows Job helper rejected its raw input channel: ${errorMessage(cause)}${cleanupFailure}`,
      { cause },
    )
  }
}

function requireSuccessfulWindowsJobHelper(terminal: WindowsJobHelperTerminal): void {
  if (
    terminal.spawnError !== undefined || terminal.watchdogExpired ||
    terminal.postKillLeaseExpired || terminal.code !== 0 || terminal.signal !== null
  ) throw new Error(windowsJobHelperFailureMessage(terminal))
}

async function readWindowsJobExecution(
  options: WindowsJobExecutionOptions,
  preparedInput: PreparedWindowsJobInput,
  workspace: WindowsJobWorkspace,
  controlEvidence: WindowsJobControlDeliveryEvidence,
  outputSinkFailure: Error | undefined,
): Promise<WindowsJobExecution> {
  const statusSnapshot = await readStableRegularFileSnapshot(
    workspace.statusPath,
    WINDOWS_JOB_STATUS_MAXIMUM_BYTES,
    'Windows Job authority status',
  )
  const inputRequested = preparedInput.metadata !== null
  const status = parseWindowsJobAuthorityStatus(
    statusSnapshot.bytes,
    options.operationId,
    workspace.nonce,
    inputRequested,
  )
  return windowsJobExecutionEvidence(status, inputRequested, controlEvidence, outputSinkFailure)
}

function windowsJobExecutionEvidence(
  status: WindowsJobStatus,
  inputRequested: boolean,
  controlEvidence: WindowsJobControlDeliveryEvidence,
  outputSinkFailure: Error | undefined,
): WindowsJobExecution {
  return Object.freeze({
    processEvidence: statusProcessEvidence(status),
    timedOut: status.timedOut,
    launched: status.root !== null,
    treeEmpty: status.supervisionOutcome === 'tree-empty' && status.activeProcessCount === 0,
    inputEvidence: Object.freeze({
      outcome: status.inputOutcome,
      failureCode: '',
      failureMessage: '',
    }),
    clientIoEvidence: Object.freeze({
      requestOutcome: 'delivered',
      rawInputOutcome: inputRequested ? 'delivered' : 'not-requested',
      controlOutcome: controlEvidence.outcome,
      outputOutcome: outputSinkFailure === undefined ? 'delivered' : 'failed',
      failureCode: controlEvidence.failure === undefined && outputSinkFailure === undefined
        ? ''
        : 'CLIENT_IO_FAILED',
      failureMessage: [controlEvidence.failure, outputSinkFailure]
        .filter((failure): failure is Error => failure !== undefined)
        .map((failure) => failure.message)
        .join('; '),
    }),
    ownershipEvidence: Object.freeze({
      supervisionOutcome: status.supervisionOutcome,
      terminationReason: status.terminationReason,
      activeProcessCount: status.activeProcessCount,
      root: status.root,
      spawnFailure: status.spawnFailure,
    }),
  })
}

function requireWindowsJobExecution(
  execution: WindowsJobExecution | undefined,
  authorityFailure: unknown,
  outputSinkFailure: Error | undefined,
): WindowsJobExecution {
  if (authorityFailure !== undefined && outputSinkFailure !== undefined) {
    throw new AggregateError(
      [authorityFailure, outputSinkFailure],
      'Windows Job authority and output forwarding both failed',
      { cause: authorityFailure },
    )
  }
  if (authorityFailure !== undefined) throw authorityFailure
  if (execution === undefined) throw new Error('Windows Job ended without terminal evidence')
  return execution
}

function nonAuthoritativeByteSink(
  stream: 'stdout' | 'stderr',
  sink: (chunk: Uint8Array) => void,
  recordFailure: (failure: Error) => void,
): (chunk: Uint8Array) => void {
  let failed = false
  return (chunk) => {
    try {
      if (!failed) sink(chunk)
    } catch (cause) {
      failed = true
      recordFailure(new Error(`Windows Job ${stream} sink failed`, { cause }))
    } finally {
      // Stream chunks may contain secrets even after the consumer's first
      // failure. Erase every delivered backing range instead of retaining the
      // unobserved tail in Node's pooled Buffer memory.
      chunk.fill(0)
    }
  }
}

function requireExecutableSha256(value: string): string {
  if (!/^[0-9a-f]{64}$/u.test(value)) {
    throw new Error('Windows Job target executable SHA-256 must be lowercase 64-hex')
  }
  return value
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

function prepareWindowsJobRawInput(command: WindowsJobCommand): {
  readonly bytes: Buffer
  readonly metadata: ReturnType<typeof canonicalWindowsJobStdinMetadata> | null
} {
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

interface WindowsJobRawInputPipe {
  once(event: 'error', listener: (cause: unknown) => void): unknown
  once(event: 'close', listener: () => void): unknown
  end(bytes: Uint8Array, callback: () => void): unknown
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

interface WindowsJobTerminationObservation {
  readonly requested: Promise<void>
  readonly close: () => void
}

interface WindowsJobControlDeliveryEvidence {
  readonly outcome: 'not-requested' | 'delivered' | 'failed'
  readonly failure?: Error
}

function observeWindowsJobTermination(
  signal: AbortSignal | undefined,
): WindowsJobTerminationObservation {
  let resolveRequested!: () => void
  const requested = new Promise<void>((resolve) => { resolveRequested = resolve })
  let remove: () => void = () => undefined
  if (signal !== undefined) {
    const abort = () => resolveRequested()
    if (signal.aborted) abort()
    else {
      signal.addEventListener('abort', abort, { once: true })
      remove = () => signal.removeEventListener('abort', abort)
    }
  }
  return Object.freeze({ requested, close: remove })
}

async function deliverWindowsJobTermination(
  observation: WindowsJobTerminationObservation,
  terminal: Promise<WindowsJobHelperTerminal>,
  controlPath: string,
  control: Readonly<Record<string, unknown>>,
): Promise<WindowsJobControlDeliveryEvidence> {
  const decision = await Promise.race([
    observation.requested.then(() => 'requested' as const),
    terminal.then(() => 'terminal' as const),
  ])
  if (decision === 'terminal') return Object.freeze({ outcome: 'not-requested' as const })

  const stagedPath = `${controlPath}.${randomBytes(16).toString('hex')}.tmp`
  const frame = encodeControlFrame(control)
  try {
    const staged = await open(stagedPath, 'wx', 0o400)
    try {
      await staged.writeFile(frame)
      await staged.sync()
    } finally {
      await staged.close()
    }
    // A same-volume hard link makes the complete authenticated frame visible
    // create-new, so the owner never observes a partially written request.
    await link(stagedPath, controlPath)
    return Object.freeze({ outcome: 'delivered' as const })
  } catch (cause) {
    return Object.freeze({
      outcome: 'failed' as const,
      failure: new Error('Windows Job termination control publication failed', { cause }),
    })
  } finally {
    frame.fill(0)
    await rm(stagedPath, { force: true }).catch(() => undefined)
  }
}

function requirePortableScope(value: string, label: string): string {
  requireWireText(value, 256, label)
  if (!/^[A-Za-z0-9._-]+$/u.test(value)) throw new Error(`${label} is not portable`)
  return value
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

function requireBoundedPositiveInteger(
  value: number,
  maximum: number,
  label: string,
): void {
  if (!Number.isSafeInteger(value) || value < 1 || value > maximum) {
    throw new Error(`${label} must be an integer in [1, ${maximum}]`)
  }
}

function encodeControlFrame(value: unknown): Uint8Array {
  const body = Buffer.from(JSON.stringify(value), 'utf8')
  if (body.byteLength > WINDOWS_JOB_CONTROL_MAXIMUM_BYTES) {
    throw new Error('Windows Job start request exceeds its control-frame limit')
  }
  const header = Buffer.allocUnsafe(4)
  header.writeUInt32BE(body.byteLength)
  return Buffer.concat([header, body])
}

export function createWindowsJobHelperLease(
  target: WindowsJobHelperLeaseTarget,
  watchdogMs: number,
  dependencies: {
    readonly clock?: WindowsJobHelperLeaseClock
    readonly postKillLeaseMs?: number
  } = {},
): WindowsJobHelperLease {
  requireBoundedPositiveInteger(
    watchdogMs,
    WINDOWS_JOB_MAXIMUM_DEADLINE_MS + WINDOWS_JOB_MAXIMUM_TERMINATION_GRACE_MS
      + WINDOWS_JOB_WATCHDOG_SLACK_MS,
    'Windows Job authority watchdog',
  )
  const postKillLeaseMs = dependencies.postKillLeaseMs ?? WINDOWS_JOB_POST_KILL_LEASE_MS
  requireBoundedPositiveInteger(
    postKillLeaseMs,
    WINDOWS_JOB_MAXIMUM_TERMINATION_GRACE_MS,
    'Windows Job post-kill lease',
  )
  const clock = dependencies.clock ?? SYSTEM_WINDOWS_JOB_LEASE_CLOCK
  let spawnError: unknown
  let watchdogExpired = false
  let killOutcome: WindowsJobHelperKillOutcome = 'not-attempted'
  let killError: unknown
  let handleReleaseErrors: readonly string[] = Object.freeze([])
  let settled = false
  let terminationStarted = false
  let postKillLeaseExpiring = false
  let watchdog: unknown
  let postKillLease: unknown
  let stopObservingError: () => void = () => undefined
  let stopObservingClose: () => void = () => undefined
  let resolveTerminal!: (terminal: WindowsJobHelperTerminal) => void
  const terminal = new Promise<WindowsJobHelperTerminal>((resolve) => {
    resolveTerminal = resolve
  })

  const clearLeaseTimers = () => {
    if (watchdog !== undefined) {
      clock.clearTimeout(watchdog)
      watchdog = undefined
    }
    if (postKillLease !== undefined) {
      clock.clearTimeout(postKillLease)
      postKillLease = undefined
    }
  }
  const settle = (
    code: number | null,
    signal: NodeJS.Signals | null,
    postKillLeaseExpired: boolean,
  ) => {
    if (settled) return
    settled = true
    clearLeaseTimers()
    stopObservingError()
    stopObservingClose()
    resolveTerminal(Object.freeze({
      code,
      signal,
      spawnError,
      watchdogExpired,
      postKillLeaseExpired,
      postKillLeaseMs,
      killOutcome,
      killError,
      handleReleaseErrors,
    }))
  }
  const forceTermination = (watchdogTriggered: boolean) => {
    if (settled || terminationStarted) return
    terminationStarted = true
    watchdogExpired = watchdogTriggered
    if (watchdog !== undefined) {
      clock.clearTimeout(watchdog)
      watchdog = undefined
    }
    try {
      killOutcome = target.kill() ? 'accepted' : 'rejected'
    } catch (cause) {
      killOutcome = 'threw'
      killError = cause
    }
    if (settled) return
    // kill() is only a request. A second referenced lease ensures a missing close
    // event cannot retain Node's stdio and child-process handles indefinitely.
    postKillLease = clock.setReferencedTimeout(() => {
      postKillLeaseExpiring = true
      try {
        handleReleaseErrors = Object.freeze([...target.releaseHandles()])
      } catch (cause) {
        handleReleaseErrors = Object.freeze([
          `helper handle release threw: ${errorMessage(cause)}`,
        ])
      }
      settle(null, null, true)
    }, postKillLeaseMs)
  }

  stopObservingError = target.onError((cause) => {
    // ChildProcess also uses "error" for a failed kill request. Once forced
    // termination starts, that event describes cleanup rather than spawn authority.
    if (terminationStarted) {
      killError ??= cause
      return
    }
    spawnError = cause
    forceTermination(false)
  })
  stopObservingClose = target.onClose((code, signal) => {
    if (!postKillLeaseExpiring) settle(code, signal, false)
  })
  watchdog = clock.setReferencedTimeout(() => forceTermination(true), watchdogMs)

  return Object.freeze({
    terminal,
    terminateRejectedStart: () => forceTermination(false),
  })
}

function windowsJobHelperLeaseTarget(
  helper: ReturnType<typeof spawn>,
): WindowsJobHelperLeaseTarget {
  return Object.freeze({
    onError(listener: (cause: unknown) => void) {
      helper.once('error', listener)
      return () => helper.off('error', listener)
    },
    onClose(listener: (code: number | null, signal: NodeJS.Signals | null) => void) {
      helper.once('close', listener)
      return () => helper.off('close', listener)
    },
    kill: () => helper.kill('SIGKILL'),
    releaseHandles: () => releaseWindowsJobHelperHandles(helper),
  })
}

function releaseWindowsJobHelperHandles(
  helper: ReturnType<typeof spawn>,
): readonly string[] {
  const failures: string[] = []
  for (const [label, release] of [
    ['stdin', () => helper.stdin?.destroy()],
    ['stdout', () => helper.stdout?.destroy()],
    ['stderr', () => helper.stderr?.destroy()],
    ['process', () => helper.unref()],
  ] as const) {
    try {
      release()
    } catch (cause) {
      failures.push(`${label} handle: ${errorMessage(cause)}`)
    }
  }
  return Object.freeze(failures)
}

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
  const nonce = requireString(status.nonce, 'Windows Job status nonce', 64)
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
    : requireString(status.spawnFailure, 'Windows Job spawn failure', 512)
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

function statusProcessEvidence(status: WindowsJobStatus): RunnerProcessEvidence {
  if (status.root !== null) {
    return Object.freeze({ terminal: 'exited', exitCode: status.root.exitCode })
  }
  return Object.freeze({
    terminal: 'spawn-failed',
    errorCode: 'WINDOWS_TARGET_SPAWN_FAILED',
    errorMessage: status.spawnFailure ?? 'Windows target spawn failed',
  })
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

function errorMessage(cause: unknown): string {
  return cause instanceof Error ? cause.message : String(cause)
}

export function windowsJobHelperFailureMessage(terminal: WindowsJobHelperTerminal): string {
  if (terminal.spawnError !== undefined) {
    const detail = terminal.spawnError instanceof Error
      ? terminal.spawnError.message
      : String(terminal.spawnError)
    const failure = `Windows Job helper failed to spawn: ${detail}`
    return terminal.postKillLeaseExpired
      ? `${failure}; ${windowsJobPostKillLeaseFailureMessage(terminal)}`
      : failure
  }
  if (terminal.postKillLeaseExpired) {
    const trigger = terminal.watchdogExpired
      ? 'Windows Job helper exceeded its authority watchdog'
      : 'Windows Job helper termination did not complete'
    return `${trigger}; ${windowsJobPostKillLeaseFailureMessage(terminal)}`
  }
  if (terminal.watchdogExpired) {
    return `Windows Job helper exceeded its authority watchdog; SIGKILL was ${terminal.killOutcome}`
  }
  if (terminal.signal !== null) return `Windows Job helper terminated by ${terminal.signal}`
  return `Windows Job helper exited without authority (code ${String(terminal.code)})`
}

function windowsJobPostKillLeaseFailureMessage(terminal: WindowsJobHelperTerminal): string {
  return `${windowsJobHelperKillMessage(terminal)}; helper did not close within `
    + `${terminal.postKillLeaseMs} ms; ${windowsJobHelperHandleReleaseMessage(terminal)}`
}

function windowsJobHelperKillMessage(terminal: WindowsJobHelperTerminal): string {
  if (terminal.killOutcome === 'accepted') return 'SIGKILL was accepted'
  if (terminal.killOutcome === 'rejected') {
    return terminal.killError === undefined
      ? 'SIGKILL was rejected'
      : `SIGKILL was rejected: ${errorMessage(terminal.killError)}`
  }
  if (terminal.killOutcome === 'threw') {
    return `SIGKILL threw: ${errorMessage(terminal.killError)}`
  }
  return 'SIGKILL was not attempted'
}

function windowsJobHelperHandleReleaseMessage(terminal: WindowsJobHelperTerminal): string {
  if (terminal.handleReleaseErrors.length === 0) {
    return 'stdio and process handles were released'
  }
  return `handle release reported ${terminal.handleReleaseErrors.join('; ')}`
}

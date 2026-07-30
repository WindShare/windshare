import { spawn, type ChildProcessWithoutNullStreams } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { link, mkdtemp, open, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { readStableRegularFileSnapshot } from '../../filesystem/snapshot.ts'
import {
  WINDOWS_JOB_NONCE_BYTES,
  WINDOWS_JOB_SCHEMA_VERSION,
  WINDOWS_JOB_STATUS_MAXIMUM_BYTES,
  WINDOWS_JOB_WATCHDOG_SLACK_MS,
  canonicalWindowsJobEnvironment,
  deliverAndEraseWindowsJobRawInput,
  encodeWindowsJobControlFrame,
  errorMessage,
  preflightWindowsJobExecution,
  prepareWindowsJobRawInput,
  requireExecutableSha256,
  windowsJobSupervisorEnvironment,
  type PreparedWindowsJobInput,
  type WindowsJobExecution,
  type WindowsJobExecutionOptions,
  type WindowsJobStatus,
} from './contract.ts'
import {
  createWindowsJobHelperLease,
  windowsJobHelperFailureMessage,
  windowsJobHelperLeaseTarget,
  type WindowsJobHelperLease,
  type WindowsJobHelperTerminal,
} from './helper-lease.ts'
import { parseWindowsJobAuthorityStatus, statusProcessEvidence } from './status.ts'

interface WindowsJobWorkspace {
  readonly nonce: string
  readonly statusPath: string
  readonly requestPath: string
  readonly controlPath: string
}

interface WindowsJobTerminationObservation {
  readonly requested: Promise<void>
  readonly close: () => void
}

interface WindowsJobControlDeliveryEvidence {
  readonly outcome: 'not-requested' | 'delivered' | 'failed'
  readonly failure?: Error
}

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
  const controlFrame = encodeWindowsJobControlFrame(request)
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
  const helper: ChildProcessWithoutNullStreams = spawn(options.helperPath, [
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
  const frame = encodeWindowsJobControlFrame(control)
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

import { spawn, type ChildProcess } from 'node:child_process'
import { createHash } from 'node:crypto'
import { constants, type BigIntStats } from 'node:fs'
import { lstat, open, realpath, type FileHandle } from 'node:fs/promises'
import { isAbsolute, resolve } from 'node:path'
import { Readable, Writable } from 'node:stream'

import type { RunnerProcessEvidence } from '../execution-evidence.ts'
import type { ContainedSampleCommand } from './containment.ts'

export const LINUX_PROCESS_OWNER_REQUEST_SCHEMA_VERSION =
  'windshare.linux-process-owner-request/v1' as const
export const LINUX_PROCESS_OWNER_STATUS_SCHEMA_VERSION =
  'windshare.linux-process-owner-status/v2' as const

const MAXIMUM_STATUS_BYTES = 65_536
const MAXIMUM_OWNER_REQUEST_BYTES = 1_048_576
const MAXIMUM_COMMAND_STDIN_BYTES = 1_048_576
const MAXIMUM_HELPER_BYTES = 512 * 1024 * 1024
const MAXIMUM_OWNER_DEADLINE_MS = 3_600_000
const MAXIMUM_TERMINATION_GRACE_MS = 60_000
const HELPER_AUTHORITY_DESCRIPTOR = 6
const OWNER_EXIT_RESERVE_MS = 5_000
const SHA256_PATTERN = /^[a-f0-9]{64}$/u

export interface LinuxProcessOwnerArtifact {
  readonly path: string
  readonly byteLength: number
  readonly sha256: string
}

export interface LinuxProcessOwnerRequest {
  readonly helper: LinuxProcessOwnerArtifact
  readonly operationId: string
  readonly command: ContainedSampleCommand & {
    readonly executableSha256?: string
    readonly executableByteLength?: number
    readonly stdinAuthority?: LinuxProcessOwnerStdinAuthority
  }
  readonly environment: Readonly<Record<string, string>>
  readonly deadlineMs: number
  readonly terminationGraceMs: number
  readonly terminationSignal?: AbortSignal
  readonly stdout: (chunk: Uint8Array) => void
  readonly stderr: (chunk: Uint8Array) => void
  readonly trace?: (event: {
    readonly milestone: string
    readonly context?: Readonly<Record<string, unknown>>
  }) => void
}

export interface LinuxProcessOwnerStdinAuthority {
  readonly channelId: string
  readonly runId: string
  readonly profileId: string
  readonly attemptId: string
}

export interface LinuxProcessOwnershipEvidence {
  readonly ownerPid: number
  readonly rootPid: number | null
  readonly rootStartTimeTicks: string
  readonly inventoryScans: number
  readonly maximumObservedDescendants: number
  readonly quietInventoryCount: number
  readonly controlOutcome: string
  readonly cleanupOutcome: 'completed' | 'failed'
  readonly failureCode: string
  readonly failureMessage: string
}

export interface LinuxProcessInputEvidence {
  readonly outcome: 'not-started' | 'not-requested' | 'delivered' | 'failed'
  readonly failureCode: string
  readonly failureMessage: string
}

export interface LinuxProcessClientIoEvidence {
  readonly requestOutcome: 'delivered' | 'failed'
  readonly rawInputOutcome: 'not-requested' | 'delivered' | 'failed'
  readonly controlOutcome: 'not-requested' | 'delivered' | 'failed'
  readonly outputOutcome: 'delivered' | 'failed'
  readonly failureCode: string
  readonly failureMessage: string
}

export interface LinuxProcessOwnerExecution {
  readonly processEvidence: RunnerProcessEvidence
  readonly timedOut: boolean
  readonly launched: boolean
  readonly treeEmpty: boolean
  readonly inputEvidence: LinuxProcessInputEvidence
  readonly clientIoEvidence: LinuxProcessClientIoEvidence
  readonly ownershipEvidence: LinuxProcessOwnershipEvidence
}

export async function executeLinuxProcessOwner(
  request: LinuxProcessOwnerRequest,
): Promise<LinuxProcessOwnerExecution> {
  const externalTermination = observeOwnerTermination(request.terminationSignal)
  let rawChildInput: Buffer = Buffer.alloc(0)
  let requestBytes: Buffer = Buffer.alloc(0)
  try {
    validateLinuxProcessOwnerRequest(request)
    // Canonicalization and secret copying precede held-file acquisition so an
    // invalid request can never leak the helper descriptor it did not use.
    const protocol = prepareOwnerProtocol(request)
    rawChildInput = protocol.rawChildInput
    requestBytes = protocol.requestBytes
    const helper = await holdLinuxProcessOwnerArtifact(request.helper, 'Linux process owner helper')
    try {
      return await runLinuxProcessOwner(
        request,
        externalTermination,
        helper,
        rawChildInput,
        requestBytes,
      )
    } finally {
      await helper.close()
    }
  } finally {
    requestBytes.fill(0)
    rawChildInput.fill(0)
    externalTermination.close()
  }
}

function validateLinuxProcessOwnerRequest(request: LinuxProcessOwnerRequest): void {
  requireOperationId(request.operationId)
  requireBoundedPositiveInteger(
    request.deadlineMs,
    MAXIMUM_OWNER_DEADLINE_MS,
    'Linux process owner deadline',
  )
  requireBoundedPositiveInteger(
    request.terminationGraceMs,
    MAXIMUM_TERMINATION_GRACE_MS,
    'Linux process owner termination grace',
  )
}

interface LinuxProcessOwnerPipes {
  readonly request: Writable
  readonly stdout: Readable
  readonly stderr: Readable
  readonly status: Readable
  readonly control: Writable
  readonly rawInput: Writable
}

type LinuxProcessOwnerProgress =
  | Readonly<{ outcome: 'owner-lifetime'; completed: RunnerProcessEvidence | undefined }>
  | Readonly<{ outcome: 'request-failed'; failure: unknown }>
  | Readonly<{ outcome: 'termination-requested' }>

interface LinuxProcessControlEvidence {
  readonly requested: boolean
  readonly failure: unknown
}

async function runLinuxProcessOwner(
  request: LinuxProcessOwnerRequest,
  externalTermination: OwnerTerminationObservation,
  helper: HeldLinuxProcessOwnerArtifact,
  rawChildInput: Buffer,
  requestBytes: Buffer,
): Promise<LinuxProcessOwnerExecution> {
  const owner = spawn(`/proc/self/fd/${HELPER_AUTHORITY_DESCRIPTOR}`, ['run'], {
    env: {},
    shell: false,
    stdio: ['pipe', 'pipe', 'pipe', 'pipe', 'pipe', 'pipe', helper.handle.fd],
    windowsHide: true,
  })
  const terminal = childTerminal(owner)
  try {
    const pipes = linuxProcessOwnerPipes(owner)
    return await executeLinuxProcessOwnerProtocol({
      request,
      externalTermination,
      helper,
      terminal,
      pipes,
      rawChildInput,
      requestBytes,
    })
  } catch (cause) {
    return await rethrowAfterLinuxOwnerSettlement(owner, terminal, cause)
  }
}

function linuxProcessOwnerPipes(owner: ChildProcess): LinuxProcessOwnerPipes {
  const requestPipe = owner.stdin
  const stdout = owner.stdout
  const stderr = owner.stderr
  if (requestPipe === null || stdout === null || stderr === null) {
    throw new Error('Linux process owner did not create its standard protocol pipes')
  }
  // Node models only descriptors 0-4 in ChildProcess.stdio. Capability checks
  // preserve the exact fd 3/4/5 directions before any authority bytes move.
  return Object.freeze({
    request: requestPipe,
    stdout,
    stderr,
    status: requireReadableProtocolPipe(owner.stdio.at(3), 'status'),
    control: requireWritableProtocolPipe(owner.stdio.at(4), 'control'),
    rawInput: requireWritableProtocolPipe(owner.stdio.at(5), 'raw input'),
  })
}

interface LinuxProcessOwnerProtocolContext {
  readonly request: LinuxProcessOwnerRequest
  readonly externalTermination: OwnerTerminationObservation
  readonly helper: HeldLinuxProcessOwnerArtifact
  readonly terminal: Promise<RunnerProcessEvidence>
  readonly pipes: LinuxProcessOwnerPipes
  readonly rawChildInput: Buffer
  readonly requestBytes: Buffer
}

async function executeLinuxProcessOwnerProtocol(
  context: LinuxProcessOwnerProtocolContext,
): Promise<LinuxProcessOwnerExecution> {
  const { request, pipes } = context
  const status = boundedCollector('Linux process owner status', MAXIMUM_STATUS_BYTES)
  let outputSinkFailure: Error | undefined
  let statusSinkFailure: Error | undefined
  const forwardStdout = nonAuthoritativeSink(request.stdout, (cause) => {
    outputSinkFailure ??= cause
  })
  const forwardStderr = nonAuthoritativeSink(request.stderr, (cause) => {
    outputSinkFailure ??= cause
  })
  const collectStatus = nonAuthoritativeSink(status.write, (cause) => {
    statusSinkFailure ??= cause
  })
  pipes.stdout.on('data', forwardStdout)
  pipes.stderr.on('data', forwardStderr)
  pipes.status.on('data', collectStatus)
  try {
    const maximumOwnerLifetime = request.deadlineMs + (2 * request.terminationGraceMs) +
      OWNER_EXIT_RESERVE_MS
    // Request-pipe backpressure belongs to the same owner lifetime and cannot
    // postpone the subreaper's settlement authority.
    const ownerLifetime = boundedWait(context.terminal, maximumOwnerLifetime)
    const rawInputDelivery = deliverAndEraseRawChildInput(
      pipes.rawInput,
      context.rawChildInput,
    )
    const requestDelivery = deliverAndEraseOwnerRequest(pipes.request, context.requestBytes)
    emitTrace(request, 'linux-process-owner-launched', {
      operationId: request.operationId,
      helperSha256: context.helper.sha256,
    })
    const progress = await linuxProcessOwnerProgress(
      ownerLifetime,
      requestDelivery,
      context.externalTermination,
    )
    const control = await settleLinuxProcessOwnerProgress(progress, pipes, context.terminal)
    const terminalEvidence = await context.terminal
    pipes.request.destroy()
    pipes.rawInput.destroy()
    const [requestFailure, rawInputFailure] = await Promise.all([
      requestDelivery,
      rawInputDelivery,
    ])
    requireSuccessfulLinuxOwnerTerminal(terminalEvidence, statusSinkFailure)
    await context.helper.assertLive()
    const execution = parseLinuxProcessOwnerStatus(
      oneCanonicalStatusLine(status.text()),
      request.operationId,
    )
    const clientIoEvidence = linuxProcessClientIoEvidence({
      inputRequested: request.command.stdin !== undefined,
      requestFailure,
      rawInputFailure,
      control,
      outputSinkFailure,
    })
    const result = Object.freeze({ ...execution, clientIoEvidence })
    emitLinuxProcessOwnerResult(request, result)
    return result
  } finally {
    pipes.stdout.off('data', forwardStdout)
    pipes.stderr.off('data', forwardStderr)
    pipes.status.off('data', collectStatus)
  }
}

async function linuxProcessOwnerProgress(
  ownerLifetime: Promise<RunnerProcessEvidence | undefined>,
  requestDelivery: Promise<unknown>,
  externalTermination: OwnerTerminationObservation,
): Promise<LinuxProcessOwnerProgress> {
  const requestFailed = requestDelivery.then((failure) => failure === undefined
    ? new Promise<never>(() => undefined)
    : Object.freeze({ outcome: 'request-failed' as const, failure }))
  return Promise.race([
    ownerLifetime.then((completed) => Object.freeze({
      outcome: 'owner-lifetime' as const,
      completed,
    })),
    requestFailed,
    externalTermination.requested.then(() => Object.freeze({
      outcome: 'termination-requested' as const,
    })),
  ])
}

async function settleLinuxProcessOwnerProgress(
  progress: LinuxProcessOwnerProgress,
  pipes: LinuxProcessOwnerPipes,
  terminal: Promise<RunnerProcessEvidence>,
): Promise<LinuxProcessControlEvidence> {
  if (progress.outcome === 'request-failed') {
    pipes.request.destroy()
    // EOF transfers the failed request into the sole subreaper's cleanup state
    // machine without manufacturing a client-side tree claim.
    pipes.control.destroy()
    await terminal
    return Object.freeze({ requested: false, failure: undefined })
  }
  const controlRequested = progress.outcome === 'termination-requested' ||
    progress.completed === undefined
  if (!controlRequested) {
    pipes.control.destroy()
    return Object.freeze({ requested: false, failure: undefined })
  }
  // Abort and client lifetime expiry are requests over the inherited control
  // capability. The subreaper remains the sole publisher of tree-empty proof.
  const failure = await requestOwnerSettlement(pipes.control)
  if (failure !== undefined) pipes.control.destroy()
  await terminal
  pipes.control.destroy()
  return Object.freeze({ requested: true, failure })
}

function requireSuccessfulLinuxOwnerTerminal(
  terminal: RunnerProcessEvidence,
  statusSinkFailure: Error | undefined,
): void {
  if (terminal.terminal !== 'exited' || terminal.exitCode !== 0) {
    throw new Error('Linux process owner exited without an authoritative status')
  }
  if (statusSinkFailure !== undefined) {
    throw new Error('Linux process owner status capture failed', { cause: statusSinkFailure })
  }
}

function linuxProcessClientIoEvidence(input: Readonly<{
  inputRequested: boolean
  requestFailure: unknown
  rawInputFailure: unknown
  control: LinuxProcessControlEvidence
  outputSinkFailure: Error | undefined
}>): LinuxProcessClientIoEvidence {
  const ioFailures = [
    input.requestFailure,
    input.rawInputFailure,
    input.control.failure,
    input.outputSinkFailure,
  ].filter((failure) => failure !== undefined)
  return Object.freeze({
    requestOutcome: input.requestFailure === undefined ? 'delivered' : 'failed',
    rawInputOutcome: linuxRawInputOutcome(input.inputRequested, input.rawInputFailure),
    controlOutcome: linuxControlOutcome(input.control),
    outputOutcome: input.outputSinkFailure === undefined ? 'delivered' : 'failed',
    failureCode: ioFailures.length === 0 ? '' : 'CLIENT_IO_FAILED',
    failureMessage: ioFailures.length === 0 ? '' : boundedFailureMessage(ioFailures),
  })
}

function linuxRawInputOutcome(
  inputRequested: boolean,
  failure: unknown,
): LinuxProcessClientIoEvidence['rawInputOutcome'] {
  if (failure !== undefined) return 'failed'
  return inputRequested ? 'delivered' : 'not-requested'
}

function linuxControlOutcome(
  evidence: LinuxProcessControlEvidence,
): LinuxProcessClientIoEvidence['controlOutcome'] {
  if (!evidence.requested) return 'not-requested'
  return evidence.failure === undefined ? 'delivered' : 'failed'
}

function emitLinuxProcessOwnerResult(
  request: LinuxProcessOwnerRequest,
  result: LinuxProcessOwnerExecution,
): void {
  emitTrace(request, result.treeEmpty
    ? 'linux-process-owner-tree-empty'
    : 'linux-process-owner-evidence-failed', {
    operationId: request.operationId,
    timedOut: result.timedOut,
    inventoryScans: result.ownershipEvidence.inventoryScans,
    maximumObservedDescendants: result.ownershipEvidence.maximumObservedDescendants,
    clientIoOutcome: result.clientIoEvidence.failureCode === '' ? 'delivered' : 'failed',
  })
}

async function rethrowAfterLinuxOwnerSettlement(
  owner: ChildProcess,
  terminal: Promise<RunnerProcessEvidence>,
  cause: unknown,
): Promise<never> {
  const settlementFailures: unknown[] = []
  try { owner.stdin?.destroy() } catch (failure) { settlementFailures.push(failure) }
  try { owner.stdio.at(4)?.destroy() } catch (failure) { settlementFailures.push(failure) }
  try { await terminal } catch (failure) { settlementFailures.push(failure) }
  if (settlementFailures.length === 0) throw cause
  throw new AggregateError(
    [cause, ...settlementFailures],
    'Linux process owner client settlement failed',
    { cause },
  )
}

function prepareOwnerProtocol(request: LinuxProcessOwnerRequest): {
  readonly rawChildInput: Buffer
  readonly requestBytes: Buffer
} {
  let rawChildInput = Buffer.alloc(0)
  let requestBytes = Buffer.alloc(0)
  try {
    const command = canonicalCommand(request.command, request.environment)
    rawChildInput = request.command.stdin === undefined
      ? Buffer.alloc(0)
      : Buffer.from(request.command.stdin)
    requestBytes = Buffer.from(JSON.stringify({
      schemaVersion: LINUX_PROCESS_OWNER_REQUEST_SCHEMA_VERSION,
      operationId: request.operationId,
      command,
      deadlineMs: request.deadlineMs,
      terminationGraceMs: request.terminationGraceMs,
    }), 'utf8')
    if (requestBytes.byteLength < 1 || requestBytes.byteLength > MAXIMUM_OWNER_REQUEST_BYTES) {
      throw new Error('Linux process owner request is empty or exceeds its byte limit')
    }
    return Object.freeze({ rawChildInput, requestBytes })
  } catch (cause) {
    rawChildInput.fill(0)
    requestBytes.fill(0)
    throw cause
  } finally {
    request.command.stdin?.fill(0)
  }
}

function requireReadableProtocolPipe(value: unknown, label: string): Readable {
  if (!(value instanceof Readable)) {
    throw new Error(`Linux process owner ${label} descriptor is not a readable pipe`)
  }
  return value
}

function requireWritableProtocolPipe(value: unknown, label: string): Writable {
  if (!(value instanceof Writable)) {
    throw new Error(`Linux process owner ${label} descriptor is not a writable pipe`)
  }
  return value
}

export function deliverAndEraseRawChildInput(
  pipe: Pick<NodeJS.WritableStream, 'once' | 'end'>,
  rawChildInput: Buffer,
): Promise<unknown> {
  return deliverAndEraseOwnerRequest(pipe, rawChildInput)
}

export function deliverAndEraseOwnerRequest(
  pipe: Pick<NodeJS.WritableStream, 'once' | 'end'>,
  bytes: Buffer,
): Promise<unknown> {
  return new Promise((resolveDelivery) => {
    let settled = false
    const settle = (failure: unknown): void => {
      if (settled) return
      settled = true
      bytes.fill(0)
      resolveDelivery(failure)
    }
    try {
      pipe.once('error', settle)
      pipe.once('close', () => settle(new Error('owner pipe closed before delivery completed')))
      pipe.end(bytes, () => settle(undefined))
    } catch (cause) {
      // Writable.end() can fail before either callback is registered by Node.
      // A single settlement path keeps raw credentials erasable on every exit.
      settle(cause)
    }
  })
}

export function requestOwnerSettlement(
  pipe: Pick<NodeJS.WritableStream, 'once' | 'write'>,
): Promise<unknown> {
  const requestByte = Buffer.from([1])
  return new Promise((resolveDelivery) => {
    let settled = false
    const settle = (failure: unknown): void => {
      if (settled) return
      settled = true
      requestByte.fill(0)
      resolveDelivery(failure)
    }
    try {
      pipe.once('error', settle)
      pipe.once('close', () => settle(new Error('owner control pipe closed before delivery completed')))
      pipe.write(requestByte, () => settle(undefined))
    } catch (cause) {
      settle(cause)
    }
  })
}

interface OwnerTerminationObservation {
  readonly requested: Promise<void>
  readonly close: () => void
}

function observeOwnerTermination(signal: AbortSignal | undefined): OwnerTerminationObservation {
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

function canonicalCommand(
  command: LinuxProcessOwnerRequest['command'],
  environment: Readonly<Record<string, string>>,
) {
  const executable = canonicalAbsolutePath(command.executable, 'owned executable')
  const cwd = canonicalAbsolutePath(command.cwd, 'owned working directory')
  if (!Array.isArray(command.arguments) || command.arguments.some((argument) =>
    typeof argument !== 'string' || argument.includes('\0'))) {
    throw new Error('owned command arguments are invalid')
  }
  if (
    command.executableSha256 !== undefined &&
    !SHA256_PATTERN.test(command.executableSha256)
  ) throw new Error('owned executable digest is invalid')
  if ((command.executableSha256 === undefined) !== (command.executableByteLength === undefined)) {
    throw new Error('owned executable digest and byte length must appear together')
  }
  if (
    command.executableByteLength !== undefined &&
    (!Number.isSafeInteger(command.executableByteLength) || command.executableByteLength < 1)
  ) throw new Error('owned executable byte length is invalid')
  if (
    command.stdin !== undefined &&
    (command.stdin.byteLength === 0 || command.stdin.byteLength > MAXIMUM_COMMAND_STDIN_BYTES)
  ) throw new Error('owned command stdin is empty or exceeds its byte limit')
  if ((command.stdin === undefined) !== (command.stdinAuthority === undefined)) {
    throw new Error('owned command stdin bytes and nonsecret authority must appear together')
  }
  const stdin = command.stdin === undefined
    ? null
    : Object.freeze({
        descriptor: 5,
        byteLength: command.stdin.byteLength,
        channelId: requirePortableToken(command.stdinAuthority?.channelId, 'stdin channel ID'),
        runId: requirePortableToken(command.stdinAuthority?.runId, 'stdin run ID'),
        profileId: requirePortableToken(command.stdinAuthority?.profileId, 'stdin profile ID'),
        attemptId: requirePortableToken(command.stdinAuthority?.attemptId, 'stdin attempt ID'),
      })
  return Object.freeze({
    executable,
    executableSha256: command.executableSha256 ?? null,
    executableByteLength: command.executableByteLength ?? null,
    arguments: Object.freeze([...command.arguments]),
    cwd,
    environment: canonicalEnvironment(environment),
    stdin,
  })
}

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

function parseLinuxProcessInputEvidence(value: unknown): LinuxProcessInputEvidence {
  const input = requireRecord(value, 'Linux input evidence')
  exactKeys(input, ['outcome', 'failureCode', 'failureMessage'], 'Linux input evidence')
  const evidence = Object.freeze({
    outcome: requireEnum(input.outcome, [
      'not-started', 'not-requested', 'delivered', 'failed',
    ] as const, 'Linux input outcome'),
    failureCode: requireOptionalPortableToken(input.failureCode, 'Linux input failure code'),
    failureMessage: requireString(input.failureMessage, 'Linux input failure message', 512),
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
      32,
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
      512,
    ),
  })
}

function validateLinuxProcessOwnerStatus(
  status: ParsedLinuxProcessOwnerStatus,
): void {
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
      ownership.quietInventoryCount !== 2 ||
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
      errorMessage: requireString(record.errorMessage, 'Linux owner error message', 512),
    })
  }
  throw new Error('Linux root process terminal is invalid')
}

interface HeldLinuxProcessOwnerArtifact extends LinuxProcessOwnerArtifact {
  readonly handle: FileHandle
  readonly assertLive: () => Promise<void>
  readonly close: () => Promise<void>
}

export async function holdLinuxProcessOwnerArtifact(
  artifact: LinuxProcessOwnerArtifact,
  label: string,
): Promise<HeldLinuxProcessOwnerArtifact> {
  if (!SHA256_PATTERN.test(artifact.sha256)) throw new Error(`${label} digest is invalid`)
  if (
    !Number.isSafeInteger(artifact.byteLength) || artifact.byteLength < 1 ||
    artifact.byteLength > MAXIMUM_HELPER_BYTES
  ) throw new Error(`${label} byte length is invalid`)
  const path = canonicalAbsolutePath(artifact.path, label)
  const canonical = await realpath(path)
  if (canonical !== path) throw new Error(`${label} path is not its canonical real path`)
  const before = await lstat(path, { bigint: true })
  requireHeldArtifactMetadata(before, artifact.byteLength, label)
  const handle = await open(path, constants.O_RDONLY | constants.O_NOFOLLOW)
  let closed = false
  try {
    const held = await handle.stat({ bigint: true })
    if (!sameArtifactRevision(before, held)) throw new Error(`${label} changed while opened`)
    await authenticateHeldArtifact(handle, artifact, label)
    const assertLive = async (): Promise<void> => {
      if (closed) throw new Error(`${label} authority is already closed`)
      const [named, opened] = await Promise.all([
        lstat(path, { bigint: true }),
        handle.stat({ bigint: true }),
      ])
      requireHeldArtifactMetadata(named, artifact.byteLength, label)
      if (!sameArtifactRevision(before, named) || !sameArtifactRevision(before, opened)) {
        throw new Error(`${label} changed while held`)
      }
      await authenticateHeldArtifact(handle, artifact, label)
    }
    return Object.freeze({
      path,
      byteLength: artifact.byteLength,
      sha256: artifact.sha256,
      handle,
      assertLive,
      async close() {
        if (closed) return
        closed = true
        await handle.close()
      },
    })
  } catch (cause) {
    await handle.close().catch(() => undefined)
    throw cause
  }
}

async function authenticateHeldArtifact(
  handle: FileHandle,
  artifact: LinuxProcessOwnerArtifact,
  label: string,
): Promise<void> {
  const digest = createHash('sha256')
  const buffer = Buffer.allocUnsafe(64 * 1024)
  let offset = 0
  try {
    while (offset < artifact.byteLength) {
      const expected = Math.min(buffer.byteLength, artifact.byteLength - offset)
      const { bytesRead } = await handle.read(buffer, 0, expected, offset)
      if (bytesRead < 1) throw new Error(`${label} ended before its exact byte length`)
      digest.update(buffer.subarray(0, bytesRead))
      offset += bytesRead
    }
    const extra = Buffer.alloc(1)
    try {
      const { bytesRead } = await handle.read(extra, 0, 1, artifact.byteLength)
      if (bytesRead !== 0) throw new Error(`${label} exceeds its exact byte length`)
    } finally {
      extra.fill(0)
    }
  } finally {
    buffer.fill(0)
  }
  if (digest.digest('hex') !== artifact.sha256) {
    throw new Error(`${label} digest differs from its authority`)
  }
}

function requireHeldArtifactMetadata(
  metadata: BigIntStats,
  byteLength: number,
  label: string,
): void {
  if (
    !metadata.isFile() || metadata.isSymbolicLink() ||
    metadata.size !== BigInt(byteLength)
  ) throw new Error(`${label} is not an exact bounded regular file`)
}

function sameArtifactRevision(
  left: BigIntStats,
  right: BigIntStats,
): boolean {
  return left.dev === right.dev && left.ino === right.ino && left.size === right.size &&
    left.mtimeNs === right.mtimeNs && left.ctimeNs === right.ctimeNs && left.mode === right.mode
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

function childTerminal(child: ChildProcess): Promise<RunnerProcessEvidence> {
  return new Promise((resolveTerminal) => {
    let settled = false
    const settle = (evidence: RunnerProcessEvidence) => {
      if (settled) return
      settled = true
      resolveTerminal(Object.freeze(evidence))
    }
    child.once('error', (cause: NodeJS.ErrnoException) => settle({
      terminal: 'spawn-failed',
      errorCode: cause.code ?? 'UNKNOWN',
      errorMessage: cause.message,
    }))
    child.once('close', (code, signal) => settle(code === null
      ? { terminal: 'signaled', signal: signal ?? 'UNKNOWN' }
      : { terminal: 'exited', exitCode: code }))
  })
}

function boundedCollector(label: string, maximumBytes: number) {
  const chunks: Buffer[] = []
  let byteLength = 0
  return Object.freeze({
    write(chunk: Uint8Array) {
      byteLength += chunk.byteLength
      if (byteLength > maximumBytes) throw new Error(`${label} exceeds its byte limit`)
      chunks.push(Buffer.from(chunk))
    },
    text() {
      try {
        return new TextDecoder('utf-8', { fatal: true }).decode(Buffer.concat(chunks))
      } catch (cause) {
        throw new Error(`${label} is not valid UTF-8`, { cause })
      }
    },
  })
}

function nonAuthoritativeSink(
  sink: (chunk: Uint8Array) => void,
  recordFailure: (failure: Error) => void,
): (chunk: Uint8Array) => void {
  let failed = false
  return (chunk) => {
    try {
      if (!failed) sink(chunk)
    } catch (cause) {
      failed = true
      recordFailure(cause instanceof Error ? cause : new Error(String(cause)))
    } finally {
      chunk.fill(0)
    }
  }
}

function canonicalEnvironment(value: Readonly<Record<string, string>>): Readonly<Record<string, string>> {
  const record = requireRecord(value, 'owned process environment')
  const result: Record<string, string> = {}
  for (const name of Object.keys(record).sort()) {
    const entry = record[name]
    if (
      name === '' || name.includes('=') || name.includes('\0') ||
      typeof entry !== 'string' || entry.includes('\0')
    ) throw new Error('owned process environment contains an invalid entry')
    result[name] = entry
  }
  return Object.freeze(result)
}

function canonicalAbsolutePath(value: unknown, label: string): string {
  if (typeof value !== 'string' || !isAbsolute(value) || resolve(value) !== value) {
    throw new Error(`${label} must be absolute and canonical`)
  }
  return value
}

function requireRecord(value: unknown, label: string): Record<string, unknown> {
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

function requireBoundedPositiveInteger(value: unknown, maximum: number, label: string): number {
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

function requirePortableToken(value: unknown, label: string): string {
  const result = requireString(value, label, 256)
  if (!/^[A-Za-z0-9._-]+$/u.test(result)) throw new Error(`${label} is not portable`)
  return result
}

function requireOptionalPortableToken(value: unknown, label: string): string {
  const result = requireString(value, label, 256)
  if (result !== '' && !/^[A-Za-z0-9._-]+$/u.test(result)) {
    throw new Error(`${label} is not portable`)
  }
  return result
}

function canonicalUint64(value: string): boolean {
  if (!/^[1-9]\d*$/u.test(value)) return false
  try {
    const parsed = BigInt(value)
    return parsed <= 0xffff_ffff_ffff_ffffn && parsed.toString(10) === value
  } catch {
    return false
  }
}

function boundedFailureMessage(failures: readonly unknown[]): string {
  const joined = failures.map((failure) =>
    failure instanceof Error ? failure.message : String(failure)).join('; ')
  let result = ''
  for (const character of joined) {
    if (Buffer.byteLength(result + character, 'utf8') > 512) break
    result += character
  }
  return result === '' ? 'client I/O failed' : result
}

function requireEnum<const T extends string>(
  value: unknown,
  values: readonly T[],
  label: string,
): T {
  if (!values.includes(value as T)) throw new Error(`${label} is invalid`)
  return value as T
}

function requireOperationId(value: string): void {
  if (!/^[A-Za-z0-9._-]{1,256}$/u.test(value)) throw new Error('Linux operation ID is invalid')
}

async function boundedWait<T>(promise: Promise<T>, maximumWaitMs: number): Promise<T | undefined> {
  let timer: ReturnType<typeof setTimeout> | undefined
  const timeout = new Promise<undefined>((resolveTimeout) => {
    timer = setTimeout(() => resolveTimeout(undefined), maximumWaitMs)
    timer.ref()
  })
  try {
    return await Promise.race([promise, timeout])
  } finally {
    if (timer !== undefined) clearTimeout(timer)
  }
}

function emitTrace(
  request: LinuxProcessOwnerRequest,
  milestone: string,
  context: Readonly<Record<string, unknown>>,
): void {
  try {
    request.trace?.(Object.freeze({ milestone, context: Object.freeze(context) }))
  } catch {
    // Trace delivery cannot interrupt the helper's descendant cleanup lease.
  }
}

import { spawn } from 'node:child_process'
import { lstat } from 'node:fs/promises'
import { isAbsolute, resolve } from 'node:path'
import { performance } from 'node:perf_hooks'

import {
  encodeExistingDirectoryPublisherRequest,
  parseExistingDirectoryPublisherResponse,
  PUBLISHER_HELPER_MAXIMUM_MESSAGE_BYTES,
  type ExistingDirectoryPublisherRequest,
  type ExistingDirectoryPublisherResponse,
  type ExistingDirectoryPublisherResponseFor,
} from '../../browser-network-matrix/cli/publisher-helper-protocol.ts'
import {
  assertGuardExecutionWindowUsable,
  type GuardExecutionWindow,
} from '../execution/guard-execution-lease.ts'
import {
  GuardUploadDirectoryPublisherUnsettledError,
  type GuardUploadDirectoryPublisher,
} from '../artifact/directory-publisher.ts'

const MAXIMUM_PUBLISHER_INVOCATION_MS = 30_000
const MAXIMUM_PUBLISHER_TERMINATION_RESERVE_MS = 2_000

export interface NativeDirectoryPublisherFixture {
  readonly path: string
}

export function createNativeDirectoryPublisher(
  fixture: NativeDirectoryPublisherFixture,
): GuardUploadDirectoryPublisher {
  const ownedFixture = Object.freeze({ ...fixture })
  return Object.freeze({
    invoke: <Request extends ExistingDirectoryPublisherRequest>(
      request: Request,
      executionWindow: GuardExecutionWindow,
    ) => invokeNativeDirectoryPublisher(ownedFixture, request, executionWindow),
  })
}

export async function invokeNativeDirectoryPublisher<
  Request extends ExistingDirectoryPublisherRequest,
>(
  fixture: NativeDirectoryPublisherFixture,
  request: Request,
  window: GuardExecutionWindow,
): Promise<ExistingDirectoryPublisherResponseFor<Request['operation']>> {
  requireExecutionWindow(window)
  window.signal.throwIfAborted()
  const startedAt = performance.now()
  const invocationDeadline = AbortSignal.timeout(window.maximumDurationMs)
  const invocationSignal = AbortSignal.any([window.signal, invocationDeadline])
  let helperPath: string
  try {
    helperPath = await publisherExecutablePath(fixture, invocationSignal)
  } catch (cause) {
    if (invocationDeadline.aborted && !window.signal.aborted) {
      throw new Error('native publisher invocation deadline exceeded', { cause })
    }
    throw cause
  }
  let primaryFailure: unknown
  let terminal: NativePublisherTerminal | undefined
  try {
    const remainingDurationMs = Math.max(
      1,
      Math.floor(window.maximumDurationMs - (performance.now() - startedAt)),
    )
    terminal = await executePublisher(
      helperPath,
      encodeExistingDirectoryPublisherRequest(request),
      Object.freeze({ signal: invocationSignal, maximumDurationMs: remainingDurationMs }),
    )
  } catch (cause) {
    primaryFailure = cause
  }
  if (primaryFailure !== undefined) throw primaryFailure
  return parseTerminal(request.operation, terminal as NativePublisherTerminal) as
    ExistingDirectoryPublisherResponseFor<Request['operation']>
}

interface NativePublisherTerminal {
  readonly exitCode: number
  readonly stdout: Uint8Array
  readonly stderr: Uint8Array
}

async function executePublisher(
  path: string,
  request: Uint8Array,
  window: GuardExecutionWindow,
): Promise<NativePublisherTerminal> {
  window.signal.throwIfAborted()
  const child = spawn(path, [], {
    shell: false,
    windowsHide: true,
    stdio: ['pipe', 'pipe', 'pipe'],
    env: Object.freeze({}),
  })
  const stdout: Buffer[] = []
  const stderr: Buffer[] = []
  let stdoutBytes = 0
  let stderrBytes = 0
  let processFailure: Error | undefined
  let deadlineExceeded = false
  let terminationSettlementExceeded = false
  let terminationTimer: NodeJS.Timeout | undefined
  const terminationReserveMs = Math.min(
    MAXIMUM_PUBLISHER_TERMINATION_RESERVE_MS,
    Math.max(1, Math.floor(window.maximumDurationMs / 4)),
  )
  const executionDurationMs = Math.max(1, window.maximumDurationMs - terminationReserveMs)
  const operationDeadline = AbortSignal.timeout(executionDurationMs)
  const terminate = (failure: Error): void => {
    processFailure ??= failure
    child.kill('SIGKILL')
  }
  const append = (chunks: Buffer[], chunk: Buffer, channel: 'stdout' | 'stderr'): void => {
    if (processFailure !== undefined) return
    if (channel === 'stdout') stdoutBytes += chunk.byteLength
    else stderrBytes += chunk.byteLength
    if (stdoutBytes > PUBLISHER_HELPER_MAXIMUM_MESSAGE_BYTES ||
        stderrBytes > PUBLISHER_HELPER_MAXIMUM_MESSAGE_BYTES) {
      terminate(new Error(`native publisher ${channel} exceeds its byte authority`))
      return
    }
    chunks.push(Buffer.from(chunk))
  }
  child.stdout.on('data', (chunk: Buffer) => append(stdout, chunk, 'stdout'))
  child.stderr.on('data', (chunk: Buffer) => append(stderr, chunk, 'stderr'))
  const terminal = await new Promise<{ readonly exitCode: number | null; readonly signal: string | null }>(
    (settle, reject) => {
      let settled = false
      const finish = (
        action: typeof settle | typeof reject,
        value: { readonly exitCode: number | null; readonly signal: string | null } | Error,
      ): void => {
        if (settled) return
        settled = true
        if (terminationTimer !== undefined) clearTimeout(terminationTimer)
        window.signal.removeEventListener('abort', abort)
        operationDeadline.removeEventListener('abort', abort)
        action(value as never)
      }
      const awaitForcedTermination = (failure: Error): void => {
        terminate(failure)
        terminationTimer ??= setTimeout(() => {
          terminationSettlementExceeded = true
          child.stdin.destroy()
          child.stdout.destroy()
          child.stderr.destroy()
          // Returning here would let recovery or cleanup race a process that
          // still owns the publication request. Only the outer workflow may
          // terminate this guard process if the OS never reports child close.
        }, terminationReserveMs)
      }
      const abort = (): void => {
        deadlineExceeded = true
        awaitForcedTermination(new Error('native publisher process deadline exceeded'))
      }
      window.signal.addEventListener('abort', abort, { once: true })
      operationDeadline.addEventListener('abort', abort, { once: true })
      child.once('error', (cause) => {
        const failure = cause instanceof Error ? cause : new Error(String(cause))
        if (child.pid === undefined) finish(reject, failure)
        else awaitForcedTermination(failure)
      })
      child.once('close', (exitCode, signal) => {
        if (terminationSettlementExceeded && processFailure !== undefined) {
          finish(reject, new GuardUploadDirectoryPublisherUnsettledError(processFailure))
        } else if (processFailure !== undefined) finish(reject, processFailure)
        else finish(settle, { exitCode, signal })
      })
      child.stdin.once('error', (cause) => awaitForcedTermination(
        cause instanceof Error ? cause : new Error(String(cause)),
      ))
      child.stdin.end(request)
    },
  )
  if (deadlineExceeded) throw new Error('native publisher process deadline exceeded')
  if (terminal.exitCode === null || terminal.signal !== null) {
    throw new Error('native publisher process did not exit normally')
  }
  return Object.freeze({
    exitCode: terminal.exitCode,
    stdout: Uint8Array.from(Buffer.concat(stdout, stdoutBytes)),
    stderr: Uint8Array.from(Buffer.concat(stderr, stderrBytes)),
  })
}

function parseTerminal(
  operation: ExistingDirectoryPublisherRequest['operation'],
  terminal: NativePublisherTerminal,
): ExistingDirectoryPublisherResponse {
  if (terminal.exitCode === 0) {
    if (terminal.stderr.byteLength !== 0) {
      throw new Error('successful native publisher wrote failure-channel bytes')
    }
    const response = parseExistingDirectoryPublisherResponse(terminal.stdout, operation)
    if (response.outcome !== 'completed') {
      throw new Error('successful native publisher returned a failed response')
    }
    return response
  }
  if (terminal.stdout.byteLength !== 0) {
    throw new Error('failed native publisher wrote success-channel bytes')
  }
  const response = parseExistingDirectoryPublisherResponse(terminal.stderr, operation)
  if (response.outcome !== 'failed') {
    throw new Error('failed native publisher returned a completed response')
  }
  const expectedExitCode = response.failureCode === 'destination-exists' ? 3 : 2
  if (terminal.exitCode !== expectedExitCode) {
    throw new Error('native publisher exit code contradicts its failure response')
  }
  return response
}

async function publisherExecutablePath(
  fixture: NativeDirectoryPublisherFixture,
  signal: AbortSignal,
): Promise<string> {
  signal.throwIfAborted()
  if (Object.keys(fixture).length !== 1 || !isAbsolute(fixture.path) ||
      resolve(fixture.path) !== fixture.path || fixture.path.includes('\0')) {
    throw new Error('native publisher fixture path is not canonical')
  }
  const metadata = await lstat(fixture.path)
  signal.throwIfAborted()
  if (!metadata.isFile() || metadata.isSymbolicLink() || metadata.size < 1) {
    throw new Error('native publisher fixture is not a non-empty regular file')
  }
  if (process.platform !== 'win32' && (metadata.mode & 0o111) === 0) {
    throw new Error('native publisher executable lacks an execute bit')
  }
  return fixture.path
}

function requireExecutionWindow(window: GuardExecutionWindow): void {
  assertGuardExecutionWindowUsable(window)
  if (!(window.signal instanceof AbortSignal) ||
      !Number.isSafeInteger(window.maximumDurationMs) ||
      window.maximumDurationMs < 1 ||
      window.maximumDurationMs > MAXIMUM_PUBLISHER_INVOCATION_MS) {
    throw new Error('native publisher execution window exceeds its frozen authority')
  }
}

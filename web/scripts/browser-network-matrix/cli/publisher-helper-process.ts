import { createHash, randomBytes } from 'node:crypto'
import { spawn } from 'node:child_process'
import type { BigIntStats } from 'node:fs'
import { lstat, open, type FileHandle } from 'node:fs/promises'
import { isAbsolute, resolve } from 'node:path'
import { finished } from 'node:stream/promises'

import {
  executeWindowsJob,
  type WindowsJobExecutionOptions,
} from '../../browser-evidence/process/windows-job-client.ts'
import {
  openHelperBuildManifestAuthority,
  type AuthenticatedHelperBuildManifest,
  type HelperBuildManifestAuthority,
} from './helper-build-manifest.ts'
import { PUBLISHER_HELPER_MAXIMUM_MESSAGE_BYTES } from './publisher-helper-protocol.ts'

const MAXIMUM_EXECUTABLE_BYTES = 64 * 1024 * 1024
const EXECUTABLE_HASH_CHUNK_BYTES = 64 * 1024
const DEFAULT_LAUNCH_DEADLINE_MS = 5_000
const DEFAULT_RESPONSE_DEADLINE_MS = 60_000
const DEFAULT_REAP_DEADLINE_MS = 5_000

export interface PublisherHelperProcessDeadlines {
  readonly launchMs: number
  readonly responseMs: number
  readonly reapMs: number
}

export interface PublisherHelperProcessResult {
  readonly exitCode: number
  readonly stdout: Uint8Array
  readonly stderr: Uint8Array
}

export interface PublisherHelperProcessAuthorityOptions {
  readonly helperManifestPath: string
  readonly publisherHelperPath: string
  readonly windowsJobHelperPath?: string
  readonly deadlines?: Partial<PublisherHelperProcessDeadlines>
  readonly platform?: NodeJS.Platform
  readonly executeWindowsJob?: (options: WindowsJobExecutionOptions) =>
    ReturnType<typeof executeWindowsJob>
}

export interface PublisherHelperProcessAuthority {
  execute(request: Uint8Array): Promise<PublisherHelperProcessResult>
  close(): Promise<void>
}

export async function openPublisherHelperProcessAuthority(
  options: PublisherHelperProcessAuthorityOptions,
): Promise<PublisherHelperProcessAuthority> {
  const platform = options.platform ?? process.platform
  if (platform !== 'linux' && platform !== 'win32') {
    throw new Error(`browser network matrix publisher is unsupported on ${platform}`)
  }
  const deadlines = normalizeDeadlines(options.deadlines)
  const manifest = await openHelperBuildManifestAuthority(options.helperManifestPath, platform)
  let publisher: HeldExecutable | undefined
  let windowsJob: HeldExecutable | undefined
  try {
    const bindings = requirePlatformHelperBindings(options, manifest.manifest, platform)
    publisher = await HeldExecutable.open(
      bindings.publisher.path,
      'publisher helper',
      platform,
      bindings.publisher.sha256,
    )
    if (bindings.windowsJob !== undefined) {
      windowsJob = await HeldExecutable.open(
        bindings.windowsJob.path,
        'Windows Job helper',
        platform,
        bindings.windowsJob.sha256,
      )
    }
    await Promise.all([
      manifest.assertUnchanged(),
      publisher.assertUnchanged(),
      windowsJob?.assertUnchanged(),
    ])
    return new ProcessAuthority(
      platform,
      manifest,
      publisher,
      windowsJob,
      deadlines,
      options.executeWindowsJob ?? executeWindowsJob,
    )
  } catch (cause) {
    await Promise.allSettled([
      manifest.close(),
      ...(publisher === undefined ? [] : [publisher.close()]),
      ...(windowsJob === undefined ? [] : [windowsJob.close()]),
    ])
    throw cause
  }
}

interface AuthenticatedHelperBinding {
  readonly path: string
  readonly sha256: string
}

function requirePlatformHelperBindings(
  options: PublisherHelperProcessAuthorityOptions,
  manifest: AuthenticatedHelperBuildManifest,
  platform: 'linux' | 'win32',
): Readonly<{
  publisher: AuthenticatedHelperBinding
  windowsJob?: AuthenticatedHelperBinding
}> {
  const publisherEntry = manifest.helpers.find(({ role }) => role === 'artifact-publisher')
  if (publisherEntry === undefined) {
    throw new Error('helper manifest does not authorize an artifact publisher')
  }
  requireExactManifestPath(options.publisherHelperPath, publisherEntry.path, 'publisher helper')
  const publisher = Object.freeze({
    path: options.publisherHelperPath,
    sha256: publisherEntry.sha256,
  })
  if (platform === 'linux') {
    if (options.windowsJobHelperPath !== undefined) {
      throw new Error('Linux must not receive a Windows Job helper path')
    }
    return Object.freeze({ publisher })
  }

  const windowsJobEntry = manifest.helpers.find(({ role }) => role === 'windows-job')
  if (windowsJobEntry === undefined) {
    throw new Error('helper manifest does not authorize a Windows Job helper')
  }
  const windowsJobHelperPath = options.windowsJobHelperPath
  if (windowsJobHelperPath === undefined) {
    throw new Error('Windows requires an explicit Windows Job helper path')
  }
  requireExactManifestPath(windowsJobHelperPath, windowsJobEntry.path, 'Windows Job helper')
  return Object.freeze({
    publisher,
    windowsJob: Object.freeze({
      path: windowsJobHelperPath,
      sha256: windowsJobEntry.sha256,
    }),
  })
}

class ProcessAuthority implements PublisherHelperProcessAuthority {
  readonly #platform: 'linux' | 'win32'
  readonly #manifest: HelperBuildManifestAuthority
  readonly #publisher: HeldExecutable
  readonly #windowsJob: HeldExecutable | undefined
  readonly #deadlines: PublisherHelperProcessDeadlines
  readonly #executeWindowsJob: (options: WindowsJobExecutionOptions) =>
    ReturnType<typeof executeWindowsJob>
  #active = false
  #closed = false

  constructor(
    platform: 'linux' | 'win32',
    manifest: HelperBuildManifestAuthority,
    publisher: HeldExecutable,
    windowsJob: HeldExecutable | undefined,
    deadlines: PublisherHelperProcessDeadlines,
    executeWindowsJobFunction: (options: WindowsJobExecutionOptions) =>
      ReturnType<typeof executeWindowsJob>,
  ) {
    this.#platform = platform
    this.#manifest = manifest
    this.#publisher = publisher
    this.#windowsJob = windowsJob
    this.#deadlines = deadlines
    this.#executeWindowsJob = executeWindowsJobFunction
  }

  async execute(request: Uint8Array): Promise<PublisherHelperProcessResult> {
    if (this.#closed) throw new Error('browser network matrix publisher authority is closed')
    if (this.#active) throw new Error('browser network matrix publisher authority is already active')
    if (request.byteLength < 1 || request.byteLength > PUBLISHER_HELPER_MAXIMUM_MESSAGE_BYTES) {
      throw new Error('browser network matrix publisher request exceeds its byte authority')
    }
    this.#active = true
    try {
      await Promise.all([
        this.#manifest.assertUnchanged(),
        this.#publisher.assertUnchanged(),
        this.#windowsJob?.assertUnchanged(),
      ])
      let result: PublisherHelperProcessResult | undefined
      let primaryFailure: unknown
      let failed = false
      try {
        result = this.#platform === 'linux'
          ? await executeLinux(this.#publisher, request, this.#deadlines)
          : await this.#executeWindows(request)
      } catch (cause) {
        primaryFailure = cause
        failed = true
      }
      try {
        await Promise.all([
          this.#manifest.assertUnchanged(),
          this.#publisher.assertUnchanged(),
          this.#windowsJob?.assertUnchanged(),
        ])
      } catch (identityFailure) {
        if (failed) {
          throw new AggregateError(
            [primaryFailure, identityFailure],
            'publisher process and executable revalidation both failed',
            { cause: identityFailure },
          )
        }
        throw identityFailure
      }
      if (failed) throw primaryFailure
      return result as PublisherHelperProcessResult
    } finally {
      this.#active = false
    }
  }

  async close(): Promise<void> {
    if (this.#closed) return
    if (this.#active) throw new Error('cannot close an active browser network matrix publisher')
    this.#closed = true
    const failures = await Promise.allSettled([
      this.#manifest.close(),
      this.#publisher.close(),
      ...(this.#windowsJob === undefined ? [] : [this.#windowsJob.close()]),
    ])
    const errors = failures
      .filter((failure): failure is PromiseRejectedResult => failure.status === 'rejected')
      .map(({ reason }) => reason)
    if (errors.length > 0) throw new AggregateError(errors, 'close publisher executable authorities')
  }

  async #executeWindows(request: Uint8Array): Promise<PublisherHelperProcessResult> {
    const windowsJob = this.#windowsJob
    if (windowsJob === undefined) throw new Error('Windows Job helper authority is absent')
    const output = new BoundedOutput()
    // The local threat model excludes a malicious same-account process replacing
    // trusted build output in the final CreateProcess instant. Held pre/post checks
    // still make every observed supervisor swap fail closed, while the supervised
    // publisher target is SHA-bound and natively locked by the Job helper itself.
    const execution = await this.#executeWindowsJob({
      helperPath: windowsJob.path,
      operationId: `network-matrix-publish-${randomBytes(8).toString('hex')}`,
      command: {
        executable: this.#publisher.path,
        arguments: Object.freeze([]),
        stdin: Uint8Array.from(request),
        executableSha256: this.#publisher.sha256,
      },
      // Publication is byte authority, not an ambient-process capability. The
      // native helper needs no host environment and must not receive its secrets.
      inheritedEnvironment: Object.freeze({}),
      injectedEnvironment: Object.freeze({}),
      deadlineMs: this.#deadlines.responseMs,
      terminationGraceMs: this.#deadlines.reapMs,
      stdout: (chunk) => output.appendStdout(chunk),
      stderr: (chunk) => output.appendStderr(chunk),
    })
    output.requireWithinAuthority()
    if (execution.timedOut) throw new Error('publisher helper response deadline exceeded')
    if (execution.processEvidence.terminal !== 'exited') {
      throw new Error('publisher helper failed to launch inside its Windows Job')
    }
    return output.result(execution.processEvidence.exitCode)
  }
}

class HeldExecutable {
  readonly path: string
  readonly handle: FileHandle
  readonly identity: BigIntStats
  readonly sha256: string
  readonly #label: string
  readonly #platform: 'linux' | 'win32'

  private constructor(
    path: string,
    handle: FileHandle,
    identity: BigIntStats,
    sha256: string,
    label: string,
    platform: 'linux' | 'win32',
  ) {
    this.path = path
    this.handle = handle
    this.identity = identity
    this.sha256 = sha256
    this.#label = label
    this.#platform = platform
  }

  static async open(
    pathValue: string,
    label: string,
    platform: 'linux' | 'win32',
    expectedSHA256: string,
  ): Promise<HeldExecutable> {
    if (!isAbsolute(pathValue) || resolve(pathValue) !== pathValue || pathValue.includes('\0')) {
      throw new Error(`${label} path must be explicit, absolute, and canonical`)
    }
    const named = await lstat(pathValue, { bigint: true })
    requireExecutable(named, label, platform)
    const handle = await open(pathValue, 'r')
    try {
      const opened = await handle.stat({ bigint: true })
      requireExecutable(opened, label, platform)
      if (!sameIdentity(named, opened) || !sameRevision(named, opened)) {
        throw new Error(`${label} changed while its executable authority was opened`)
      }
      const digest = await digestHeldExecutable(handle, opened, label)
      if (digest !== expectedSHA256) throw new Error(`${label} bytes differ from the held helper manifest`)
      return new HeldExecutable(pathValue, handle, opened, digest, label, platform)
    } catch (cause) {
      await handle.close().catch(() => undefined)
      throw cause
    }
  }

  async assertUnchanged(): Promise<void> {
    const [named, opened] = await Promise.all([
      lstat(this.path, { bigint: true }),
      this.handle.stat({ bigint: true }),
    ])
    requireExecutable(named, this.#label, this.#platform)
    requireExecutable(opened, this.#label, this.#platform)
    if (
      !sameIdentity(this.identity, named) || !sameIdentity(named, opened) ||
      !sameRevision(this.identity, named) || !sameRevision(named, opened)
    ) throw new Error(`${this.#label} identity or revision changed`)
    if (await digestHeldExecutable(this.handle, opened, this.#label) !== this.sha256) {
      throw new Error(`${this.#label} bytes changed`)
    }
  }

  close(): Promise<void> {
    return this.handle.close()
  }
}

function requireExactManifestPath(provided: string, authenticated: string, label: string): void {
  if (provided !== authenticated) {
    throw new Error(`${label} explicit path differs from the held helper manifest`)
  }
}

async function executeLinux(
  executable: HeldExecutable,
  request: Uint8Array,
  deadlines: PublisherHelperProcessDeadlines,
): Promise<PublisherHelperProcessResult> {
  const child = spawn('/proc/self/fd/3', [], {
    shell: false,
    detached: true,
    // Publication is a byte protocol and has no ambient-process capability.
    // An explicit empty map prevents CI tokens and caller secrets from crossing it.
    env: Object.freeze({}),
    windowsHide: true,
    stdio: ['pipe', 'pipe', 'pipe', executable.handle.fd],
  })
  const input = child.stdin
  const stdout = child.stdout
  const stderr = child.stderr
  if (input === null || stdout === null || stderr === null) {
    child.kill('SIGKILL')
    throw new Error('publisher helper did not expose its authenticated pipe set')
  }
  const output = new BoundedOutput()
  stdout.on('data', (chunk: Buffer) => output.appendStdout(chunk))
  stderr.on('data', (chunk: Buffer) => output.appendStderr(chunk))
  const terminal = new Promise<{ code: number | null; signal: NodeJS.Signals | null }>((resolveClose) => {
    child.once('close', (code, signal) => resolveClose({ code, signal }))
  })
  try {
    await withDeadline(
      new Promise<void>((resolveSpawn, rejectSpawn) => {
        child.once('spawn', resolveSpawn)
        child.once('error', rejectSpawn)
      }),
      deadlines.launchMs,
      'publisher helper launch',
    )
    // The response lease starts before request delivery so a child that exits or
    // stops reading cannot retain the authority indefinitely through pipe backpressure.
    const settled = await withDeadline((async () => {
      await writePublisherHelperRequestAndClose(input, request)
      return terminal
    })(), deadlines.responseMs, 'publisher helper response')
    output.requireWithinAuthority()
    if (settled.code === null || settled.signal !== null) {
      throw new Error('publisher helper did not exit normally')
    }
    return output.result(settled.code)
  } catch (cause) {
    await terminateLinuxProcessGroupAndReap(child.pid, terminal, deadlines.reapMs)
    throw cause
  }
}

async function terminateLinuxProcessGroupAndReap(
  pid: number | undefined,
  terminal: Promise<unknown>,
  reapDeadlineMs: number,
): Promise<void> {
  if (pid !== undefined) {
    try {
      process.kill(-pid, 'SIGKILL')
    } catch (cause) {
      if (!isErrno(cause, 'ESRCH')) throw cause
    }
  }
  await withDeadline(terminal, reapDeadlineMs, 'publisher helper reap')
}

export async function writePublisherHelperRequestAndClose(
  stream: NodeJS.WritableStream,
  bytes: Uint8Array,
): Promise<void> {
  stream.end(Buffer.from(bytes))
  // `finished` observes both a complete flush and late EPIPE/close failures. An
  // `end` callback alone can resolve after only accepting bytes into user-space.
  await finished(stream, { cleanup: true })
}

class BoundedOutput {
  readonly #stdout: Buffer[] = []
  readonly #stderr: Buffer[] = []
  #byteLength = 0
  #overflow = false

  appendStdout(chunk: Uint8Array): void {
    this.#append(this.#stdout, chunk)
  }

  appendStderr(chunk: Uint8Array): void {
    this.#append(this.#stderr, chunk)
  }

  requireWithinAuthority(): void {
    if (this.#overflow) throw new Error('publisher helper output exceeded its byte authority')
  }

  result(exitCode: number): PublisherHelperProcessResult {
    this.requireWithinAuthority()
    return Object.freeze({
      exitCode,
      stdout: Uint8Array.from(Buffer.concat(this.#stdout)),
      stderr: Uint8Array.from(Buffer.concat(this.#stderr)),
    })
  }

  #append(destination: Buffer[], chunk: Uint8Array): void {
    const remaining = PUBLISHER_HELPER_MAXIMUM_MESSAGE_BYTES - this.#byteLength
    if (remaining <= 0) {
      this.#overflow = true
      return
    }
    const accepted = Buffer.from(chunk).subarray(0, remaining)
    destination.push(Buffer.from(accepted))
    this.#byteLength += accepted.byteLength
    if (accepted.byteLength !== chunk.byteLength) this.#overflow = true
  }
}

async function digestHeldExecutable(
  handle: FileHandle,
  metadata: BigIntStats,
  label: string,
): Promise<string> {
  if (metadata.size < 1n || metadata.size > BigInt(MAXIMUM_EXECUTABLE_BYTES)) {
    throw new Error(`${label} size is outside the executable authority`)
  }
  const digest = createHash('sha256')
  let observed = 0
  const expectedBytes = Number(metadata.size)
  const chunk = Buffer.allocUnsafe(Math.min(EXECUTABLE_HASH_CHUNK_BYTES, expectedBytes))
  while (observed < expectedBytes) {
    const requested = Math.min(chunk.byteLength, expectedBytes - observed)
    const { bytesRead } = await handle.read(chunk, 0, requested, observed)
    if (bytesRead < 1) throw new Error(`${label} ended while its bytes were authenticated`)
    digest.update(chunk.subarray(0, bytesRead))
    observed += bytesRead
  }
  const after = await handle.stat({ bigint: true })
  if (!sameIdentity(metadata, after) || !sameRevision(metadata, after) || observed !== Number(after.size)) {
    throw new Error(`${label} changed while its bytes were authenticated`)
  }
  return digest.digest('hex')
}

function requireExecutable(
  metadata: BigIntStats,
  label: string,
  platform: 'linux' | 'win32',
): void {
  if (!metadata.isFile() || metadata.isSymbolicLink() || metadata.ino === 0n) {
    throw new Error(`${label} must be a regular file with native identity`)
  }
  if (platform === 'linux' && (metadata.mode & 0o111n) === 0n) {
    throw new Error(`${label} is not executable`)
  }
}

function sameIdentity(left: BigIntStats, right: BigIntStats): boolean {
  return left.dev === right.dev && left.ino === right.ino
}

function sameRevision(left: BigIntStats, right: BigIntStats): boolean {
  return left.size === right.size && left.mtimeNs === right.mtimeNs && left.ctimeNs === right.ctimeNs
}

function normalizeDeadlines(
  values: Partial<PublisherHelperProcessDeadlines> | undefined,
): PublisherHelperProcessDeadlines {
  const deadlines = Object.freeze({
    launchMs: values?.launchMs ?? DEFAULT_LAUNCH_DEADLINE_MS,
    responseMs: values?.responseMs ?? DEFAULT_RESPONSE_DEADLINE_MS,
    reapMs: values?.reapMs ?? DEFAULT_REAP_DEADLINE_MS,
  })
  for (const [name, value] of Object.entries(deadlines)) {
    if (!Number.isSafeInteger(value) || value < 1 || value > 300_000) {
      throw new Error(`publisher helper ${name} deadline is outside the safe range`)
    }
  }
  return deadlines
}

async function withDeadline<T>(operation: Promise<T>, milliseconds: number, label: string): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined
  try {
    return await Promise.race([
      operation,
      new Promise<never>((_, reject) => {
        timer = setTimeout(() => reject(new Error(`${label} deadline exceeded`)), milliseconds)
        timer.ref()
      }),
    ])
  } finally {
    if (timer !== undefined) clearTimeout(timer)
  }
}

function isErrno(cause: unknown, code: string): cause is NodeJS.ErrnoException {
  return cause instanceof Error && 'code' in cause && cause.code === code
}

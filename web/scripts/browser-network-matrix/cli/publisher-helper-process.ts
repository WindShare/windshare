import { randomBytes } from 'node:crypto'
import type { BigIntStats } from 'node:fs'
import { lstat, open, type FileHandle } from 'node:fs/promises'
import { dirname, isAbsolute, resolve } from 'node:path'
import { finished } from 'node:stream/promises'

import {
  executeTestProcessOwner,
  type ExecuteTestProcessOwnerOptions,
  type TestProcessOwnerArtifact,
} from '../../browser-evidence/process/test-process-owner-client.mjs'
import {
  openHelperBuildManifest,
  type HelperBuildManifest,
  type HelperBuildManifestHandle,
} from './helper-build-manifest.ts'
import { PUBLISHER_HELPER_MAXIMUM_MESSAGE_BYTES } from './publisher-helper-protocol.ts'

const MAXIMUM_EXECUTABLE_BYTES = 64 * 1024 * 1024
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
  readonly processOwnerPath: string
  readonly deadlines?: Partial<PublisherHelperProcessDeadlines>
  readonly platform?: NodeJS.Platform
  readonly executeProcessOwner?: (options: ExecuteTestProcessOwnerOptions) =>
    ReturnType<typeof executeTestProcessOwner>
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
  const manifest = await openHelperBuildManifest(options.helperManifestPath, platform)
  let publisher: HeldExecutable | undefined
  let processOwner: HeldExecutable | undefined
  try {
    const bindings = requirePlatformHelperBindings(options, manifest.manifest)
    publisher = await HeldExecutable.open(
      bindings.publisher.path,
      'publisher helper',
      platform,
    )
    processOwner = await HeldExecutable.open(
      bindings.processOwner.path,
      'test process owner',
      platform,
    )
    await Promise.all([
      manifest.assertUnchanged(),
      publisher.assertUnchanged(),
      processOwner.assertUnchanged(),
    ])
    return new ProcessAuthority(
      platform,
      manifest,
      publisher,
      processOwner,
      deadlines,
      options.executeProcessOwner ?? executeTestProcessOwner,
    )
  } catch (cause) {
    await Promise.allSettled([
      manifest.close(),
      ...(publisher === undefined ? [] : [publisher.close()]),
      ...(processOwner === undefined ? [] : [processOwner.close()]),
    ])
    throw cause
  }
}

interface HelperBinding {
  readonly path: string
}

function requirePlatformHelperBindings(
  options: PublisherHelperProcessAuthorityOptions,
  manifest: HelperBuildManifest,
): Readonly<{
  publisher: HelperBinding
  processOwner: HelperBinding
}> {
  const publisherEntry = manifest.helpers.find(({ role }) => role === 'artifact-publisher')
  if (publisherEntry === undefined) {
    throw new Error('helper manifest does not describe an artifact publisher')
  }
  requireExactManifestPath(options.publisherHelperPath, publisherEntry.path, 'publisher helper')
  const publisher = Object.freeze({
    path: options.publisherHelperPath,
  })
  const processOwnerEntry = manifest.helpers.find(({ role }) => role === 'test-process-owner')
  if (processOwnerEntry === undefined) {
    throw new Error('helper manifest does not describe a test process owner')
  }
  requireExactManifestPath(options.processOwnerPath, processOwnerEntry.path, 'test process owner')
  return Object.freeze({
    publisher,
    processOwner: Object.freeze({
      path: options.processOwnerPath,
    }),
  })
}

class ProcessAuthority implements PublisherHelperProcessAuthority {
  readonly #platform: 'linux' | 'win32'
  readonly #manifest: HelperBuildManifestHandle
  readonly #publisher: HeldExecutable
  readonly #processOwner: HeldExecutable
  readonly #deadlines: PublisherHelperProcessDeadlines
  readonly #executeProcessOwner: (options: ExecuteTestProcessOwnerOptions) =>
    ReturnType<typeof executeTestProcessOwner>
  #active = false
  #closed = false

  constructor(
    platform: 'linux' | 'win32',
    manifest: HelperBuildManifestHandle,
    publisher: HeldExecutable,
    processOwner: HeldExecutable,
    deadlines: PublisherHelperProcessDeadlines,
    executeProcessOwner: (options: ExecuteTestProcessOwnerOptions) =>
      ReturnType<typeof executeTestProcessOwner>,
  ) {
    this.#platform = platform
    this.#manifest = manifest
    this.#publisher = publisher
    this.#processOwner = processOwner
    this.#deadlines = deadlines
    this.#executeProcessOwner = executeProcessOwner
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
        this.#processOwner.assertUnchanged(),
      ])
      let result: PublisherHelperProcessResult | undefined
      let primaryFailure: unknown
      let failed = false
      try {
        result = await this.#executeOwned(request)
      } catch (cause) {
        primaryFailure = cause
        failed = true
      }
      try {
        await Promise.all([
          this.#manifest.assertUnchanged(),
          this.#publisher.assertUnchanged(),
          this.#processOwner.assertUnchanged(),
        ])
      } catch (identityFailure) {
        if (failed) {
          throw new AggregateError(
            [primaryFailure, identityFailure],
            'publisher process and helper-file revalidation both failed',
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
      this.#processOwner.close(),
    ])
    const errors = failures
      .filter((failure): failure is PromiseRejectedResult => failure.status === 'rejected')
      .map(({ reason }) => reason)
    if (errors.length > 0) throw new AggregateError(errors, 'close publisher helper files')
  }

  async #executeOwned(request: Uint8Array): Promise<PublisherHelperProcessResult> {
    const output = new BoundedOutput()
    const execution = await this.#executeProcessOwner({
      owner: this.#processOwner.artifact(),
      runId: 'network-matrix-publisher',
      operationId: `network-matrix-publish-${randomBytes(8).toString('hex')}`,
      scenario: 'network-matrix-publication',
      command: {
        executable: this.#publisher.path,
        arguments: Object.freeze([]),
        cwd: dirname(this.#publisher.path),
        stdin: Uint8Array.from(request),
      },
      environment: Object.freeze({}),
      deadlineMs: this.#deadlines.responseMs,
      terminationGraceMs: this.#deadlines.reapMs,
      platform: this.#platform,
      capture: Object.freeze({
        stdoutBytes: PUBLISHER_HELPER_MAXIMUM_MESSAGE_BYTES,
        stderrBytes: PUBLISHER_HELPER_MAXIMUM_MESSAGE_BYTES,
      }),
    })
    output.appendStdout(execution.output.stdout.bytes())
    output.appendStderr(execution.output.stderr.bytes())
    output.requireWithinAuthority()
    if (execution.ownershipEvidence.terminationReason === 'deadline') {
      throw new Error('publisher helper response deadline exceeded')
    }
    if (!execution.treeEmpty || execution.cleanupOutcome !== 'completed') {
      throw new Error('publisher helper process tree did not settle cleanly')
    }
    if (execution.inputEvidence.outcome !== 'delivered') {
      throw new Error('publisher helper request was not delivered exactly once')
    }
    if (execution.processEvidence.terminal !== 'exited') {
      throw new Error('publisher helper failed to launch inside its process owner')
    }
    return output.result(execution.processEvidence.exitCode ?? 1)
  }
}

class HeldExecutable {
  readonly path: string
  readonly handle: FileHandle
  readonly identity: BigIntStats
  readonly #label: string
  readonly #platform: 'linux' | 'win32'

  private constructor(
    path: string,
    handle: FileHandle,
    identity: BigIntStats,
    label: string,
    platform: 'linux' | 'win32',
  ) {
    this.path = path
    this.handle = handle
    this.identity = identity
    this.#label = label
    this.#platform = platform
  }

  static async open(
    pathValue: string,
    label: string,
    platform: 'linux' | 'win32',
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
        throw new Error(`${label} changed while it was opened`)
      }
      return new HeldExecutable(pathValue, handle, opened, label, platform)
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
  }

  artifact(): TestProcessOwnerArtifact {
    return Object.freeze({
      path: this.path,
    })
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

function requireExecutable(
  metadata: BigIntStats,
  label: string,
  platform: 'linux' | 'win32',
): void {
  if (
    !metadata.isFile() || metadata.isSymbolicLink() || metadata.ino === 0n ||
    metadata.size < 1n || metadata.size > BigInt(MAXIMUM_EXECUTABLE_BYTES)
  ) {
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

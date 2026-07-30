import { execFile } from 'node:child_process'
import { mkdir, mkdtemp, readFile, rm, stat, writeFile } from 'node:fs/promises'
import { connect, createServer, type Server, type Socket } from 'node:net'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'

import { ManagedProcess, settleCleanupTasks } from './managed-process'
import type { SenderAttemptEvidenceSnapshot } from './hot-switch-evidence'
import {
  openWindowsStableRunner,
  stableWindowsE2EDirectory,
  type BinaryPaths,
  type WindowsStableRunner,
} from './windows-stable-runner'

const execFileAsync = promisify(execFile)
const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')
const BUILD_TIMEOUT_MILLISECONDS = 180_000
const MAXIMUM_BUILD_OUTPUT_BYTES = 1_000_000
const E2E_BLOCK_BYTES = 64 * 1024
const MAXIMUM_SENDER_EVIDENCE_BYTES = 1_000_000
const SHA256_PATTERN = /^[0-9a-f]{64}$/u
const REAL_STACK_READY_TIMEOUT_MILLISECONDS = 30_000
const REAL_STACK_STOP_TIMEOUT_MILLISECONDS = 10_000

const binaryCleanup = new WeakMap<BinaryPaths, () => Promise<void>>()
const binaryRunners = new WeakMap<BinaryPaths, WindowsStableRunner>()

export interface SplitShare {
  readonly bareLink: string
  readonly key: string
  readonly senderEvidencePath: string
}

export interface FixtureOperationBounds {
  readonly signal?: AbortSignal
  readonly timeoutMilliseconds?: number
}

interface LockedTestIceTopology {
  readonly profilePath: string
  readonly resolutionPath: string
  readonly lock: {
    readonly profileSha256: string
    readonly resolutionSha256: string
  }
}

export class RelayProxy {
  readonly url: string
  readonly #server: Server
  readonly #connections = new Set<readonly [Socket, Socket]>()
  #accepting = true
  #stopAcceptingTask: Promise<void> | undefined

  private constructor(server: Server, url: string) {
    this.#server = server
    this.url = url
  }

  static async start(upstreamUrl: string, signal?: AbortSignal): Promise<RelayProxy> {
    signal?.throwIfAborted()
    const upstream = new URL(upstreamUrl)
    const port = Number(upstream.port)
    if (upstream.protocol !== 'ws:' || upstream.hostname === '' || !Number.isInteger(port)) {
      throw new TypeError('Relay proxy requires an explicit ws:// host and port')
    }
    const holder: { value?: RelayProxy } = {}
    const server = createServer((client) => {
      holder.value?.forward(client, upstream.hostname, port)
    })
    const listening = new Promise<void>((resolveListen, rejectListen) => {
      server.once('error', rejectListen)
      server.listen(0, '127.0.0.1', () => {
        server.off('error', rejectListen)
        resolveListen()
      })
    })
    await awaitWithAbort(listening, signal, () => closeServerLocally(server))
    const address = server.address()
    if (address === null || typeof address === 'string') {
      server.close()
      throw new Error('Relay proxy did not expose a TCP address')
    }
    const proxy = new RelayProxy(server, `ws://127.0.0.1:${address.port}`)
    holder.value = proxy
    return proxy
  }

  get accepting(): boolean {
    return this.#accepting
  }

  async cutAndWait(
    receiverRelayIneligible: () => Promise<unknown>,
    signal?: AbortSignal,
  ): Promise<{
    readonly proxyAccepting: false
    readonly receiverRelayEligible: false
  }> {
    const stopAccepting = this.#stopAccepting()
    this.#destroyConnections()
    // The receiver seal starts only after every proxy path is physically closed,
    // so a zero-lane observation cannot precede a buffered replacement admission.
    const proxyResult = await Promise.allSettled([
      awaitWithAbort(stopAccepting, signal, () => {
        this.#destroyConnections()
        closeServerLocally(this.#server)
      }),
    ])
    let receiverResult: readonly PromiseSettledResult<unknown>[] = []
    if (signal?.aborted !== true) {
      let receiverTask: Promise<unknown>
      try {
        // Invocation is the admission point. Keeping the check and invocation in
        // one turn prevents an abort microtask from starting receiver work late.
        receiverTask = Promise.resolve(receiverRelayIneligible())
      } catch (error) {
        receiverTask = Promise.reject(error)
      }
      receiverResult = await Promise.allSettled([awaitWithAbort(receiverTask, signal)])
    }
    const failures = [...new Set([...proxyResult, ...receiverResult].flatMap((result) =>
      result.status === 'rejected' ? [result.reason] : [],
    ))]
    if (failures.length === 1) throw failures[0]
    if (failures.length > 1) throw new AggregateError(failures, 'Relay cut fence failed')
    return Object.freeze({
      proxyAccepting: false as const,
      receiverRelayEligible: false as const,
    })
  }

  async close(signal?: AbortSignal): Promise<void> {
    await this.cutAndWait(() => Promise.resolve(), signal)
  }

  private forward(client: Socket, host: string, port: number): void {
    if (!this.#accepting) {
      client.destroy()
      return
    }
    const upstream = connect({ host, port })
    const pair = [client, upstream] as const
    this.#connections.add(pair)
    let settled = false
    const settle = () => {
      if (settled) return
      settled = true
      this.#connections.delete(pair)
      client.destroy()
      upstream.destroy()
    }
    client.on('error', settle).on('close', settle)
    upstream.on('error', settle).on('close', settle)
    client.pipe(upstream)
    upstream.pipe(client)
  }

  #stopAccepting(): Promise<void> {
    if (this.#stopAcceptingTask !== undefined) return this.#stopAcceptingTask
    this.#accepting = false
    this.#stopAcceptingTask = new Promise<void>((resolveClose, rejectClose) => {
      if (!this.#server.listening) {
        resolveClose()
        return
      }
      this.#server.close((error) => {
        if (error === undefined) resolveClose()
        else rejectClose(error)
      })
    })
    return this.#stopAcceptingTask
  }

  #destroyConnections(): void {
    for (const [client, upstream] of this.#connections) {
      client.destroy()
      upstream.destroy()
    }
  }
}

export async function acquireRealStackBinaries(signal?: AbortSignal): Promise<BinaryPaths> {
  signal?.throwIfAborted()
  const stableDirectory = stableWindowsE2EDirectory(
    process.platform,
    process.env.WINDSHARE_WINDOWS_OS_NETWORK,
    process.env.WINDSHARE_D5_E2E_LEASE_TOKEN,
  )
  if (stableDirectory !== undefined) {
    const runner = await openWindowsStableRunner(stableDirectory, signal)
    if (signal?.aborted === true) {
      runner.close()
      signal.throwIfAborted()
    }
    binaryRunners.set(runner.paths, runner)
    binaryCleanup.set(runner.paths, async () => runner.close())
    return runner.paths
  }

  const directory = await mkdtemp(join(tmpdir(), 'windshare-r7-browser-'))
  const paths = Object.freeze({
    directory,
    windshare: join(directory, executableName('windshare-e2e')),
    relay: join(directory, executableName('wsrelay')),
  })
  const results = await Promise.allSettled([
    build(paths.windshare, './cmd/windshare/e2e', signal),
    build(paths.relay, './relay/cmd/wsrelay', signal),
  ])
  const failures = results.flatMap((result) => result.status === 'rejected' ? [result.reason] : [])
  if (failures.length > 0) {
    await rm(directory, { recursive: true, force: true }).catch((error) => failures.push(error))
    throw failures.length === 1
      ? failures[0]
      : new AggregateError(failures, 'Real-stack binary build failed')
  }
  if (signal?.aborted === true) {
    await rm(directory, { recursive: true, force: true })
    signal.throwIfAborted()
  }
  binaryCleanup.set(paths, () => rm(directory, { recursive: true, force: true }))
  return paths
}

export async function releaseRealStackBinaries(paths: BinaryPaths): Promise<void> {
  const cleanup = binaryCleanup.get(paths)
  if (cleanup === undefined) throw new Error('Real-stack binaries are not owned by this worker')
  await cleanup()
  // Retain ownership across a transient close/rm failure so a bounded caller or
  // later fixture teardown can retry instead of orphaning the binary directory.
  binaryCleanup.delete(paths)
}

export class V2RealStack {
  readonly #binaries: BinaryPaths
  readonly #topologyProfilePath: string
  readonly #topologyResolutionPath: string
  readonly #topologyProfileSha256: string
  readonly #topologyResolutionSha256: string
  readonly #runner: WindowsStableRunner | undefined
  readonly #processes: ManagedProcess[] = []
  readonly #temporaryDirectories: string[] = []
  readonly #proxies: RelayProxy[] = []
  relayUrl = ''

  constructor(binaries: BinaryPaths, topology: LockedTestIceTopology) {
    this.#binaries = binaries
    requireAbsolutePath(topology.profilePath, 'Test ICE topology profile')
    requireAbsolutePath(topology.resolutionPath, 'Test ICE topology resolution')
    requireSha256(topology.lock.profileSha256, 'Test ICE topology profile')
    requireSha256(topology.lock.resolutionSha256, 'Test ICE topology resolution')
    this.#topologyProfilePath = topology.profilePath
    this.#topologyResolutionPath = topology.resolutionPath
    this.#topologyProfileSha256 = topology.lock.profileSha256
    this.#topologyResolutionSha256 = topology.lock.resolutionSha256
    this.#runner = binaryRunners.get(binaries)
    if (process.platform === 'win32' && this.#runner === undefined) {
      throw new Error('Windows real-stack execution requires the D5 stable runner')
    }
  }

  async start(bounds: FixtureOperationBounds = {}): Promise<void> {
    await this.#runner?.assertBeforeLaunch(bounds.signal)
    bounds.signal?.throwIfAborted()
    const stateDirectory = await mkdtemp(join(tmpdir(), 'windshare-r7-relay-state-'))
    this.#temporaryDirectories.push(stateDirectory)
    bounds.signal?.throwIfAborted()
    const relay = this.track(new ManagedProcess(
      this.#binaries.relay,
      ['-listen', '127.0.0.1:0', '-state-dir', stateDirectory],
    ))
    const ready = await relay.waitFor(
      'stderr',
      /wsrelay: listening on ([^\s]+)/u,
      operationTimeout(bounds, REAL_STACK_READY_TIMEOUT_MILLISECONDS),
      bounds.signal,
    )
    const address = requiredCapture(ready, 1, 'relay address')
    this.relayUrl = `ws://${address}`
  }

  async createRelayProxy(bounds: FixtureOperationBounds = {}): Promise<RelayProxy> {
    const proxy = await RelayProxy.start(this.relayUrl, bounds.signal)
    this.#proxies.push(proxy)
    return proxy
  }

  async createFile(
    name: string,
    data: Uint8Array,
    bounds: FixtureOperationBounds = {},
  ): Promise<string> {
    if (name.length === 0 || name.includes('/') || name.includes('\\')) {
      throw new TypeError('Real-stack file name must be one path segment')
    }
    const directory = await mkdtemp(join(tmpdir(), 'windshare-r7-share-'))
    this.#temporaryDirectories.push(directory)
    bounds.signal?.throwIfAborted()
    const filePath = join(directory, name)
    await writeFile(filePath, data, { signal: bounds.signal })
    return filePath
  }

  async share(
    filePath: string,
    frontUrl: string,
    bounds: FixtureOperationBounds = {},
  ): Promise<SplitShare> {
    await this.#runner?.assertBeforeLaunch(bounds.signal)
    bounds.signal?.throwIfAborted()
    const evidenceDirectory = await mkdtemp(join(tmpdir(), 'windshare-sender-evidence-'))
    this.#temporaryDirectories.push(evidenceDirectory)
    bounds.signal?.throwIfAborted()
    const senderEvidencePath = join(evidenceDirectory, 'attempts.jsonl')
    const sender = this.track(new ManagedProcess(this.#binaries.windshare, [
      '--test-ice-topology',
      this.#topologyProfilePath,
      '--test-ice-topology-resolution',
      this.#topologyResolutionPath,
      '--test-ice-topology-profile-sha256',
      this.#topologyProfileSha256,
      '--test-ice-topology-resolution-sha256',
      this.#topologyResolutionSha256,
      '--sender-evidence',
      senderEvidencePath,
      'share',
      filePath,
      '--relay',
      this.relayUrl,
      '--front-url',
      frontUrl,
      '--block-size',
      String(E2E_BLOCK_BYTES),
      '--split-key',
    ], { redactStdout: true }))
    const readinessTimeout = operationTimeout(bounds, REAL_STACK_READY_TIMEOUT_MILLISECONDS)
    const bare = await sender.waitFor(
      'stdout',
      /^Bare link: (.+)$/mu,
      readinessTimeout,
      bounds.signal,
    )
    const key = await sender.waitFor('stdout', /^Key: (.+)$/mu, readinessTimeout, bounds.signal)
    const split = Object.freeze({
      bareLink: requiredCapture(bare, 1, 'bare share link'),
      key: requiredCapture(key, 1, 'separate key'),
      senderEvidencePath,
    })
    sender.forgetCapturedStdout()
    return split
  }

  async dispose(bounds: FixtureOperationBounds = {}): Promise<void> {
    const failures: unknown[] = []
    const phases = [
      this.#proxies.map((proxy) => () => proxy.close(bounds.signal)),
      [...this.#processes].reverse().map((process) => () => process.stopAndDrain(
        REAL_STACK_STOP_TIMEOUT_MILLISECONDS,
      )),
      this.#temporaryDirectories.map((directory) => () => (
        rm(directory, { recursive: true, force: true })
      )),
    ]
    for (const operations of phases) {
      try {
        await settleCleanupTasks(
          operations.map((operation) => operation()),
          'Real-stack fixture',
        )
      } catch (error) {
        failures.push(error)
      }
    }
    if (failures.length === 1) throw failures[0]
    if (failures.length > 1) throw new AggregateError(failures, 'Real-stack cleanup failed')
  }

  private track(child: ManagedProcess): ManagedProcess {
    // Local ownership must precede the fallible external guard registration.
    // A guard-loss race kills the child and throws, but dispose can still drain it.
    this.#processes.push(child)
    this.#runner?.track(child)
    return child
  }
}

export async function readSenderAttemptEvidenceSnapshot(
  path: string,
  signal?: AbortSignal,
): Promise<SenderAttemptEvidenceSnapshot> {
  signal?.throwIfAborted()
  const metadata = await stat(path)
  signal?.throwIfAborted()
  if (!metadata.isFile()) throw new Error('Sender attempt evidence is not a regular file')
  if (metadata.size > MAXIMUM_SENDER_EVIDENCE_BYTES) {
    throw new Error('Sender attempt evidence exceeds the fixture byte limit')
  }
  const data = await readFile(path, { signal })
  if (data.byteLength > MAXIMUM_SENDER_EVIDENCE_BYTES) {
    throw new Error('Sender attempt evidence grew beyond the fixture byte limit while being read')
  }
  const encoded = data.toString('utf8')
  if (encoded === '') {
    return Object.freeze({
      records: Object.freeze([]),
      hasUnterminatedFinalRecord: false,
    })
  }
  const hasUnterminatedFinalRecord = !encoded.endsWith('\n')
  const lastTerminator = encoded.lastIndexOf('\n')
  const completedPrefix = lastTerminator < 0 ? '' : encoded.slice(0, lastTerminator + 1)
  const lines = completedPrefix === '' ? [] : completedPrefix.slice(0, -1).split('\n')
  const records = lines.map((line, index) => {
    if (line === '') throw new Error(`Sender attempt evidence line ${index + 1} is empty`)
    try {
      return JSON.parse(line) as unknown
    } catch (cause) {
      throw new Error(`Sender attempt evidence line ${index + 1} is invalid JSON`, { cause })
    }
  })
  return Object.freeze({
    records: Object.freeze(records),
    hasUnterminatedFinalRecord,
  })
}

export function replaceRelayHint(link: string, relayUrl: string): string {
  const parsed = new URL(link)
  parsed.searchParams.delete('r')
  parsed.searchParams.append('r', relayUrl)
  return parsed.toString()
}

async function build(
  output: string,
  packagePath: string,
  signal?: AbortSignal,
): Promise<void> {
  signal?.throwIfAborted()
  await mkdir(dirname(output), { recursive: true })
  signal?.throwIfAborted()
  await execFileAsync('go', ['build', '-o', output, packagePath], {
    cwd: REPOSITORY_ROOT,
    env: { ...process.env, GOWORK: 'auto' },
    timeout: BUILD_TIMEOUT_MILLISECONDS,
    signal,
    windowsHide: true,
    maxBuffer: MAXIMUM_BUILD_OUTPUT_BYTES,
  })
}

function operationTimeout(bounds: FixtureOperationBounds, defaultMilliseconds: number): number {
  const timeout = bounds.timeoutMilliseconds ?? defaultMilliseconds
  if (!Number.isSafeInteger(timeout) || timeout <= 0) {
    throw new RangeError('Real-stack operation timeout must be a positive integer')
  }
  return Math.min(timeout, defaultMilliseconds)
}

function awaitWithAbort<T>(
  task: Promise<T>,
  signal?: AbortSignal,
  abortOwner: () => void = () => undefined,
): Promise<T> {
  if (signal === undefined) return task
  return new Promise<T>((resolveTask, rejectTask) => {
    let settled = false
    const finish = (settle: () => void) => {
      if (settled) return
      settled = true
      signal.removeEventListener('abort', aborted)
      settle()
    }
    const aborted = () => {
      abortOwner()
      finish(() => rejectTask(signal.reason ?? new Error('Real-stack operation was aborted')))
    }
    signal.addEventListener('abort', aborted, { once: true })
    task.then(
      (value) => finish(() => resolveTask(value)),
      (error: unknown) => finish(() => rejectTask(error)),
    )
    if (signal.aborted) aborted()
  })
}

function closeServerLocally(server: Server): void {
  try {
    server.close()
  } catch {
    // A listen request can be accepted by Node before `listening` becomes true.
    // Closing on that transition prevents an aborted proxy acquisition leaking it.
    server.once('listening', () => server.close())
  }
}

function executableName(name: string): string {
  return process.platform === 'win32' ? `${name}.exe` : name
}

function requiredCapture(match: RegExpMatchArray, index: number, label: string): string {
  const value = match[index]
  if (value === undefined || value.length === 0) {
    throw new Error(`${label} readiness output did not contain a value`)
  }
  return value
}

function requireAbsolutePath(path: string, label: string): void {
  if (resolve(path) !== path) throw new Error(`${label} path must be absolute and canonical`)
}

function requireSha256(digest: string, label: string): void {
  if (!SHA256_PATTERN.test(digest)) throw new Error(`${label} digest must be lowercase SHA-256`)
}

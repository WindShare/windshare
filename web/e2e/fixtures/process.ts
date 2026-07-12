import { createHash } from 'node:crypto'
import { EventEmitter } from 'node:events'
import { execFile, spawn, type ChildProcessWithoutNullStreams } from 'node:child_process'
import {
  mkdir,
  mkdtemp,
  rm,
  writeFile,
} from 'node:fs/promises'
import { connect, createServer, type Server, type Socket } from 'node:net'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'

import {
  openWindowsStableRunner,
  stableWindowsE2EDirectory,
  type BinaryPaths,
  type WindowsStableRunner,
} from './windows-stable-runner'

export type { BinaryPaths } from './windows-stable-runner'

const execFileAsync = promisify(execFile)
const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')
const PROCESS_READY_TIMEOUT_MS = 30_000
const PROCESS_STOP_TIMEOUT_MS = 10_000
const BUILD_TIMEOUT_MS = 180_000
export const E2E_BLOCK_SIZE = 64 * 1024
const MAX_CAPTURED_OUTPUT_CHARACTERS = 1_000_000

const binaryCleanup = new WeakMap<BinaryPaths, () => Promise<void>>()
const binaryWindowsRunners = new WeakMap<BinaryPaths, WindowsStableRunner>()

export interface SharedTree {
  readonly root: string
  readonly files: ReadonlyMap<string, Uint8Array>
  readonly directories: readonly string[]
}

export interface ShareLink {
  readonly process: ManagedProcess
  readonly link?: string
  readonly bareLink?: string
  readonly key?: string
}

export interface ManagedProcessOptions {
  readonly redactDiagnostics?: boolean
}

interface ProcessOutcome {
  readonly code: number | null
  readonly signal: NodeJS.Signals | null
}

export class RelayProxy {
  readonly url: string
  readonly #server: Server
  readonly #connections = new Set<readonly [Socket, Socket]>()

  private constructor(server: Server, url: string) {
    this.#server = server
    this.url = url
  }

  static async start(upstreamUrl: string): Promise<RelayProxy> {
    const upstream = new URL(upstreamUrl)
    const port = Number(upstream.port)
    if (upstream.protocol !== 'ws:' || upstream.hostname === '' || !Number.isInteger(port)) {
      throw new Error(`Cannot proxy invalid relay URL ${upstreamUrl}`)
    }
    const holder: { value: RelayProxy | undefined } = { value: undefined }
    const server = createServer((client) => {
      if (holder.value !== undefined) holder.value.#forward(client, upstream.hostname, port)
    })
    await new Promise<void>((resolveListen, rejectListen) => {
      server.once('error', rejectListen)
      server.listen(0, '127.0.0.1', () => {
        server.off('error', rejectListen)
        resolveListen()
      })
    })
    const address = server.address()
    if (address === null || typeof address === 'string') {
      server.close()
      throw new Error('Relay proxy did not expose a TCP listen address')
    }
    holder.value = new RelayProxy(server, `ws://127.0.0.1:${address.port}`)
    return holder.value
  }

  cutConnections(): void {
    for (const [client, upstream] of this.#connections) {
      client.destroy()
      upstream.destroy()
    }
  }

  async close(): Promise<void> {
    this.cutConnections()
    await new Promise<void>((resolveClose, rejectClose) => this.#server.close((error) => {
      if (error === undefined) resolveClose()
      else rejectClose(error)
    }))
  }

  #forward(client: Socket, host: string, port: number): void {
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
}

function requiredCapture(match: RegExpMatchArray, index: number, label: string): string {
  const value = match[index]
  if (value === undefined || value === '') {
    throw new Error(`${label} readiness output did not include the expected value`)
  }
  return value
}

function executableName(name: string): string {
  return process.platform === 'win32' ? `${name}.exe` : name
}

async function build(
  output: string,
  packagePath: string,
  race: boolean,
): Promise<void> {
  const args = ['build']
  if (race) args.push('-race')
  args.push('-o', output, packagePath)
  await execFileAsync('go', args, {
    cwd: REPOSITORY_ROOT,
    env: { ...process.env, GOWORK: 'auto' },
    timeout: BUILD_TIMEOUT_MS,
    windowsHide: true,
    maxBuffer: MAX_CAPTURED_OUTPUT_CHARACTERS,
  })
}

export async function buildE2EBinaries(): Promise<BinaryPaths> {
  const stableDirectory = stableWindowsE2EDirectory(
    process.platform,
    process.env.WINDSHARE_WINDOWS_OS_NETWORK,
    process.env.WINDSHARE_D5_E2E_LEASE_TOKEN,
  )
  if (stableDirectory !== undefined) {
    const runner = await openWindowsStableRunner(stableDirectory)
    const paths = runner.paths
    binaryWindowsRunners.set(paths, runner)
    binaryCleanup.set(paths, async () => runner.close())
    return paths
  }
  const directory = await mkdtemp(join(tmpdir(), 'windshare-c5-'))
  const windshare = join(directory, executableName('windshare'))
  const relay = join(directory, executableName('wsrelay'))
  const hostileSender = join(directory, executableName('hostile-sender'))
  const results = await Promise.allSettled([
    build(windshare, './cmd/windshare', false),
    build(relay, './relay/cmd/wsrelay', false),
    build(hostileSender, './web/e2e/fixtures/hostile-sender', false),
  ])
  const failures = results.flatMap((result) =>
    result.status === 'rejected' ? [result.reason] : [],
  )
  if (failures.length > 0) {
    // All builders must release their output handles before the shared directory is
    // removed. Otherwise one failed build can race cleanup with a surviving sibling.
    try {
      await rm(directory, { recursive: true, force: true })
    } catch (cleanupFailure) {
      failures.push(cleanupFailure)
    }
    if (failures.length === 1) throw failures[0]
    throw new AggregateError(failures, 'E2E binary build or cleanup failed')
  }
  const paths = Object.freeze({ directory, windshare, relay, hostileSender })
  binaryCleanup.set(paths, () => rm(directory, { recursive: true, force: true }))
  return paths
}

export async function removeE2EBinaries(paths: BinaryPaths): Promise<void> {
  const cleanup = binaryCleanup.get(paths)
  if (cleanup === undefined) {
    throw new Error('E2E binary paths are not owned by this fixture process')
  }
  binaryCleanup.delete(paths)
  await cleanup()
}

function boundedAppend(current: string, chunk: Buffer): string {
  const next = current + chunk.toString('utf8')
  return next.length <= MAX_CAPTURED_OUTPUT_CHARACTERS
    ? next
    : next.slice(next.length - MAX_CAPTURED_OUTPUT_CHARACTERS)
}

export async function settleCleanupTasks(
  tasks: readonly Promise<unknown>[],
  boundary: string,
): Promise<void> {
  const results = await Promise.allSettled(tasks)
  const failures = results.flatMap((result) =>
    result.status === 'rejected' ? [result.reason] : [],
  )
  if (failures.length === 1) throw failures[0]
  if (failures.length > 1) {
    throw new AggregateError(failures, `${boundary} cleanup failed`)
  }
}

export class ManagedProcess {
  readonly #child: ChildProcessWithoutNullStreams
  readonly #events = new EventEmitter()
  #stdout = ''
  #stderr = ''
  #spawnFailure: unknown
  #outcome: ProcessOutcome | undefined
  #prematureOutcome: ProcessOutcome | undefined
  #stopRequested = false
  readonly #exit: Promise<ProcessOutcome>
  readonly #redactDiagnostics: boolean

  constructor(
    command: string,
    args: readonly string[],
    options: ManagedProcessOptions = {},
  ) {
    this.#redactDiagnostics = options.redactDiagnostics === true
    this.#child = spawn(command, args, {
      cwd: REPOSITORY_ROOT,
      windowsHide: true,
      stdio: ['pipe', 'pipe', 'pipe'],
    })
    this.#child.stdin.end()
    this.#child.stdout.on('data', (chunk: Buffer) => {
      this.#stdout = boundedAppend(this.#stdout, chunk)
      this.#events.emit('output')
    })
    this.#child.stderr.on('data', (chunk: Buffer) => {
      this.#stderr = boundedAppend(this.#stderr, chunk)
      this.#events.emit('output')
    })
    this.#exit = new Promise((resolveExit) => {
      this.#child.once('error', (error) => {
        this.#spawnFailure = error
        const outcome = { code: null, signal: null }
        this.#outcome = outcome
        if (!this.#stopRequested) this.#prematureOutcome = outcome
        this.#events.emit('output')
        resolveExit(outcome)
      })
      this.#child.once('exit', (code, signal) => {
        const outcome = { code, signal }
        this.#outcome = outcome
        if (!this.#stopRequested) this.#prematureOutcome = outcome
        this.#events.emit('output')
        resolveExit(outcome)
      })
    })
  }

  get stderr(): string {
    return this.#redactDiagnostics ? this.#diagnostic('stderr') : this.#stderr
  }

  forgetCapturedStdout(): void {
    this.#stdout = ''
  }

  async waitFor(
    stream: 'stdout' | 'stderr',
    expression: RegExp,
    timeoutMs = PROCESS_READY_TIMEOUT_MS,
  ): Promise<RegExpMatchArray> {
    const current = () => (stream === 'stdout' ? this.#stdout : this.#stderr)
    const match = () => current().match(expression)
    const immediate = match()
    if (immediate !== null) {
      return immediate
    }
    return await new Promise<RegExpMatchArray>((resolveMatch, rejectMatch) => {
      const timeout = setTimeout(() => {
        cleanup()
        rejectMatch(
          new Error(
            `Timed out waiting for ${expression} in ${stream}. ` +
            `stdout=${this.#diagnostic('stdout')} stderr=${this.#diagnostic('stderr')}`,
          ),
        )
      }, timeoutMs)
      const inspect = () => {
        const found = match()
        if (found !== null) {
          cleanup()
          resolveMatch(found)
          return
        }
        if (this.#spawnFailure !== undefined || this.#outcome !== undefined) {
          cleanup()
          const outcome = this.#outcome
          rejectMatch(new Error(
            `E2E child exited before ${expression} appeared in ${stream} ` +
            `(code=${String(outcome?.code)}, signal=${String(outcome?.signal)}). ` +
            `stdout=${this.#diagnostic('stdout')} stderr=${this.#diagnostic('stderr')}`,
            { cause: this.#spawnFailure },
          ))
        }
      }
      const cleanup = () => {
        clearTimeout(timeout)
        this.#events.off('output', inspect)
      }
      this.#events.on('output', inspect)
      inspect()
    })
  }

  async waitForExit(): Promise<void> {
    await this.#exit
  }

  terminateForRunnerLoss(): void {
    const running = this.#child.exitCode === null && this.#child.signalCode === null
    if (!running) return
    this.#spawnFailure ??= new Error('The auditing runner guard disconnected')
    this.#child.kill('SIGKILL')
  }

  async stop(): Promise<void> {
    const stopAlreadyRequested = this.#stopRequested
    const running = this.#child.exitCode === null && this.#child.signalCode === null
    const settledBeforeStop = !stopAlreadyRequested && (this.#outcome !== undefined || !running)
    this.#stopRequested = true
    if (running) {
      this.#child.kill('SIGKILL')
    }
    let timeout: ReturnType<typeof setTimeout> | undefined
    try {
      const outcome = await Promise.race([
        this.#exit,
        new Promise<never>((_, rejectStop) => {
          timeout = setTimeout(
            () => rejectStop(new Error('Timed out stopping an E2E child process')),
            PROCESS_STOP_TIMEOUT_MS,
          )
        }),
      ])
      if (this.#spawnFailure !== undefined) {
        throw new Error('E2E child process could not be started', { cause: this.#spawnFailure })
      }
      if (this.#prematureOutcome !== undefined || settledBeforeStop) {
        const premature = this.#prematureOutcome ?? outcome
        throw new Error(
          `E2E child exited before cleanup (code=${String(premature.code)}, signal=${String(premature.signal)})`,
        )
      }
    } finally {
      if (timeout !== undefined) clearTimeout(timeout)
    }
  }

  #diagnostic(stream: 'stdout' | 'stderr'): string {
    const captured = stream === 'stdout' ? this.#stdout : this.#stderr
    if (this.#redactDiagnostics) {
      return `<redacted capability ${stream}; ${captured.length} characters captured>`
    }
    return JSON.stringify(captured)
  }
}

export class RealStack {
  readonly #binaries: BinaryPaths
  readonly #windowsRunner: WindowsStableRunner | undefined
  readonly #processes: ManagedProcess[] = []
  readonly #temporaryDirectories: string[] = []
  readonly #proxies: RelayProxy[] = []
  relayUrl = ''

  constructor(binaries: BinaryPaths) {
    this.#binaries = binaries
    this.#windowsRunner = binaryWindowsRunners.get(binaries)
    if (process.platform === 'win32' && this.#windowsRunner === undefined) {
      throw new Error('Windows real-stack fixture is missing its auditing runner guard')
    }
  }

  async start(): Promise<void> {
    await this.#windowsRunner?.assertBeforeLaunch()
    const relay = this.#track(
      new ManagedProcess(this.#binaries.relay, ['-listen', '127.0.0.1:0']),
    )
    const match = await relay.waitFor('stderr', /listening on ([^\s(]+)/u)
    const address = match[1]
    if (address === undefined) {
      throw new Error('Relay readiness output did not include its dynamic address')
    }
    this.relayUrl = `ws://${address}`
  }

  async createRelayProxy(): Promise<RelayProxy> {
    const proxy = await RelayProxy.start(this.relayUrl)
    this.#proxies.push(proxy)
    return proxy
  }

  async createTree(
    files: Readonly<Record<string, Uint8Array>>,
    directories: readonly string[] = [],
  ): Promise<SharedTree> {
    const parent = await mkdtemp(join(tmpdir(), 'windshare-c5-tree-'))
    this.#temporaryDirectories.push(parent)
    const root = join(parent, 'tree')
    await mkdir(root, { recursive: true })
    for (const [relativePath, data] of Object.entries(files)) {
      const destination = join(root, ...relativePath.split('/'))
      await mkdir(dirname(destination), { recursive: true })
      await writeFile(destination, data)
    }
    for (const relativePath of directories) {
      await mkdir(join(root, ...relativePath.split('/')), { recursive: true })
    }
    return Object.freeze({
      root,
      files: new Map(Object.entries(files)),
      directories: Object.freeze([...directories]),
    })
  }

  async share(
    paths: readonly string[],
    frontUrl: string,
    options: { readonly splitKey?: boolean } = {},
  ): Promise<ShareLink> {
    await this.#windowsRunner?.assertBeforeLaunch()
    const args = [
      'share',
      ...paths,
      '--relay',
      this.relayUrl,
      '--front-url',
      frontUrl,
      '--block-size',
      String(E2E_BLOCK_SIZE),
    ]
    if (options.splitKey === true) {
      args.push('--split-key')
    }
    const process = this.#track(
      new ManagedProcess(this.#binaries.windshare, args, { redactDiagnostics: true }),
    )
    if (options.splitKey === true) {
      const bare = await process.waitFor('stdout', /^Bare link: (.+)$/mu)
      const key = await process.waitFor('stdout', /^Key: (.+)$/mu)
      const bareLink = requiredCapture(bare, 1, 'Bare link')
      const separateKey = requiredCapture(key, 1, 'Separate key')
      process.forgetCapturedStdout()
      return Object.freeze({
        process,
        bareLink,
        key: separateKey,
      })
    }
    const link = await process.waitFor('stdout', /^Link: (.+)$/mu)
    const capabilityLink = requiredCapture(link, 1, 'Share link')
    process.forgetCapturedStdout()
    return Object.freeze({ process, link: capabilityLink })
  }

  async hostileShare(frontUrl: string): Promise<ShareLink> {
    await this.#windowsRunner?.assertBeforeLaunch()
    const process = this.#track(
      new ManagedProcess(
        this.#binaries.hostileSender,
        ['--relay', this.relayUrl, '--front-url', frontUrl],
        { redactDiagnostics: true },
      ),
    )
    const link = await process.waitFor('stdout', /^Link: (.+)$/mu)
    const capabilityLink = requiredCapture(link, 1, 'Hostile share link')
    process.forgetCapturedStdout()
    return Object.freeze({ process, link: capabilityLink })
  }

  async dispose(): Promise<void> {
    const failures: unknown[] = []
    const phases = [
      this.#proxies.map((proxy) => () => proxy.close()),
      [...this.#processes].reverse().map((process) => () => process.stop()),
      this.#temporaryDirectories.map((directory) => () =>
        rm(directory, { recursive: true, force: true }),
      ),
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
    if (failures.length > 1) {
      throw new AggregateError(failures, 'Real-stack fixture cleanup failed')
    }
  }

  #track(process: ManagedProcess): ManagedProcess {
    this.#windowsRunner?.track(process)
    this.#processes.push(process)
    return process
  }
}

export function deterministicBytes(length: number, seed: number): Uint8Array {
  const bytes = new Uint8Array(length)
  let state = seed >>> 0
  for (let index = 0; index < bytes.length; index += 1) {
    state = (Math.imul(state, 1_664_525) + 1_013_904_223) >>> 0
    bytes[index] = state >>> 24
  }
  return bytes
}

export function sha256(data: Uint8Array): string {
  return createHash('sha256').update(data).digest('hex')
}

export function replaceRelayHint(link: string, relayUrl: string): string {
  const parsed = new URL(link)
  parsed.searchParams.delete('r')
  parsed.searchParams.append('r', relayUrl)
  return parsed.toString()
}

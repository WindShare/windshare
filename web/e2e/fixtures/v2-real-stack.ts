import { execFile } from 'node:child_process'
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises'
import { connect, createServer, type Server, type Socket } from 'node:net'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'

import { ManagedProcess, settleCleanupTasks } from './managed-process'
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

const binaryCleanup = new WeakMap<BinaryPaths, () => Promise<void>>()
const binaryRunners = new WeakMap<BinaryPaths, WindowsStableRunner>()

export interface SplitShare {
  readonly bareLink: string
  readonly key: string
}

export class RelayProxy {
  readonly url: string
  readonly #server: Server
  readonly #connections = new Set<readonly [Socket, Socket]>()
  #accepting = true

  private constructor(server: Server, url: string) {
    this.#server = server
    this.url = url
  }

  static async start(upstreamUrl: string): Promise<RelayProxy> {
    const upstream = new URL(upstreamUrl)
    const port = Number(upstream.port)
    if (upstream.protocol !== 'ws:' || upstream.hostname === '' || !Number.isInteger(port)) {
      throw new TypeError('Relay proxy requires an explicit ws:// host and port')
    }
    const holder: { value?: RelayProxy } = {}
    const server = createServer((client) => {
      holder.value?.forward(client, upstream.hostname, port)
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
      throw new Error('Relay proxy did not expose a TCP address')
    }
    const proxy = new RelayProxy(server, `ws://127.0.0.1:${address.port}`)
    holder.value = proxy
    return proxy
  }

  cutConnections(): void {
    this.#accepting = false
    for (const [client, upstream] of this.#connections) {
      client.destroy()
      upstream.destroy()
    }
  }

  async close(): Promise<void> {
    this.cutConnections()
    await new Promise<void>((resolveClose, rejectClose) => {
      this.#server.close((error) => {
        if (error === undefined) resolveClose()
        else rejectClose(error)
      })
    })
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
}

export async function acquireRealStackBinaries(): Promise<BinaryPaths> {
  const stableDirectory = stableWindowsE2EDirectory(
    process.platform,
    process.env.WINDSHARE_WINDOWS_OS_NETWORK,
    process.env.WINDSHARE_D5_E2E_LEASE_TOKEN,
  )
  if (stableDirectory !== undefined) {
    const runner = await openWindowsStableRunner(stableDirectory)
    binaryRunners.set(runner.paths, runner)
    binaryCleanup.set(runner.paths, async () => runner.close())
    return runner.paths
  }

  const directory = await mkdtemp(join(tmpdir(), 'windshare-r7-browser-'))
  const paths = Object.freeze({
    directory,
    windshare: join(directory, executableName('windshare')),
    relay: join(directory, executableName('wsrelay')),
  })
  const results = await Promise.allSettled([
    build(paths.windshare, './cmd/windshare'),
    build(paths.relay, './relay/cmd/wsrelay'),
  ])
  const failures = results.flatMap((result) => result.status === 'rejected' ? [result.reason] : [])
  if (failures.length > 0) {
    await rm(directory, { recursive: true, force: true }).catch((error) => failures.push(error))
    throw failures.length === 1
      ? failures[0]
      : new AggregateError(failures, 'Real-stack binary build failed')
  }
  binaryCleanup.set(paths, () => rm(directory, { recursive: true, force: true }))
  return paths
}

export async function releaseRealStackBinaries(paths: BinaryPaths): Promise<void> {
  const cleanup = binaryCleanup.get(paths)
  if (cleanup === undefined) throw new Error('Real-stack binaries are not owned by this worker')
  binaryCleanup.delete(paths)
  await cleanup()
}

export class V2RealStack {
  readonly #binaries: BinaryPaths
  readonly #runner: WindowsStableRunner | undefined
  readonly #processes: ManagedProcess[] = []
  readonly #temporaryDirectories: string[] = []
  readonly #proxies: RelayProxy[] = []
  relayUrl = ''

  constructor(binaries: BinaryPaths) {
    this.#binaries = binaries
    this.#runner = binaryRunners.get(binaries)
    if (process.platform === 'win32' && this.#runner === undefined) {
      throw new Error('Windows real-stack execution requires the D5 stable runner')
    }
  }

  async start(): Promise<void> {
    await this.#runner?.assertBeforeLaunch()
    const stateDirectory = await mkdtemp(join(tmpdir(), 'windshare-r7-relay-state-'))
    this.#temporaryDirectories.push(stateDirectory)
    const relay = this.track(new ManagedProcess(
      this.#binaries.relay,
      ['-listen', '127.0.0.1:0', '-state-dir', stateDirectory],
    ))
    const ready = await relay.waitFor('stderr', /wsrelay: listening on ([^\s]+)/u)
    const address = requiredCapture(ready, 1, 'relay address')
    this.relayUrl = `ws://${address}`
  }

  async createRelayProxy(): Promise<RelayProxy> {
    const proxy = await RelayProxy.start(this.relayUrl)
    this.#proxies.push(proxy)
    return proxy
  }

  async createFile(name: string, data: Uint8Array): Promise<string> {
    if (name.length === 0 || name.includes('/') || name.includes('\\')) {
      throw new TypeError('Real-stack file name must be one path segment')
    }
    const directory = await mkdtemp(join(tmpdir(), 'windshare-r7-share-'))
    this.#temporaryDirectories.push(directory)
    const filePath = join(directory, name)
    await writeFile(filePath, data)
    return filePath
  }

  async share(filePath: string, frontUrl: string): Promise<SplitShare> {
    await this.#runner?.assertBeforeLaunch()
    const sender = this.track(new ManagedProcess(this.#binaries.windshare, [
      'share',
      filePath,
      '--relay',
      this.relayUrl,
      '--front-url',
      frontUrl,
      '--block-size',
      String(E2E_BLOCK_BYTES),
      '--split-key',
    ], { redactDiagnostics: true }))
    const bare = await sender.waitFor('stdout', /^Bare link: (.+)$/mu)
    const key = await sender.waitFor('stdout', /^Key: (.+)$/mu)
    const split = Object.freeze({
      bareLink: requiredCapture(bare, 1, 'bare share link'),
      key: requiredCapture(key, 1, 'separate key'),
    })
    sender.forgetCapturedStdout()
    return split
  }

  async dispose(): Promise<void> {
    const failures: unknown[] = []
    const phases = [
      this.#proxies.map((proxy) => () => proxy.close()),
      [...this.#processes].reverse().map((process) => () => process.stop()),
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
    this.#runner?.track(child)
    this.#processes.push(child)
    return child
  }
}

export function replaceRelayHint(link: string, relayUrl: string): string {
  const parsed = new URL(link)
  parsed.searchParams.delete('r')
  parsed.searchParams.append('r', relayUrl)
  return parsed.toString()
}

async function build(output: string, packagePath: string): Promise<void> {
  await mkdir(dirname(output), { recursive: true })
  await execFileAsync('go', ['build', '-o', output, packagePath], {
    cwd: REPOSITORY_ROOT,
    env: { ...process.env, GOWORK: 'auto' },
    timeout: BUILD_TIMEOUT_MILLISECONDS,
    windowsHide: true,
    maxBuffer: MAXIMUM_BUILD_OUTPUT_BYTES,
  })
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

import { execFile } from 'node:child_process'
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises'
import { connect, createServer, type Server, type Socket } from 'node:net'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'

import { createServer as createViteServer, type ViteDevServer } from 'vite'

import { DirectProcess } from './direct-process'

const execFileAsync = promisify(execFile)
const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')
const WEB_ROOT = join(REPOSITORY_ROOT, 'web')
const NATIVE_CLI_EXECUTABLE = 'wind'
const NATIVE_CLI_PACKAGE = './cmd/wind'
const BUILD_TIMEOUT_MILLISECONDS = 60_000
const BUILD_OUTPUT_LIMIT_BYTES = 1_000_000
const PROCESS_READINESS_TIMEOUT_MILLISECONDS = 20_000
const PROCESS_STOP_TIMEOUT_MILLISECONDS = 5_000
const RELAY_LISTENING_PATTERN = /wsrelay: listening on ([^\s]+) /u
const BARE_LINK_PATTERN = /^Bare link: (.+)$/mu
const SEPARATE_KEY_PATTERN = /^Key: (.+)$/mu
/** Default sender block geometry used by native-peer and non-WebKit routes. */
export const DIRECT_TEST_BLOCK_BYTES = 64 * 1024

/**
 * WPE's Linux WebKit build cannot reliably process relay frames above 32 KiB.
 * Keep this compatibility value at the fixture boundary; production defaults
 * remain unchanged and the transfer payload is still validated end-to-end.
 */
export const DIRECT_WEBKIT_RELAY_BLOCK_BYTES = 32 * 1024

export interface DirectShareOptions {
  readonly blockSizeBytes?: number
  readonly relayUrl?: string
}

export interface DirectStackTrace {
  readonly component: 'browser-direct-stack'
  readonly scenarioId: string
  readonly operationId: string
  readonly milestone: 'started' | 'ready' | 'stopped' | 'failed'
}

export interface DirectShare {
  readonly bareLink: string
  readonly key: string
}

export interface DirectDirectoryEntry {
  readonly name: string
  readonly bytes: Uint8Array
}

export interface DirectBinaryPaths {
  readonly directory: string
  readonly relay: string
  readonly wind: string
}

export class DirectProductStack {
  readonly #scenarioId: string
  readonly #observe: (event: DirectStackTrace) => void
  readonly #processes: DirectProcess[] = []
  readonly #proxies: RelayCutProxy[] = []
  #rootDirectory: string | undefined
  #vite: ViteDevServer | undefined
  #binaries: DirectBinaryPaths | undefined
  #relayUrl: string | undefined
  #senderSequence = 0
  baseURL = ''

  constructor(
    scenarioId: string,
    observe: (event: DirectStackTrace) => void = writeStructuredTrace,
  ) {
    if (!/^[a-z0-9][a-z0-9-]{0,63}$/u.test(scenarioId)) {
      throw new TypeError('Direct browser scenario ID must be a stable portable token')
    }
    this.#scenarioId = scenarioId
    this.#observe = observe
  }

  async start(): Promise<void> {
    if (this.#rootDirectory !== undefined) throw new Error('Direct product stack already started')
    this.#rootDirectory = await mkdtemp(join(tmpdir(), 'windshare-browser-direct-'))
    try {
      const [binaries] = await Promise.all([
        this.#buildBinaries(),
        this.#startWebServer(),
      ])
      this.#binaries = binaries
      await this.#startRelay()
    } catch (error) {
      const cleanup = await Promise.allSettled([this.dispose()])
      const cleanupErrors = cleanup.flatMap((result) =>
        result.status === 'rejected' ? [result.reason] : [])
      throw cleanupErrors.length === 0
        ? error
        : new AggregateError([error, ...cleanupErrors], 'Direct product stack startup failed')
    }
  }

  async createFile(name: string, bytes: Uint8Array): Promise<string> {
    const directory = await this.createDirectory('single-file', [{ name, bytes }])
    return join(directory, name)
  }

  async createDirectory(
    name: string,
    entries: readonly DirectDirectoryEntry[],
  ): Promise<string> {
    const root = this.#requireRoot()
    requirePathSegment(name, 'shared directory')
    const directory = join(root, 'shares', name)
    await mkdir(directory, { recursive: true })
    await Promise.all(entries.map(async (entry) => {
      requirePathSegment(entry.name, 'shared file')
      await writeFile(join(directory, entry.name), entry.bytes)
    }))
    return directory
  }

  async share(path: string, options: DirectShareOptions = {}): Promise<DirectShare> {
    const binaries = this.#requireBinaries()
    const relayUrl = options.relayUrl ?? this.#requireRelayUrl()
    const blockSizeBytes = options.blockSizeBytes ?? DIRECT_TEST_BLOCK_BYTES
    validateBlockSize(blockSizeBytes)
    this.#senderSequence += 1
    const operationId = `${this.#scenarioId}-sender-${this.#senderSequence}`
    const sender = this.#track(new DirectProcess(binaries.wind, [
      'share',
      path,
      '--relay',
      relayUrl,
      '--front-url',
      this.baseURL,
      '--block-size',
      String(blockSizeBytes),
      '--split-key',
    ], {
      cwd: REPOSITORY_ROOT,
      environment: localGoEnvironment(),
      operationId,
      disclosure: { stdout: 'capability', stderr: 'private' },
    }))
    this.#trace(operationId, 'started')
    let bare: RegExpMatchArray
    let key: RegExpMatchArray
    try {
      [bare, key] = await Promise.all([
        sender.waitFor('stdout', BARE_LINK_PATTERN, {
          timeoutMilliseconds: PROCESS_READINESS_TIMEOUT_MILLISECONDS,
        }),
        sender.waitFor('stdout', SEPARATE_KEY_PATTERN, {
          timeoutMilliseconds: PROCESS_READINESS_TIMEOUT_MILLISECONDS,
        }),
      ])
    } catch (error) {
      this.#trace(operationId, 'failed')
      throw error
    }
    const bareLink = requiredCapture(bare, 1, 'bare share link')
    const separateKey = requiredCapture(key, 1, 'separate capability key')
    // Sender stdout is a readiness-only capability channel. Once both values
    // have been copied into the frozen share record, erase the raw capture so
    // later cleanup diagnostics cannot retain a second capability transport.
    sender.consumeReadiness('stdout')
    this.#trace(operationId, 'ready')
    return Object.freeze({ bareLink, key: separateKey })
  }

  async createRelayCutProxy(): Promise<RelayCutProxy> {
    const proxy = await RelayCutProxy.start(this.#requireRelayUrl())
    this.#proxies.push(proxy)
    return proxy
  }

  diagnostic(): readonly string[] {
    return Object.freeze(this.#processes.map((process) => process.diagnostic()))
  }

  async dispose(): Promise<void> {
    const failures: unknown[] = []
    for (const proxy of [...this.#proxies].reverse()) {
      await proxy.close().catch((error) => failures.push(error))
    }
    this.#proxies.length = 0
    for (const process of [...this.#processes].reverse()) {
      try {
        await process.stop(PROCESS_STOP_TIMEOUT_MILLISECONDS)
        this.#trace(process.operationId, 'stopped')
      } catch (error) {
        this.#trace(process.operationId, 'failed')
        failures.push(error)
      }
    }
    this.#processes.length = 0
    if (this.#vite !== undefined) {
      await this.#vite.close().catch((error) => failures.push(error))
      this.#vite = undefined
    }
    if (this.#rootDirectory !== undefined) {
      await rm(this.#rootDirectory, { recursive: true, force: true })
        .catch((error) => failures.push(error))
      this.#rootDirectory = undefined
    }
    if (failures.length === 1) throw failures[0]
    if (failures.length > 1) {
      throw new AggregateError(failures, 'Direct product stack cleanup failed')
    }
  }

  async #buildBinaries(): Promise<DirectBinaryPaths> {
    const output = join(this.#requireRoot(), 'bin')
    await mkdir(output, { recursive: true })
    const binaries = directBinaryPaths(output)
    await Promise.all([
      buildGoBinary(binaries.relay, './relay/cmd/wsrelay'),
      buildGoBinary(binaries.wind, NATIVE_CLI_PACKAGE),
    ])
    return binaries
  }

  async #startWebServer(): Promise<void> {
    const vite = await createViteServer({
      root: WEB_ROOT,
      logLevel: 'error',
      server: { host: '127.0.0.1', port: 0, strictPort: true },
    })
    this.#vite = vite
    await vite.listen()
    const address = vite.httpServer?.address()
    if (address === null || address === undefined || typeof address === 'string') {
      throw new Error('Vite did not expose its direct browser listener')
    }
    this.baseURL = `http://127.0.0.1:${address.port}`
  }

  async #startRelay(): Promise<void> {
    const binaries = this.#requireBinaries()
    const stateDirectory = join(this.#requireRoot(), 'relay-state')
    await mkdir(stateDirectory, { recursive: true })
    const operationId = `${this.#scenarioId}-relay`
    const relay = this.#track(new DirectProcess(binaries.relay, [
      '-listen',
      '127.0.0.1:0',
      '-state-dir',
      stateDirectory,
    ], {
      cwd: REPOSITORY_ROOT,
      environment: localGoEnvironment(),
      operationId,
      disclosure: { stdout: 'private', stderr: 'safe' },
    }))
    this.#trace(operationId, 'started')
    let ready: RegExpMatchArray
    try {
      ready = await relay.waitFor('stderr', RELAY_LISTENING_PATTERN, {
        timeoutMilliseconds: PROCESS_READINESS_TIMEOUT_MILLISECONDS,
      })
    } catch (error) {
      this.#trace(operationId, 'failed')
      throw error
    }
    this.#relayUrl = `ws://${requiredCapture(ready, 1, 'relay listener address')}`
    this.#trace(operationId, 'ready')
  }

  #track(process: DirectProcess): DirectProcess {
    this.#processes.push(process)
    return process
  }

  #trace(operationId: string, milestone: DirectStackTrace['milestone']): void {
    try {
      this.#observe(Object.freeze({
        component: 'browser-direct-stack',
        scenarioId: this.#scenarioId,
        operationId,
        milestone,
      }))
    } catch {
      // Test diagnostics cannot own process or product lifecycle.
    }
  }

  #requireRoot(): string {
    if (this.#rootDirectory === undefined) throw new Error('Direct product stack is not started')
    return this.#rootDirectory
  }

  #requireBinaries(): DirectBinaryPaths {
    if (this.#binaries === undefined) throw new Error('Direct product binaries are unavailable')
    return this.#binaries
  }

  #requireRelayUrl(): string {
    if (this.#relayUrl === undefined) throw new Error('Direct product relay is unavailable')
    return this.#relayUrl
  }
}

export class RelayCutProxy {
  readonly url: string
  readonly #server: Server
  readonly #connections = new Set<readonly [Socket, Socket]>()
  #closed = false

  private constructor(server: Server, url: string) {
    this.#server = server
    this.url = url
  }

  static async start(upstreamUrl: string): Promise<RelayCutProxy> {
    const upstream = new URL(upstreamUrl)
    const port = Number(upstream.port)
    if (upstream.protocol !== 'ws:' || upstream.hostname === '' || !Number.isInteger(port)) {
      throw new TypeError('Relay cut proxy requires an explicit ws:// endpoint')
    }
    const holder: { value?: RelayCutProxy } = {}
    const server = createServer((client) => {
      const proxy = holder.value
      if (proxy === undefined) client.destroy()
      else proxy.#forward(client, upstream.hostname, port)
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
      throw new Error('Relay cut proxy did not expose a listener')
    }
    const proxy = new RelayCutProxy(server, `ws://127.0.0.1:${address.port}`)
    holder.value = proxy
    return proxy
  }

  async cut(): Promise<void> {
    if (this.#closed) return
    this.#closed = true
    for (const [client, upstream] of this.#connections) {
      client.destroy()
      upstream.destroy()
    }
    await new Promise<void>((resolveClose, rejectClose) => {
      this.#server.close((error) => error === undefined ? resolveClose() : rejectClose(error))
    })
  }

  async close(): Promise<void> {
    await this.cut()
  }

  #forward(client: Socket, host: string, port: number): void {
    if (this.#closed) {
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

export function capabilityUrl(share: DirectShare, relayUrl?: string): string {
  const receiver = new URL(relayReceiverUrl(share, relayUrl))
  receiver.hash = share.key
  return receiver.toString()
}

export function relayReceiverUrl(share: DirectShare, relayUrl?: string): string {
  const receiver = new URL(share.bareLink)
  if (relayUrl !== undefined) {
    receiver.searchParams.delete('r')
    receiver.searchParams.append('r', relayUrl)
  }
  return receiver.toString()
}

export function directBinaryPaths(
  directory: string,
  platform: NodeJS.Platform = process.platform,
): DirectBinaryPaths {
  if (resolve(directory) !== directory) {
    throw new TypeError('Direct product binary directory path must be absolute and canonical')
  }
  return Object.freeze({
    directory,
    relay: join(directory, executableName('wsrelay', platform)),
    wind: join(directory, executableName(NATIVE_CLI_EXECUTABLE, platform)),
  })
}

async function buildGoBinary(output: string, packagePath: string): Promise<void> {
  await execFileAsync('go', ['build', '-o', output, packagePath], {
    cwd: REPOSITORY_ROOT,
    env: localGoEnvironment(),
    timeout: BUILD_TIMEOUT_MILLISECONDS,
    windowsHide: true,
    maxBuffer: BUILD_OUTPUT_LIMIT_BYTES,
  })
}

function localGoEnvironment(): NodeJS.ProcessEnv {
  return { ...process.env, GOTOOLCHAIN: 'local', GOWORK: 'off' }
}

function executableName(name: string, platform: NodeJS.Platform = process.platform): string {
  return platform === 'win32' ? `${name}.exe` : name
}

function requirePathSegment(value: string, label: string): void {
  if (value.length === 0 || value === '.' || value === '..' || value.includes('/') || value.includes('\\')) {
    throw new TypeError(`${label} name must be one path segment`)
  }
}

function validateBlockSize(value: number): void {
  if (!Number.isSafeInteger(value) || value <= 0) {
    throw new TypeError('Direct sender block size must be a positive safe integer')
  }
}

function requiredCapture(match: RegExpMatchArray, index: number, label: string): string {
  const value = match[index]
  if (value === undefined || value.length === 0) throw new Error(`${label} is missing`)
  return value
}

function writeStructuredTrace(event: DirectStackTrace): void {
  console.info(JSON.stringify(event))
}

import { execFile } from 'node:child_process'
import { mkdir, mkdtemp, rm } from 'node:fs/promises'
import { isIP } from 'node:net'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'

import { DirectProcess } from './direct-process'

const execFileAsync = promisify(execFile)
const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')
const BUILD_TIMEOUT_MILLISECONDS = 60_000
const BUILD_OUTPUT_LIMIT_BYTES = 1_000_000
const READY_TIMEOUT_MILLISECONDS = 20_000
const STOP_TIMEOUT_MILLISECONDS = 5_000
const READY_LINE_PATTERN = /^(\{[^\r\n]+\})$/mu
const TURN_LOOPBACK_URL_PATTERN = /^turn:127\.0\.0\.1:([1-9]\d{0,4})\?transport=udp$/u

export interface LocalTurnReadyRecord {
  readonly component: 'browser-local-turn-server'
  readonly scenarioId: 'chromium-turn-route'
  readonly operationId: 'chromium-turn-route-server'
  readonly milestone: 'listener-ready'
  readonly url: string
  readonly relayAddress: string
  readonly username: string
  readonly credential: string
}

export class LocalTurnServer {
  #rootDirectory: string | undefined
  #process: DirectProcess | undefined
  #configuration: RTCConfiguration | undefined
  #relayAddress: string | undefined

  get rtcConfiguration(): RTCConfiguration {
    if (this.#configuration === undefined) throw new Error('Local TURN server is not ready')
    return structuredClone(this.#configuration)
  }

  async start(): Promise<void> {
    if (this.#rootDirectory !== undefined) throw new Error('Local TURN server already started')
    this.#rootDirectory = await mkdtemp(join(tmpdir(), 'windshare-browser-turn-'))
    try {
      const binaryDirectory = join(this.#rootDirectory, 'bin')
      await mkdir(binaryDirectory, { recursive: true })
      const executable = join(binaryDirectory, executableName('browserturnserver'))
      await execFileAsync(
        process.env.WINDSHARE_GO_EXECUTABLE ?? 'go',
        ['build', '-o', executable, './transport/webrtc/testdata/browser/turnserver'],
        {
          cwd: REPOSITORY_ROOT,
          env: localGoEnvironment(),
          timeout: BUILD_TIMEOUT_MILLISECONDS,
          windowsHide: true,
          maxBuffer: BUILD_OUTPUT_LIMIT_BYTES,
        },
      )
      const child = new DirectProcess(executable, [], {
        cwd: REPOSITORY_ROOT,
        environment: localGoEnvironment(),
        operationId: 'chromium-turn-route-server',
        redactStdout: true,
      })
      this.#process = child
      const match = await child.waitFor('stdout', READY_LINE_PATTERN, {
        timeoutMilliseconds: READY_TIMEOUT_MILLISECONDS,
      })
      const ready = parseLocalTurnReadyRecord(requiredCapture(match, 1))
      this.#relayAddress = ready.relayAddress
      this.#configuration = Object.freeze({
        iceServers: [Object.freeze({
          urls: [ready.url],
          username: ready.username,
          credential: ready.credential,
        })],
        iceTransportPolicy: 'relay',
      })
    } catch (error) {
      await this.dispose().catch(() => undefined)
      throw error
    }
  }

  diagnostic(): string | null {
    if (this.#process === undefined && this.#relayAddress === undefined) return null
    return JSON.stringify({
      relayAddress: this.#relayAddress,
      process: this.#process?.diagnostic() ?? null,
    })
  }

  async dispose(): Promise<void> {
    const failures: unknown[] = []
    if (this.#process !== undefined) {
      await this.#process.stop(STOP_TIMEOUT_MILLISECONDS).catch((error) => failures.push(error))
      this.#process = undefined
    }
    this.#configuration = undefined
    this.#relayAddress = undefined
    if (this.#rootDirectory !== undefined) {
      await rm(this.#rootDirectory, { recursive: true, force: true })
        .catch((error) => failures.push(error))
      this.#rootDirectory = undefined
    }
    if (failures.length === 1) throw failures[0]
    if (failures.length > 1) throw new AggregateError(failures, 'Local TURN cleanup failed')
  }
}

export function parseLocalTurnReadyRecord(encoded: string): LocalTurnReadyRecord {
  const value: unknown = JSON.parse(encoded)
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new TypeError('Local TURN readiness is not an object')
  }
  const record = value as Partial<LocalTurnReadyRecord>
  if (
    record.component !== 'browser-local-turn-server' ||
    record.scenarioId !== 'chromium-turn-route' ||
    record.operationId !== 'chromium-turn-route-server' ||
    record.milestone !== 'listener-ready' ||
    typeof record.url !== 'string' || typeof record.relayAddress !== 'string' ||
    typeof record.username !== 'string' ||
    typeof record.credential !== 'string' || record.username === '' || record.credential === ''
  ) throw new TypeError('Local TURN readiness fields are invalid')
  const match = TURN_LOOPBACK_URL_PATTERN.exec(record.url)
  const port = Number(match?.[1])
  if (match === null || !Number.isSafeInteger(port) || port > 65_535) {
    throw new TypeError('Local TURN readiness URL is not an owned UDP loopback listener')
  }
  if (!isUsableNonLoopbackIPv4(record.relayAddress)) {
    throw new TypeError('Local TURN readiness relay address is not a usable owned IPv4 route')
  }
  return Object.freeze(record as LocalTurnReadyRecord)
}

function isUsableNonLoopbackIPv4(value: string): boolean {
  if (isIP(value) !== 4) return false
  const firstOctet = Number(value.slice(0, value.indexOf('.')))
  return firstOctet !== 0 && firstOctet !== 127 && (firstOctet < 224 || firstOctet > 239)
}

function localGoEnvironment(): NodeJS.ProcessEnv {
  return { ...process.env, GOTOOLCHAIN: 'local' }
}

function executableName(base: string): string {
  return process.platform === 'win32' ? `${base}.exe` : base
}

function requiredCapture(match: RegExpMatchArray, index: number): string {
  const value = match[index]
  if (value === undefined || value === '') throw new Error('Local TURN readiness is missing')
  return value
}

import { createHash } from 'node:crypto'
import { open, readFile } from 'node:fs/promises'
import { connect, type Socket } from 'node:net'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')
const STABLE_WINDOWS_CONTRACT = 'stable-harness-v3'
const STABLE_LEASE_TOKEN_ENV = 'WINDSHARE_D5_E2E_LEASE_TOKEN'
const STABLE_RUNNER_PIPE_ENV = 'WINDSHARE_D5_RUNNER_PIPE'
const STABLE_LEASE_TOKEN_PATTERN = /^[0-9a-f]{32}$/u
const STABLE_RUNNER_PIPE_PATTERN = /^windshare-d5-[1-9]\d*-[0-9a-f]{32}$/u
const SHA256_PATTERN = /^[0-9a-f]{64}$/u
const RUNNER_GUARD_CONNECT_TIMEOUT_MS = 10_000
const STABLE_E2E_DIRECTORY = join(
  REPOSITORY_ROOT,
  'tmp',
  'd5-harness',
  'e2e-bin',
)
const STABLE_BUILD_LOCK = join(STABLE_E2E_DIRECTORY, '.owner.lock')

export interface BinaryPaths {
  readonly directory: string
  readonly windshare: string
  readonly relay: string
}

interface StableBuildLeaseRecord {
  readonly contract: string
  readonly ownerPid: number
  readonly acquiredAt: string
  readonly tokenSha256: string
}

interface StableBinaryEvidence {
  readonly path: string
  readonly bytes: number
  readonly sha256: string
}

interface StableBinaryManifest {
  readonly recordedAt: string
  readonly binaries: readonly StableBinaryEvidence[]
}

export interface RunnerGuardedProcess {
  terminateForRunnerLoss(): void
}

export interface WindowsStableRunner {
  readonly paths: BinaryPaths
  assertBeforeLaunch(): Promise<void>
  track(child: RunnerGuardedProcess): void
  close(): void
}

export function stableWindowsE2EDirectory(
  platform: NodeJS.Platform,
  contract: string | undefined,
  leaseToken: string | undefined,
): string | undefined {
  if (platform !== 'win32') return undefined
  if (
    contract !== STABLE_WINDOWS_CONTRACT ||
    leaseToken === undefined ||
    !STABLE_LEASE_TOKEN_PATTERN.test(leaseToken)
  ) {
    throw new Error(
      'Windows real-stack Playwright requires scripts/d5-windows-performance.ps1 -Mode BrowserTests',
    )
  }
  return STABLE_E2E_DIRECTORY
}

function sha256(data: Uint8Array): string {
  return createHash('sha256').update(data).digest('hex')
}

function parseStableBuildLease(raw: string): StableBuildLeaseRecord {
  let parsed: unknown
  try {
    parsed = JSON.parse(raw)
  } catch (error) {
    throw new Error('The stable Windows E2E lease record is not valid JSON', { cause: error })
  }
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
    throw new Error('The stable Windows E2E lease record has an invalid shape')
  }
  const record = parsed as Partial<StableBuildLeaseRecord>
  if (
    Object.keys(record).sort().join(',') !== 'acquiredAt,contract,ownerPid,tokenSha256' ||
    typeof record.contract !== 'string' ||
    !Number.isInteger(record.ownerPid) ||
    (record.ownerPid ?? 0) <= 0 ||
    typeof record.acquiredAt !== 'string' ||
    !Number.isFinite(Date.parse(record.acquiredAt)) ||
    typeof record.tokenSha256 !== 'string' ||
    !SHA256_PATTERN.test(record.tokenSha256)
  ) {
    throw new Error('The stable Windows E2E lease record has an invalid shape')
  }
  return record as StableBuildLeaseRecord
}

export async function assertStableWindowsE2ELeaseProof(
  contract: string | undefined,
  leaseToken: string | undefined,
  requireWriteDenied: () => Promise<void>,
  readRecord: () => Promise<string>,
): Promise<void> {
  if (
    contract !== STABLE_WINDOWS_CONTRACT ||
    leaseToken === undefined ||
    !STABLE_LEASE_TOKEN_PATTERN.test(leaseToken)
  ) {
    throw new Error('The stable Windows E2E runner lease token is missing or invalid')
  }
  // The denial must precede the identity read. Otherwise owner A can disappear
  // after its record is read and owner B's handle can accidentally prove A live.
  await requireWriteDenied()
  const record = parseStableBuildLease(await readRecord())
  if (
    record.contract !== STABLE_WINDOWS_CONTRACT ||
    record.tokenSha256 !== sha256(Buffer.from(leaseToken, 'utf8'))
  ) {
    throw new Error('The stable Windows E2E runner does not own this lease record')
  }
}

async function requireStableLeaseWriteDenied(lockPath: string): Promise<void> {
  let writableHandle
  try {
    writableHandle = await open(lockPath, 'r+')
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'EBUSY') return
    throw new Error('The stable Windows E2E lease liveness could not be proven', {
      cause: error,
    })
  }
  await writableHandle.close()
  throw new Error('The stable Windows E2E lease is not held by the auditing runner')
}

export async function assertStableWindowsE2ELeaseAt(
  lockPath: string,
  contract: string | undefined,
  leaseToken: string | undefined,
): Promise<void> {
  await assertStableWindowsE2ELeaseProof(
    contract,
    leaseToken,
    () => requireStableLeaseWriteDenied(lockPath),
    () => readFile(lockPath, 'utf8'),
  )
}

async function assertRunnerOwnsStableWindowsE2ELease(): Promise<void> {
  await assertStableWindowsE2ELeaseAt(
    STABLE_BUILD_LOCK,
    process.env.WINDSHARE_WINDOWS_OS_NETWORK,
    process.env[STABLE_LEASE_TOKEN_ENV],
  )
}

function parseStableBinaryManifest(raw: string): StableBinaryManifest {
  let parsed: unknown
  try {
    parsed = JSON.parse(raw)
  } catch (error) {
    throw new Error('The stable Windows binary manifest is not valid JSON', { cause: error })
  }
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
    throw new Error('The stable Windows binary manifest has an invalid shape')
  }
  const manifest = parsed as Partial<StableBinaryManifest>
  if (
    Object.keys(manifest).sort().join(',') !== 'binaries,recordedAt' ||
    typeof manifest.recordedAt !== 'string' ||
    !Number.isFinite(Date.parse(manifest.recordedAt)) ||
    !Array.isArray(manifest.binaries) ||
    manifest.binaries.length !== 2
  ) {
    throw new Error('The stable Windows binary manifest has an invalid shape')
  }
  for (const candidate of manifest.binaries) {
    if (
      candidate === null ||
      typeof candidate !== 'object' ||
      Array.isArray(candidate) ||
      Object.keys(candidate).sort().join(',') !== 'bytes,path,sha256'
    ) {
      throw new Error('The stable Windows binary manifest has an invalid entry')
    }
    const binary = candidate as Partial<StableBinaryEvidence>
    if (
      typeof binary.path !== 'string' ||
      !Number.isInteger(binary.bytes) ||
      (binary.bytes ?? 0) <= 0 ||
      typeof binary.sha256 !== 'string' ||
      !SHA256_PATTERN.test(binary.sha256)
    ) {
      throw new Error('The stable Windows binary manifest has an invalid entry')
    }
  }
  return manifest as StableBinaryManifest
}

async function loadStableWindowsBinaries(directory: string): Promise<BinaryPaths> {
  const outputs = {
    windshare: join(directory, 'windshare.exe'),
    relay: join(directory, 'wsrelay.exe'),
  }
  const manifestPath = process.env.WINDSHARE_D5_CHILD_MANIFEST
  if (manifestPath === undefined || manifestPath.length === 0) {
    throw new Error('WINDSHARE_D5_CHILD_MANIFEST is required for stable Windows E2E binaries')
  }
  const manifest = parseStableBinaryManifest(await readFile(manifestPath, 'utf8'))
  const expected = new Set(Object.values(outputs))
  const recorded = new Set(manifest.binaries.map((binary) => binary.path))
  if (
    recorded.size !== expected.size ||
    [...expected].some((path) => !recorded.has(path))
  ) {
    throw new Error('The auditing runner did not record the exact stable Windows binaries')
  }
  await Promise.all(manifest.binaries.map(async (binary) => {
    const data = await readFile(binary.path)
    if (data.byteLength !== binary.bytes || sha256(data) !== binary.sha256) {
      throw new Error('A stable Windows binary differs from the runner-owned manifest')
    }
  }))
  return Object.freeze({ directory, ...outputs })
}

class RunnerGuard {
  readonly #socket: Socket
  readonly #processes = new Set<RunnerGuardedProcess>()
  #alive = true
  #closing = false

  constructor(socket: Socket) {
    this.#socket = socket
    const lose = () => this.#lose()
    socket.once('end', lose)
    socket.once('close', lose)
    socket.once('error', lose)
    socket.resume()
  }

  assertAlive(): void {
    if (!this.#alive || this.#socket.destroyed || this.#socket.readableEnded) {
      throw new Error('The auditing runner guard is no longer connected')
    }
  }

  track(child: RunnerGuardedProcess): void {
    if (!this.#alive) {
      child.terminateForRunnerLoss()
      throw new Error('The auditing runner guard disconnected before child tracking')
    }
    this.#processes.add(child)
  }

  close(): void {
    if (this.#closing) return
    this.#closing = true
    this.#alive = false
    this.#processes.clear()
    this.#socket.destroy()
  }

  #lose(): void {
    if (!this.#alive || this.#closing) return
    this.#alive = false
    for (const child of this.#processes) child.terminateForRunnerLoss()
  }
}

async function connectRunnerGuard(): Promise<RunnerGuard> {
  const pipeName = process.env[STABLE_RUNNER_PIPE_ENV]
  if (pipeName === undefined || !STABLE_RUNNER_PIPE_PATTERN.test(pipeName)) {
    throw new Error('The auditing runner guard name is missing or invalid')
  }
  const socket = connect(`\\\\.\\pipe\\${pipeName}`)
  await new Promise<void>((resolveConnect, rejectConnect) => {
    const timeout = setTimeout(() => {
      cleanup()
      socket.destroy()
      rejectConnect(new Error('Timed out connecting to the auditing runner guard'))
    }, RUNNER_GUARD_CONNECT_TIMEOUT_MS)
    const cleanup = () => {
      clearTimeout(timeout)
      socket.off('connect', connected)
      socket.off('error', failed)
    }
    const connected = () => {
      cleanup()
      resolveConnect()
    }
    const failed = (error: Error) => {
      cleanup()
      rejectConnect(new Error('Could not connect to the auditing runner guard', { cause: error }))
    }
    socket.once('connect', connected)
    socket.once('error', failed)
  })
  return new RunnerGuard(socket)
}

class StableWindowsRunner implements WindowsStableRunner {
  readonly paths: BinaryPaths
  readonly #guard: RunnerGuard

  constructor(paths: BinaryPaths, guard: RunnerGuard) {
    this.paths = paths
    this.#guard = guard
  }

  async assertBeforeLaunch(): Promise<void> {
    this.#guard.assertAlive()
    await assertRunnerOwnsStableWindowsE2ELease()
    this.#guard.assertAlive()
  }

  track(child: RunnerGuardedProcess): void {
    this.#guard.track(child)
  }

  close(): void {
    this.#guard.close()
  }
}

export async function openWindowsStableRunner(directory: string): Promise<WindowsStableRunner> {
  if (directory !== STABLE_E2E_DIRECTORY) {
    throw new Error('Windows E2E children must use the fixed runner-owned directory')
  }
  await assertRunnerOwnsStableWindowsE2ELease()
  const guard = await connectRunnerGuard()
  try {
    await assertRunnerOwnsStableWindowsE2ELease()
    guard.assertAlive()
    const paths = await loadStableWindowsBinaries(directory)
    await assertRunnerOwnsStableWindowsE2ELease()
    guard.assertAlive()
    return new StableWindowsRunner(paths, guard)
  } catch (error) {
    guard.close()
    throw error
  }
}

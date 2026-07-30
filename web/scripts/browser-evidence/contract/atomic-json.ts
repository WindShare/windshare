import { randomBytes } from 'node:crypto'
import { basename, dirname, join } from 'node:path'
import { mkdir, open, rename, unlink } from 'node:fs/promises'

import { parseBrowserSampleResult, type BrowserSampleResult } from '../result.ts'
import { readStableRegularFileSnapshot } from '../filesystem/snapshot.ts'
import { parseCanonicalJsonText } from './strict-json.ts'
import type { VerifiedTestIceTopologyLock } from '../test-ice-topology.ts'

const ATOMIC_REPLACE_ATTEMPTS = 8
const ATOMIC_REPLACE_RETRY_MS = 25
const MAXIMUM_PARENT_FINAL_RESULT_BYTES = 16 * 1024 * 1024

export async function writeAtomicJson(path: string, value: unknown): Promise<void> {
  const directory = dirname(path)
  await mkdir(directory, { recursive: true })
  const temporaryPath = join(
    directory,
    `.${basename(path)}.${process.pid}.${randomBytes(8).toString('hex')}.tmp`,
  )
  let temporaryExists = false
  try {
    const handle = await open(temporaryPath, 'wx', 0o600)
    temporaryExists = true
    try {
      // Sealed uploads preserve these bytes verbatim, so the parent writer must
      // emit the one representation authenticated by the standalone verdict.
      await handle.writeFile(JSON.stringify(value), 'utf8')
      await handle.sync()
    } finally {
      await handle.close()
    }
    await replaceAtomic(temporaryPath, path)
    temporaryExists = false
    await syncDirectory(directory)
  } finally {
    if (temporaryExists) await unlink(temporaryPath).catch(() => undefined)
  }
}

/**
 * The stateful writer makes the parent process's authority explicit: a sample
 * first becomes crash-recoverable as provisional evidence, then receives one
 * atomic terminal replacement after the child has exited.
 */
export class BrowserSampleResultWriter {
  readonly #path: string
  readonly #topologyLock: VerifiedTestIceTopologyLock
  #state: 'new' | 'provisional' | 'final' = 'new'

  constructor(path: string, topologyLock: VerifiedTestIceTopologyLock) {
    this.#path = path
    this.#topologyLock = topologyLock
  }

  async writeProvisional(value: unknown): Promise<BrowserSampleResult> {
    if (this.#state !== 'new') throw new Error('browser sample provisional result can only be written once')
    const result = parseBrowserSampleResult(value, this.#topologyLock)
    if (result.resultStatus !== 'provisional') {
      throw new Error('browser sample initial result must be provisional')
    }
    await writeAtomicJson(this.#path, result)
    this.#state = 'provisional'
    return result
  }

  async writeFinal(value: unknown): Promise<BrowserSampleResult> {
    if (this.#state !== 'provisional') {
      throw new Error('browser sample final result requires one provisional predecessor')
    }
    const result = parseBrowserSampleResult(value, this.#topologyLock)
    if (result.resultStatus === 'provisional') throw new Error('browser sample final result is not terminal')
    await writeAtomicJson(this.#path, result)
    this.#state = 'final'
    return result
  }
}

export interface ParentFinalizedBrowserSampleResult {
  readonly result: BrowserSampleResult
  readonly bytes: Uint8Array
}

/**
 * An inherited sample driver cannot know that detached descendants are gone.
 * The surviving orchestrator calls this only after its OS owner proves an empty
 * tree. Requiring the still-provisional predecessor prevents the driver from
 * smuggling an authoritative terminal file across that ownership boundary.
 */
export async function finalizeParentOwnedBrowserSampleResult(
  path: string,
  candidate: unknown,
  topologyLock: VerifiedTestIceTopologyLock,
): Promise<ParentFinalizedBrowserSampleResult> {
  const terminal = parseBrowserSampleResult(candidate, topologyLock)
  if (terminal.resultStatus === 'provisional') {
    throw new Error('parent browser sample candidate is not terminal')
  }
  const predecessorSnapshot = await readStableRegularFileSnapshot(
    path,
    MAXIMUM_PARENT_FINAL_RESULT_BYTES,
    'parent browser sample provisional predecessor',
  )
  const predecessor = parseBrowserSampleResult(
    parseCanonicalJsonText(
      decodeUtf8(predecessorSnapshot.bytes, 'parent browser sample provisional predecessor'),
      'parent browser sample provisional predecessor',
    ),
    topologyLock,
  )
  if (predecessor.resultStatus !== 'provisional') {
    throw new Error('parent browser sample finalization requires a provisional predecessor')
  }
  if (sampleAuthority(predecessor) !== sampleAuthority(terminal)) {
    throw new Error('parent browser sample candidate differs from its provisional authority')
  }
  await writeAtomicJson(path, terminal)
  const finalSnapshot = await readStableRegularFileSnapshot(
    path,
    MAXIMUM_PARENT_FINAL_RESULT_BYTES,
    'parent browser sample final result',
  )
  const persisted = parseBrowserSampleResult(
    parseCanonicalJsonText(
      decodeUtf8(finalSnapshot.bytes, 'parent browser sample final result'),
      'parent browser sample final result',
    ),
    topologyLock,
  )
  if (JSON.stringify(persisted) !== JSON.stringify(terminal)) {
    throw new Error('parent browser sample final result changed during persistence')
  }
  return Object.freeze({ result: persisted, bytes: finalSnapshot.bytes })
}

async function replaceAtomic(source: string, destination: string): Promise<void> {
  let lastError: unknown
  for (let attempt = 1; attempt <= ATOMIC_REPLACE_ATTEMPTS; attempt += 1) {
    try {
      await rename(source, destination)
      return
    } catch (cause) {
      lastError = cause
      if (!replaceMayBeTransient(cause) || attempt === ATOMIC_REPLACE_ATTEMPTS) throw cause
      await delay(ATOMIC_REPLACE_RETRY_MS * attempt)
    }
  }
  throw lastError
}

function replaceMayBeTransient(cause: unknown): boolean {
  if (typeof cause !== 'object' || cause === null || !('code' in cause)) return false
  return cause.code === 'EACCES' || cause.code === 'EBUSY' || cause.code === 'EPERM'
}

async function syncDirectory(directory: string): Promise<void> {
  if (process.platform === 'win32') return
  const handle = await open(directory, 'r')
  try {
    await handle.sync()
  } finally {
    await handle.close()
  }
}

function delay(milliseconds: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, milliseconds))
}

function sampleAuthority(result: BrowserSampleResult): string {
  return JSON.stringify({
    schemaVersion: result.schemaVersion,
    runId: result.runId,
    runPolicy: result.runPolicy,
    suite: result.suite,
    browser: result.browser,
    sampleIndex: result.sampleIndex,
    checkoutSha: result.checkoutSha,
    topologyId: result.topologyId,
    topologyProfileSha256: result.topologyProfileSha256,
    topologyResolutionSha256: result.topologyResolutionSha256,
  })
}

function decodeUtf8(bytes: Uint8Array, label: string): string {
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch (cause) {
    throw new Error(`${label} is not valid UTF-8`, { cause })
  }
}

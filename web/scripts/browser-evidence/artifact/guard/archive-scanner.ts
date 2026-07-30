import { createHash } from 'node:crypto'
import type { BigIntStats } from 'node:fs'
import { lstat, open, type FileHandle } from 'node:fs/promises'
import { basename, extname } from 'node:path'

import {
  scanTrustedZip,
  TrustedZipFailure,
  type ArchiveByteSource,
  type TrustedZipEntry,
} from '../../archive/trusted-zip.ts'
import { requirePortableRelativePath } from '../../filesystem/portable-path.ts'
import type { ArtifactIndexEntry } from '../../result.ts'
import {
  GUARD_MAXIMUM_ARCHIVE_BYTES,
  GUARD_MAXIMUM_ARCHIVE_ENTRIES,
  GUARD_MAXIMUM_ARCHIVE_NESTING_DEPTH,
  GUARD_MAXIMUM_ARTIFACT_FILE_BYTES,
  GUARD_MAXIMUM_EXPANDED_ARCHIVE_BYTES,
  type GuardMatchEvidence,
} from '../guard-result.ts'
import { artifactAbsolutePath } from '../index.ts'
import {
  GuardFailure,
  type ExplicitGuardSecret,
  type ScanState,
} from './contract.ts'

const GITHUB_TOKEN_PATTERN = /(?:^|\W)(?:gh[pousr]_[A-Za-z0-9]{36,255}|github_pat_\w{20,255})(?:$|\W)/u
const TOKEN_SCAN_TAIL_BYTES = 512
const ZIP_PREFIX_BYTES = 4
const ZIP_END_OF_CENTRAL_DIRECTORY_SEARCH_BYTES = 65_557

interface ScannedFileBytes {
  readonly prefix: Uint8Array
  readonly tail: Uint8Array
  readonly byteLength: number
  readonly sha256: string
}

export function assertControlPlaneSecretFree(
  sampleResultBytes: Uint8Array,
  artifacts: readonly ArtifactIndexEntry[],
  explicitSecrets: readonly Uint8Array[],
): void {
  const scanner = new StreamSecretScanner(explicitSecrets)
  scanner.scan(sampleResultBytes)
  scanner.scan(Buffer.from(JSON.stringify(artifacts), 'utf8'))
  if (scanner.detectors.size > 0) {
    throw new GuardFailure('contract', 'browser evidence control-plane bytes contain a protected secret')
  }
}

export async function scanArtifact(
  artifact: ArtifactIndexEntry,
  artifactRoot: string,
  explicitSecrets: readonly Uint8Array[],
  state: ScanState,
  signal: AbortSignal,
): Promise<void> {
  signal.throwIfAborted()
  const path = artifactAbsolutePath(artifactRoot, artifact.relativePath)
  state.scannedFileCount += 1
  let reader: NodeFileReader | undefined
  try {
    reader = await NodeFileReader.open(path, signal)
    const scanner = new StreamSecretScanner(explicitSecrets)
    if (reader.byteLength !== artifact.byteLength) {
      throw new GuardFailure('contract', `artifact ${artifact.relativePath} length changed before its guard scan`)
    }
    const scanned = await reader.scan(scanner, artifact.byteLength)
    if (scanned.byteLength !== artifact.byteLength || scanned.sha256 !== artifact.sha256) {
      throw new GuardFailure('contract', `artifact ${artifact.relativePath} changed before its guard scan`)
    }
    recordMatches(state, artifact.artifactId, 'file', null, scanner.detectors)
    if (!isZipArtifact(artifact.relativePath, scanned.prefix, scanned.tail)) return
    state.observedMaximumArchiveDepth = Math.max(state.observedMaximumArchiveDepth, 1)
    state.observedArchiveBytes += artifact.byteLength
    if (state.observedArchiveBytes > GUARD_MAXIMUM_ARCHIVE_BYTES) {
      throw new GuardFailure('archive-byte-limit', 'archive bytes exceed the frozen guard limit')
    }
    await scanZip(reader, artifact.relativePath, artifact.artifactId, explicitSecrets, state, signal)
    await reader.assertStable()
  } catch (cause) {
    if (cause instanceof GuardFailure) throw cause
    throw new GuardFailure('scanner-crashed', `artifact scanner crashed for ${artifact.relativePath}`, {
      cause,
    })
  } finally {
    await reader?.close().catch(() => undefined)
  }
}

async function scanZip(
  reader: NodeFileReader,
  relativePath: string,
  artifactId: string,
  explicitSecrets: readonly Uint8Array[],
  state: ScanState,
  signal: AbortSignal,
): Promise<void> {
  const entryPaths = new Set<string>()
  const initialEntryCount = state.scannedArchiveEntryCount
  const initialExpandedBytes = state.expandedArchiveBytes
  let active: {
    readonly entry: TrustedZipEntry
    readonly entryPath: string
    readonly scanner: StreamSecretScanner | null
    readonly prefix: number[]
    readonly tail: ByteTail
  } | null = null
  try {
    await scanTrustedZip(reader, {
      maximumEntries: GUARD_MAXIMUM_ARCHIVE_ENTRIES - initialEntryCount,
      maximumExpandedBytes: GUARD_MAXIMUM_EXPANDED_ARCHIVE_BYTES - initialExpandedBytes,
      maximumPathBytes: 1_024,
    }, {
      start(entry) {
        signal.throwIfAborted()
        if (active !== null) throw new Error('trusted ZIP visitor received overlapping entries')
        state.scannedArchiveEntryCount += 1
        const entryPath = normalizedArchivePath(entry.path, entry.directory)
        if (entryPaths.has(entryPath)) {
          throw new GuardFailure('invalid-archive', 'ZIP contains duplicate entry paths')
        }
        entryPaths.add(entryPath)
        active = {
          entry,
          entryPath,
          scanner: entry.directory ? null : new StreamSecretScanner(explicitSecrets),
          prefix: [],
          tail: new ByteTail(ZIP_END_OF_CENTRAL_DIRECTORY_SEARCH_BYTES),
        }
      },
      chunk(entry, chunk) {
        signal.throwIfAborted()
        if (active === null || active.entry !== entry || active.scanner === null) {
          throw new Error('trusted ZIP visitor received bytes outside an active file entry')
        }
        state.expandedArchiveBytes += chunk.byteLength
        for (const byte of chunk.subarray(0, ZIP_PREFIX_BYTES - active.prefix.length)) {
          active.prefix.push(byte)
        }
        active.tail.push(chunk)
        active.scanner.scan(chunk)
      },
      end(entry) {
        signal.throwIfAborted()
        if (active === null || active.entry !== entry) {
          throw new Error('trusted ZIP visitor ended a different entry')
        }
        if (active.scanner !== null) {
          if (isZipByteSequence(Uint8Array.from(active.prefix), active.tail.bytes())) {
            state.observedMaximumArchiveDepth = GUARD_MAXIMUM_ARCHIVE_NESTING_DEPTH + 1
            throw new GuardFailure(
              'archive-nesting-limit',
              'nested ZIP archive exceeds the frozen depth limit',
            )
          }
          recordMatches(
            state,
            artifactId,
            'archive-entry',
            active.entryPath,
            active.scanner.detectors,
          )
        }
        active = null
      },
    })
  } catch (cause) {
    if (cause instanceof GuardFailure) throw cause
    if (cause instanceof TrustedZipFailure) {
      if (cause.kind === 'archive-entry-limit') {
        state.scannedArchiveEntryCount = Math.max(
          state.scannedArchiveEntryCount,
          initialEntryCount + (cause.observedEntryCount ?? 0),
        )
      }
      if (cause.kind === 'archive-expanded-byte-limit') {
        state.expandedArchiveBytes = Math.max(
          state.expandedArchiveBytes,
          initialExpandedBytes + (cause.observedExpandedBytes ?? 0),
        )
      }
      if (cause.kind !== 'invalid-archive') {
        throw new GuardFailure(cause.kind, cause.message, { cause })
      }
      throw new GuardFailure(
        'invalid-archive',
        `ZIP archive is malformed: ${basename(relativePath)} (${cause.message})`,
        { cause },
      )
    }
    throw cause
  }
}

class StreamSecretScanner {
  readonly #secrets: readonly Uint8Array[]
  readonly detectors = new Set<'explicit-secret' | 'github-token-pattern'>()
  #tail = Buffer.alloc(0)
  readonly #tailBytes: number

  constructor(secrets: readonly Uint8Array[]) {
    this.#secrets = secrets
    this.#tailBytes = Math.max(TOKEN_SCAN_TAIL_BYTES, ...secrets.map((secret) => secret.byteLength))
  }

  scan(chunk: Uint8Array): void {
    const combined = Buffer.concat([this.#tail, Buffer.from(chunk)])
    if (this.#secrets.some((secret) => combined.indexOf(secret) >= 0)) {
      this.detectors.add('explicit-secret')
    }
    if (GITHUB_TOKEN_PATTERN.test(combined.toString('latin1'))) {
      this.detectors.add('github-token-pattern')
    }
    const retained = Math.min(this.#tailBytes, combined.byteLength)
    this.#tail = combined.subarray(combined.byteLength - retained)
  }
}

class ByteTail {
  readonly #maximumBytes: number
  #value = Buffer.alloc(0)

  constructor(maximumBytes: number) {
    this.#maximumBytes = maximumBytes
  }

  push(chunk: Uint8Array): void {
    const combined = Buffer.concat([this.#value, Buffer.from(chunk)])
    this.#value = combined.subarray(Math.max(0, combined.byteLength - this.#maximumBytes))
  }

  bytes(): Uint8Array {
    return Uint8Array.from(this.#value)
  }
}

class NodeFileReader implements ArchiveByteSource {
  readonly #path: string
  readonly #handle: FileHandle
  readonly #pathBefore: BigIntStats
  readonly #openedBefore: BigIntStats
  readonly byteLength: number
  readonly #signal: AbortSignal

  private constructor(
    path: string,
    handle: FileHandle,
    pathBefore: BigIntStats,
    openedBefore: BigIntStats,
    signal: AbortSignal,
  ) {
    this.#path = path
    this.#handle = handle
    this.#pathBefore = pathBefore
    this.#openedBefore = openedBefore
    this.byteLength = Number(openedBefore.size)
    this.#signal = signal
  }

  static async open(path: string, signal: AbortSignal): Promise<NodeFileReader> {
    signal.throwIfAborted()
    const pathBefore = await lstat(path, { bigint: true })
    if (!pathBefore.isFile() || pathBefore.isSymbolicLink()) {
      throw new GuardFailure('contract', 'indexed artifact is not a regular file')
    }
    const handle = await open(path, 'r')
    try {
      const openedBefore = await handle.stat({ bigint: true })
      if (
        !sameFileIdentity(pathBefore, openedBefore) ||
        openedBefore.size > BigInt(GUARD_MAXIMUM_ARTIFACT_FILE_BYTES)
      ) {
        throw new GuardFailure('contract', 'indexed artifact changed while it was opened')
      }
      return new NodeFileReader(path, handle, pathBefore, openedBefore, signal)
    } catch (cause) {
      await handle.close().catch(() => undefined)
      throw cause
    }
  }

  async scan(scanner: StreamSecretScanner, maximumBytes: number): Promise<ScannedFileBytes> {
    const prefix: number[] = []
    const tail = new ByteTail(ZIP_END_OF_CENTRAL_DIRECTORY_SEARCH_BYTES)
    const digest = createHash('sha256')
    let byteLength = 0
    for await (const value of this.#handle.createReadStream({
      autoClose: false,
      start: 0,
      signal: this.#signal,
    })) {
      this.#signal.throwIfAborted()
      const chunk = Buffer.isBuffer(value) ? value : Buffer.from(value)
      for (const byte of chunk.subarray(0, ZIP_PREFIX_BYTES - prefix.length)) prefix.push(byte)
      tail.push(chunk)
      scanner.scan(chunk)
      digest.update(chunk)
      byteLength += chunk.byteLength
      if (byteLength > maximumBytes) {
        throw new GuardFailure('contract', 'indexed artifact grew beyond its byte authority during guard scan')
      }
    }
    await this.assertStable(byteLength)
    return Object.freeze({
      prefix: Uint8Array.from(prefix),
      tail: tail.bytes(),
      byteLength,
      sha256: digest.digest('hex'),
    })
  }

  async readExactly(index: number, length: number): Promise<Uint8Array> {
    this.#signal.throwIfAborted()
    const buffer = Buffer.allocUnsafe(length)
    let offset = 0
    while (offset < length) {
      this.#signal.throwIfAborted()
      const { bytesRead } = await this.#handle.read(buffer, offset, length - offset, index + offset)
      if (bytesRead === 0) break
      offset += bytesRead
    }
    return Uint8Array.from(buffer.subarray(0, offset))
  }

  async assertStable(observedBytes = Number(this.#openedBefore.size)): Promise<void> {
    this.#signal.throwIfAborted()
    const openedAfter = await this.#handle.stat({ bigint: true })
    const pathAfter = await lstat(this.#path, { bigint: true })
    if (
      !sameFileIdentity(this.#pathBefore, openedAfter) ||
      !sameFileIdentity(openedAfter, pathAfter) ||
      this.#openedBefore.size !== openedAfter.size ||
      this.#openedBefore.mtimeNs !== openedAfter.mtimeNs ||
      this.#openedBefore.ctimeNs !== openedAfter.ctimeNs ||
      openedAfter.size !== BigInt(observedBytes)
    ) {
      throw new GuardFailure('contract', 'indexed artifact changed during its guard scan')
    }
  }

  async close(): Promise<void> {
    await this.#handle.close()
  }
}

export function sameFileIdentity(left: BigIntStats, right: BigIntStats): boolean {
  return left.dev === right.dev && left.ino === right.ino
}

function recordMatches(
  state: ScanState,
  artifactId: string,
  location: GuardMatchEvidence['location'],
  archiveEntryPath: string | null,
  detectors: ReadonlySet<GuardMatchEvidence['detector']>,
): void {
  for (const detector of detectors) {
    state.matches.push(Object.freeze({ artifactId, location, archiveEntryPath, detector }))
  }
}

export function parseExplicitSecrets(secrets: readonly ExplicitGuardSecret[]): readonly Uint8Array[] {
  const values = new Map<string, Uint8Array>()
  for (const secret of secrets) {
    if (secret.value.length === 0) throw new GuardFailure('contract', 'explicit guard secrets must be non-empty')
    const bytes = Buffer.from(secret.value, 'utf8')
    values.set(bytes.toString('base64'), bytes)
  }
  return Object.freeze([...values.values()])
}

function normalizedArchivePath(filename: string, directory: boolean): string {
  const value = directory && filename.endsWith('/') ? filename.slice(0, -1) : filename
  try {
    requirePortableRelativePath(value, 'ZIP entry path')
  } catch (cause) {
    throw new GuardFailure('archive-path', 'ZIP entry path is not portable and root-confined', { cause })
  }
  if (Buffer.byteLength(value, 'utf8') > 1_024) {
    throw new GuardFailure('archive-path', 'ZIP entry path exceeds the guard limit')
  }
  return value
}

function isZipArtifact(relativePath: string, prefix: Uint8Array, tail: Uint8Array): boolean {
  return extname(relativePath).toLowerCase() === '.zip' || isZipByteSequence(prefix, tail)
}

function isZipByteSequence(prefix: Uint8Array, tail: Uint8Array): boolean {
  return isZipPrefix(prefix) || containsZipEndOfCentralDirectory(tail)
}

function isZipPrefix(prefix: Uint8Array): boolean {
  if (prefix.length < ZIP_PREFIX_BYTES) return false
  return prefix[0] === 0x50 && prefix[1] === 0x4b && (
    (prefix[2] === 0x03 && prefix[3] === 0x04) ||
    (prefix[2] === 0x05 && prefix[3] === 0x06) ||
    (prefix[2] === 0x07 && prefix[3] === 0x08)
  )
}

function containsZipEndOfCentralDirectory(tail: Uint8Array): boolean {
  for (let index = 0; index <= tail.length - ZIP_PREFIX_BYTES; index += 1) {
    if (
      tail[index] === 0x50 && tail[index + 1] === 0x4b &&
      tail[index + 2] === 0x05 && tail[index + 3] === 0x06
    ) return true
  }
  return false
}

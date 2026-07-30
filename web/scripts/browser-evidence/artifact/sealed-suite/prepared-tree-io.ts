import { createHash } from 'node:crypto'
import type { BigIntStats } from 'node:fs'
import { lstat, open, type FileHandle } from 'node:fs/promises'
import { join } from 'node:path'

import type {
  ExistingDirectoryPublisherSnapshot,
} from '../../../browser-network-matrix/cli/publisher-helper-protocol.ts'
import { parseCanonicalJsonText } from '../../contract/strict-json.ts'
import type { ArtifactIndexEntry, BrowserSampleResult } from '../../result.ts'
import { sha256Bytes } from '../manifest.ts'
import type { GuardUploadFileAuthority } from './contract.ts'

const COPY_BUFFER_BYTES = 64 * 1_024

export async function writePreparedFile(
  path: string,
  bytes: Uint8Array,
  label: string,
  signal: AbortSignal,
): Promise<void> {
  signal.throwIfAborted()
  const namedBefore = await lstat(path, { bigint: true })
  requireRegularFileMetadata(namedBefore, label)
  if (namedBefore.size !== BigInt(bytes.byteLength)) {
    throw new Error(`${label} native-prepared size differs from its byte authority`)
  }
  const handle = await open(path, 'r+')
  try {
    const openedBefore = await handle.stat({ bigint: true })
    if (!sameIdentity(namedBefore, openedBefore) || openedBefore.size !== BigInt(bytes.byteLength)) {
      throw new Error(`${label} changed while its prepared file was opened`)
    }
    await writeEntireBuffer(handle, bytes, 0, signal)
    await handle.sync()
    const [openedAfter, namedAfter] = await Promise.all([
      handle.stat({ bigint: true }),
      lstat(path, { bigint: true }),
    ])
    if (!sameIdentity(openedBefore, openedAfter) || !sameIdentity(openedAfter, namedAfter) ||
        openedAfter.size !== BigInt(bytes.byteLength)) {
      throw new Error(`${label} changed while bytes were materialized`)
    }
  } finally {
    await handle.close()
  }
}

export async function copyVerifiedArtifact(
  sourcePath: string,
  destinationPath: string,
  expected: ArtifactIndexEntry,
  signal: AbortSignal,
): Promise<void> {
  signal.throwIfAborted()
  const [sourceNamed, destinationNamed] = await Promise.all([
    lstat(sourcePath, { bigint: true }),
    lstat(destinationPath, { bigint: true }),
  ])
  requireRegularFileMetadata(sourceNamed, `artifact ${expected.relativePath}`)
  requireRegularFileMetadata(destinationNamed, `prepared artifact ${expected.relativePath}`)
  if (sourceNamed.size !== BigInt(expected.byteLength) ||
      destinationNamed.size !== BigInt(expected.byteLength)) {
    throw new Error(`artifact ${expected.relativePath} length differs before guard staging`)
  }
  const source = await open(sourcePath, 'r')
  let destination: FileHandle | undefined
  try {
    destination = await open(destinationPath, 'r+')
    const [sourceBefore, destinationBefore] = await Promise.all([
      source.stat({ bigint: true }),
      destination.stat({ bigint: true }),
    ])
    if (
      !sameIdentity(sourceNamed, sourceBefore) || !sameRevision(sourceNamed, sourceBefore) ||
      !sameIdentity(destinationNamed, destinationBefore) ||
      destinationBefore.size !== BigInt(expected.byteLength)
    ) throw new Error(`artifact ${expected.relativePath} changed while opened for guard staging`)
    const digest = createHash('sha256')
    const buffer = Buffer.allocUnsafe(COPY_BUFFER_BYTES)
    let offset = 0
    while (offset < expected.byteLength) {
      signal.throwIfAborted()
      const requested = Math.min(buffer.byteLength, expected.byteLength - offset)
      const { bytesRead } = await source.read(buffer, 0, requested, offset)
      if (bytesRead < 1) throw new Error(`artifact ${expected.relativePath} ended during guard staging`)
      const chunk = buffer.subarray(0, bytesRead)
      await writeEntireBuffer(destination, chunk, offset, signal)
      digest.update(chunk)
      offset += bytesRead
    }
    await destination.sync()
    const [sourceAfter, sourceNamedAfter, destinationAfter, destinationNamedAfter] = await Promise.all([
      source.stat({ bigint: true }),
      lstat(sourcePath, { bigint: true }),
      destination.stat({ bigint: true }),
      lstat(destinationPath, { bigint: true }),
    ])
    if (
      !sameIdentity(sourceBefore, sourceAfter) || !sameIdentity(sourceAfter, sourceNamedAfter) ||
      !sameRevision(sourceBefore, sourceAfter) || !sameRevision(sourceAfter, sourceNamedAfter) ||
      !sameIdentity(destinationBefore, destinationAfter) ||
      !sameIdentity(destinationAfter, destinationNamedAfter) ||
      destinationAfter.size !== BigInt(expected.byteLength) || digest.digest('hex') !== expected.sha256
    ) throw new Error(`artifact ${expected.relativePath} changed while copied into guard staging`)
  } finally {
    await destination?.close().catch(() => undefined)
    await source.close().catch(() => undefined)
  }
}

async function writeEntireBuffer(
  handle: FileHandle,
  bytes: Uint8Array,
  initialOffset = 0,
  signal?: AbortSignal,
): Promise<void> {
  let offset = 0
  while (offset < bytes.byteLength) {
    signal?.throwIfAborted()
    const { bytesWritten } = await handle.write(
      bytes,
      offset,
      bytes.byteLength - offset,
      initialOffset + offset,
    )
    if (bytesWritten < 1) throw new Error('guard upload destination stopped accepting bytes')
    offset += bytesWritten
  }
}

export async function assertDirectoryAncestry(
  root: string,
  segments: readonly string[],
  label: string,
  signal: AbortSignal,
): Promise<void> {
  signal.throwIfAborted()
  await requireRegularDirectory(root, `${label} root`)
  let current = root
  for (const segment of segments) {
    signal.throwIfAborted()
    current = join(current, segment)
    await requireRegularDirectory(current, `${label} directory`)
  }
}

async function requireRegularDirectory(path: string, label: string): Promise<void> {
  const metadata = await lstat(path, { bigint: true })
  if (!metadata.isDirectory() || metadata.isSymbolicLink() || metadata.ino === 0n) {
    throw new Error(`${label} is not a regular identity-bearing directory`)
  }
}

function requireRegularFileMetadata(metadata: BigIntStats, label: string): void {
  if (!metadata.isFile() || metadata.isSymbolicLink() || metadata.ino === 0n) {
    throw new Error(`${label} is not a regular identity-bearing file`)
  }
}

export function snapshotMap(
  values: readonly ExistingDirectoryPublisherSnapshot[],
): Map<string, ExistingDirectoryPublisherSnapshot> {
  const result = new Map<string, ExistingDirectoryPublisherSnapshot>()
  for (const value of values) {
    if (result.has(value.relativePath)) throw new Error('native publisher repeated a snapshot path')
    result.set(value.relativePath, value)
  }
  return result
}

export function requireSnapshotAuthority(
  snapshots: ReadonlyMap<string, ExistingDirectoryPublisherSnapshot>,
  authority: GuardUploadFileAuthority,
): ExistingDirectoryPublisherSnapshot {
  return requireSnapshot(snapshots, authority.relativePath, authority.byteLength, authority.sha256)
}

export function requireSnapshot(
  snapshots: ReadonlyMap<string, ExistingDirectoryPublisherSnapshot>,
  relativePath: string,
  byteLength: string,
  sha256: string,
): ExistingDirectoryPublisherSnapshot {
  const snapshot = snapshots.get(relativePath)
  if (snapshot === undefined || snapshot.byteLength !== byteLength || snapshot.sha256 !== sha256 ||
      String(snapshot.bytes.byteLength) !== byteLength || sha256Bytes(snapshot.bytes) !== sha256) {
    throw new Error(`native publisher snapshot ${relativePath} differs from its manifest authority`)
  }
  return snapshot
}

export function assertSampleResultSnapshot(bytes: Uint8Array, sample: BrowserSampleResult): void {
  const parsed = parseCanonicalJsonText(
    decodeUtf8(bytes, 'guard upload sample result'),
    'guard upload sample result',
  )
  if (JSON.stringify(parsed) !== JSON.stringify(sample)) {
    throw new Error('guard upload sample object differs from its immutable result bytes')
  }
}

export function decodeUtf8(bytes: Uint8Array, label: string): string {
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch {
    throw new Error(`${label} is not valid UTF-8`)
  }
}

function sameIdentity(left: BigIntStats, right: BigIntStats): boolean {
  return left.dev === right.dev && left.ino === right.ino
}

function sameRevision(left: BigIntStats, right: BigIntStats): boolean {
  return left.size === right.size && left.mtimeNs === right.mtimeNs && left.ctimeNs === right.ctimeNs
}

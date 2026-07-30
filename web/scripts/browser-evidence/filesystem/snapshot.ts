import { createHash } from 'node:crypto'
import type { Stats } from 'node:fs'
import { lstat, open } from 'node:fs/promises'

export interface RegularFileSnapshot {
  readonly bytes: Buffer
  readonly sha256: string
}

/**
 * Evidence contracts cross a process boundary. Reading through one descriptor,
 * with a hard byte ceiling and path-identity checks, prevents symlink swaps or
 * concurrent growth from changing which bytes the parent actually authorizes.
 */
export async function readStableRegularFileSnapshot(
  path: string,
  maximumBytes: number,
  label: string,
): Promise<RegularFileSnapshot> {
  if (!Number.isSafeInteger(maximumBytes) || maximumBytes < 1) {
    throw new Error(`${label} byte limit must be a positive safe integer`)
  }
  const pathBefore = await lstat(path)
  requireRegularFile(pathBefore, label)
  requireWithinLimit(pathBefore.size, maximumBytes, label)
  const handle = await open(path, 'r')
  try {
    const openedBefore = await handle.stat()
    requireRegularFile(openedBefore, label)
    requireWithinLimit(openedBefore.size, maximumBytes, label)
    if (!sameIdentity(pathBefore, openedBefore) || !sameRevision(pathBefore, openedBefore)) {
      throw new Error(`${label} changed while it was opened`)
    }
    const chunks: Buffer[] = []
    const digest = createHash('sha256')
    let byteLength = 0
    for await (const value of handle.createReadStream({
      autoClose: false,
      start: 0,
      // FileHandle streams use an inclusive end; max+1 bytes exposes growth
      // without ever buffering more than the frozen ceiling plus one byte.
      end: maximumBytes,
    })) {
      const chunk = Buffer.isBuffer(value) ? value : Buffer.from(value)
      byteLength += chunk.byteLength
      requireWithinLimit(byteLength, maximumBytes, label)
      chunks.push(chunk)
      digest.update(chunk)
    }
    const openedAfter = await handle.stat()
    const pathAfter = await lstat(path)
    if (
      !sameIdentity(openedBefore, openedAfter) || !sameIdentity(openedAfter, pathAfter) ||
      !sameRevision(openedBefore, openedAfter) || !sameRevision(openedAfter, pathAfter) ||
      openedAfter.size !== byteLength
    ) {
      throw new Error(`${label} changed while its immutable snapshot was read`)
    }
    return Object.freeze({
      bytes: Buffer.concat(chunks, byteLength),
      sha256: digest.digest('hex'),
    })
  } finally {
    await handle.close()
  }
}

function requireRegularFile(metadata: Stats, label: string): void {
  if (!metadata.isFile() || metadata.isSymbolicLink()) {
    throw new Error(`${label} path is not a regular file`)
  }
}

function requireWithinLimit(byteLength: number, maximumBytes: number, label: string): void {
  if (byteLength > maximumBytes) throw new Error(`${label} exceeds the frozen contract byte limit`)
}

function sameIdentity(left: Stats, right: Stats): boolean {
  return left.dev === right.dev && left.ino === right.ino
}

function sameRevision(left: Stats, right: Stats): boolean {
  return left.size === right.size && left.mtimeMs === right.mtimeMs && left.ctimeMs === right.ctimeMs
}

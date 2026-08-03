import type { BigIntStats } from 'node:fs'
import { lstat, open } from 'node:fs/promises'
import type { FileHandle } from 'node:fs/promises'
import { isAbsolute, join, resolve } from 'node:path'

import { sameFileIdentity } from './archive-scanner.ts'
import { GuardFailure } from './contract.ts'

export interface ArtifactRootLease {
  readonly path: string
  assertStable(): Promise<void>
  assertAncestry(relativePath: string): Promise<void>
  close(): Promise<void>
}

class HeldArtifactRoot implements ArtifactRootLease {
  readonly path: string
  readonly #handle: FileHandle
  readonly #identity: BigIntStats

  constructor(path: string, handle: FileHandle, identity: BigIntStats) {
    this.path = path
    this.#handle = handle
    this.#identity = identity
  }

  async assertStable(): Promise<void> {
    const [opened, named] = await Promise.all([
      this.#handle.stat({ bigint: true }),
      lstat(this.path, { bigint: true }),
    ])
    requireArtifactDirectory(opened, 'artifact root')
    requireArtifactDirectory(named, 'artifact root')
    if (!sameFileIdentity(this.#identity, opened) || !sameFileIdentity(opened, named)) {
      throw new GuardFailure('contract', 'artifact root no longer names its owner-held directory')
    }
  }

  async assertAncestry(relativePath: string): Promise<void> {
    const segments = relativePath.split('/')
    let current = this.path
    for (const segment of segments.slice(0, -1)) {
      current = join(current, segment)
      requireArtifactDirectory(await lstat(current, { bigint: true }), `artifact directory ${segment}`)
    }
  }

  async close(): Promise<void> {
    await this.#handle.close()
  }
}

export async function holdArtifactRoot(path: string, signal: AbortSignal): Promise<ArtifactRootLease> {
  signal.throwIfAborted()
  const canonicalPath = requireCanonicalArtifactRoot(path)
  const namedBefore = await lstat(canonicalPath, { bigint: true })
  requireArtifactDirectory(namedBefore, 'artifact root')
  const handle = await open(canonicalPath, 'r')
  try {
    const identity = await handle.stat({ bigint: true })
    const namedAfter = await lstat(canonicalPath, { bigint: true })
    requireArtifactDirectory(identity, 'artifact root')
    requireArtifactDirectory(namedAfter, 'artifact root')
    if (!sameFileIdentity(namedBefore, identity) || !sameFileIdentity(identity, namedAfter)) {
      throw new GuardFailure('contract', 'artifact root changed while its authority was acquired')
    }
    return Object.freeze(new HeldArtifactRoot(canonicalPath, handle, identity))
  } catch (cause) {
    await handle.close().catch(() => undefined)
    throw cause
  }
}

export function requireCanonicalArtifactRoot(path: string): string {
  if (!isAbsolute(path) || resolve(path) !== path) {
    throw new GuardFailure('contract', 'artifact root must be canonical and absolute')
  }
  return path
}

function requireArtifactDirectory(metadata: BigIntStats, label: string): void {
  if (!metadata.isDirectory() || metadata.isSymbolicLink()) {
    throw new GuardFailure('contract', `${label} is not a regular no-follow directory`)
  }
}

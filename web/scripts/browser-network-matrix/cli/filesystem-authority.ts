import type { BigIntStats } from 'node:fs'
import { lstat, open } from 'node:fs/promises'

export interface HeldDirectoryAuthority {
  readonly path: string
  readonly identity: BigIntStats
  readonly handle: Awaited<ReturnType<typeof open>>
}

export async function openHeldDirectory(
  path: string,
  label: string,
): Promise<HeldDirectoryAuthority> {
  const named = await lstat(path, { bigint: true })
  requireRegularDirectory(named, label)
  const handle = await open(path, 'r')
  try {
    const opened = await handle.stat({ bigint: true })
    requireRegularDirectory(opened, label)
    if (!sameFileIdentity(named, opened)) throw new Error(`${label} changed while it was opened`)
    return Object.freeze({ path, identity: named, handle })
  } catch (cause) {
    await handle.close().catch(() => undefined)
    throw cause
  }
}

export async function assertHeldDirectory(
  authority: HeldDirectoryAuthority,
  label: string,
): Promise<void> {
  const [named, opened] = await Promise.all([
    lstat(authority.path, { bigint: true }),
    authority.handle.stat({ bigint: true }),
  ])
  requireRegularDirectory(named, label)
  requireRegularDirectory(opened, label)
  if (
    !sameFileIdentity(authority.identity, named) ||
    !sameFileIdentity(named, opened)
  ) throw new Error(`${label} identity changed during publication`)
}

export async function requirePathAbsent(path: string, label: string): Promise<void> {
  try {
    await lstat(path)
  } catch (cause) {
    if (isFileSystemError(cause, 'ENOENT')) return
    throw cause
  }
  throw new Error(`${label} already exists; run-attempt outputs are immutable`)
}

export function requireRegularDirectory(metadata: BigIntStats, label: string): void {
  if (!metadata.isDirectory()) throw new Error(`${label} must be a regular directory authority`)
}

export function sameFileIdentity(left: BigIntStats, right: BigIntStats): boolean {
  return left.dev === right.dev && left.ino === right.ino
}

export function isFileSystemError(cause: unknown, code: string): cause is NodeJS.ErrnoException {
  return cause instanceof Error && 'code' in cause && cause.code === code
}

export async function syncHeldDirectory(authority: HeldDirectoryAuthority): Promise<void> {
  if (process.platform !== 'win32') await authority.handle.sync()
}

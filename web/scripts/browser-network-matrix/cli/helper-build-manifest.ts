import type { BigIntStats } from 'node:fs'
import { lstat, open, type FileHandle } from 'node:fs/promises'
import { isAbsolute, resolve } from 'node:path'

import {
  requireArray,
  requireEnum,
  requireExactKeys,
  requireLiteral,
  requireRecord,
  requireString,
} from '../../browser-evidence/contract/json.ts'
import { parseCanonicalJsonText } from '../../browser-evidence/contract/strict-json.ts'
import { HELPER_BUILD_MANIFEST_SCHEMA_VERSION } from './build-helpers.mjs'

const MAXIMUM_HELPER_MANIFEST_BYTES = 16 * 1024
const MANIFEST_KEYS = Object.freeze(['schemaVersion', 'platform', 'architecture', 'helpers'])
const HELPER_KEYS = Object.freeze(['role', 'path'])
const PLATFORMS = Object.freeze(['win32', 'linux'] as const)
const ARCHITECTURES = Object.freeze(['amd64', 'arm64'] as const)
const ROLES = Object.freeze(['artifact-publisher', 'test-process-owner'] as const)
const UTF8_DECODER = new TextDecoder('utf-8', { fatal: true })

export type HelperBuildRole = typeof ROLES[number]

export interface HelperBuildEntry {
  readonly role: HelperBuildRole
  readonly path: string
}

export interface HelperBuildManifest {
  readonly platform: 'win32' | 'linux'
  readonly architecture: 'amd64' | 'arm64'
  readonly helpers: readonly HelperBuildEntry[]
}

export interface HelperBuildManifestHandle {
  readonly manifest: HelperBuildManifest
  assertUnchanged(): Promise<void>
  close(): Promise<void>
}

export async function openHelperBuildManifest(
  pathValue: string,
  platform: 'win32' | 'linux',
): Promise<HelperBuildManifestHandle> {
  requireCanonicalAbsolutePath(pathValue, 'helper manifest')
  const named = await lstat(pathValue, { bigint: true })
  requireBoundedRegularFile(named, 'helper manifest')
  const handle = await open(pathValue, 'r')
  try {
    const opened = await handle.stat({ bigint: true })
    requireBoundedRegularFile(opened, 'helper manifest')
    if (!sameFileRevision(named, opened)) {
      throw new Error('helper manifest changed while it was opened')
    }
    const encoded = await readHeldFile(handle, opened, 'helper manifest')
    const manifest = parseHelperBuildManifest(encoded, platform)
    return new HeldHelperBuildManifest(pathValue, handle, opened, encoded, manifest)
  } catch (cause) {
    await handle.close().catch(() => undefined)
    throw cause
  }
}

class HeldHelperBuildManifest implements HelperBuildManifestHandle {
  readonly manifest: HelperBuildManifest
  readonly #path: string
  readonly #handle: FileHandle
  readonly #identity: BigIntStats
  readonly #encoded: Uint8Array

  constructor(
    path: string,
    handle: FileHandle,
    identity: BigIntStats,
    encoded: Uint8Array,
    manifest: HelperBuildManifest,
  ) {
    this.#path = path
    this.#handle = handle
    this.#identity = identity
    this.#encoded = Uint8Array.from(encoded)
    this.manifest = manifest
  }

  async assertUnchanged(): Promise<void> {
    const [named, opened] = await Promise.all([
      lstat(this.#path, { bigint: true }),
      this.#handle.stat({ bigint: true }),
    ])
    requireBoundedRegularFile(named, 'helper manifest')
    requireBoundedRegularFile(opened, 'helper manifest')
    if (!sameFileRevision(this.#identity, named) || !sameFileRevision(named, opened)) {
      throw new Error('helper manifest identity or revision changed')
    }
    const actual = await readHeldFile(this.#handle, opened, 'helper manifest')
    if (!Buffer.from(actual).equals(Buffer.from(this.#encoded))) {
      throw new Error('helper manifest canonical bytes changed')
    }
  }

  close(): Promise<void> {
    return this.#handle.close()
  }
}

function parseHelperBuildManifest(
  encoded: Uint8Array,
  expectedPlatform: 'win32' | 'linux',
): HelperBuildManifest {
  let text: string
  try {
    text = UTF8_DECODER.decode(encoded)
  } catch {
    throw new Error('helper manifest is not valid UTF-8')
  }
  const parsed = requireRecord(
    parseCanonicalJsonText(text, 'helper manifest'),
    'helper manifest',
  )
  requireExactKeys(parsed, MANIFEST_KEYS, [], 'helper manifest')
  requireKeyOrder(parsed, MANIFEST_KEYS, 'helper manifest')
  if (text !== `${JSON.stringify(parsed)}\n`) throw new Error('helper manifest is not canonical JSON')
  requireLiteral(
    parsed.schemaVersion,
    HELPER_BUILD_MANIFEST_SCHEMA_VERSION,
    'helper manifest schema version',
  )
  const platform = requireEnum(parsed.platform, PLATFORMS, 'helper manifest platform')
  if (platform !== expectedPlatform) throw new Error('helper manifest platform differs from runtime')
  const architecture = requireEnum(
    parsed.architecture,
    ARCHITECTURES,
    'helper manifest architecture',
  )
  if (architecture !== runtimeGoArchitecture()) {
    throw new Error('helper manifest architecture differs from runtime')
  }
  const values = requireArray(parsed.helpers, 'helper manifest helpers')
  const expectedRoles: readonly HelperBuildRole[] = ['artifact-publisher', 'test-process-owner']
  if (values.length !== expectedRoles.length) {
    throw new Error('helper manifest has a missing or extra platform helper')
  }
  const helpers = Object.freeze(values.map((value, index) => {
    const label = `helper manifest entry ${index}`
    const entry = requireRecord(value, label)
    requireExactKeys(entry, HELPER_KEYS, [], label)
    requireKeyOrder(entry, HELPER_KEYS, label)
    const role = requireEnum(entry.role, ROLES, `${label} role`)
    if (role !== expectedRoles[index]) throw new Error(`${label} role is not platform-exact`)
    const path = requireString(entry.path, `${label} path`, 32_767)
    requireCanonicalAbsolutePath(path, label)
    return Object.freeze({
      role,
      path,
    })
  }))
  return Object.freeze({ platform, architecture, helpers })
}

async function readHeldFile(
  handle: FileHandle,
  metadata: BigIntStats,
  label: string,
): Promise<Uint8Array> {
  const length = Number(metadata.size)
  const encoded = Buffer.allocUnsafe(length)
  let offset = 0
  while (offset < length) {
    const { bytesRead } = await handle.read(encoded, offset, length - offset, offset)
    if (bytesRead < 1) throw new Error(`${label} ended while read`)
    offset += bytesRead
  }
  const after = await handle.stat({ bigint: true })
  if (!sameFileRevision(metadata, after)) throw new Error(`${label} changed while read`)
  return Uint8Array.from(encoded)
}

function requireBoundedRegularFile(metadata: BigIntStats, label: string): void {
  if (
    !metadata.isFile() || metadata.isSymbolicLink() || metadata.ino === 0n ||
    metadata.size < 1n || metadata.size > BigInt(MAXIMUM_HELPER_MANIFEST_BYTES)
  ) throw new Error(`${label} must be a bounded regular file with native identity`)
}

function requireCanonicalAbsolutePath(value: string, label: string): void {
  if (!isAbsolute(value) || resolve(value) !== value || value.includes('\0')) {
    throw new Error(`${label} path must be explicit, absolute, and canonical`)
  }
}

function sameFileRevision(left: BigIntStats, right: BigIntStats): boolean {
  return left.dev === right.dev && left.ino === right.ino && left.size === right.size &&
    left.mtimeNs === right.mtimeNs && left.ctimeNs === right.ctimeNs
}

function requireKeyOrder(
  value: Readonly<Record<string, unknown>>,
  expected: readonly string[],
  label: string,
): void {
  if (Object.keys(value).some((key, index) => key !== expected[index])) {
    throw new Error(`${label} fields are not in canonical order`)
  }
}

function runtimeGoArchitecture(): 'amd64' | 'arm64' {
  if (process.arch === 'x64') return 'amd64'
  if (process.arch === 'arm64') return 'arm64'
  throw new Error(`helper manifest runtime architecture ${process.arch} is unsupported`)
}

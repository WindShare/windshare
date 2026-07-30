import { createHash } from 'node:crypto'
import { lstat, open, readdir } from 'node:fs/promises'
import { extname, join, relative, sep } from 'node:path'

import type { ChildArtifactRegistration } from '../child-evidence.ts'
import { artifactIdForManifest } from './manifest.ts'
import { comparePortablePaths, requirePortableRelativePath } from '../filesystem/portable-path.ts'
import {
  ARTIFACT_KINDS,
  type ArtifactIndexEntry,
  type ArtifactKind,
} from '../result.ts'
import type { BrowserSuite } from '../vocabulary.ts'

export const MAXIMUM_INDEXED_ARTIFACT_FILES = 10_000 as const

interface ArtifactFile {
  readonly absolutePath: string
  readonly relativePath: string
}

export interface ArtifactIndexResult {
  readonly artifacts: readonly ArtifactIndexEntry[]
  readonly integrityViolations: readonly string[]
}

export async function indexArtifacts(
  artifactRoot: string,
  suite: BrowserSuite,
  registrations: readonly ChildArtifactRegistration[],
): Promise<ArtifactIndexResult> {
  const violations: string[] = []
  const files = await enumerateArtifactFiles(artifactRoot, violations)
  const registrationMap = collectRegistrations(registrations, violations)
  const artifacts: ArtifactIndexEntry[] = []
  for (const file of files) {
    try {
      const registration = registrationMap.get(file.relativePath)
      const kind = registration?.kind ?? inferArtifactKind(file.relativePath, suite)
      const mediaType = registration?.mediaType ?? inferMediaType(file.relativePath, kind)
      const digest = await digestStableFile(file.absolutePath)
      const manifest = {
        kind,
        relativePath: file.relativePath,
        mediaType,
        byteLength: digest.byteLength,
        sha256: digest.sha256,
      }
      artifacts.push(Object.freeze({
        artifactId: artifactIdForManifest(manifest),
        ...manifest,
      }))
      registrationMap.delete(file.relativePath)
    } catch (cause) {
      violations.push(`artifact ${file.relativePath} cannot be indexed: ${errorMessage(cause)}`)
    }
  }
  for (const missing of registrationMap.keys()) {
    violations.push(`registered artifact ${missing} is missing from the sample workspace`)
  }
  artifacts.sort((left, right) => comparePortablePaths(left.relativePath, right.relativePath) ||
    compareStrings(left.artifactId, right.artifactId))
  return Object.freeze({
    artifacts: Object.freeze(artifacts),
    integrityViolations: normalizedViolations(violations),
  })
}

export function artifactAbsolutePath(artifactRoot: string, relativePath: string): string {
  return join(artifactRoot, ...relativePath.split('/'))
}

async function enumerateArtifactFiles(
  artifactRoot: string,
  violations: string[],
): Promise<readonly ArtifactFile[]> {
  const files: ArtifactFile[] = []
  try {
    await walkArtifactDirectory(artifactRoot, artifactRoot, files, violations)
  } catch (cause) {
    violations.push(`artifact workspace cannot be enumerated: ${errorMessage(cause)}`)
  }
  files.sort((left, right) => comparePortablePaths(left.relativePath, right.relativePath))
  return files
}

async function walkArtifactDirectory(
  artifactRoot: string,
  directory: string,
  files: ArtifactFile[],
  violations: string[],
): Promise<void> {
  const entries = await readdir(directory, { withFileTypes: true })
  entries.sort((left, right) => compareStrings(left.name, right.name))
  for (const entry of entries) {
    const absolutePath = join(directory, entry.name)
    const relativePath = portableRelativePath(artifactRoot, absolutePath)
    if (entry.isSymbolicLink()) {
      violations.push(`artifact workspace contains symbolic link ${relativePath}`)
    } else if (entry.isDirectory()) {
      await walkArtifactDirectory(artifactRoot, absolutePath, files, violations)
    } else if (entry.isFile()) {
      files.push({ absolutePath, relativePath })
      if (files.length > MAXIMUM_INDEXED_ARTIFACT_FILES) {
        throw new Error(`artifact workspace exceeds ${MAXIMUM_INDEXED_ARTIFACT_FILES} files`)
      }
    } else {
      violations.push(`artifact workspace contains unsupported filesystem entry ${relativePath}`)
    }
  }
}

function collectRegistrations(
  registrations: readonly ChildArtifactRegistration[],
  violations: string[],
): Map<string, ChildArtifactRegistration> {
  const result = new Map<string, ChildArtifactRegistration>()
  for (const registration of registrations) {
    if (!ARTIFACT_KINDS.includes(registration.kind)) {
      violations.push(`artifact registration ${registration.relativePath} has an unknown kind`)
      continue
    }
    if (result.has(registration.relativePath)) {
      violations.push(`artifact ${registration.relativePath} is registered more than once`)
      continue
    }
    result.set(registration.relativePath, registration)
  }
  return result
}

async function digestStableFile(path: string): Promise<{ readonly byteLength: number; readonly sha256: string }> {
  const pathBefore = await lstat(path)
  if (!pathBefore.isFile() || pathBefore.isSymbolicLink()) throw new Error('path is not a regular file')
  const handle = await open(path, 'r')
  try {
    const openedBefore = await handle.stat()
    if (!sameFileIdentity(pathBefore, openedBefore)) throw new Error('file changed while it was opened')
    const digest = createHash('sha256')
    let byteLength = 0
    for await (const value of handle.createReadStream({ autoClose: false })) {
      const chunk = Buffer.isBuffer(value) ? value : Buffer.from(value)
      digest.update(chunk)
      byteLength += chunk.byteLength
    }
    const openedAfter = await handle.stat()
    const pathAfter = await lstat(path)
    if (
      openedBefore.size !== openedAfter.size || openedBefore.mtimeMs !== openedAfter.mtimeMs ||
      !sameFileIdentity(openedBefore, openedAfter) || !sameFileIdentity(openedAfter, pathAfter) ||
      openedAfter.size !== byteLength
    ) throw new Error('file changed while it was indexed')
    return { byteLength, sha256: digest.digest('hex') }
  } finally {
    await handle.close()
  }
}

function sameFileIdentity(left: Awaited<ReturnType<typeof lstat>>, right: Awaited<ReturnType<typeof lstat>>): boolean {
  return left.dev === right.dev && left.ino === right.ino
}

function inferArtifactKind(relativePath: string, suite: BrowserSuite): ArtifactKind {
  const lower = relativePath.toLowerCase()
  if (lower === 'runner/stdout.log') return 'runner-stdout'
  if (lower === 'runner/stderr.log') return 'runner-stderr'
  if (lower === 'runner/diagnostic.json') return 'result-diagnostic'
  if (lower === 'child/evidence.jsonl') {
    return suite === 'main' ? 'attempt-evidence' : 'native-interop-evidence'
  }
  if (lower.endsWith('.zip')) return 'trace'
  if (/\.(?:webm|mp4)$/u.test(lower)) return 'video'
  if (/\.(?:png|jpe?g)$/u.test(lower)) return 'screenshot'
  if (lower.endsWith('error-context.md')) return 'error-context'
  if (lower.includes('console')) return 'console-log'
  return 'process-log'
}

function inferMediaType(relativePath: string, kind: ArtifactKind): string {
  const extension = extname(relativePath).toLowerCase()
  if (extension === '.json') return 'application/json'
  if (extension === '.jsonl') return 'application/x-ndjson'
  if (extension === '.zip') return 'application/zip'
  if (extension === '.webm') return 'video/webm'
  if (extension === '.mp4') return 'video/mp4'
  if (extension === '.png') return 'image/png'
  if (extension === '.jpg' || extension === '.jpeg') return 'image/jpeg'
  if (extension === '.md') return 'text/markdown'
  if (extension === '.log' || extension === '.txt' || kind === 'console-log') return 'text/plain'
  return 'application/octet-stream'
}

function portableRelativePath(root: string, path: string): string {
  const value = relative(root, path).split(sep).join('/')
  if (
    value.length === 0 || value.startsWith('/') || value.includes('\\') ||
    value.split('/').some((segment) => segment === '' || segment === '.' || segment === '..')
  ) throw new Error('artifact path escapes its sample workspace')
  return requirePortableRelativePath(value, 'artifact path')
}

function normalizedViolations(violations: readonly string[]): readonly string[] {
  return Object.freeze([...new Set(violations)].sort(compareStrings))
}

function errorMessage(cause: unknown): string {
  return cause instanceof Error ? cause.message : String(cause)
}


function compareStrings(left: string, right: string): number {
  if (left === right) return 0
  return left < right ? -1 : 1
}

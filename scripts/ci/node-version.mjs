import { readFileSync } from 'node:fs'
import { isAbsolute, join, resolve } from 'node:path'

const MAXIMUM_NODE_VERSION_SOURCE_BYTES = 64
const CANONICAL_NODE_VERSION_PATTERN = /^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)/u

export function parsePinnedNodeVersion(source) {
  if (
    typeof source !== 'string' ||
    Buffer.byteLength(source, 'utf8') > MAXIMUM_NODE_VERSION_SOURCE_BYTES
  ) throw new Error('pinned Node version source must be bounded UTF-8 text')

  const version = source.endsWith('\r\n')
    ? source.slice(0, -2)
    : source.endsWith('\n')
      ? source.slice(0, -1)
      : source
  if (!isCanonicalNodeVersion(version)) {
    throw new Error('pinned Node version must be exactly one canonical MAJOR.MINOR.PATCH value')
  }
  return version
}

export function readPinnedNodeVersion(repositoryRoot) {
  const root = requireCanonicalAbsolutePath(repositoryRoot, 'Node version repository root')
  const path = join(root, '.node-version')
  const bytes = readFileSync(path)
  if (bytes.byteLength > MAXIMUM_NODE_VERSION_SOURCE_BYTES) {
    throw new Error('pinned Node version source must be bounded UTF-8 text')
  }

  let source
  try {
    source = new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch (cause) {
    throw new Error('pinned Node version source is not valid UTF-8', { cause })
  }
  return parsePinnedNodeVersion(source)
}

export function assertPinnedNodeVersion(options) {
  if (options === null || typeof options !== 'object' || Array.isArray(options)) {
    throw new Error('Node version assertion options must be an object')
  }
  const { actualVersion, pinnedVersion } = options
  if (!isCanonicalNodeVersion(pinnedVersion)) {
    throw new Error('pinned Node version assertion value must be canonical MAJOR.MINOR.PATCH text')
  }
  if (
    typeof actualVersion !== 'string' ||
    !actualVersion.startsWith('v') ||
    !isCanonicalNodeVersion(actualVersion.slice(1))
  ) throw new Error('active Node version must be canonical vMAJOR.MINOR.PATCH text')
  if (actualVersion.slice(1) !== pinnedVersion) {
    throw new Error(
      `active Node version ${JSON.stringify(actualVersion)} does not match repository pin ` +
      `${JSON.stringify(`v${pinnedVersion}`)}`,
    )
  }
  return pinnedVersion
}

function isCanonicalNodeVersion(value) {
  if (typeof value !== 'string') return false
  const match = CANONICAL_NODE_VERSION_PATTERN.exec(value)
  return match !== null && match[0].length === value.length
}

function requireCanonicalAbsolutePath(value, label) {
  if (typeof value !== 'string' || !isAbsolute(value) || resolve(value) !== value) {
    throw new Error(`${label} must be absolute and canonical`)
  }
  return value
}

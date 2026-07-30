import { createRequire } from 'node:module'

const MAXIMUM_TOOL_LOCK_BYTES = 2 * 1_024 * 1_024
const VERSION_PATTERN = /^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)$/u
const VERSION_REFERENCE_PATTERN = /^((?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*))(?:\([^()\r\n]+\))*$/u
const requireFromWeb = createRequire(new URL('../../../../../web/package.json', import.meta.url))

export const GENERATED_SEMANTIC_REQUIRED_TOOL_VERSIONS = Object.freeze({
  vite: '8.1.3',
  rolldown: '1.1.4',
})

export function parseGeneratedSemanticToolAuthorization(encodedLock) {
  const source = decodeToolLock(encodedLock)
  const { parseDocument } = requireFromWeb('yaml')
  const document = parseDocument(source, {
    prettyErrors: false,
    uniqueKeys: true,
  })
  if (document.errors.length !== 0) {
    throw new Error('generated semantic tool lock is invalid YAML')
  }

  let root
  try {
    // Lockfiles do not need aliases; rejecting them removes an expansion and
    // object-identity ambiguity from this authorization boundary.
    root = requireMap(
      document.toJS({ mapAsMap: true, maxAliasCount: 0 }),
      'generated semantic tool lock',
    )
  } catch (cause) {
    throw new Error('generated semantic tool lock cannot be resolved safely', { cause })
  }
  if (root.get('lockfileVersion') !== '9.0') {
    throw new Error('generated semantic tool lock has an unsupported format')
  }

  const importers = requireMapEntry(root, 'importers', 'generated semantic tool lock importers')
  const webImporter = requireMapEntry(importers, '.', 'generated semantic web importer')
  const developmentDependencies = requireMapEntry(
    webImporter,
    'devDependencies',
    'generated semantic web development dependencies',
  )
  const viteDependency = requireMapEntry(
    developmentDependencies,
    'vite',
    'generated semantic Vite importer resolution',
  )
  const viteReference = requireStringEntry(
    viteDependency,
    'version',
    'generated semantic Vite importer resolution',
  )
  const vite = baseVersion(viteReference, 'generated semantic Vite importer resolution')
  if (vite !== GENERATED_SEMANTIC_REQUIRED_TOOL_VERSIONS.vite) {
    throw new Error('generated semantic Vite lock resolution differs from policy')
  }

  const packages = requireMapEntry(root, 'packages', 'generated semantic tool packages')
  requireMapEntry(packages, `vite@${vite}`, 'generated semantic Vite package resolution')
  const snapshots = requireMapEntry(root, 'snapshots', 'generated semantic tool snapshots')
  const viteSnapshot = requireMapEntry(
    snapshots,
    `vite@${viteReference}`,
    'generated semantic Vite snapshot resolution',
  )
  const viteSnapshotDependencies = requireMapEntry(
    viteSnapshot,
    'dependencies',
    'generated semantic Vite snapshot dependencies',
  )
  const rolldownReference = requireStringEntry(
    viteSnapshotDependencies,
    'rolldown',
    'generated semantic Rolldown dependency resolution',
  )
  const rolldown = baseVersion(
    rolldownReference,
    'generated semantic Rolldown dependency resolution',
  )
  if (rolldown !== GENERATED_SEMANTIC_REQUIRED_TOOL_VERSIONS.rolldown) {
    throw new Error('generated semantic Rolldown lock resolution differs from policy')
  }
  requireMapEntry(packages, `rolldown@${rolldown}`, 'generated semantic Rolldown package resolution')
  requireMapEntry(
    snapshots,
    `rolldown@${rolldownReference}`,
    'generated semantic Rolldown snapshot resolution',
  )
  return GENERATED_SEMANTIC_REQUIRED_TOOL_VERSIONS
}

export function assertGeneratedSemanticToolVersions(
  actual,
  authorized = GENERATED_SEMANTIC_REQUIRED_TOOL_VERSIONS,
) {
  requireVersionMap(actual, 'observed generated semantic tools')
  requireVersionMap(authorized, 'authorized generated semantic tools')
  for (const name of ['vite', 'rolldown']) {
    if (actual[name] !== authorized[name]) {
      throw new Error(`generated semantic ${name} version is not lock-authorized`)
    }
  }
  return Object.freeze({ vite: actual.vite, rolldown: actual.rolldown })
}

function decodeToolLock(value) {
  if (typeof value === 'string') {
    if (Buffer.byteLength(value, 'utf8') > MAXIMUM_TOOL_LOCK_BYTES) {
      throw new Error('generated semantic tool lock exceeds its byte limit')
    }
    return value
  }
  if (!(value instanceof Uint8Array) || value.byteLength > MAXIMUM_TOOL_LOCK_BYTES) {
    throw new Error('generated semantic tool lock must be bounded UTF-8 text')
  }
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(value)
  } catch (cause) {
    throw new Error('generated semantic tool lock is not valid UTF-8', { cause })
  }
}

function baseVersion(reference, label) {
  const match = VERSION_REFERENCE_PATTERN.exec(reference)
  if (match === null) throw new Error(`${label} has an invalid version reference`)
  return match[1]
}

function requireMapEntry(map, key, label) {
  if (!map.has(key)) throw new Error(`${label} is missing`)
  return requireMap(map.get(key), label)
}

function requireStringEntry(map, key, label) {
  if (!map.has(key)) throw new Error(`${label} is missing`)
  const value = map.get(key)
  if (typeof value !== 'string' || value.length === 0) {
    throw new Error(`${label} must be text`)
  }
  return value
}

function requireMap(value, label) {
  if (!(value instanceof Map)) throw new Error(`${label} must be a mapping`)
  return value
}

function requireVersionMap(value, label) {
  if (
    value === null || typeof value !== 'object' || Array.isArray(value) ||
    Object.keys(value).length !== 2 ||
    !Object.hasOwn(value, 'vite') || !Object.hasOwn(value, 'rolldown') ||
    !VERSION_PATTERN.test(value.vite) || !VERSION_PATTERN.test(value.rolldown)
  ) throw new Error(`${label} are invalid`)
}

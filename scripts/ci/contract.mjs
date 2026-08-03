import { readFileSync, statSync } from 'node:fs'
import { spawnSync } from 'node:child_process'
import { dirname, resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

const DEFAULT_REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..')
const MAXIMUM_CODE_FILES_PER_DIRECTORY = 20
const PLATFORM_ENTRYPOINT_ASSIGNMENT = 'PLATFORM_ENTRYPOINTS'
const COMPOSITE_TARGETS = new Set(['ci', 'e2e', 'browser-smoke', 'browser'])
const CODE_FILE_EXTENSIONS = new Set([
  '.c', '.cc', '.cpp', '.cjs', '.cs', '.go', '.h', '.hpp', '.java', '.js', '.jsx',
  '.kt', '.kts', '.mjs', '.php', '.ps1', '.psm1', '.py', '.rb', '.rs', '.sh',
  '.swift', '.ts', '.tsx',
])
const TEST_CODE_FILE_PATTERN = /(?:_test\.go|(?:^|[._-])(?:spec|specs|test|tests)\.[^.]+)$/u

export const PUBLIC_LOCAL_TARGETS = Object.freeze(
  'ci check hygiene sloc lint workflow-lint vet race coverage vectors core-release web web-dependencies integration e2e-go e2e browser-preflight browser-process browser-smoke browser-local browser-network browser-stability browser'.split(' '),
)

export const LOCAL_CI_GATES = Object.freeze(
  'hygiene sloc workflow-lint lint vet race coverage vectors core-release web browser-preflight browser-process'.split(' '),
)

export const REQUIRED_PLATFORM_ENTRYPOINTS = Object.freeze(
  PUBLIC_LOCAL_TARGETS.filter((target) => !COMPOSITE_TARGETS.has(target)),
)

/**
 * Keep this contract intentionally shallow: semantic target names and their native
 * leaves are stable APIs, while Make flags, parsers, shells, PATH, and descriptors
 * are ordinary caller/toolchain state rather than evidence authority.
 */
export function inspectCIContract(repositoryRoot) {
  const violations = []
  let makefile
  try {
    makefile = readFileSync(resolve(repositoryRoot, 'Makefile'), 'utf8')
  } catch (error) {
    return {
      publicTargets: [],
      entrypoints: [],
      ciGates: [],
      violations: [`cannot read Makefile: ${error.message}`],
    }
  }

  const publicTargets = readNames(makefile, 'PUBLIC_TARGETS', violations)
  const entrypoints = readNames(makefile, PLATFORM_ENTRYPOINT_ASSIGNMENT, violations)
  const ciGates = readNames(makefile, 'CI_GATES', violations)

  validateExactWords('PUBLIC_TARGETS', publicTargets, PUBLIC_LOCAL_TARGETS, violations)
  validateExactWords('CI_GATES', ciGates, LOCAL_CI_GATES, violations)
  validatePlatformEntrypoints(entrypoints, violations)

  for (const entrypoint of entrypoints) {
    for (const [directory, extension] of [['windows', 'ps1'], ['linux', 'sh']]) {
      const script = `scripts/ci/${directory}/${entrypoint}.${extension}`
      if (!isRegularFile(resolve(repositoryRoot, script))) {
        violations.push(`Makefile entrypoint ${entrypoint} requires existing ${script}`)
      }
    }
  }

  const browserSmoke = 'scripts/ci/windows/browser/smoke.ps1'
  if (!isRegularFile(resolve(repositoryRoot, browserSmoke))) {
    violations.push(`Makefile target browser-smoke requires existing ${browserSmoke}`)
  }

  violations.push(...inspectCodeDirectoryBoundaries(repositoryRoot))
  return { publicTargets, entrypoints, ciGates, violations }
}

export function parsePublicLocalTargetNames(makefile) {
  return parseMakeWordNames(makefile, 'PUBLIC_TARGETS')
}

export function parsePlatformEntrypointNames(makefile) {
  return parseMakeWordNames(makefile, PLATFORM_ENTRYPOINT_ASSIGNMENT)
}

export function parseMakeWordNames(makefile, assignment) {
  const value = parseMakeAssignment(makefile, assignment)
  const names = value.length === 0 ? [] : value.split(/\s+/u)
  if (names.length === 0) throw new Error(`${assignment} assignment is empty`)
  for (const name of names) {
    if (!/^[a-z0-9]+(?:-[a-z0-9]+)*$/u.test(name)) {
      throw new Error(`unsupported ${assignment} name ${name}`)
    }
  }
  if (new Set(names).size !== names.length) {
    throw new Error(`${assignment} contains duplicate names`)
  }
  return names
}

export function parseMakeAssignment(makefile, name) {
  const escapedName = name.replace(/[.*+?^$()|[\]\\]/gu, '\\$&')
  const lines = makefile.split(/\r?\n/u)
  const assignment = new RegExp(`^(?:override\\s+)?${escapedName}\\s*:?=`, 'u')
  const index = lines.findIndex((line) => assignment.test(line))
  if (index < 0) throw new Error(`${name} assignment is missing`)

  let value = lines[index].replace(
    new RegExp(`^(?:override\\s+)?${escapedName}\\s*:?=\\s*`, 'u'),
    '',
  )
  let next = index + 1
  while (/\\\s*$/u.test(value) && next < lines.length) {
    value = `${value.replace(/\\\s*$/u, '')} ${lines[next].trim()}`
    next += 1
  }
  return value.replace(/\s+#.*$/u, '').trim()
}

export function inspectCodeDirectoryBoundaries(
  repositoryRoot,
  maximumFiles = MAXIMUM_CODE_FILES_PER_DIRECTORY,
) {
  if (!Number.isSafeInteger(maximumFiles) || maximumFiles < 1) {
    throw new Error('code directory maximum must be a positive safe integer')
  }

  const result = spawnSync(
    'git',
    ['ls-files', '-z', '--cached', '--others', '--exclude-standard'],
    { cwd: repositoryRoot, encoding: 'utf8' },
  )
  if (result.error !== undefined || result.status !== 0) {
    const detail = result.error?.message ?? result.stderr.trim() ?? `exit ${result.status}`
    return [`cannot inspect code directory boundaries: ${detail}`]
  }

  const counts = new Map()
  for (const rawPath of result.stdout.split('\0')) {
    if (rawPath.length === 0) continue
    const path = rawPath.replaceAll('\\', '/')
    if (!isRegularFile(resolve(repositoryRoot, path))) continue
    const separator = path.lastIndexOf('/')
    const name = separator < 0 ? path : path.slice(separator + 1)
    if (!CODE_FILE_EXTENSIONS.has(extensionOf(name)) || TEST_CODE_FILE_PATTERN.test(name)) continue
    const directory = separator < 0 ? '.' : path.slice(0, separator)
    counts.set(directory, (counts.get(directory) ?? 0) + 1)
  }

  return [...counts.entries()]
    .filter(([, count]) => count > maximumFiles)
    .sort(([left], [right]) => left.localeCompare(right))
    .map(([directory, count]) =>
      `${directory} contains ${count} non-test code files; maximum is ${maximumFiles}`,
    )
}

function readNames(makefile, assignment, violations) {
  try {
    return parseMakeWordNames(makefile, assignment)
  } catch (error) {
    violations.push(`cannot read Makefile ${assignment}: ${error.message}`)
    return []
  }
}

function validateExactWords(assignment, actual, expected, violations) {
  if (actual.length === 0) return
  if (actual.length === expected.length && actual.every((value, index) => value === expected[index])) {
    return
  }
  violations.push(
    `Makefile ${assignment} must be ${expected.join(' ')}; got ${actual.join(' ')}`,
  )
}

function validatePlatformEntrypoints(entrypoints, violations) {
  if (entrypoints.length === 0) return
  const actual = new Set(entrypoints)
  for (const required of REQUIRED_PLATFORM_ENTRYPOINTS) {
    if (!actual.has(required)) {
      violations.push(`Makefile ${PLATFORM_ENTRYPOINT_ASSIGNMENT} must include public gate ${required}`)
    }
  }
  const allowed = new Set(REQUIRED_PLATFORM_ENTRYPOINTS)
  for (const entrypoint of entrypoints) {
    if (!allowed.has(entrypoint)) {
      violations.push(`Makefile ${PLATFORM_ENTRYPOINT_ASSIGNMENT} contains unknown gate ${entrypoint}`)
    }
  }
}

function extensionOf(name) {
  const dot = name.lastIndexOf('.')
  return dot < 0 ? '' : name.slice(dot).toLowerCase()
}

function isRegularFile(path) {
  try {
    return statSync(path).isFile()
  } catch {
    return false
  }
}

function main() {
  const result = inspectCIContract(DEFAULT_REPOSITORY_ROOT)
  if (result.violations.length > 0) {
    for (const violation of result.violations) console.error(`ci-contract: ${violation}`)
    process.exitCode = 1
    return
  }
  console.log(
    `ci-contract: PASS (${result.publicTargets.length} public targets, ${result.entrypoints.length} platform entrypoints)`,
  )
}

const entryPoint = process.argv[1]
if (entryPoint !== undefined && pathToFileURL(resolve(entryPoint)).href === import.meta.url) main()

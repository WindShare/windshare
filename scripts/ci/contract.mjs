import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs'
import { spawnSync } from 'node:child_process'
import { dirname, isAbsolute, relative, resolve, sep } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

const DEFAULT_REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..')
const PLATFORM_ENTRYPOINT_ASSIGNMENT = 'PLATFORM_ENTRYPOINTS'
const ENTRYPOINT_PLATFORMS = Object.freeze([
  Object.freeze({ directory: 'windows', extension: 'ps1' }),
  Object.freeze({ directory: 'linux', extension: 'sh' }),
])
const MAXIMUM_CODE_FILES_PER_DIRECTORY = 20
const CODE_FILE_EXTENSIONS = new Set([
  '.c', '.cc', '.cpp', '.cjs', '.cs', '.go', '.h', '.hpp', '.java', '.js', '.jsx',
  '.kt', '.kts', '.mjs', '.php', '.ps1', '.psm1', '.py', '.rb', '.rs', '.sh',
  '.swift', '.ts', '.tsx',
])
const TEST_CODE_FILE_PATTERN = /(?:_test\.go|(?:^|[._-])(?:spec|specs|test|tests)\.[^.]+)$/u
const WORKFLOW_EXTENSIONS = new Set(['.yaml', '.yml'])
const BARE_SHELL_INVOCATION =
  /(?:^|\brun:\s*|&&\s*|\|\|\s*|;\s*|\bthen\s+|\bdo\s+)["']?(?:\.\/)?(scripts\/ci\/[A-Za-z0-9._/-]+\.sh)["']?(?=\s|$)/u
const STATIC_SHELL_CONTENT_ASSERTION =
  /^\s*assert_(not_)?contains\s+"([^"$`]+)"(?:\s|$)/u
const STATIC_SHELL_LITERAL_CONTENT_ASSERTION =
  /^\s*assert_(not_)?contains\s+"([^"$`]+)"\s+'([^']*)'(?:\s|$)/u

/**
 * The zero-dependency contract owns only filesystem and Make boundaries. YAML
 * semantics live in the strict parser exercised by the browser contract gate;
 * keeping them out of this module prevents a second, weaker workflow language.
 */
export function inspectCIContract(
  repositoryRoot,
  { requireTracked = process.env.GITHUB_ACTIONS === 'true' } = {},
) {
  const violations = []
  let entrypoints = []
  try {
    const makefile = readFileSync(resolve(repositoryRoot, 'Makefile'), 'utf8')
    entrypoints = parsePlatformEntrypointNames(makefile)
  } catch (error) {
    violations.push(`cannot read Makefile ${PLATFORM_ENTRYPOINT_ASSIGNMENT}: ${error.message}`)
  }

  const expectedScripts = entrypoints.flatMap((entrypoint) =>
    ENTRYPOINT_PLATFORMS.map((platform) => entrypointScriptPath(entrypoint, platform)),
  )
  const ignored = ignoredFiles(repositoryRoot, expectedScripts, violations)
  const tracked = requireTracked ? trackedFiles(repositoryRoot, violations) : undefined

  for (const entrypoint of entrypoints) {
    for (const platform of ENTRYPOINT_PLATFORMS) {
      const script = entrypointScriptPath(entrypoint, platform)
      if (!existsSync(resolve(repositoryRoot, script))) {
        violations.push(`Makefile entrypoint ${entrypoint} requires existing ${script}`)
      } else if (ignored.has(script)) {
        violations.push(`Makefile entrypoint ${entrypoint} requires non-ignored ${script}`)
      } else if (tracked !== undefined && !tracked.has(script)) {
        violations.push(`Makefile entrypoint ${entrypoint} requires tracked ${script} in GitHub Actions`)
      }

      const legacyPath = `scripts/ci/${entrypoint}.${platform.extension}`
      if (existsSync(resolve(repositoryRoot, legacyPath))) {
        violations.push(`Makefile entrypoint ${entrypoint} forbids legacy wrapper ${legacyPath}`)
      }
    }
  }
  validateExactPlatformEntrypointSet(repositoryRoot, entrypoints, violations)
  violations.push(...inspectCodeDirectoryBoundaries(repositoryRoot))

  const workflowDirectory = resolve(repositoryRoot, '.github', 'workflows')
  const workflows = readdirSync(workflowDirectory, { withFileTypes: true })
    .filter((entry) => entry.isFile() && WORKFLOW_EXTENSIONS.has(extensionOf(entry.name)))
    .map((entry) => entry.name)
    .sort()
  for (const workflow of workflows) {
    const source = readFileSync(resolve(workflowDirectory, workflow), 'utf8')
    for (const invocation of findBareShellInvocations(source)) {
      // Explicit bash invocation makes checkout file modes irrelevant on both
      // Windows-authored commits and Linux runners.
      violations.push(
        `.github/workflows/${workflow}:${invocation.line} invokes ${invocation.path} without an explicit shell`,
      )
    }
  }

  const ciDirectory = resolve(repositoryRoot, 'scripts', 'ci')
  const shellContracts = readdirSync(ciDirectory, { withFileTypes: true })
    .filter((entry) => entry.isFile() && entry.name.endsWith('.sh'))
    .map((entry) => entry.name)
    .sort()
  for (const shellContract of shellContracts) {
    const contractPath = `scripts/ci/${shellContract}`
    const source = readFileSync(resolve(ciDirectory, shellContract), 'utf8')
    for (const assertion of findStaticShellContentAssertions(source)) {
      const assertedPath = resolve(repositoryRoot, assertion.path)
      const repositoryRelative = relative(repositoryRoot, assertedPath)
      const escapesRepository = repositoryRelative === '..'
        || repositoryRelative.startsWith(`..${sep}`) || isAbsolute(repositoryRelative)
      if (escapesRepository || !isRegularFile(assertedPath)) {
        violations.push(
          `${contractPath}:${assertion.line} asserts content of missing repository file ${assertion.path}`,
        )
        continue
      }
      if (assertion.literal === undefined) continue

      const assertedSource = readFileSync(assertedPath, 'utf8')
      const containsLiteral = assertedSource.includes(assertion.literal)
      if (assertion.negated ? containsLiteral : !containsLiteral) {
        const expectation = assertion.negated ? 'not contain' : 'contain'
        violations.push(
          `${contractPath}:${assertion.line} expects ${assertion.path} to ${expectation} literal ${JSON.stringify(assertion.literal)}`,
        )
      }
    }
  }

  return { entrypoints, workflows, violations }
}

export function parsePlatformEntrypointNames(makefile) {
  const value = parseMakeAssignment(makefile, PLATFORM_ENTRYPOINT_ASSIGNMENT)
  const entrypoints = value.length === 0 ? [] : value.split(/\s+/u)
  if (entrypoints.length === 0) throw new Error(`${PLATFORM_ENTRYPOINT_ASSIGNMENT} assignment is empty`)
  for (const entrypoint of entrypoints) {
    if (!/^[a-z0-9]+(?:-[a-z0-9]+)*$/u.test(entrypoint)) {
      throw new Error(`unsupported platform entrypoint name ${entrypoint}`)
    }
  }
  if (new Set(entrypoints).size !== entrypoints.length) {
    throw new Error(`${PLATFORM_ENTRYPOINT_ASSIGNMENT} contains duplicate names`)
  }
  return entrypoints
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

export function parseMakeAssignment(makefile, name) {
  const lines = makefile.split(/\r?\n/u)
  const assignmentPattern = new RegExp(`^(?:override\\s+)?${name}\\s*:?=`, 'u')
  const assignmentIndex = lines.findIndex((line) => assignmentPattern.test(line))
  if (assignmentIndex < 0) throw new Error(`${name} assignment is missing`)

  let value = lines[assignmentIndex].replace(
    new RegExp(`^(?:override\\s+)?${name}\\s*:?=\\s*`, 'u'),
    '',
  )
  let nextLine = assignmentIndex + 1
  while (/\\\s*$/u.test(value) && nextLine < lines.length) {
    value = `${value.replace(/\\\s*$/u, '')} ${lines[nextLine].trim()}`
    nextLine += 1
  }
  return value.replace(/\s+#.*$/u, '').trim()
}

export function parseMakeTargetPrerequisites(makefile, target) {
  const targetPattern = new RegExp(`^${target}:\\s*(.*)$`, 'u')
  for (const line of makefile.split(/\r?\n/u)) {
    const match = targetPattern.exec(line)
    if (match !== null) {
      const prerequisites = match[1].replace(/\s+#.*$/u, '').trim()
      return prerequisites.length === 0 ? [] : prerequisites.split(/\s+/u)
    }
  }
  throw new Error(`${target} target is missing`)
}

export function findBareShellInvocations(workflow) {
  const invocations = []
  for (const [index, line] of workflow.split(/\r?\n/u).entries()) {
    const match = BARE_SHELL_INVOCATION.exec(line.trimStart())
    if (match !== null) invocations.push({ line: index + 1, path: match[1] })
  }
  return invocations
}

export function findStaticShellContentAssertions(source) {
  const assertions = []
  for (const [index, line] of source.split(/\r?\n/u).entries()) {
    const match = STATIC_SHELL_LITERAL_CONTENT_ASSERTION.exec(line)
      ?? STATIC_SHELL_CONTENT_ASSERTION.exec(line)
    if (match !== null) {
      assertions.push({
        line: index + 1,
        path: match[2],
        literal: match[3],
        negated: match[1] !== undefined,
      })
    }
  }
  return assertions
}

function validateExactPlatformEntrypointSet(repositoryRoot, entrypoints, violations) {
  const declared = new Set(entrypoints)
  for (const platform of ENTRYPOINT_PLATFORMS) {
    const directory = resolve(repositoryRoot, 'scripts', 'ci', platform.directory)
    const actual = readdirSync(directory, { withFileTypes: true })
      .filter((entry) => entry.isFile() && extensionOf(entry.name) === `.${platform.extension}`)
      .map((entry) => entry.name.slice(0, -(platform.extension.length + 1)))
      .sort()
    for (const entrypoint of actual) {
      if (!declared.has(entrypoint)) {
        violations.push(
          `Makefile ${PLATFORM_ENTRYPOINT_ASSIGNMENT} must declare ${entrypointScriptPath(entrypoint, platform)}`,
        )
      }
    }
  }
}

function entrypointScriptPath(entrypoint, platform) {
  return `scripts/ci/${platform.directory}/${entrypoint}.${platform.extension}`
}

function trackedFiles(repositoryRoot, violations) {
  const result = spawnSync('git', ['ls-files', '-z', '--', 'scripts/ci'], {
    cwd: repositoryRoot,
    encoding: 'utf8',
  })
  if (result.error !== undefined || result.status !== 0) {
    const detail = result.error?.message ?? result.stderr.trim() ?? `exit ${result.status}`
    violations.push(`cannot inspect tracked CI scripts: ${detail}`)
    return new Set()
  }
  return new Set(result.stdout.split('\0').filter(Boolean).map((path) => path.replaceAll('\\', '/')))
}

function ignoredFiles(repositoryRoot, paths, violations) {
  if (paths.length === 0) return new Set()
  const result = spawnSync('git', ['check-ignore', '-z', '--stdin'], {
    cwd: repositoryRoot,
    encoding: 'utf8',
    input: `${paths.join('\0')}\0`,
  })
  if (result.error !== undefined || (result.status !== 0 && result.status !== 1)) {
    const detail = result.error?.message ?? result.stderr.trim() ?? `exit ${result.status}`
    violations.push(`cannot inspect ignored CI scripts: ${detail}`)
    return new Set()
  }
  return new Set(result.stdout.split('\0').filter(Boolean).map((path) => path.replaceAll('\\', '/')))
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
    `ci-contract: PASS (${result.entrypoints.length} platform entrypoints, ${result.workflows.length} workflows)`,
  )
}

const entryPoint = process.argv[1]
if (entryPoint !== undefined && pathToFileURL(resolve(entryPoint)).href === import.meta.url) main()

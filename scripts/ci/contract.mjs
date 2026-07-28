import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs'
import { spawnSync } from 'node:child_process'
import { dirname, isAbsolute, relative, resolve, sep } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

const DEFAULT_REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..')
const GATE_SCRIPT_EXTENSIONS = ['ps1', 'sh']
const WORKFLOW_EXTENSIONS = new Set(['.yaml', '.yml'])
const BARE_SHELL_INVOCATION =
  /(?:^|\brun:\s*|&&\s*|\|\|\s*|;\s*|\bthen\s+|\bdo\s+)["']?(?:\.\/)?(scripts\/ci\/[A-Za-z0-9._/-]+\.sh)["']?(?=\s|$)/u
const STATIC_SHELL_CONTENT_ASSERTION =
  /^\s*assert_(?:not_)?contains\s+"([^"$`]+)"(?:\s|$)/u

export function inspectCIContract(
  repositoryRoot,
  { requireTracked = process.env.GITHUB_ACTIONS === 'true' } = {},
) {
  const violations = []
  let gates = []

  try {
    gates = parseGateNames(readFileSync(resolve(repositoryRoot, 'Makefile'), 'utf8'))
  } catch (error) {
    violations.push(`cannot read Makefile GATES: ${error.message}`)
  }

  const scripts = gates.flatMap((gate) =>
    GATE_SCRIPT_EXTENSIONS.map((extension) => `scripts/ci/${gate}.${extension}`),
  )
  const ignored = ignoredFiles(repositoryRoot, scripts, violations)
  const tracked = requireTracked ? trackedFiles(repositoryRoot, violations) : undefined
  for (const gate of gates) {
    for (const extension of GATE_SCRIPT_EXTENSIONS) {
      const script = `scripts/ci/${gate}.${extension}`
      if (!existsSync(resolve(repositoryRoot, script))) {
        violations.push(`Makefile gate ${gate} requires existing ${script}`)
      } else if (ignored.has(script)) {
        // Pre-commit validation must accept new files, but ignored private files
        // cannot reach a clean clone and therefore cannot satisfy a CI gate.
        violations.push(`Makefile gate ${gate} requires non-ignored ${script}`)
      } else if (tracked !== undefined && !tracked.has(script)) {
        violations.push(`Makefile gate ${gate} requires tracked ${script} in GitHub Actions`)
      }
    }
  }

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
      const escapesRepository = repositoryRelative === '..' ||
        repositoryRelative.startsWith(`..${sep}`) || isAbsolute(repositoryRelative)
      if (escapesRepository || !isRegularFile(assertedPath)) {
        // Content assertions encode refactor-sensitive source contracts. Checking
        // their literal targets on every host prevents Windows validation from
        // overlooking a stale path that only the Linux shell suite would open.
        violations.push(
          `${contractPath}:${assertion.line} asserts content of missing repository file ${assertion.path}`,
        )
      }
    }
  }

  return { gates, workflows, violations }
}

export function parseGateNames(makefile) {
  const lines = makefile.split(/\r?\n/u)
  const assignmentIndex = lines.findIndex((line) => /^GATES\s*:?=/u.test(line))
  if (assignmentIndex < 0) throw new Error('GATES assignment is missing')

  let value = lines[assignmentIndex].replace(/^GATES\s*:?=\s*/u, '')
  let nextLine = assignmentIndex + 1
  while (/\\\s*$/u.test(value) && nextLine < lines.length) {
    value = `${value.replace(/\\\s*$/u, '')} ${lines[nextLine].trim()}`
    nextLine += 1
  }
  value = value.replace(/\s+#.*$/u, '').trim()

  const gates = value.length === 0 ? [] : value.split(/\s+/u)
  if (gates.length === 0) throw new Error('GATES assignment is empty')
  for (const gate of gates) {
    if (!/^[A-Za-z0-9_-]+$/u.test(gate)) throw new Error(`unsupported gate name ${gate}`)
  }
  if (new Set(gates).size !== gates.length) throw new Error('GATES contains duplicate names')
  return gates
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
    const match = STATIC_SHELL_CONTENT_ASSERTION.exec(line)
    if (match !== null) assertions.push({ line: index + 1, path: match[1] })
  }
  return assertions
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
  // Exit 1 is the documented success case when no supplied path is ignored.
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
    `ci-contract: PASS (${result.gates.length} Makefile gates, ${result.workflows.length} workflows)`,
  )
}

const entryPoint = process.argv[1]
if (entryPoint !== undefined && pathToFileURL(resolve(entryPoint)).href === import.meta.url) main()

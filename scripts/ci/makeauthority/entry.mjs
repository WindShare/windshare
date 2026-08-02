import { spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import {
  chmodSync,
  closeSync,
  constants as fsConstants,
  existsSync,
  fstatSync,
  lstatSync,
  mkdtempSync,
  openSync,
  readFileSync,
  readSync,
  realpathSync,
  rmSync,
  unlinkSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { basename, dirname, isAbsolute, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { types as nodeTypes } from 'node:util'

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..', '..')
const CANONICAL_MAKEFILE = resolve(REPOSITORY_ROOT, 'Makefile')
const PLATFORM_ENTRYPOINT_ASSIGNMENT = 'PLATFORM_ENTRYPOINTS'
const FORBIDDEN_ENVIRONMENT = Object.freeze([
  'MAKEFLAGS',
  'MFLAGS',
  'GNUMAKEFLAGS',
  'MAKEFILES',
  'MAKEOVERRIDES',
  'MAKE_RESTARTS',
  'MAKELEVEL',
  'MAKESHELL',
  'BASH_ENV',
  'ENV',
  'GOFLAGS',
  'GOWORK',
  'GOOS',
  'GOARCH',
  'GOENV',
  'GOTOOLCHAIN',
  'GOROOT',
])
const INTERNAL_AUTHORITY_VARIABLES = Object.freeze([
  'WINDSHARE_HOST_GOOS',
  'WINDSHARE_CORE_ARTIFACT_COMMIT_SHA',
  'WINDSHARE_MAKEFILE_SHA256',
  'WINDSHARE_RETAINED_MAKEFILE',
  'WINDSHARE_RECIPE_SHELL',
  'WINDSHARE_BASH_EXECUTABLE',
  'WINDSHARE_PWSH_EXECUTABLE',
])
const MAXIMUM_MAKEFILE_BYTES = 1024 * 1024
const MAXIMUM_MAKE_ARGUMENTS = 8
const MAXIMUM_MAKE_ARGUMENT_BYTES = 8192
const MAXIMUM_ENVIRONMENT_ENTRIES = 4096
const MAXIMUM_ENVIRONMENT_BYTES = 2 * 1024 * 1024
const MAXIMUM_ENVIRONMENT_NAME_BYTES = 256
const MAXIMUM_ENVIRONMENT_VALUE_BYTES = 64 * 1024
const MAXIMUM_PROTECTED_PATH_BYTES = 4096
const SAFE_PROTECTED_PATH_PATTERN = /^[^\0\r\n"'`$]+$/u
const WINDOWS_SYSTEM_MODULES = new Set(['kernel32.dll', 'ntdll.dll'])
const PROTECTED_PATH_ASSIGNMENTS = new Set([
  'BROWSER_NETWORK_COMPLETION',
])
const PROTECTED_PATH_TARGETS = new Set([
  'browser',
  'browser-network',
  'ci-full',
])
const PUBLIC_TARGETS = new Set([
  'authority-context',
  'browser',
  'browser-smoke',
  'ci',
  'ci-full',
  'e2e',
  'plan-browser',
  'plan-ci',
  'plan-ci-full',
])

export function validateMakeInvocation(arguments_, environment = process.env) {
  arguments_ = snapshotInvocationArguments(arguments_)
  environment = snapshotEnvironment(environment)
  if (!Array.isArray(arguments_) || arguments_.length === 0) {
    throw new Error('validation Make authority requires at least one explicit target')
  }
  for (const name of FORBIDDEN_ENVIRONMENT) {
    if (hasEnvironmentName(environment, name)) {
      throw new Error(`${name} must be absent before validation Make authority`)
    }
  }
  for (const name of INTERNAL_AUTHORITY_VARIABLES) {
    if (hasEnvironmentName(environment, name)) {
      throw new Error(`${name} must be absent before validation Make authority`)
    }
  }
  for (const name of Object.keys(environment)) {
    if (isInternalAuthorityName(name)) {
      throw new Error(`${name} must be absent before validation Make authority`)
    }
  }
  for (const name of PROTECTED_PATH_ASSIGNMENTS) {
    if (hasEnvironmentName(environment, name)) {
      throw new Error(`${name} must be supplied only as an explicit protected operand`)
    }
  }

  const makefile = readCanonicalMakefileSnapshot()
  const allowedTargets = loadAllowedTargets(makefile.bytes)
  const targets = new Set()
  const assignments = new Map()
  for (const argument of arguments_) {
    if (typeof argument !== 'string' || argument.length === 0) {
      throw new Error('validation Make arguments must be non-empty strings')
    }
    if (argument.startsWith('-')) {
      throw new Error(`GNU Make option is forbidden by validation authority: ${argument}`)
    }
    const assignmentSeparator = argument.indexOf('=')
    if (assignmentSeparator >= 0) {
      const name = argument.slice(0, assignmentSeparator)
      const value = argument.slice(assignmentSeparator + 1)
      if (!PROTECTED_PATH_ASSIGNMENTS.has(name)) {
        throw new Error(`unsupported validation Make assignment: ${name}`)
      }
      if (assignments.has(name)) throw new Error(`duplicate validation Make assignment: ${name}`)
      assignments.set(name, canonicalProtectedPath(name, value))
      continue
    }
    if (!allowedTargets.has(argument)) throw new Error(`unsupported validation Make target: ${argument}`)
    if (targets.has(argument)) throw new Error(`duplicate validation Make target: ${argument}`)
    targets.add(argument)
  }
  if (targets.size === 0) throw new Error('validation Make authority requires an explicit target')
  const requiresProtectedPaths = [...targets].some((target) => PROTECTED_PATH_TARGETS.has(target))
  if (requiresProtectedPaths !== (assignments.size === PROTECTED_PATH_ASSIGNMENTS.size)) {
    throw new Error('browser network completion must exactly match a completion-consuming target')
  }
  return Object.freeze({
    targets: Object.freeze([...targets]),
    assignments: Object.freeze(
      [...assignments].map(([name, value]) => Object.freeze([name, value])),
    ),
    makefileSha256: makefile.sha256,
  })
}

export function runMake(arguments_, environment = process.env) {
  const environmentSnapshot = snapshotEnvironment(environment)
  const validation = validateMakeInvocation(arguments_, environmentSnapshot)
  const makefile = readCanonicalMakefileSnapshot()
  if (makefile.sha256 !== validation.makefileSha256) {
    throw new Error('canonical Makefile changed after invocation validation')
  }
  const childEnvironment = copyEnvironment(environmentSnapshot)
  for (const name of Object.keys(childEnvironment)) {
    if (
      name.toUpperCase().startsWith('GIT_') || isInternalAuthorityName(name) ||
      FORBIDDEN_ENVIRONMENT.includes(name.toUpperCase())
    ) delete childEnvironment[name]
  }
  if (process.platform === 'win32') {
    const systemDirectory = nativeWindowsSystemDirectory()
    const windowsDirectory = dirname(systemDirectory)
    setEnvironmentValue(childEnvironment, 'SystemRoot', windowsDirectory)
    setEnvironmentValue(childEnvironment, 'WINDIR', windowsDirectory)
    setEnvironmentValue(childEnvironment, 'ComSpec', join(systemDirectory, 'cmd.exe'))
  }
  const launcher = process.platform === 'win32'
    ? resolve(dirname(fileURLToPath(import.meta.url)), 'launch.ps1')
    : resolve(dirname(fileURLToPath(import.meta.url)), 'launch.sh')
  const executable = platformShell()
  const protectedPathAuthority = createRetainedProtectedPathAuthority(validation.assignments)
  let makefileAuthority
  let result
  try {
    makefileAuthority = createRetainedMakefileAuthority(makefile.bytes)
    const retainedMakeArguments = [
      ...validation.targets,
      ...protectedPathAuthority.assignments.map(([name, value]) => `${name}=${value}`),
    ]
    const launcherArguments = process.platform === 'win32'
      ? ['-NoLogo', '-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', launcher,
          makefileAuthority.path, makefile.sha256, REPOSITORY_ROOT, ...retainedMakeArguments]
      : [launcher, makefileAuthority.path, makefile.sha256, REPOSITORY_ROOT, ...retainedMakeArguments]
    result = spawnSync(executable, launcherArguments, {
      cwd: REPOSITORY_ROOT,
      env: childEnvironment,
      stdio: 'inherit',
      shell: false,
    })
  } finally {
    makefileAuthority?.close()
    protectedPathAuthority.close()
  }
  if (result.error !== undefined) throw result.error
  if (result.signal !== null) throw new Error(`validation Make authority terminated by ${result.signal}`)
  return result.status ?? 1
}

function loadAllowedTargets(makefileBytes) {
  const makefile = new TextDecoder('utf-8', { fatal: true }).decode(makefileBytes)
  const assignment = makefile.split(/\r?\n/u)
    .find((line) => new RegExp(`^(?:override\\s+)?${PLATFORM_ENTRYPOINT_ASSIGNMENT}\\s*:?=`, 'u').test(line))
  if (assignment === undefined || assignment.endsWith('\\')) {
    throw new Error(`${PLATFORM_ENTRYPOINT_ASSIGNMENT} must be one canonical Makefile line`)
  }
  const value = assignment.replace(
    new RegExp(`^(?:override\\s+)?${PLATFORM_ENTRYPOINT_ASSIGNMENT}\\s*:?=\\s*`, 'u'),
    '',
  ).replace(/\s+#.*$/u, '').trim()
  const targets = new Set(PUBLIC_TARGETS)
  for (const target of value.split(/\s+/u)) {
    if (!/^[a-z0-9]+(?:-[a-z0-9]+)*$/u.test(target)) {
      throw new Error(`Makefile declares an invalid platform entrypoint: ${target}`)
    }
    targets.add(target)
  }
  return targets
}

function readCanonicalMakefileSnapshot() {
  const namedBefore = lstatSync(CANONICAL_MAKEFILE, { bigint: true })
  if (
    !namedBefore.isFile() || namedBefore.isSymbolicLink() ||
    namedBefore.size < 1n || namedBefore.size > BigInt(MAXIMUM_MAKEFILE_BYTES)
  ) throw new Error('canonical Makefile must be one bounded non-symlink regular file')
  const descriptor = openSync(CANONICAL_MAKEFILE, 'r')
  try {
    const openedBefore = fstatSync(descriptor, { bigint: true })
    if (!sameIdentity(namedBefore, openedBefore) || !sameRevision(namedBefore, openedBefore)) {
      throw new Error('canonical Makefile changed before it could be read')
    }
    const bytes = readFileSync(descriptor)
    const openedAfter = fstatSync(descriptor, { bigint: true })
    const namedAfter = lstatSync(CANONICAL_MAKEFILE, { bigint: true })
    if (
      bytes.byteLength !== Number(openedAfter.size) ||
      !sameIdentity(openedBefore, openedAfter) || !sameIdentity(openedAfter, namedAfter) ||
      !sameRevision(openedBefore, openedAfter) || !sameRevision(openedAfter, namedAfter)
    ) throw new Error('canonical Makefile changed while it was read')
    return Object.freeze({
      bytes: Uint8Array.from(bytes),
      sha256: createHash('sha256').update(bytes).digest('hex'),
    })
  } finally {
    closeSync(descriptor)
  }
}

export function createRetainedMakefileAuthority(bytes, repositoryRoot = REPOSITORY_ROOT) {
  if (process.platform === 'win32') {
    const gitAuthorityRoot = join(repositoryRoot, '.git')
    const metadata = lstatSync(gitAuthorityRoot)
    if (!metadata.isDirectory() || metadata.isSymbolicLink()) {
      throw new Error('validation Make authority requires one real Git metadata directory')
    }
    const root = mkdtempSync(join(gitAuthorityRoot, 'windshare-make-authority-'))
    const path = join(root, 'Makefile')
    let descriptor
    try {
      writeFileSync(path, bytes, { flag: 'wx', mode: 0o400 })
      chmodSync(path, 0o400)
      descriptor = openSync(path, 'r')
    } catch (cause) {
      if (descriptor !== undefined) closeSync(descriptor)
      rmSync(root, { recursive: true, force: true })
      throw cause
    }
    const relativeRoot = basename(root)
    let closed = false
    return Object.freeze({
      // Only this safe relative spelling crosses into GNU Make's variable language;
      // repository metacharacters remain outside parser-visible data.
      path: `.git/${relativeRoot}/Makefile`,
      close() {
        if (closed) return
        closed = true
        closeSync(descriptor)
        rmSync(root, { recursive: true, force: true })
      },
    })
  }
  if (process.platform !== 'linux') {
    throw new Error(`validation Make authority is unsupported on ${process.platform}`)
  }
  const root = mkdtempSync(join(tmpdir(), 'windshare-makefile-authority-'))
  const path = join(root, 'Makefile')
  let descriptor
  try {
    writeFileSync(path, bytes, { flag: 'wx', mode: 0o400 })
    chmodSync(path, 0o400)
    descriptor = openSync(path, 'r')
    unlinkSync(path)
  } catch (cause) {
    if (descriptor !== undefined) closeSync(descriptor)
    rmSync(root, { recursive: true, force: true })
    throw cause
  }
  let closed = false
  return Object.freeze({
    path: `/proc/${process.pid}/fd/${descriptor}`,
    close() {
      if (closed) return
      closed = true
      closeSync(descriptor)
      rmSync(root, { recursive: true, force: true })
    },
  })
}

export function createRetainedProtectedPathAuthority(assignments) {
  const canonicalAssignments = snapshotProtectedAssignments(assignments)
  if (process.platform === 'win32') {
    return Object.freeze({ assignments: canonicalAssignments, close() {} })
  }
  if (process.platform !== 'linux') {
    if (canonicalAssignments.length !== 0) {
      throw new Error(`protected path authority is unsupported on ${process.platform}`)
    }
    return Object.freeze({ assignments: canonicalAssignments, close() {} })
  }

  const descriptors = []
  const retainedAssignments = []
  try {
    for (const [name, path] of canonicalAssignments) {
      const namedBefore = lstatSync(path, { bigint: true })
      const flags = fsConstants.O_RDONLY | fsConstants.O_NOFOLLOW
      const descriptor = openSync(path, flags)
      descriptors.push(descriptor)
      const opened = fstatSync(descriptor, { bigint: true })
      const namedAfter = lstatSync(path, { bigint: true })
      if (
        !sameIdentity(namedBefore, opened) || !sameIdentity(opened, namedAfter) ||
        !sameRevision(namedBefore, opened) || !sameRevision(opened, namedAfter) ||
        !opened.isFile()
      ) throw new Error(`${name} changed while its protected identity was retained`)
      retainedAssignments.push(Object.freeze([
        name,
        `/proc/${process.pid}/fd/${descriptor}`,
      ]))
    }
  } catch (cause) {
    for (const descriptor of descriptors) closeSync(descriptor)
    throw cause
  }
  let closed = false
  return Object.freeze({
    assignments: Object.freeze(retainedAssignments),
    close() {
      if (closed) return
      closed = true
      for (const descriptor of descriptors) closeSync(descriptor)
    },
  })
}

function canonicalProtectedPath(name, value) {
  if (
    typeof value !== 'string' || value.length === 0 ||
    Buffer.byteLength(value, 'utf8') > MAXIMUM_PROTECTED_PATH_BYTES || !isAbsolute(value) ||
    resolve(value) !== value || !SAFE_PROTECTED_PATH_PATTERN.test(value)
  ) throw new Error(`${name} must be one canonical injection-safe absolute path`)
  const metadata = lstatSync(value)
  if (metadata.isSymbolicLink() || !metadata.isFile()) {
    throw new Error(`${name} has the wrong protected leaf type`)
  }
  const canonical = realpathSync(value)
  if (!samePlatformPath(canonical, value)) {
    throw new Error(`${name} must not traverse a symbolic-link alias`)
  }
  const expected = join(REPOSITORY_ROOT, 'test-results', 'browser-network-completion.json')
  if (!samePlatformPath(canonical, expected)) {
    throw new Error(`${name} must name the canonical transferred completion artifact`)
  }
  return canonical
}

function sameIdentity(left, right) {
  return left.dev === right.dev && left.ino === right.ino
}

function sameRevision(left, right) {
  return left.size === right.size && left.mtimeNs === right.mtimeNs &&
    left.ctimeNs === right.ctimeNs && left.mode === right.mode
}

function samePlatformPath(left, right) {
  return process.platform === 'win32'
    ? left.toLowerCase() === right.toLowerCase()
    : left === right
}

function snapshotProtectedAssignments(value) {
  if (nodeTypes.isProxy(value) || !Array.isArray(value)) {
    throw new Error('protected path assignments must be one inert array')
  }
  const result = []
  for (const entry of snapshotDenseDataArray(value, 'protected path assignments')) {
    const pair = snapshotDenseDataArray(entry, 'protected path assignment')
    if (
      pair.length !== 2 || typeof pair[0] !== 'string' || typeof pair[1] !== 'string' ||
      !PROTECTED_PATH_ASSIGNMENTS.has(pair[0])
    ) throw new Error('protected path assignment is invalid')
    result.push(Object.freeze([pair[0], pair[1]]))
  }
  return Object.freeze(result)
}

function snapshotDenseDataArray(value, label) {
  if (nodeTypes.isProxy(value) || !Array.isArray(value)) throw new Error(`${label} must be inert`)
  const descriptors = Object.getOwnPropertyDescriptors(value)
  const names = Reflect.ownKeys(descriptors)
  if (names.some((name) => typeof name !== 'string')) throw new Error(`${label} may not contain symbols`)
  const length = descriptors.length?.value
  if (!Number.isSafeInteger(length) || length < 0 || names.length !== length + 1) {
    throw new Error(`${label} must be dense`)
  }
  const result = []
  for (let index = 0; index < length; index += 1) {
    const descriptor = descriptors[String(index)]
    if (descriptor === undefined || !Object.hasOwn(descriptor, 'value') || descriptor.enumerable !== true) {
      throw new Error(`${label} must contain only enumerable data`)
    }
    result.push(descriptor.value)
  }
  return result
}

function isInternalAuthorityName(name) {
  const folded = name.toUpperCase()
  return INTERNAL_AUTHORITY_VARIABLES.includes(folded) ||
    folded.startsWith('WINDSHARE_MAKE') || folded.startsWith('WINDSHARE_GIT')
}

function hasEnvironmentName(environment, expected) {
  const folded = expected.toUpperCase()
  return Object.keys(environment).some((name) => name.toUpperCase() === folded)
}

function snapshotInvocationArguments(value) {
  if (nodeTypes.isProxy(value) || !Array.isArray(value)) {
    throw new Error('validation Make arguments must be one inert array')
  }
  const descriptors = Object.getOwnPropertyDescriptors(value)
  const names = Reflect.ownKeys(descriptors)
  if (names.some((name) => typeof name !== 'string')) {
    throw new Error('validation Make arguments may not contain symbol fields')
  }
  const length = descriptors.length?.value
  if (
    !Number.isSafeInteger(length) || length < 1 || length > MAXIMUM_MAKE_ARGUMENTS ||
    names.length !== length + 1
  ) {
    throw new Error('validation Make authority requires at least one explicit target')
  }
  const result = []
  for (let index = 0; index < length; index += 1) {
    const descriptor = descriptors[String(index)]
    if (
      descriptor === undefined || !Object.hasOwn(descriptor, 'value') ||
      descriptor.enumerable !== true || typeof descriptor.value !== 'string' ||
      Buffer.byteLength(descriptor.value, 'utf8') > MAXIMUM_MAKE_ARGUMENT_BYTES
    ) throw new Error('validation Make arguments must be enumerable inert strings')
    result.push(descriptor.value)
  }
  return Object.freeze(result)
}

function snapshotEnvironment(value) {
  const nativeProcessEnvironment = value === process.env
  if (
    value === null || typeof value !== 'object' || nodeTypes.isProxy(value) || Array.isArray(value)
  ) throw new Error('validation Make environment must be one inert data record')
  const prototype = Object.getPrototypeOf(value)
  if (!nativeProcessEnvironment && prototype !== Object.prototype && prototype !== null) {
    throw new Error('validation Make environment must be one inert data record')
  }
  const descriptors = Object.getOwnPropertyDescriptors(value)
  const names = Reflect.ownKeys(descriptors)
  if (names.some((name) => typeof name !== 'string')) {
    throw new Error('validation Make environment may not contain symbol entries')
  }
  const folded = new Set()
  const result = Object.create(null)
  if (names.length > MAXIMUM_ENVIRONMENT_ENTRIES) {
    throw new Error('validation Make environment exceeds its entry capacity')
  }
  let totalBytes = 0
  for (const name of names.sort()) {
    const descriptor = descriptors[name]
    if (
      !Object.hasOwn(descriptor, 'value') || descriptor.enumerable !== true ||
      typeof descriptor.value !== 'string' || name.length === 0 ||
      name.includes('=') || name.includes('\0') || descriptor.value.includes('\0') ||
      Buffer.byteLength(name, 'utf8') > MAXIMUM_ENVIRONMENT_NAME_BYTES ||
      Buffer.byteLength(descriptor.value, 'utf8') > MAXIMUM_ENVIRONMENT_VALUE_BYTES
    ) throw new Error('validation Make environment contains active or invalid data')
    totalBytes += Buffer.byteLength(name, 'utf8') + Buffer.byteLength(descriptor.value, 'utf8')
    if (totalBytes > MAXIMUM_ENVIRONMENT_BYTES) {
      throw new Error('validation Make environment exceeds its byte capacity')
    }
    const foldedName = name.toUpperCase()
    if (folded.has(foldedName)) {
      throw new Error('validation Make environment contains case-insensitive duplicate names')
    }
    folded.add(foldedName)
    Object.defineProperty(result, name, {
      value: descriptor.value,
      enumerable: true,
      writable: false,
      configurable: false,
    })
  }
  return Object.freeze(result)
}

function copyEnvironment(environment) {
  const result = Object.create(null)
  for (const [name, value] of Object.entries(environment)) {
    Object.defineProperty(result, name, {
      value,
      enumerable: true,
      writable: true,
      configurable: true,
    })
  }
  return result
}

function setEnvironmentValue(environment, expectedName, value) {
  for (const name of Object.keys(environment)) {
    if (name.toUpperCase() === expectedName.toUpperCase()) delete environment[name]
  }
  Object.defineProperty(environment, expectedName, {
    value,
    enumerable: true,
    writable: true,
    configurable: true,
  })
}

function platformShell() {
  if (process.platform !== 'win32') {
    return requireNativePlatformShell('/bin/bash', Buffer.from([0x7f, 0x45, 0x4c, 0x46]))
  }
  const systemDirectory = nativeWindowsSystemDirectory()
  const executable = join(systemDirectory, 'WindowsPowerShell', 'v1.0', 'powershell.exe')
  return requireNativePlatformShell(executable, Buffer.from([0x4d, 0x5a]))
}

function nativeWindowsSystemDirectory() {
  const report = process.report?.getReport()
  const sharedObjects = report?.sharedObjects
  if (!Array.isArray(sharedObjects)) {
    throw new Error('validation Make authority cannot derive the native Windows system directory')
  }
  const directories = new Map()
  for (const path of sharedObjects) {
    if (typeof path !== 'string' || !WINDOWS_SYSTEM_MODULES.has(basename(path).toLowerCase())) continue
    const canonicalDirectory = dirname(realpathSync(path))
    directories.set(canonicalDirectory.toLowerCase(), canonicalDirectory)
  }
  if (directories.size !== 1) {
    throw new Error('validation Make authority cannot derive one native Windows system directory')
  }
  return directories.values().next().value
}

function requireNativePlatformShell(path, magic) {
  if (!existsSync(path)) throw new Error('validation Make authority requires its fixed platform shell')
  const metadata = lstatSync(path)
  if (!metadata.isFile() || metadata.isSymbolicLink()) {
    throw new Error('validation Make authority requires a native fixed platform shell')
  }
  const descriptor = openSync(path, 'r')
  try {
    const actual = Buffer.alloc(magic.length)
    if (readSync(descriptor, actual, 0, actual.length, 0) !== actual.length || !actual.equals(magic)) {
      throw new Error('validation Make authority requires a native fixed platform shell')
    }
  } finally {
    closeSync(descriptor)
  }
  return path
}

if (process.argv[1] !== undefined
  && realpathSync(process.argv[1]) === realpathSync(fileURLToPath(import.meta.url))) {
  try {
    process.exitCode = runMake(process.argv.slice(2))
  } catch {
    console.error(JSON.stringify({ failureCode: 'validation-make-authority-failed' }))
    process.exitCode = 2
  }
}

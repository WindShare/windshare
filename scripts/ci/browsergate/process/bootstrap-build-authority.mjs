import { spawn } from 'node:child_process'
import { createHash } from 'node:crypto'
import { constants } from 'node:fs'
import { lstat, mkdir, open, realpath } from 'node:fs/promises'
import { isAbsolute, join, resolve } from 'node:path'

export const BOOTSTRAP_BUILD_RECEIPT_SCHEMA_VERSION =
  'windshare.bootstrap-build-receipt/v1'

const BOOTSTRAP_BUILD_DEADLINE_MS = 300_000
const BOOTSTRAP_TERMINATION_GRACE_MS = 5_000
const MAXIMUM_BUILD_OUTPUT_BYTES = 1_048_576
const MAXIMUM_EXECUTABLE_BYTES = 512 * 1024 * 1024
const MAXIMUM_SOURCE_BYTES = 16 * 1024 * 1024
const MAXIMUM_DIAGNOSTIC_PREVIEW_CHARACTERS = 512
const BOOTSTRAP_GO_VERSION_RECIPE_IDENTITY = 'windshare.bootstrap-go-version/v1'
const BOOTSTRAP_OWNER_BUILD_RECIPE_IDENTITY = Object.freeze({
  linux: 'windshare.bootstrap-owner-build/linux-process-owner/v1',
  win32: 'windshare.bootstrap-owner-build/windows-job/v1',
})
const GO_DOWNLOAD_PROGRESS_LINE_PATTERN =
  /^go: downloading ([A-Za-z0-9][A-Za-z0-9._~+!-]*(?:\/[A-Za-z0-9][A-Za-z0-9._~+!-]*)*) (v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)$/u
const GO_SUM_LINE_PATTERN = /^([^\s]+) ([^\s]+?)(?:\/go\.mod)? h1:[A-Za-z0-9+/=]+$/u

const OWNER_SOURCES = Object.freeze({
  linux: Object.freeze([
    'go.mod',
    'go.sum',
    'go.work',
    'go.work.sum',
    'web/scripts/browser-evidence/linuxprocessowner/main_linux.go',
  ]),
  win32: Object.freeze([
    'go.mod',
    'go.sum',
    'go.work',
    'go.work.sum',
    'web/scripts/browser-evidence/windowsjob/main.go',
    'web/scripts/browser-evidence/windowsjob/platform_windows.go',
    'web/scripts/browser-evidence/windowsjob/protocol.go',
  ]),
})

/**
 * Bootstrap is supply-chain authority, not process-containment authority. It is
 * intentionally limited to one owner package, one pinned Go executable, and a
 * secret-free recipe. The receipt therefore has no tree-empty field.
 */
export async function buildBootstrapProcessOwner({
  repositoryRoot,
  runtimeRoot,
  platform,
  architecture = process.arch,
  goExecutable,
  outputPath,
  packagePath,
  cwd,
  deadlineMs = BOOTSTRAP_BUILD_DEADLINE_MS,
}) {
  const root = canonicalAbsolutePath(repositoryRoot, 'bootstrap repository root')
  const privateRoot = canonicalAbsolutePath(runtimeRoot, 'bootstrap runtime root')
  const executable = canonicalAbsolutePath(goExecutable, 'bootstrap Go executable')
  const output = canonicalAbsolutePath(outputPath, 'bootstrap owner output')
  const workingDirectory = canonicalAbsolutePath(cwd, 'bootstrap build working directory')
  const expectedKind = platform === 'linux'
    ? 'linux-process-owner'
    : platform === 'win32'
      ? 'windows-job'
      : null
  const expectedPackage = platform === 'linux'
    ? './web/scripts/browser-evidence/linuxprocessowner'
    : platform === 'win32'
      ? './web/scripts/browser-evidence/windowsjob'
      : null
  if (expectedKind === null || packagePath !== expectedPackage || workingDirectory !== root) {
    throw new Error('bootstrap build is restricted to the current platform owner package')
  }
  requirePositiveInteger(deadlineMs, 'bootstrap build deadline')
  if (deadlineMs > BOOTSTRAP_BUILD_DEADLINE_MS) {
    throw new Error('bootstrap build deadline exceeds its named authority')
  }
  const goArchitecture = canonicalGoArchitecture(architecture)
  const goOS = platform === 'win32' ? 'windows' : 'linux'
  const sourcePaths = OWNER_SOURCES[platform]
  const cacheRoot = join(privateRoot, '.bootstrap-cache')
  const environment = canonicalBootstrapEnvironment({
    cacheRoot,
    repositoryRoot: root,
    goArchitecture,
    goOS,
  })
  await Promise.all([
    mkdir(join(cacheRoot, 'build'), { recursive: true, mode: 0o700 }),
    mkdir(join(cacheRoot, 'modules'), { recursive: true, mode: 0o700 }),
    mkdir(join(cacheRoot, 'gopath'), { recursive: true, mode: 0o700 }),
    mkdir(join(cacheRoot, 'home'), { recursive: true, mode: 0o700 }),
    mkdir(join(cacheRoot, 'tmp'), { recursive: true, mode: 0o700 }),
  ])

  const heldToolchain = await holdRegularFile(executable, MAXIMUM_EXECUTABLE_BYTES, 'Go toolchain')
  const heldSources = []
  let heldOutput
  try {
    for (const sourcePath of sourcePaths) {
      heldSources.push(await holdRegularFile(
        join(root, ...sourcePath.split('/')),
        MAXIMUM_SOURCE_BYTES,
        `bootstrap source ${sourcePath}`,
      ))
    }
    const goSumSource = heldSources[sourcePaths.indexOf('go.sum')]
    const goSumBytes = await goSumSource.readBytes()
    let encodedGoSum
    try {
      encodedGoSum = new TextDecoder('utf-8', { fatal: true }).decode(goSumBytes)
    } catch (cause) {
      throw new Error('bootstrap go.sum is not valid UTF-8', { cause })
    } finally {
      goSumBytes.fill(0)
    }
    const allowedModuleDownloads = goModuleDownloadClosure(encodedGoSum)
    const version = await runSealedBootstrapProcess({
      executable,
      arguments: Object.freeze(['version']),
      cwd: root,
      environment,
      deadlineMs: 30_000,
      operation: 'bootstrap-go-version',
      recipeIdentity: BOOTSTRAP_GO_VERSION_RECIPE_IDENTITY,
      spawnProcess: spawn,
    })
    if (version.stderr !== '' || !version.stdout.endsWith(` ${goOS}/${goArchitecture}\n`) ||
        !/^go version go[^\s]+ [^\s]+\/[^\s]+\n$/u.test(version.stdout)) {
      throw new Error('bootstrap Go version probe emitted an unexpected identity')
    }
    await assertHeldSetLive(heldToolchain, heldSources)

    const arguments_ = Object.freeze([
      'build',
      '-mod=readonly',
      '-trimpath',
      '-buildvcs=false',
      '-ldflags=-buildid=',
      '-o',
      output,
      packagePath,
    ])
    const build = await runSealedBootstrapProcess({
      executable,
      arguments: arguments_,
      cwd: workingDirectory,
      environment,
      deadlineMs,
      operation: `bootstrap-build-${expectedKind}`,
      recipeIdentity: BOOTSTRAP_OWNER_BUILD_RECIPE_IDENTITY[platform],
      spawnProcess: spawn,
    })
    assertBootstrapBuildOutput(build, allowedModuleDownloads)
    await assertHeldSetLive(heldToolchain, heldSources)
    heldOutput = await holdRegularFile(output, MAXIMUM_EXECUTABLE_BYTES, 'bootstrap owner output')
    const sourceEntries = Object.freeze(heldSources.map((source, index) => Object.freeze({
      relativePath: sourcePaths[index],
      byteLength: source.byteLength,
      sha256: source.sha256,
    })))
    const receipt = Object.freeze({
      schemaVersion: BOOTSTRAP_BUILD_RECEIPT_SCHEMA_VERSION,
      kind: expectedKind,
      platform,
      architecture: goArchitecture,
      toolchain: Object.freeze({
        path: heldToolchain.path,
        byteLength: heldToolchain.byteLength,
        sha256: heldToolchain.sha256,
        version: version.stdout.slice(0, -1),
      }),
      source: Object.freeze({
        files: sourceEntries,
        aggregateSha256: sha256(Buffer.from(JSON.stringify(sourceEntries), 'utf8')),
      }),
      recipe: Object.freeze({
        executable,
        arguments: arguments_,
        cwd: workingDirectory,
        environment,
        deadlineMs,
      }),
      process: Object.freeze({
        terminal: 'exited',
        exitCode: 0,
        timedOut: false,
      }),
      output: Object.freeze({
        path: heldOutput.path,
        byteLength: heldOutput.byteLength,
        sha256: heldOutput.sha256,
        identity: heldOutput.identity,
        revision: heldOutput.revision,
      }),
    })
    if (containsForbiddenTreeAuthority(receipt)) {
      throw new Error('bootstrap receipt must not contain process-containment authority')
    }
    let closed = false
    return Object.freeze({
      receipt,
      async assertLive() {
        if (closed) throw new Error('bootstrap build authority is closed')
        await assertHeldSetLive(heldToolchain, [...heldSources, heldOutput])
      },
      async close() {
        if (closed) return
        closed = true
        await Promise.allSettled([
          heldToolchain.close(),
          ...heldSources.map((source) => source.close()),
          heldOutput.close(),
        ])
      },
    })
  } catch (cause) {
    await Promise.allSettled([
      heldToolchain.close(),
      ...heldSources.map((source) => source.close()),
      ...(heldOutput === undefined ? [] : [heldOutput.close()]),
    ])
    throw cause
  }
}

function canonicalBootstrapEnvironment({ cacheRoot, repositoryRoot, goArchitecture, goOS }) {
  const architectureFeature = goArchitecture === 'amd64'
    ? { GOAMD64: 'v1' }
    : { GOARM64: 'v8.0' }
  return Object.freeze(Object.fromEntries(Object.entries({
    CGO_ENABLED: '0',
    GOARCH: goArchitecture,
    GOCACHE: join(cacheRoot, 'build'),
    GOENV: 'off',
    GOEXPERIMENT: '',
    GOFLAGS: '',
    GOMODCACHE: join(cacheRoot, 'modules'),
    GONOSUMDB: '',
    GOOS: goOS,
    GOPATH: join(cacheRoot, 'gopath'),
    GOPRIVATE: '',
    GOPROXY: 'https://proxy.golang.org',
    GOSUMDB: 'sum.golang.org',
    GOTOOLCHAIN: 'local',
    GOWORK: join(repositoryRoot, 'go.work'),
    HOME: join(cacheRoot, 'home'),
    TEMP: join(cacheRoot, 'tmp'),
    TMP: join(cacheRoot, 'tmp'),
    ...architectureFeature,
  }).sort(([left], [right]) => left.localeCompare(right, 'en'))))
}

export async function runSealedBootstrapProcess({
  executable,
  arguments: arguments_,
  cwd,
  environment,
  deadlineMs,
  operation,
  recipeIdentity,
  spawnProcess,
}) {
  const stdout = boundedCollector()
  const stderr = boundedCollector()
  let child
  try {
    child = spawnProcess(executable, arguments_, {
      cwd,
      env: environment,
      shell: false,
      windowsHide: true,
      detached: process.platform !== 'win32',
      stdio: ['ignore', 'pipe', 'pipe'],
    })
  } catch (cause) {
    throw sealedProcessFailure({
      operation,
      recipeIdentity,
      terminal: Object.freeze({
        terminal: 'spawn-failed',
        errorCode: safeErrorCode(cause),
      }),
      timedOut: false,
      stdout: stdout.snapshot(),
      stderr: stderr.snapshot(),
    })
  }
  if (child.stdout === null || child.stderr === null) {
    terminateBestEffort(child, 'SIGKILL')
    throw sealedProcessFailure({
      operation,
      recipeIdentity,
      terminal: Object.freeze({ terminal: 'pipe-contract-failed' }),
      timedOut: false,
      stdout: stdout.snapshot(),
      stderr: stderr.snapshot(),
    })
  }
  const onStdout = stdout.write
  const onStderr = stderr.write
  child.stdout.on('data', onStdout)
  child.stderr.on('data', onStderr)
  const terminal = childTerminal(child)
  let timedOut = false
  let timer
  try {
    const deadline = new Promise((resolveDeadline) => {
      timer = setTimeout(() => {
        timedOut = true
        resolveDeadline(undefined)
      }, deadlineMs)
      timer.ref()
    })
    const first = await Promise.race([terminal, deadline])
    if (first === undefined) {
      terminateBestEffort(child, 'SIGTERM')
      const graceful = await boundedWait(terminal, BOOTSTRAP_TERMINATION_GRACE_MS)
      if (graceful === undefined) {
        terminateBestEffort(child, 'SIGKILL')
        await terminal
      }
    }
    return settleSealedProcess({
      operation,
      recipeIdentity,
      terminal: await terminal,
      timedOut,
      stdout: stdout.snapshot(),
      stderr: stderr.snapshot(),
    })
  } finally {
    if (timer !== undefined) clearTimeout(timer)
    child.stdout.off('data', onStdout)
    child.stderr.off('data', onStderr)
  }
}

function terminateBestEffort(child, signal) {
  try {
    if (process.platform !== 'win32' && child.pid !== undefined) process.kill(-child.pid, signal)
    else child.kill(signal)
  } catch {
    // Bootstrap cleanup is best effort and never appears as tree authority.
  }
}

async function holdRegularFile(path, maximumBytes, label) {
  const canonical = canonicalAbsolutePath(path, label)
  if (await realpath(canonical) !== canonical) throw new Error(`${label} is not its canonical real path`)
  const named = await lstat(canonical, { bigint: true })
  requireBoundedRegularFile(named, maximumBytes, label)
  const handle = await open(canonical, constants.O_RDONLY | constants.O_NOFOLLOW)
  let closed = false
  try {
    const opened = await handle.stat({ bigint: true })
    if (!sameRevision(named, opened)) throw new Error(`${label} changed while opened`)
    const digest = await hashHeldFile(handle, Number(opened.size), label)
    const authority = Object.freeze({
      path: canonical,
      byteLength: Number(opened.size),
      sha256: digest,
      identity: Object.freeze({ dev: String(opened.dev), ino: String(opened.ino) }),
      revision: Object.freeze({
        size: String(opened.size),
        mtimeNs: String(opened.mtimeNs),
        ctimeNs: String(opened.ctimeNs),
        mode: String(opened.mode),
      }),
      async assertLive() {
        if (closed) throw new Error(`${label} held authority is closed`)
        const [currentNamed, currentOpened] = await Promise.all([
          lstat(canonical, { bigint: true }),
          handle.stat({ bigint: true }),
        ])
        if (!sameRevision(opened, currentNamed) || !sameRevision(opened, currentOpened)) {
          throw new Error(`${label} changed while held`)
        }
        if (await hashHeldFile(handle, authority.byteLength, label) !== authority.sha256) {
          throw new Error(`${label} digest changed while held`)
        }
      },
      async readBytes() {
        await authority.assertLive()
        const bytes = await readHeldBytes(handle, authority.byteLength, label)
        await authority.assertLive()
        return bytes
      },
      async close() {
        if (closed) return
        closed = true
        await handle.close()
      },
    })
    return authority
  } catch (cause) {
    await handle.close().catch(() => undefined)
    throw cause
  }
}

async function assertHeldSetLive(toolchain, files) {
  await Promise.all([toolchain.assertLive(), ...files.map((file) => file.assertLive())])
}

async function hashHeldFile(handle, byteLength, label) {
  const digest = createHash('sha256')
  const buffer = Buffer.allocUnsafe(64 * 1024)
  let offset = 0
  try {
    while (offset < byteLength) {
      const expected = Math.min(buffer.byteLength, byteLength - offset)
      const { bytesRead } = await handle.read(buffer, 0, expected, offset)
      if (bytesRead < 1) throw new Error(`${label} ended before its held length`)
      digest.update(buffer.subarray(0, bytesRead))
      offset += bytesRead
    }
    const extra = Buffer.alloc(1)
    try {
      const { bytesRead } = await handle.read(extra, 0, 1, byteLength)
      if (bytesRead !== 0) throw new Error(`${label} grew beyond its held length`)
    } finally {
      extra.fill(0)
    }
    return digest.digest('hex')
  } finally {
    buffer.fill(0)
  }
}

async function readHeldBytes(handle, byteLength, label) {
  const bytes = Buffer.alloc(byteLength)
  let offset = 0
  try {
    while (offset < byteLength) {
      const { bytesRead } = await handle.read(bytes, offset, byteLength - offset, offset)
      if (bytesRead < 1) throw new Error(`${label} ended before its held length`)
      offset += bytesRead
    }
    const extra = Buffer.alloc(1)
    try {
      const { bytesRead } = await handle.read(extra, 0, 1, byteLength)
      if (bytesRead !== 0) throw new Error(`${label} grew beyond its held length`)
    } finally {
      extra.fill(0)
    }
    return bytes
  } catch (cause) {
    bytes.fill(0)
    throw cause
  }
}

function requireBoundedRegularFile(metadata, maximumBytes, label) {
  if (
    !metadata.isFile() || metadata.isSymbolicLink() || metadata.size < 1n ||
    metadata.size > BigInt(maximumBytes)
  ) throw new Error(`${label} is not a bounded regular file`)
}

function sameRevision(left, right) {
  return left.dev === right.dev && left.ino === right.ino && left.size === right.size &&
    left.mtimeNs === right.mtimeNs && left.ctimeNs === right.ctimeNs && left.mode === right.mode
}

function boundedCollector() {
  const chunks = []
  const digest = createHash('sha256')
  let observedByteLength = 0
  let capturedByteLength = 0
  return Object.freeze({
    write(chunk) {
      const bytes = Buffer.from(chunk)
      observedByteLength += bytes.byteLength
      digest.update(bytes)
      const remainingCapacity = MAXIMUM_BUILD_OUTPUT_BYTES - capturedByteLength
      if (remainingCapacity < 1) return
      const captured = bytes.subarray(0, remainingCapacity)
      chunks.push(Buffer.from(captured))
      capturedByteLength += captured.byteLength
    },
    snapshot() {
      const bytes = Buffer.concat(chunks, capturedByteLength)
      let text
      try {
        text = new TextDecoder('utf-8', { fatal: true }).decode(bytes)
      } catch {
        // Invalid bytes are fingerprinted for correlation but never reflected in diagnostics.
      }
      return Object.freeze({
        observedByteLength,
        capturedByteLength,
        truncated: observedByteLength > capturedByteLength,
        sha256: digest.copy().digest('hex'),
        utf8: text === undefined ? 'invalid' : 'valid',
        ...(text === undefined ? {} : { text }),
      })
    },
  })
}

function settleSealedProcess({
  operation,
  recipeIdentity,
  terminal,
  timedOut,
  stdout,
  stderr,
}) {
  const failureReasons = []
  if (timedOut) failureReasons.push('deadline-exceeded')
  if (terminal.terminal !== 'exited' || terminal.exitCode !== 0) {
    failureReasons.push('terminal-not-successful')
  }
  if (stdout.truncated) failureReasons.push('stdout-byte-limit-exceeded')
  if (stderr.truncated) failureReasons.push('stderr-byte-limit-exceeded')
  if (stdout.utf8 !== 'valid') failureReasons.push('stdout-invalid-utf8')
  if (stderr.utf8 !== 'valid') failureReasons.push('stderr-invalid-utf8')
  if (failureReasons.length > 0) {
    throw sealedProcessFailure({
      operation,
      recipeIdentity,
      terminal,
      timedOut,
      stdout,
      stderr,
      failureReasons,
    })
  }
  return Object.freeze({ stdout: stdout.text, stderr: stderr.text })
}

function sealedProcessFailure({
  operation,
  recipeIdentity,
  terminal,
  timedOut,
  stdout,
  stderr,
  failureReasons = Object.freeze(['bootstrap-process-contract-failed']),
}) {
  // Static recipe identities preserve correlation without widening diagnostics to
  // command arguments or environment values, which are authority rather than evidence.
  const diagnostic = Object.freeze({
    operation: diagnosticIdentity(operation, 'bootstrap operation'),
    recipeIdentity: diagnosticIdentity(recipeIdentity, 'bootstrap recipe'),
    failureReasons: Object.freeze([...failureReasons]),
    process: processDiagnostic(terminal, timedOut),
    stdout: outputDiagnostic(stdout),
    stderr: outputDiagnostic(stderr),
  })
  return new Error('bootstrap sealed process failed: ' + JSON.stringify(diagnostic))
}

function processDiagnostic(terminal, timedOut) {
  const kind = diagnosticIdentity(terminal?.terminal, 'bootstrap terminal')
  return Object.freeze({
    terminal: kind,
    exitCode: kind === 'exited' && Number.isSafeInteger(terminal.exitCode)
      ? terminal.exitCode
      : null,
    timedOut,
    ...(kind === 'signaled' ? { signal: safeDiagnosticToken(terminal.signal) } : {}),
    ...(kind === 'spawn-failed' ? { errorCode: safeDiagnosticToken(terminal.errorCode) } : {}),
  })
}

function outputDiagnostic(snapshot) {
  const base = {
    observedByteLength: snapshot.observedByteLength,
    capturedByteLength: snapshot.capturedByteLength,
    truncated: snapshot.truncated,
    sha256: snapshot.sha256,
    utf8: snapshot.utf8,
  }
  if (snapshot.utf8 !== 'valid') return Object.freeze(base)
  const previewTruncated = snapshot.truncated ||
    snapshot.text.length > MAXIMUM_DIAGNOSTIC_PREVIEW_CHARACTERS
  return Object.freeze({
    ...base,
    preview: snapshot.text.slice(0, MAXIMUM_DIAGNOSTIC_PREVIEW_CHARACTERS),
    previewTruncated,
  })
}

function diagnosticIdentity(value, label) {
  if (
    typeof value !== 'string' || value.length < 1 || value.length > 128 ||
    !/^[A-Za-z0-9][A-Za-z0-9./-]*$/u.test(value)
  ) throw new Error(`${label} identity is invalid`)
  return value
}

function safeDiagnosticToken(value) {
  return typeof value === 'string' && /^[A-Za-z0-9_-]{1,64}$/u.test(value)
    ? value
    : 'UNKNOWN'
}

function safeErrorCode(cause) {
  return safeDiagnosticToken(cause?.code)
}

export function assertBootstrapBuildOutput({ stdout, stderr }, allowedDownloads) {
  if (typeof stdout !== 'string' || typeof stderr !== 'string') {
    throw new Error('bootstrap owner build output must be text')
  }
  // The private module cache deliberately starts empty. Go's verified module
  // acquisition progress is therefore expected on a cold build, while any
  // compiler warning, shell text, or malformed diagnostic remains fatal.
  const allowed = new Set(requireStringArray(allowedDownloads, 'allowed Go module downloads'))
  const lines = stderr === '' ? [] : stderr.endsWith('\n')
    ? stderr.slice(0, -1).split('\n')
    : null
  const unexpected = lines === null || lines.some((line) => {
    const match = GO_DOWNLOAD_PROGRESS_LINE_PATTERN.exec(line)
    return match === null || !allowed.has(`${match[1]} ${match[2]}`)
  })
  if (stdout !== '' || unexpected) {
    throw new Error(
      'bootstrap owner build emitted unexpected output: ' +
      `stdout=${diagnosticText(stdout)} stderr=${diagnosticText(stderr)}`,
    )
  }
}

export function goModuleDownloadClosure(encodedGoSum) {
  if (typeof encodedGoSum !== 'string' || encodedGoSum === '' || !encodedGoSum.endsWith('\n')) {
    throw new Error('bootstrap go.sum must be nonempty newline-terminated text')
  }
  const dependencies = new Set()
  for (const line of encodedGoSum.slice(0, -1).split('\n')) {
    const match = GO_SUM_LINE_PATTERN.exec(line)
    if (match === null) throw new Error('bootstrap go.sum contains a malformed entry')
    const module = match[1]
    const version = match[2]
    if (GO_DOWNLOAD_PROGRESS_LINE_PATTERN.exec(`go: downloading ${module} ${version}`) === null) {
      throw new Error('bootstrap go.sum contains a non-portable module identity')
    }
    dependencies.add(`${module} ${version}`)
  }
  return Object.freeze([...dependencies].sort())
}

function requireStringArray(value, label) {
  if (!Array.isArray(value) || value.some((entry) => typeof entry !== 'string')) {
    throw new Error(`${label} must be a string array`)
  }
  return value
}

function diagnosticText(value) {
  return JSON.stringify({
    preview: value.slice(0, MAXIMUM_DIAGNOSTIC_PREVIEW_CHARACTERS),
    previewTruncated: value.length > MAXIMUM_DIAGNOSTIC_PREVIEW_CHARACTERS,
  })
}

function childTerminal(child) {
  return new Promise((resolveTerminal) => {
    let settled = false
    const settle = (value) => {
      if (settled) return
      settled = true
      resolveTerminal(Object.freeze(value))
    }
    child.once('error', (cause) => settle({
      terminal: 'spawn-failed',
      errorCode: safeErrorCode(cause),
    }))
    child.once('close', (code, signal) => settle(code === null
      ? { terminal: 'signaled', signal: signal ?? 'UNKNOWN' }
      : { terminal: 'exited', exitCode: code }))
  })
}

async function boundedWait(promise, maximumWaitMs) {
  let timer
  const timeout = new Promise((resolveTimeout) => {
    timer = setTimeout(() => resolveTimeout(undefined), maximumWaitMs)
    timer.ref()
  })
  try { return await Promise.race([promise, timeout]) } finally {
    if (timer !== undefined) clearTimeout(timer)
  }
}

function canonicalGoArchitecture(value) {
  if (value === 'x64' || value === 'amd64') return 'amd64'
  if (value === 'arm64') return 'arm64'
  throw new Error(`bootstrap Go architecture ${JSON.stringify(value)} is unsupported`)
}

function canonicalAbsolutePath(value, label) {
  if (typeof value !== 'string' || !isAbsolute(value) || resolve(value) !== value) {
    throw new Error(`${label} must be absolute and canonical`)
  }
  return value
}

function requirePositiveInteger(value, label) {
  if (!Number.isSafeInteger(value) || value < 1) throw new Error(`${label} must be positive`)
}

function sha256(bytes) {
  return createHash('sha256').update(bytes).digest('hex')
}

function containsForbiddenTreeAuthority(value) {
  if (Array.isArray(value)) return value.some(containsForbiddenTreeAuthority)
  if (typeof value !== 'object' || value === null) return false
  return Object.entries(value).some(([name, child]) =>
    name.toLowerCase().includes('treeempty') || containsForbiddenTreeAuthority(child))
}

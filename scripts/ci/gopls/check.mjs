import { spawn } from 'node:child_process'
import { readFile } from 'node:fs/promises'
import { resolve } from 'node:path'
import { pathToFileURL } from 'node:url'
import { LspConnection } from './jsonrpc.mjs'
import { buildEnvironment } from './environment.mjs'
import { clientHandlers, compareDiagnostics, diagnosticKey, DOCUMENT_VERSION } from './diagnostics.mjs'

const DIAGNOSE_COMMAND = 'gopls.diagnose_files'
// Bound snapshots and open-file analyses without maintaining an entire module's overlays.
const MAX_OPEN_BATCH_FILES = 8
const ANALYSIS_TIMEOUT_MS = 15 * 60 * 1000
const SHUTDOWN_TIMEOUT_MS = 30_000

export async function diagnose(connection, {
  root, files, log, read = readFile, batchSize = MAX_OPEN_BATCH_FILES,
}) {
  const initialized = await connection.request('initialize', {
    processId: process.pid,
    rootUri: pathToFileURL(root).href,
    capabilities: {
      workspace: { configuration: true },
      window: { workDoneProgress: true },
      textDocument: { publishDiagnostics: { relatedInformation: true } },
    },
  })
  if (!initialized.capabilities?.executeCommandProvider?.commands?.includes(DIAGNOSE_COMMAND)) {
    throw new Error(`Installed gopls does not support ${DIAGNOSE_COMMAND}`)
  }
  await connection.notify('initialized', {})
  let version = initialized.serverInfo?.version ?? null
  try {
    // gopls embeds its dependency manifest here; only the version explains tool drift.
    version = JSON.parse(version)?.Version ?? version
  } catch {
    // LSP also permits a plain version string.
  }
  log('initialized', { files: files.length, server: initialized.serverInfo?.name ?? null, version })
  let submitted = 0
  let batch = []
  const flush = async () => {
    if (batch.length === 0) return
    // Explicit completion preserves hints before close; neither idle time nor
    // the first publication is an analysis barrier.
    await connection.request('workspace/executeCommand', {
      command: DIAGNOSE_COMMAND, arguments: [{ Files: batch }],
    })
    for (const uri of batch) {
      await connection.notify('textDocument/didClose', { textDocument: { uri } })
    }
    log('batch_complete', { files: submitted, total: files.length })
    batch = []
  }
  for (const file of files) {
    if (batch.length === 0) log('opening_batch', { completed: submitted, total: files.length })
    const path = resolve(root, file)
    const uri = pathToFileURL(path).href
    await connection.notify('textDocument/didOpen', {
      textDocument: { uri, languageId: 'go', version: DOCUMENT_VERSION, text: await read(path, 'utf8') },
    })
    submitted++
    batch.push(uri)
    if (batch.length === batchSize) await flush()
  }
  await flush()
  log('analysis_complete', { files: submitted })
}

// Build configuration and diagnostic scope travel together. Sequential owned
// servers prevent profiles from contaminating each other or multiplying peak memory.
export async function checkSessions({
  root, sessions, log = () => {}, timeoutMs = ANALYSIS_TIMEOUT_MS, ...options
}) {
  if (sessions.length === 0) throw new Error('Maintained Go diagnostic session set is empty')
  const deadline = performance.now() + timeoutMs
  const diagnostics = new Map()
  for (const { directory, profile, build, open_files: files, diagnostic_files: diagnosticFiles } of sessions) {
    const remaining = deadline - performance.now()
    if (remaining <= 0) throw new Error('gopls analysis timed out')
    const sessionLog = (event, details) =>
      log(event, { ...details, module: directory, profile, build })
    sessionLog('session_started', { files: files.length, diagnostic_files: diagnosticFiles.length })
    const collected = await checkFiles({
      ...options, root: resolve(root, directory), files, diagnosticFiles, build,
      log: sessionLog, timeoutMs: remaining,
    })
    for (const diagnostic of collected) diagnostics.set(diagnosticKey(diagnostic), diagnostic)
  }
  return [...diagnostics.values()].sort(compareDiagnostics)
}

export async function checkFiles({
  root, files, diagnosticFiles = files, build, log = () => {}, launch = spawn, signal,
  timeoutMs = ANALYSIS_TIMEOUT_MS, shutdownTimeoutMs = SHUTDOWN_TIMEOUT_MS,
}) {
  if (files.length === 0) throw new Error('Maintained Go source set is empty')
  signal?.throwIfAborted()
  const child = launch('gopls', ['serve'], {
    cwd: root, windowsHide: true, stdio: ['pipe', 'pipe', 'inherit'],
    env: buildEnvironment(build),
  })
  const handlers = clientHandlers(root, files, diagnosticFiles, log, build)
  const connection = new LspConnection(child.stdout, child.stdin, handlers)
  const exited = new Promise((done) => {
    child.once('error', (error) => { connection.fail(error); done({ error }) })
    child.once('exit', (code, signal) => {
      connection.fail(new Error(`gopls exited: code=${code}, signal=${signal}`))
      done({ code, signal })
    })
  })
  const abort = () => connection.fail(signal.reason)
  signal?.addEventListener('abort', abort, { once: true })
  const deadline = setTimeout(() => connection.fail(new Error('gopls analysis timed out')), timeoutMs)
  deadline.unref()
  let completed = false
  try {
    await diagnose(connection, { root, files, log })
    if (handlers.failures.length) throw new Error(handlers.failures.join('\n'))
    log('diagnostics_collected', { files: files.length, diagnostics: handlers.diagnostics.size })
    completed = true
    return [...handlers.diagnostics.values()].sort(compareDiagnostics)
  } finally {
    clearTimeout(deadline)
    signal?.removeEventListener('abort', abort)
    // No shared daemon or stale analysis survives the gate, including failures.
    const shutdownDeadline = setTimeout(() => {
      connection.fail(new Error('gopls shutdown timed out'))
      child.kill()
    }, shutdownTimeoutMs)
    try {
      let shutdownError
      let shutdownAcknowledged = false
      try {
        if (!connection.failure) {
          await connection.request('shutdown', null)
          shutdownAcknowledged = true
          await connection.notify('exit')
        }
      } catch (error) {
        shutdownError = error
      }
      if (connection.failure || shutdownError) child.kill()
      child.stdin.end()
      const outcome = await exited
      // Exit may precede the stdin write callback for the exit notification.
      if (completed && ((!shutdownAcknowledged && shutdownError) || outcome.error || outcome.code !== 0)) {
        throw shutdownError ?? outcome.error ?? new Error(`gopls shutdown failed: ${JSON.stringify(outcome)}`)
      }
    } finally {
      clearTimeout(shutdownDeadline)
    }
  }
}

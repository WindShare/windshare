import { spawn } from 'node:child_process'
import { readFile } from 'node:fs/promises'
import { resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { LspConnection } from './jsonrpc.mjs'

const DIAGNOSE_COMMAND = 'gopls.diagnose_files'
const VIEWS_COMMAND = 'gopls.views'
// Each open/close makes gopls reconsider every open file across build views.
// Keep that working set small; retained witnesses still keep each view loaded.
const MAX_OPEN_BATCH_FILES = 8
const HINT_SEVERITY = 4
const DOCUMENT_VERSION = 1
const ANALYSIS_TIMEOUT_MS = 15 * 60 * 1000
// Shutdown drains snapshots from every loaded build view; allow that work to
// finish under the same contention as the gate, while still bounding a hung server.
const SHUTDOWN_TIMEOUT_MS = 30_000

// Match the extra analyses enabled by "gopls check", in addition to gopls defaults.
export const CHECK_SETTINGS = {
  analyses: { fillreturns: true, nonewvars: true, noresultvalues: true, undeclaredname: true },
}

export function clientHandlers(root, files, log) {
  const selected = new Set(files.map((file) => pathToFileURL(resolve(root, file)).href))
  const diagnostics = new Map()
  const failures = []
  return {
    diagnostics,
    failures,
    onRequest(method, params) {
      switch (method) {
        case 'workspace/configuration':
          return { result: params.items.map(({ section }) => section === 'gopls' ? CHECK_SETTINGS : null) }
        case 'window/workDoneProgress/create':
        case 'client/registerCapability':
        case 'client/unregisterCapability':
          return { result: null }
        case 'workspace/workspaceFolders':
          return { result: null }
        default:
          return { error: { code: -32601, message: `Unsupported client request: ${method}` } }
      }
    },
    onNotification(method, params) {
      if (method === 'textDocument/publishDiagnostics' && selected.has(params.uri)) {
        // Like the CLI, retain diagnostics from every build view of an opened file.
        // Replacing each publication could discard an error found for another GOOS.
        if (params.version !== DOCUMENT_VERSION) return
        for (const diagnostic of params.diagnostics) {
          if ((diagnostic.severity ?? 1) > HINT_SEVERITY) continue
          const key = JSON.stringify([params.uri, diagnostic.range, diagnostic.severity,
            diagnostic.code, diagnostic.source, diagnostic.message])
          diagnostics.set(key, { uri: params.uri, ...diagnostic })
        }
      } else if (method === 'window/showMessage') {
        log('server_message', params)
        if (params.type === 1) failures.push(params.message)
      }
    },
  }
}

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
  for (const command of [DIAGNOSE_COMMAND, VIEWS_COMMAND]) {
    if (!initialized.capabilities?.executeCommandProvider?.commands?.includes(command)) {
      throw new Error(`Installed gopls does not support ${command}`)
    }
  }
  await connection.notify('initialized', {})
  log('initialized', { files: files.length })
  let submitted = 0
  let batch = []
  const anchors = new Set()
  const flush = async () => {
    if (batch.length === 0) return
    // Explicit completion preserves hints for every open file. An idle interval
    // or first publishDiagnostics notification is not an analysis barrier.
    await connection.request('workspace/executeCommand', {
      command: DIAGNOSE_COMMAND, arguments: [{ Files: batch }],
    })
    for (const uri of batch) {
      if (!anchors.has(uri)) {
        await connection.notify('textDocument/didClose', { textDocument: { uri } })
      }
    }
    log('batch_complete', { files: submitted, total: files.length, retained: anchors.size })
    batch = []
  }
  let viewIDs = ''
  for (const file of files) {
    if (batch.length === 0) log('opening_batch', { completed: submitted, total: files.length })
    const path = resolve(root, file)
    const uri = pathToFileURL(path).href
    await connection.notify('textDocument/didOpen', {
      textDocument: { uri, languageId: 'go', version: DOCUMENT_VERSION, text: await read(path, 'utf8') },
    })
    submitted++
    batch.push(uri)
    const views = await connection.request('workspace/executeCommand', {
      command: VIEWS_COMMAND, arguments: [],
    })
    const currentIDs = views.map(({ ID }) => ID).sort().join(',')
    if (currentIDs !== viewIDs) {
      anchors.add(uri)
      log('loading_views', { files: submitted, views })
      await connection.request('workspace/executeCommand', {
        command: DIAGNOSE_COMMAND, arguments: [{ Files: [uri] }],
      })
      viewIDs = currentIDs
    }
    if (batch.length === batchSize) await flush()
  }
  await flush()
  log('analysis_complete', { files: submitted })
}

export async function checkFiles({
  root, files, log = () => {}, launch = spawn, signal,
  timeoutMs = ANALYSIS_TIMEOUT_MS, shutdownTimeoutMs = SHUTDOWN_TIMEOUT_MS,
}) {
  if (files.length === 0) throw new Error('Maintained Go source set is empty')
  signal?.throwIfAborted()
  const child = launch('gopls', ['serve'], {
    cwd: root, windowsHide: true, stdio: ['pipe', 'pipe', 'inherit'],
    env: { ...process.env, GOTOOLCHAIN: 'local', GOWORK: 'off' },
  })
  const handlers = clientHandlers(root, files, log)
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
    return [...handlers.diagnostics.values()].sort((a, b) =>
      a.uri.localeCompare(b.uri) || a.range.start.line - b.range.start.line ||
      a.range.start.character - b.range.start.character || a.message.localeCompare(b.message))
  } finally {
    clearTimeout(deadline)
    signal?.removeEventListener('abort', abort)
    // The server belongs to this invocation; no shared daemon or stale analysis
    // state survives the gate. Also reap it after protocol errors or cancellation.
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
      // A successful server exit may arrive before the stdin write callback for
      // the exit notification; the acknowledged shutdown and exit code suffice.
      if (completed && ((!shutdownAcknowledged && shutdownError) || outcome.error || outcome.code !== 0)) {
        throw shutdownError ?? outcome.error ?? new Error(`gopls shutdown failed: ${JSON.stringify(outcome)}`)
      }
    } finally {
      clearTimeout(shutdownDeadline)
    }
  }
}

export function formatDiagnostic(diagnostic) {
  const location = (uri, range) =>
    `${fileURLToPath(uri)}:${range.start.line + 1}:${range.start.character + 1}`
  const lines = [`${location(diagnostic.uri, diagnostic.range)}: ${diagnostic.message}`]
  for (const related of diagnostic.relatedInformation ?? []) {
    lines.push(`${location(related.location.uri, related.location.range)}: - ${related.message}`)
  }
  return lines.join('\n')
}

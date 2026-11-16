import { posix, resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

const ERROR_SEVERITY = 1
const HINT_SEVERITY = 4
export const DOCUMENT_VERSION = 1

// Fixed build contexts eliminate platform inference and the need to retain
// witness files. Each process checks only the packages owned by its scope.
export const CHECK_SETTINGS = {
  analyses: { fillreturns: true, nonewvars: true, noresultvalues: true, undeclaredname: true },
  zeroConfig: false,
}

export function sessionSettings(build, diagnosticFiles) {
  const directories = [...new Set(diagnosticFiles.map((file) => posix.dirname(file)))].sort()
  return {
    ...CHECK_SETTINGS,
    buildFlags: [`-tags=${(build.tags ?? []).join(',')}`],
    directoryFilters: directories.includes('.') ? [] : ['-', ...directories.map((dir) => `+${dir}`)],
  }
}

export function clientHandlers(root, openFiles, diagnosticFiles, log, build) {
  const uriOf = (file) => pathToFileURL(resolve(root, file)).href
  const opened = new Set(openFiles.map(uriOf))
  const selected = new Set(diagnosticFiles.map(uriOf))
  const settings = sessionSettings(build, diagnosticFiles)
  const diagnostics = new Map()
  const failures = []
  return {
    diagnostics,
    failures,
    onRequest(method, params) {
      switch (method) {
        case 'workspace/configuration':
          return { result: params.items.map(({ section }) => section === 'gopls' ? settings : null) }
        case 'window/workDoneProgress/create':
        case 'client/registerCapability':
        case 'client/unregisterCapability':
        case 'workspace/workspaceFolders':
          return { result: null }
        default:
          return { error: { code: -32601, message: `Unsupported client request: ${method}` } }
      }
    },
    onNotification(method, params) {
      if (method === 'textDocument/publishDiagnostics' && selected.has(params.uri)) {
        // Sibling files remain on disk (version 0 or omitted). Rejecting those
        // publications would hide errors caused by a profile's changed declarations.
        if (opened.has(params.uri) ? params.version !== DOCUMENT_VERSION :
          params.version !== undefined && params.version !== 0) return
        for (const diagnostic of params.diagnostics) {
          // A helper can be unused in this profile but needed by another target.
          // Keep lint/hint ownership with selected files; package errors are fatal
          // wherever they occur, including siblings that were never opened.
          const maximum = opened.has(params.uri) ? HINT_SEVERITY : ERROR_SEVERITY
          if ((diagnostic.severity ?? ERROR_SEVERITY) > maximum) continue
          diagnostics.set(diagnosticKey({ uri: params.uri, ...diagnostic }), { uri: params.uri, ...diagnostic })
        }
      } else if (method === 'window/showMessage') {
        log('server_message', params)
        if (params.type === 1) failures.push(params.message)
      }
    },
  }
}

export function diagnosticKey(diagnostic) {
  return JSON.stringify([diagnostic.uri, diagnostic.range, diagnostic.severity,
    diagnostic.code, diagnostic.source, diagnostic.message])
}

export function compareDiagnostics(a, b) {
  return a.uri.localeCompare(b.uri) || a.range.start.line - b.range.start.line ||
    a.range.start.character - b.range.start.character || a.message.localeCompare(b.message)
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

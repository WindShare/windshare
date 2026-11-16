import { spawnSync } from 'node:child_process'
import { randomUUID } from 'node:crypto'
import { fileURLToPath } from 'node:url'
import { checkSessions, formatDiagnostic } from './check.mjs'

const root = fileURLToPath(new URL('../../../', import.meta.url))
const MAX_DIAGNOSTIC_PLAN_BYTES = 16 * 1024 * 1024
const PROGRESS_INTERVAL_MS = 15_000
const started = performance.now()
const operation = randomUUID()
let phase = 'selecting_sources'
const log = (event, details = {}) => {
  phase = event === 'progress' ? phase : event
  console.log(JSON.stringify({
    gate: 'gopls', operation, event, phase,
    elapsed_seconds: Number(((performance.now() - started) / 1000).toFixed(1)), ...details,
  }))
}

const controller = new AbortController()
const interrupt = () => controller.abort(new Error('gopls gate interrupted'))
process.once('SIGINT', interrupt)
process.once('SIGTERM', interrupt)
const progress = setInterval(() => log('progress'), PROGRESS_INTERVAL_MS)
progress.unref()
try {
  log('selecting_sources')
  // Validate the complete pinned dependency projection before launching analysis.
  // The verified plan carries module ownership, build-tag sessions, and lossless paths.
  const selection = spawnSync('go', ['run', './scripts/ci/_piondeps', '-diagnostic-plan'], {
    cwd: root, encoding: 'utf8', maxBuffer: MAX_DIAGNOSTIC_PLAN_BYTES, windowsHide: true,
    env: { ...process.env, GOTOOLCHAIN: 'local', GOWORK: 'off', GOFLAGS: '' },
  })
  if (selection.error) throw selection.error
  if (selection.status !== 0) {
    throw new Error(`Maintained Go source selection failed (${selection.status}): ${selection.stderr}`)
  }
  const sessions = JSON.parse(selection.stdout)
  const fileCount = sessions.reduce((total, session) => total + session.files.length, 0)
  const taggedSessions = sessions.filter((session) => (session.build_tags?.length ?? 0) > 0).length
  log('sources_selected', {
    files: fileCount,
    sessions: sessions.length,
    tagged_sessions: taggedSessions,
    order: 'module-tags-native-first',
  })
  const diagnostics = await checkSessions({ root, sessions, log, signal: controller.signal })
  for (const diagnostic of diagnostics) console.error(formatDiagnostic(diagnostic))
  if (diagnostics.length) throw new Error(`gopls reported ${diagnostics.length} diagnostics`)
  log('passed', { files: fileCount })
} catch (error) {
  log('failed', { error: error.message })
  process.exitCode = 1
} finally {
  clearInterval(progress)
  process.removeListener('SIGINT', interrupt)
  process.removeListener('SIGTERM', interrupt)
}

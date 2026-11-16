import assert from 'node:assert/strict'
import { EventEmitter } from 'node:events'
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { PassThrough } from 'node:stream'
import { pathToFileURL } from 'node:url'
import test from 'node:test'
import { checkFiles, checkSessions, diagnose } from './check.mjs'
import { CHECK_SETTINGS, clientHandlers, formatDiagnostic, sessionSettings } from './diagnostics.mjs'
import { LspConnection } from './jsonrpc.mjs'

const root = resolve('fixture root')
const build = { goos: 'linux', goarch: 'amd64', cgo_enabled: false }
const session = (directory, files, config = build) => ({
  directory, profile: (config.tags ?? []).join(',') || 'default', build: config,
  open_files: files, diagnostic_files: files,
})
const range = { start: { line: 2, character: 4 }, end: { line: 2, character: 5 } }
const capabilities = { executeCommandProvider: { commands: ['gopls.diagnose_files', 'gopls.views'] } }

test('one session submits every file beyond Windows argv limits and awaits final diagnosis', async () => {
  const files = Array.from({ length: 1400 }, (_, i) => `long/path with spaces/文件-${i}.go`)
  assert.ok(files.join(' ').length > 32767)
  const calls = []
  let release
  let finished = false
  const pending = new Promise((done) => { release = done })
  const connection = {
    async request(method, params) {
      calls.push({ method, params })
      if (method === 'initialize') return { capabilities }
      if (params.command === 'gopls.views') return [{ ID: 'native' }]
      return params.arguments[0].Files.length === files.length ? pending : null
    },
    async notify(method, params) { calls.push({ method, params }) },
  }
  const run = diagnose(connection, {
    root, files, batchSize: files.length, log: () => {}, read: async () => 'package fixture\n',
  })
    .then(() => { finished = true })
  while (calls.at(-1)?.params?.arguments?.[0]?.Files?.length !== files.length) {
    await new Promise(setImmediate)
  }
  assert.equal(finished, false)
  const opened = calls.filter(({ method }) => method === 'textDocument/didOpen')
  assert.equal(opened.length, files.length)
  assert.deepEqual(calls.at(-1).params.arguments[0].Files, opened.map(({ params }) => params.textDocument.uri))
  assert.equal(opened[0].params.textDocument.text, 'package fixture\n')
  release(null)
  await run
  assert.equal(finished, true)
})

test('fixed configurations bound open files and diagnose before every close', async () => {
  const files = Array.from({ length: 14 }, (_, i) => `file-${i}.go`)
  const active = new Set()
  const diagnosed = new Set()
  let maxOpen = 0
  const connection = {
    async request(method, params) {
      if (method === 'initialize') return { capabilities }
      assert.equal(params.command, 'gopls.diagnose_files')
      for (const uri of params.arguments[0].Files) {
        assert.ok(active.has(uri))
        diagnosed.add(uri)
      }
      return null
    },
    async notify(method, params) {
      if (method === 'textDocument/didOpen') active.add(params.textDocument.uri)
      if (method === 'textDocument/didClose') {
        assert.ok(diagnosed.has(params.textDocument.uri))
        active.delete(params.textDocument.uri)
      }
      maxOpen = Math.max(maxOpen, active.size)
    },
  }
  await diagnose(connection, { root, files, batchSize: 3, read: async () => '', log: () => {} })
  assert.equal(diagnosed.size, files.length)
  assert.ok(maxOpen <= 3)
  assert.equal(active.size, 0)
})

test('missing command support and unreadable sources fail instead of checking a partial set', async () => {
  const connection = {
    request: async () => ({ capabilities: {} }),
    notify: async () => {},
  }
  await assert.rejects(diagnose(connection, { root, files: ['a.go'], log: () => {} }), /does not support/)
  connection.request = async () => ({ capabilities })
  await assert.rejects(diagnose(connection, {
    root, files: ['a.go'], log: () => {}, read: async () => { throw new Error('source gone') },
  }), /source gone/)
})

test('client collects sibling diagnostics while excluding stale and out-of-scope publications', () => {
  const uri = pathToFileURL(join(root, 'a.go')).href
  const config = { ...build, tags: ['windshare_probe', 'race'] }
  const handlers = clientHandlers(root, ['a.go'], ['a.go', 'sibling.go'], () => {}, config)
  const publish = (uri, version, diagnostics) =>
    handlers.onNotification('textDocument/publishDiagnostics', { uri, version, diagnostics })
  const hint = { severity: 4, range, source: 'unusedparams', message: 'unused parameter' }
  const failure = { severity: 1, range, source: 'compiler', message: 'linux error' }
  publish(uri, 1, [hint, failure])
  publish(uri, 1, [hint])
  publish(uri, 1, [])
  publish(uri, undefined, [{ ...failure, message: 'unopened view error' }])
  publish(uri, 0, [{ ...failure, message: 'stale' }])
  publish(pathToFileURL(join(root, 'unselected.go')).href, 1, [failure])
  publish(pathToFileURL(join(root, 'sibling.go')).href, 0, [
    { ...failure, message: 'sibling error' }, { ...hint, message: 'used by another profile' },
  ])
  publish(pathToFileURL(join(root, 'sibling.go')).href, undefined, [{ ...failure, message: 'sibling error' }])
  publish(pathToFileURL(join(root, 'sibling.go')).href, 2, [{ ...failure, message: 'stale sibling' }])
  assert.equal(handlers.diagnostics.size, 3)
  assert.equal(sessionSettings(build, ['pkg/a.go']).zeroConfig, false)
  assert.deepEqual(sessionSettings(build, ['pkg/a.go']).directoryFilters, ['-', '+pkg'])
  assert.deepEqual(handlers.onRequest('workspace/configuration', {
    items: [{ section: 'gopls' }, { section: 'other' }],
  }), {
    result: [{ ...CHECK_SETTINGS, buildFlags: ['-tags=windshare_probe,race'], directoryFilters: [] }, null],
  })
  handlers.onNotification('window/showMessage', { type: 1, message: 'load failed' })
  assert.deepEqual(handlers.failures, ['load failed'])
  assert.equal(handlers.onRequest('workspace/applyEdit', {}).error.code, -32601)
  assert.equal(formatDiagnostic({ uri, ...hint }), `${join(root, 'a.go')}:3:5: unused parameter`)
})

test('diagnostic sessions isolate module roots and build tags, collect every hint, and close sequentially', async (t) => {
  const root = await mkdtemp(join(tmpdir(), 'windshare-gopls-sessions-'))
  t.after(() => rm(root, { recursive: true, force: true }))
  const sessions = [
    session('.', ['a.go']),
    session('.', ['tagged.go'], { ...build, tags: ['windshare_probe'] }),
    session('nested evidence', ['a.go']),
  ]
  for (const { directory, open_files: files } of sessions) {
    await mkdir(join(root, directory), { recursive: true })
    for (const file of files) await writeFile(join(root, directory, file), 'package fixture\n')
  }
  const servers = []
  const configs = []
  const events = []
  const diagnostics = await checkSessions({
    root, sessions, log: (event, details) => events.push({ event, ...details }),
    launch(command, args, options) {
      if (servers.length) assert.equal(servers.at(-1).stopped(), true)
      const current = sessions[servers.length]
      assert.equal(options.cwd, resolve(root, current.directory))
      assert.equal(options.env.GOFLAGS.trim(), '')
      assert.notEqual(options.env.GOFLAGS, '')
      assert.equal(options.env.GOOS, current.build.goos)
      assert.equal(options.env.GOARCH, current.build.goarch)
      assert.equal(options.env.CGO_ENABLED, '0')
      const server = fakeServer('diagnostic', {
        onInitialized: async (connection) => {
          const response = await connection.request('workspace/configuration', { items: [{ section: 'gopls' }] })
          configs.push(response[0])
        },
      })
      servers.push(server)
      return server.child
    },
  })
  assert.equal(servers.length, sessions.length)
  assert.ok(servers.every((server) => server.stopped()))
  assert.deepEqual(configs, [
    sessionSettings(build, ['a.go']),
    sessionSettings({ ...build, tags: ['windshare_probe'] }, ['tagged.go']),
    sessionSettings(build, ['a.go']),
  ])
  assert.deepEqual(diagnostics.map(({ uri }) => uri).sort(), [
    pathToFileURL(join(root, 'a.go')).href,
    pathToFileURL(join(root, 'tagged.go')).href,
    pathToFileURL(join(root, 'nested evidence', 'a.go')).href,
  ].sort())
  assert.deepEqual(
    events.filter(({ event }) => event === 'session_started').map(({ module, build }) => ({ module, build })),
    [
      { module: '.', build },
      { module: '.', build: { ...build, tags: ['windshare_probe'] } },
      { module: 'nested evidence', build },
    ],
  )
})

test('session failure, cancellation, and an exhausted shared deadline stop further sessions', async (t) => {
  const root = await mkdtemp(join(tmpdir(), 'windshare-gopls-stop-'))
  t.after(() => rm(root, { recursive: true, force: true }))
  await writeFile(join(root, 'a.go'), 'package fixture\n')
  const sessions = [session('.', ['a.go']), session('unreached', ['a.go'])]
  for (const mode of ['crash', 'cancelled', 'deadline']) {
    const controller = new AbortController()
    let launches = 0
    await assert.rejects(checkSessions({
      root, sessions, signal: controller.signal, timeoutMs: mode === 'deadline' ? 0 : 5000,
      launch() {
        launches++
        const server = fakeServer(mode === 'crash' ? 'crash' : 'clean')
        if (mode === 'cancelled') server.child.once('exit', () => controller.abort(new Error('cancelled')))
        return server.child
      },
    }), /exited|cancelled|timed out/)
    assert.equal(launches, mode === 'deadline' ? 0 : 1)
  }
  await assert.rejects(checkSessions({ root, sessions: [] }), /session set is empty/)
})

function fakeServer(mode, hooks = {}) {
  const child = new EventEmitter()
  child.stdout = new PassThrough()
  child.stdin = new PassThrough()
  let stopped = false
  const stop = (code = 0) => {
    if (stopped) return
    stopped = true
    child.emit('exit', code, null)
    child.stdout.end()
  }
  child.kill = () => { stop(1); return true }
  const server = new LspConnection(child.stdin, child.stdout, {
    onRequest(method, params) {
      if (method === 'initialize') {
        if (mode === 'crash') queueMicrotask(() => stop(2))
        return { result: { capabilities } }
      }
      if (params?.command === 'gopls.views') return { result: [{ ID: 'native' }] }
      if (method === 'shutdown' && mode === 'shutdown-error') {
        return { error: { code: -1, message: 'cannot shut down' } }
      }
      return { result: null }
    },
    onNotification(method, params) {
      if (method === 'initialized' && hooks.onInitialized) {
        void hooks.onInitialized(server).catch(() => {})
      }
      if (method === 'textDocument/didOpen') {
        if (mode === 'diagnostic') {
          void server.notify('textDocument/publishDiagnostics', {
            uri: params.textDocument.uri, version: 1,
            diagnostics: [{ severity: 4, range, message: 'fixture hint' }],
          }).catch(() => {})
        }
      }
      if (method === 'exit') queueMicrotask(() => stop(mode === 'exit-error' ? 3 : 0))
    },
  })
  if (mode === 'hung') child.stdin.removeAllListeners('data')
  if (mode === 'shutdown-hung') {
    const dispatch = server.dispatch.bind(server)
    server.dispatch = (message) => {
      if (message.method !== 'shutdown') dispatch(message)
    }
  }
  return { child, stopped: () => stopped }
}

test('owned server returns diagnostics and is reaped on success and failures', async (t) => {
  const directory = await mkdtemp(join(tmpdir(), 'windshare-gopls-test-'))
  t.after(() => rm(directory, { recursive: true, force: true }))
  await writeFile(join(directory, 'a.go'), 'package fixture\n')
  for (const mode of ['clean', 'diagnostic', 'crash', 'shutdown-error', 'exit-error', 'hung', 'shutdown-hung']) {
    await t.test(mode, async () => {
      const server = fakeServer(mode)
      const result = checkFiles({
        root: directory, files: ['a.go'], build, timeoutMs: mode === 'hung' ? 25 : 5000,
        shutdownTimeoutMs: mode === 'shutdown-hung' ? 25 : 5000,
        launch(command, args, options) {
          assert.equal(command, 'gopls')
          assert.deepEqual(args, ['serve'])
          assert.equal(options.windowsHide, true)
          return server.child
        },
      })
      if (mode === 'clean') assert.deepEqual(await result, [])
      else if (mode === 'diagnostic') assert.equal((await result)[0].message, 'fixture hint')
      else await assert.rejects(result, /exited|shutdown|shut down|timed out/)
      assert.equal(server.stopped(), true)
    })
  }
})
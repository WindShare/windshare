import assert from 'node:assert/strict'
import { PassThrough, Writable } from 'node:stream'
import test from 'node:test'
import { LspConnection } from './jsonrpc.mjs'

function frame(message) {
  const body = Buffer.from(JSON.stringify({ jsonrpc: '2.0', ...message }))
  return Buffer.concat([Buffer.from(`Content-Length: ${body.length}\r\n\r\n`), body])
}

function fixture(handlers = {}) {
  const input = new PassThrough()
  const output = new PassThrough()
  const received = []
  const connection = new LspConnection(input, output, {
    onRequest: () => ({ result: null }),
    onNotification: (...message) => received.push(message),
    ...handlers,
  })
  return { input, output, received, connection }
}

test('LSP decodes fragmented UTF-8 frames and multiple frames in one read', async () => {
  const { input, received, connection } = fixture()
  const response = connection.request('initialize', {})
  const bytes = Buffer.concat([
    frame({ method: 'window/logMessage', params: { message: '中文 📂' } }),
    frame({ id: 1, result: { ready: true } }),
  ])
  for (const byte of bytes.subarray(0, bytes.length - 20)) input.write(Buffer.from([byte]))
  input.write(bytes.subarray(bytes.length - 20))
  assert.deepEqual(await response, { ready: true })
  assert.deepEqual(received, [['window/logMessage', { message: '中文 📂' }]])
})

test('LSP writes byte lengths and correlates out-of-order replies', async () => {
  const { input, output, connection } = fixture()
  let written = Buffer.alloc(0)
  output.on('data', (chunk) => { written = Buffer.concat([written, chunk]) })
  const first = connection.request('one', { text: '你好' })
  const second = connection.request('two', {})
  input.write(Buffer.concat([frame({ id: 2, result: 'second' }), frame({ id: 1, result: 'first' })]))
  assert.deepEqual(await Promise.all([first, second]), ['first', 'second'])
  assert.deepEqual(written, Buffer.concat([
    frame({ id: 1, method: 'one', params: { text: '你好' } }),
    frame({ id: 2, method: 'two', params: {} }),
  ]))
})

test('LSP answers server requests without blocking a pending client request', async () => {
  const { input, output, connection } = fixture({
    onRequest: (method, params) => ({ result: [method, params.section] }),
  })
  let written = Buffer.alloc(0)
  output.on('data', (chunk) => { written = Buffer.concat([written, chunk]) })
  const response = connection.request('initialize', {})
  input.write(frame({ id: 'config', method: 'workspace/configuration', params: { section: 'gopls' } }))
  input.write(frame({ id: 1, result: null }))
  await response
  assert.ok(written.includes(frame({ id: 'config', result: ['workspace/configuration', 'gopls'] })))
})

test('LSP errors, truncated streams, and malformed frames fail closed', async (t) => {
  for (const [name, write, expected] of [
    ['RPC error', (input) => input.write(frame({ id: 1, error: { code: -1, message: 'broken' } })), /broken/],
    ['EOF', (input) => input.end(Buffer.from('Content-Length: 100\r\n\r\n{')), /closed/],
    ['missing length', (input) => input.write(Buffer.from('Bad: 1\r\n\r\n{}')), /Content-Length/],
    ['oversized', (input) => input.write(Buffer.from('Content-Length: 999999999\r\n\r\n')), /exceeds/],
    ['bad JSON', (input) => input.write(Buffer.from('Content-Length: 1\r\n\r\nx')), /JSON|Unexpected/],
    ['unknown response', (input) => input.write(frame({ id: 2, result: null })), /Unexpected/],
  ]) {
    await t.test(name, async () => {
      const { input, connection } = fixture()
      const rejected = assert.rejects(connection.request('initialize', {}), expected)
      write(input)
      await rejected
    })
  }
})

test('cancellation rejects a notification blocked by an unresponsive server pipe', async () => {
  const sink = new Writable({ write() {} })
  const connection = new LspConnection(new PassThrough(), sink, {})
  const rejected = assert.rejects(connection.notify('textDocument/didOpen', {}), /cancelled/)
  connection.fail(new Error('cancelled'))
  await rejected
  assert.equal(connection.pendingWrites.size, 0)
  sink.destroy()
})

test('LSP propagates pipe errors to current and subsequent requests', async () => {
  const { output, connection } = fixture()
  const rejected = assert.rejects(connection.request('initialize', {}), /pipe failed/)
  output.emit('error', new Error('pipe failed'))
  await rejected
  await assert.rejects(connection.request('shutdown', null), /pipe failed/)
  await assert.rejects(connection.notify('exit'), /pipe failed/)
})

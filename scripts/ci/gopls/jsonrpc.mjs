// LSP frames count UTF-8 bytes, not JavaScript characters or shell arguments.
const HEADER_SEPARATOR = Buffer.from('\r\n\r\n')
const MAX_MESSAGE_BYTES = 32 * 1024 * 1024
const MAX_HEADER_BYTES = 8192

export class LspConnection {
  constructor(input, output, { onRequest, onNotification }) {
    this.output = output
    this.onRequest = onRequest
    this.onNotification = onNotification
    this.pending = new Map()
    this.pendingWrites = new Set()
    this.nextID = 1
    this.buffer = Buffer.alloc(0)
    this.failure = null
    input.on('data', (chunk) => {
      try {
        this.receive(chunk)
      } catch (error) {
        this.fail(error)
      }
    })
    input.on('error', (error) => this.fail(error))
    output.on('error', (error) => this.fail(error))
    input.on('end', () => this.fail(new Error('gopls closed its protocol stream')))
  }

  fail(error) {
    this.failure ??= error
    for (const { reject } of this.pending.values()) reject(this.failure)
    this.pending.clear()
    for (const { reject } of this.pendingWrites) reject(this.failure)
    this.pendingWrites.clear()
  }

  send(message) {
    if (this.failure) return Promise.reject(this.failure)
    const body = Buffer.from(JSON.stringify({ jsonrpc: '2.0', ...message }))
    const header = Buffer.from(`Content-Length: ${body.length}\r\n\r\n`)
    return new Promise((resolve, reject) => {
      const write = { reject }
      this.pendingWrites.add(write)
      this.output.write(Buffer.concat([header, body]), (error) => {
        this.pendingWrites.delete(write)
        if (error) {
          this.fail(error)
          reject(error)
        } else resolve()
      })
    })
  }

  notify(method, params) {
    return this.send({ method, params })
  }

  request(method, params) {
    if (this.failure) return Promise.reject(this.failure)
    const id = this.nextID++
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject })
      this.send({ id, method, params }).catch((error) => this.fail(error))
    })
  }

  receive(chunk) {
    if (this.failure) return
    this.buffer = Buffer.concat([this.buffer, chunk])
    while (this.buffer.length > 0) {
      const end = this.buffer.indexOf(HEADER_SEPARATOR)
      if (end < 0) {
        if (this.buffer.length > MAX_HEADER_BYTES) throw new Error('LSP header exceeds limit')
        return
      }
      if (end > MAX_HEADER_BYTES) throw new Error('LSP header exceeds limit')
      const match = /^Content-Length: (\d+)\r?$/im.exec(this.buffer.subarray(0, end).toString('ascii'))
      if (!match) throw new Error('LSP frame has no Content-Length')
      const length = Number(match[1])
      if (!Number.isSafeInteger(length) || length > MAX_MESSAGE_BYTES) {
        throw new Error('LSP message exceeds limit')
      }
      const bodyStart = end + HEADER_SEPARATOR.length
      if (this.buffer.length < bodyStart + length) return
      const message = JSON.parse(this.buffer.subarray(bodyStart, bodyStart + length).toString('utf8'))
      this.buffer = this.buffer.subarray(bodyStart + length)
      this.dispatch(message)
    }
  }

  dispatch(message) {
    if (message.jsonrpc !== '2.0') throw new Error('Invalid JSON-RPC version')
    if (message.method) {
      if (message.id !== undefined) {
        const response = this.onRequest(message.method, message.params)
        this.send({ id: message.id, ...response }).catch((error) => this.fail(error))
      } else {
        this.onNotification(message.method, message.params)
      }
      return
    }
    const pending = this.pending.get(message.id)
    if (!pending) throw new Error(`Unexpected JSON-RPC response: ${message.id}`)
    this.pending.delete(message.id)
    if (message.error) {
      pending.reject(new Error(`gopls RPC ${message.error.code}: ${message.error.message}`))
    } else pending.resolve(message.result)
  }
}

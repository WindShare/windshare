import type { NativeObjectFactory, NativeObjectIO, NativeReply, NativeRequest } from './contracts'

export const NATIVE_QUEUE_MAX_REQUESTS = 64
export const NATIVE_QUEUE_MAX_BYTES = 8 * 1024 * 1024
export const NATIVE_WRITE_CHUNK_BYTES = 256 * 1024

export interface NativeWorkerPort {
  postMessage(message: NativeRequest, transfer?: Transferable[]): void
  terminate(): void
  onmessage: ((event: MessageEvent<NativeReply>) => void) | null
  onerror: ((event: ErrorEvent) => void) | null
  onmessageerror: ((event: MessageEvent) => void) | null
}
type RequestBody = NativeRequest extends infer Request
  ? Request extends NativeRequest ? Omit<Request, 'id'> : never : never

export class NativeObjectWorkerClient implements NativeObjectIO {
  readonly #worker: NativeWorkerPort
  readonly #pending = new Map<number, {
    resolve(reply: NativeReply & { ok: true }): void
    reject(error: unknown): void
    bytes: number
  }>()
  #sequence = 0
  #pendingBytes = 0
  #activeRequests = 0
  readonly #waiting: Array<{ bytes: number; resolve(): void; reject(error: unknown): void }> = []
  #failure: unknown
  #closed = false

  constructor(worker: NativeWorkerPort) {
    this.#worker = worker
    worker.onmessage = event => this.#reply(event.data)
    worker.onerror = event => this.#fail(new Error(event.message || 'Native output worker failed'))
    worker.onmessageerror = () => this.#fail(new Error('Native output worker reply could not be decoded'))
  }

  async open(handle: FileSystemFileHandle): Promise<void> {
    await this.#request({ kind: 'open', handle })
  }

  async writeAt(offset: bigint, bytes: Uint8Array): Promise<void> {
    // Each network write can exceed the RPC budget without expanding the queue.
    for (let start = 0; start < bytes.byteLength; start += NATIVE_WRITE_CHUNK_BYTES) {
      const length = Math.min(NATIVE_WRITE_CHUNK_BYTES, bytes.byteLength - start)
      await this.#request(() => ({
        kind: 'write', offset: offset + BigInt(start), bytes: bytes.slice(start, start + length),
      }), length)
    }
  }

  async truncate(length: bigint): Promise<void> { await this.#request({ kind: 'truncate', length }) }
  async flush(): Promise<void> { await this.#request({ kind: 'flush' }) }
  async size(): Promise<bigint> {
    const reply = await this.#request({ kind: 'size' })
    if (reply.size === undefined) throw new Error('Native output size reply is missing')
    return reply.size
  }
  async close(): Promise<void> {
    if (this.#closed) return
    try {
      if (this.#failure === undefined) await this.#request({ kind: 'close' })
    } finally {
      this.#closed = true
      this.#worker.terminate()
    }
  }

  async #request(input: RequestBody | (() => RequestBody), bytes = 0): Promise<NativeReply & { ok: true }> {
    await this.#admit(bytes)
    if (this.#failure !== undefined) throw this.#failure
    const id = ++this.#sequence
    return new Promise((resolve, reject) => {
      this.#pending.set(id, { resolve, reject, bytes })
      try {
        const body = typeof input === 'function' ? input() : input
        const request = { ...body, id } as NativeRequest
        this.#worker.postMessage(request, body.kind === 'write' ? [body.bytes.buffer as ArrayBuffer] : [])
      } catch (error) { this.#fail(error) }
    })
  }

  #reply(reply: NativeReply): void {
    const request = this.#pending.get(reply.id)
    if (request === undefined) return
    if (!reply.ok) {
      this.#fail(new DOMException(reply.message, reply.name))
      return
    }
    this.#pending.delete(reply.id)
    this.#pendingBytes -= request.bytes
    this.#activeRequests -= 1
    this.#drainWaiting()
    request.resolve(reply)
  }

  #admit(bytes: number): Promise<void> {
    if (this.#failure !== undefined) return Promise.reject(this.#failure)
    if (this.#closed) return Promise.reject(new Error('Native output is closed'))
    if (this.#waiting.length >= NATIVE_QUEUE_MAX_REQUESTS) {
      return Promise.reject(new Error('Native output producers exceeded the bounded admission queue'))
    }
    return new Promise((resolve, reject) => {
      this.#waiting.push({ bytes, resolve, reject })
      this.#drainWaiting()
    })
  }

  #drainWaiting(): void {
    while (this.#waiting.length !== 0 && this.#activeRequests < NATIVE_QUEUE_MAX_REQUESTS) {
      const next = this.#waiting[0]!
      if (this.#pendingBytes + next.bytes > NATIVE_QUEUE_MAX_BYTES) return
      this.#waiting.shift()
      this.#activeRequests += 1
      this.#pendingBytes += next.bytes
      next.resolve()
    }
  }

  #fail(error: unknown): void {
    if (this.#failure !== undefined) return
    this.#failure = error
    for (const request of this.#pending.values()) request.reject(error)
    this.#pending.clear()
    for (const waiting of this.#waiting.splice(0)) waiting.reject(error)
    this.#activeRequests = 0
    this.#pendingBytes = 0
    this.#worker.terminate()
  }
}

export const openNativeObject: NativeObjectFactory = async handle => {
  const worker = new Worker(new URL('./worker.ts', import.meta.url), { type: 'module' })
  const client = new NativeObjectWorkerClient(worker)
  await client.open(handle)
  return client
}

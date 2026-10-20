import { Transform, type TransformCallback } from 'node:stream'

const MAX_HEADER_BYTES = 16 * 1024

// Cloudflare binds a share to its public endpoint. A fault proxy must preserve
// that authority while changing only which TCP connection the browser reaches.
export class RelayRequestProxy extends Transform {
  #header = Buffer.alloc(0)
  #forwarding = false
  readonly #authority: string

  constructor(host: string, port: number) {
    super()
    this.#authority = host + ':' + port
  }

  override _transform(chunk: Buffer, _encoding: BufferEncoding, callback: TransformCallback): void {
    if (this.#forwarding) { callback(null, chunk); return }
    this.#header = Buffer.concat([this.#header, chunk])
    const boundary = this.#header.indexOf('\r\n\r\n')
    if (boundary < 0) {
      callback(this.#header.length > MAX_HEADER_BYTES ? new Error('Relay proxy headers exceed limit') : undefined)
      return
    }
    const header = this.#header.subarray(0, boundary).toString('latin1')
      .replace(/^Host:[^\r\n]*/imu, 'Host: ' + this.#authority)
    this.push(Buffer.from(header + '\r\n\r\n', 'latin1'))
    this.push(this.#header.subarray(boundary + 4))
    this.#header = Buffer.alloc(0)
    this.#forwarding = true
    callback()
  }
}

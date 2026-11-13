import { canonicalNetworkHost } from './relay-endpoint'

export function canonicalSTUN(raw: string): string {
  const match = /^stun:(\[[^\]]+\]|[^:/?#@\s]+):(\d+)$/.exec(raw)
  const port = Number(match?.[2])
  if (!match || port < 1 || port > 65535 || !Number.isInteger(port)) throw new TypeError('Invalid STUN endpoint')
  if (/[\s%\\]/.test(match[1]!)) throw new TypeError('Invalid STUN host')
  let host = canonicalNetworkHost(new URL(`https://${match[1]}`))
  if (/^\d+\.\d+\.\d+\.\d+$/.test(host) && host !== match[1]) throw new TypeError('Invalid numeric STUN host')
  const mapped = /^\[::ffff:([a-f\d]+):([a-f\d]+)\]$/.exec(host)
  if (mapped) {
    const high = Number.parseInt(mapped[1]!, 16), low = Number.parseInt(mapped[2]!, 16)
    host = [high >>> 8, high & 255, low >>> 8, low & 255].join('.')
  }
  return `stun:${host}:${port}`
}

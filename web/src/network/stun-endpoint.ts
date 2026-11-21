import { canonicalNetworkHost } from './relay-endpoint'

export function canonicalSTUN(raw: string): string {
  const match = /^stun:(\[[^\]]+\]|[^:/?#@\s]+):(\d+)$/.exec(raw)
  const port = Number(match?.[2])
  if (!match || port < 1 || port > 65535 || !Number.isInteger(port)) throw new TypeError('Invalid STUN endpoint')
  if (/[\s%\\]/.test(match[1]!)) throw new TypeError('Invalid STUN host')
  const host = canonicalNetworkHost(new URL(`https://${match[1]}`))
  if (/^\d+\.\d+\.\d+\.\d+$/.test(host) && host !== match[1] && !match[1]!.startsWith('[')) throw new TypeError('Invalid numeric STUN host')
  return `stun:${host}:${port}`
}

import { canonicalNetworkHost } from './relay-endpoint'

export function canonicalSTUN(raw: string): string {
  const match = /^stun:(\[[^\]]+\]|[^:/?#@\s]+):(\d+)$/.exec(raw)
  const port = Number(match?.[2])
  if (!match || port < 1 || port > 65535 || !Number.isInteger(port)) throw new TypeError('Invalid STUN endpoint')
  return `stun:${canonicalICEHost(match[1]!)}:${port}`
}

export function canonicalICEHost(authority: string): string {
  if (/[\s%\\]/.test(authority)) throw new TypeError('Invalid STUN host')
  const host = canonicalNetworkHost(new URL(`https://${authority}`))
  if (/^\d+\.\d+\.\d+\.\d+$/.test(host) && host !== authority && !authority.startsWith('[')) throw new TypeError('Invalid numeric STUN host')
  return host
}

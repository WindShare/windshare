import { ICEEndpointPool, MAXIMUM_CATALOG_ENDPOINTS } from './endpoints'
import type { NetworkSnapshot } from '../../network/node-directory'

export const EXISTING_DEFAULT_STUN_SERVER = 'stun:stun.l.google.com:19302'

export const CLOUDFLARE_STUN_SERVER = 'stun:stun.cloudflare.com:3478'
export const TWILIO_STUN_SERVER = 'stun:global.stun.twilio.com:3478'

// Provider-published fallbacks make no claim about mainland reachability.
export function defaultICEEndpointPool(): ICEEndpointPool {
 return new ICEEndpointPool([
  { id: 'shipped-google-stun', url: EXISTING_DEFAULT_STUN_SERVER, provider: 'google', failureDomain: 'google-stun' },
  { id: 'public-cloudflare-stun', url: CLOUDFLARE_STUN_SERVER, provider: 'cloudflare', failureDomain: 'cloudflare-stun' },
  { id: 'public-twilio-stun', url: TWILIO_STUN_SERVER, provider: 'twilio', failureDomain: 'twilio-stun' },
 ].map(endpoint => ({ ...endpoint, region: '', family: 'any', trust: 'reviewed', priority: 0, enabled: true })))
}

export function directoryICEEndpointPool(snapshot: NetworkSnapshot | undefined): ICEEndpointPool {
 const entries = [...defaultICEEndpointPool().endpoints], seen = new Set(entries.map(entry => entry.url))
 const add = (url: string, region: string, provider: string, failureDomain: string) => {
  if (seen.has(url) || entries.length >= MAXIMUM_CATALOG_ENDPOINTS) return
  seen.add(url)
  entries.push({ id: `directory:${url}`, url, region, provider, failureDomain, family: 'any', trust: 'reviewed', priority: 0, enabled: true })
 }
 for (const node of snapshot?.nodes ?? []) if (node.status !== 'draining' && node.roles.includes('stun')) {
  for (const url of node.stun) add(url, node.region, node.provider, `${node.operator}:${node.provider}:${node.region}`)
 }
 for (const endpoint of snapshot?.publicSTUN ?? []) add(endpoint.url, endpoint.region, endpoint.provider, endpoint.failureDomain)
 return new ICEEndpointPool(entries)
}

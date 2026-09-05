export const ENDPOINTS_PER_PROFILE = 2
export const ENDPOINTS_PER_WAVE = 4
export const MAXIMUM_CATALOG_ENDPOINTS = 512
export const ENDPOINT_FAILURE_COOLDOWN_MS = 30_000

export interface ICEEndpoint {
 readonly id: string
 readonly url: string
 readonly region: string
 readonly failureDomain: string
 readonly provider: string
 readonly family: 'ipv4' | 'ipv6' | 'any'
 readonly trust: 'reviewed' | 'local' | 'unreviewed'
 readonly priority: number
 readonly enabled: boolean
}

// The caller is the trusted build/deployment/local configuration boundary.
// Never construct this directory from signaling or capability link data.
export class ICEEndpointPool {
 readonly endpoints: readonly ICEEndpoint[]
 constructor(entries: readonly ICEEndpoint[] = []) {
  const seen = new Set<string>()
  if (entries.length > MAXIMUM_CATALOG_ENDPOINTS) throw new Error('invalid trusted ICE endpoint configuration')
  this.endpoints = Object.freeze(entries.map(endpoint => {
   const match = /^stun:(\[[^\]]+\]|[^:/?#@\s]+):(\d+)$/.exec(endpoint.url)
   const port = Number(match?.[2])
   if (!endpoint.id || seen.has(endpoint.id) || !match || port < 1 || port > 65535 ||
    !['ipv4', 'ipv6', 'any'].includes(endpoint.family) ||
    !['reviewed', 'local', 'unreviewed'].includes(endpoint.trust) ||
    (endpoint.enabled && endpoint.trust === 'unreviewed') || !Number.isInteger(endpoint.priority) || endpoint.priority < 0) {
    throw new Error('invalid trusted ICE endpoint configuration')
   }
   seen.add(endpoint.id)
   return Object.freeze({ ...endpoint })
  }))
 }
}

export interface SelectionRequest {
 readonly networkGenerationID: string
 readonly waveID: string
 readonly sequence: number
 readonly nowMs: number
 readonly usedEndpointIDs: readonly string[]
 readonly usedFailureDomains?: readonly string[]
 readonly ipv4: boolean
 readonly ipv6: boolean
}
export interface GenerationFacts {
 readonly networkGenerationID: string
 readonly endpoints: readonly { readonly endpointID: string; readonly failedUntilMs: number }[]
 readonly profiles?: readonly { readonly profileID: string; readonly serverReflexiveProduced: boolean; readonly firstCandidateDelayMs: number }[]
}
export interface AttemptICEProfile {
 readonly id: string
 readonly networkGenerationID: string
 readonly endpointIDs: readonly string[]
 readonly failureDomains: readonly string[]
 readonly urls: readonly string[]
}
export function policyHash(value: string): number {
 let hash = 0x811c9dc5
 for (const byte of new TextEncoder().encode(value)) hash = Math.imul(hash ^ byte, 0x01000193)
 return hash >>> 0
}
export function selectAttemptProfile(pool: ICEEndpointPool, request: SelectionRequest, facts?: GenerationFacts): AttemptICEProfile {
 const used = new Set(request.usedEndpointIDs)
 const domains = new Set((request.usedFailureDomains ?? []).filter(Boolean))
 const remaining = Math.min(ENDPOINTS_PER_PROFILE, Math.max(0, ENDPOINTS_PER_WAVE - used.size))
 const seed = request.networkGenerationID + '|' + request.waveID
 const candidates = pool.endpoints.filter(endpoint => endpoint.enabled && !used.has(endpoint.id) &&
  (request.ipv4 || request.ipv6) && (endpoint.family !== 'ipv4' || request.ipv4) &&
  (endpoint.family !== 'ipv6' || request.ipv6) && (!endpoint.failureDomain || !domains.has(endpoint.failureDomain)) &&
  !(facts?.networkGenerationID === request.networkGenerationID && facts.endpoints.some(fact => fact.endpointID === endpoint.id && request.nowMs < fact.failedUntilMs)))
 candidates.sort((a, b) => {
  if (a.trust !== b.trust) return a.trust === 'reviewed' ? -1 : 1
  if (a.priority !== b.priority) return b.priority - a.priority
  return policyHash(seed + '|' + a.id) - policyHash(seed + '|' + b.id) || (a.id < b.id ? -1 : 1)
 })
 const selected: ICEEndpoint[] = []
 while (candidates.length && selected.length < remaining) {
  selected.push(candidates.splice(diverseEndpointIndex(candidates, selected[0]), 1)[0]!)
 }
 const endpointIDs = Object.freeze(selected.map(endpoint => endpoint.id))
 return Object.freeze({
  id: 'ice-' + policyHash(seed + '|' + request.sequence + '|' + endpointIDs.join(',')).toString(16).padStart(8, '0'),
  networkGenerationID: request.networkGenerationID,
  endpointIDs,
  failureDomains: Object.freeze(selected.map(endpoint => endpoint.failureDomain)),
  urls: Object.freeze(selected.map(endpoint => endpoint.url)),
 })
}

function diverseEndpointIndex(candidates: readonly ICEEndpoint[], first: ICEEndpoint | undefined): number {
 if (!first) return 0
 let index = 0
 let best = -1
 for (let i = 0; i < candidates.length; i++) {
  const endpoint = candidates[i]!
  const score = (endpoint.failureDomain && endpoint.failureDomain !== first.failureDomain ? 2 : 0) +
   (endpoint.region && endpoint.region !== first.region ? 1 : 0)
  if (score > best) { index = i; best = score }
 }
 return index
}

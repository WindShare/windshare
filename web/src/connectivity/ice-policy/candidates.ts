export const DEFAULT_CANDIDATE_LIMIT = 64
const MAXIMUM_CANDIDATE_BYTES = 4096
const CLASS_RESERVE = 2
const MINIMUM_RESERVED_CANDIDATE_LIMIT = 12
const RESERVED_CLASSES = ['lan', 'ipv4', 'ipv6', 'srflx', 'tcp', 'mapped'] as const

export interface CandidateDecision {
 readonly accepted: boolean
 readonly reason: 'accepted' | 'duplicate' | 'reserved' | 'budget' | 'invalid'
 readonly candidateClass: string
}
interface CandidateIdentity { key: string; candidateClass: string; dimensions: string[] }

// Remote signaling retains its hard limit; only locally gathered noise is pruned.
export class CandidateBudget {
 private readonly limit: number
 private sent = 0
 private readonly seen = new Set<string>()
 private readonly classes = new Map<string, number>()
 constructor(limit = DEFAULT_CANDIDATE_LIMIT) {
  this.limit = limit > 0 && limit <= DEFAULT_CANDIDATE_LIMIT ? limit : DEFAULT_CANDIDATE_LIMIT
 }
 accept(candidate: string): CandidateDecision { return this.acceptPath(candidate, false) }
 // Only native provider provenance can establish mapped origin; browsers do not guess it.
 acceptMapped(candidate: string): CandidateDecision { return this.acceptPath(candidate, true) }
 private acceptPath(candidate: string, mapped: boolean): CandidateDecision {
  const path = candidatePath(candidate)
  if (!path) return { accepted: false, reason: 'invalid', candidateClass: 'unknown' }
  if (mapped) { path.candidateClass = 'mapped'; path.dimensions.push('mapped') }
  const { key, candidateClass, dimensions } = path
  if (this.seen.has(key)) return { accepted: false, reason: 'duplicate', candidateClass }
  if (this.sent >= this.limit) return { accepted: false, reason: 'budget', candidateClass }
  const reserve = this.limit >= MINIMUM_RESERVED_CANDIDATE_LIMIT ? RESERVED_CLASSES.reduce((sum, other) =>
   sum + (dimensions.includes(other) ? 0 : Math.max(0, CLASS_RESERVE - (this.classes.get(other) ?? 0))), 0) : 0
  if (this.sent >= this.limit - reserve) return { accepted: false, reason: 'reserved', candidateClass }
  this.seen.add(key)
  this.sent++
  for (const dimension of dimensions) this.classes.set(dimension, (this.classes.get(dimension) ?? 0) + 1)
  return { accepted: true, reason: 'accepted', candidateClass }
 }
}
function candidatePath(candidate: string): CandidateIdentity | undefined {
 if (new TextEncoder().encode(candidate).length > MAXIMUM_CANDIDATE_BYTES) return
 const fields = candidate.replace(/^a=/, '').trim().split(/\s+/)
 if (fields.length < 8 || !fields[0]!.startsWith('candidate:') || fields[6] !== 'typ') return
 const protocol = fields[2]!.toLowerCase()
 const port = Number(fields[5])
 if (!['udp', 'tcp'].includes(protocol) || !/^\d+$/.test(fields[5]!) || port < 1 || port > 65535) return
 const address = fields[4]!.toLowerCase()
 const kind = fields[7]!
 if (!['host', 'srflx', 'prflx', 'relay'].includes(kind)) return
 const path = candidateClasses(address, kind, protocol)
 let tcpType = ''
 for (let i = 8; i + 1 < fields.length; i += 2) if (fields[i] === 'tcptype') tcpType = fields[i + 1]!
 return { ...path, key: [fields[1], protocol, address, fields[5], kind, tcpType].join('|') }
}
function candidateClasses(address: string, kind: string, protocol: string): Omit<CandidateIdentity, 'key'> {
 const dimensions: string[] = []
 const ipv4 = ipv4Octets(address)
 let candidateClass = 'unknown'
 if (ipv4) candidateClass = 'ipv4'
 else if (validIPv6(address)) candidateClass = 'ipv6'
 if (candidateClass !== 'unknown') dimensions.push(candidateClass)
 const local = address.endsWith('.local') || (ipv4 ? localIPv4(ipv4) : validIPv6(address) && /^(fc|fd|fe[89ab])/i.test(address))
 if (kind === 'host' && local) { candidateClass = 'lan'; dimensions.push('lan') }
 if (kind === 'srflx' || kind === 'prflx') { candidateClass = 'srflx'; dimensions.push('srflx') }
 if (protocol === 'tcp') { candidateClass = 'tcp'; dimensions.push('tcp') }
 return { candidateClass, dimensions }
}
function ipv4Octets(address: string): number[] | undefined {
 let literal = address
 if (address.includes(':')) {
  if (!validIPv6(address)) return
  const normalized = new URL('https://[' + address + ']/').hostname.slice(1, -1)
  const mapped = /^::ffff:([\da-f]{1,4}):([\da-f]{1,4})$/i.exec(normalized)
  if (!mapped) return
  const high = Number.parseInt(mapped[1]!, 16)
  const low = Number.parseInt(mapped[2]!, 16)
  literal = [high >>> 8, high & 255, low >>> 8, low & 255].join('.')
 }
 if (!/^\d+\.\d+\.\d+\.\d+$/.test(literal)) return
 const segments = literal.split('.')
 const octets = segments.map(Number)
 if (octets.some((octet, index) => octet > 255 || String(octet) !== segments[index])) return
 return octets
}
function localIPv4(octets: number[]): boolean {
 return octets[0] === 10 || (octets[0] === 172 && octets[1]! >= 16 && octets[1]! <= 31) ||
  (octets[0] === 192 && octets[1] === 168) || (octets[0] === 169 && octets[1] === 254)
}
function validIPv6(address: string): boolean {
 if (!address.includes(':')) return false
 try { return new URL('https://[' + address + ']/').hostname.startsWith('[') } catch { return false }
}

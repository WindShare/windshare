import { concatBytes, decodeBase64Url, encodeBase64Url } from '../crypto/bytes'
import { sha256 } from '../crypto/digest'
import { createEd25519Verifier } from '../crypto/ed25519'
import { canonicalRelayEndpoint } from './relay-endpoint'
import { canonicalSTUN } from './stun-endpoint'
import { array, counter, object, strictJSON, text, timestamp } from './strict-json'
import { validNodeAlias } from './alias'

export const MAXIMUM_ENVELOPE_BYTES = 512 * 1024
export const DIRECTORY_GRACE_MS = 7 * 24 * 60 * 60 * 1000
const DAY_MS = 24 * 60 * 60 * 1000
const CLOCK_SKEW_MS = 5 * 60 * 1000
const encoder = new TextEncoder()
const decoder = new TextDecoder('utf-8', { fatal: true })

export interface NetworkRoot { readonly id: string; readonly key: string }
export interface NetworkNode {
  readonly alias: string; readonly relay: string; readonly identity: string; readonly stun: readonly string[]
  readonly roles: readonly string[]; readonly zone: 'cn' | 'global'; readonly region: string
  readonly provider: string; readonly operator: string; readonly capacityMbps: number
  readonly status: 'active' | 'canary' | 'draining'
}
export interface PublicSTUN { readonly url: string; readonly region: string; readonly provider: string; readonly failureDomain: string }
export interface DirectoryCursor { readonly generation: number; readonly serial: number; readonly digest: string }
export interface NetworkSnapshot {
  readonly cursor: DirectoryCursor; readonly nodes: readonly NetworkNode[]; readonly publicSTUN: readonly PublicSTUN[]
  readonly notBefore: number; readonly trustedUntil: number; readonly envelope: string
}

function base64(value: unknown, maximum: number): Uint8Array<ArrayBuffer> {
  if (typeof value !== 'string' || value.length > Math.ceil(maximum * 4 / 3)) throw new TypeError('Invalid network encoding')
  const decoded = decodeBase64Url(value)
  if (!decoded || decoded.byteLength > maximum) throw new TypeError('Invalid network encoding')
  return decoded
}

async function signature(key: Uint8Array, domain: string, payload: Uint8Array, encoded: unknown): Promise<void> {
  const sig = base64(encoded, 64)
  if (sig.byteLength !== 64 || key.byteLength !== 32 || !await createEd25519Verifier(key).verify(concatBytes([encoder.encode(domain), payload]), sig)) {
    throw new TypeError('Network directory signature rejected')
  }
}

export async function verifyDirectory(envelopeText: string, network: string, roots: readonly NetworkRoot[]): Promise<NetworkSnapshot> {
  if (encoder.encode(envelopeText).byteLength > MAXIMUM_ENVELOPE_BYTES) throw new TypeError('Network directory is too large')
  const envelope = object(strictJSON(envelopeText), ['payload', 'certificate', 'signature'])
  const certificate = object(envelope.certificate, ['root', 'payload', 'signature'])
  const matches = roots.filter(root => root.id === certificate.root)
  if (matches.length !== 1) throw new TypeError('Unknown network root')
  const certificateBytes = base64(certificate.payload, 4096)
  await signature(base64(matches[0]!.key, 32), 'windshare/network-certificate/v1\0', certificateBytes, certificate.signature)
  const authority = object(strictJSON(decoder.decode(certificateBytes)), ['format', 'network', 'generation', 'key', 'not_before', 'expires_at'])
  const generation = counter(authority.generation)
  const notBefore = timestamp(authority.not_before), certificateExpiry = timestamp(authority.expires_at)
  if (authority.format !== 1 || authority.network !== network || certificateExpiry <= notBefore || certificateExpiry - notBefore > 180 * DAY_MS) throw new TypeError('Invalid network certificate')
  const payloadBytes = base64(envelope.payload, MAXIMUM_ENVELOPE_BYTES)
  await signature(base64(authority.key, 32), 'windshare/network-directory/v1\0', payloadBytes, envelope.signature)
  const payload = object(strictJSON(decoder.decode(payloadBytes)), ['format', 'network', 'generation', 'serial', 'issued_at', 'expires_at', 'nodes', 'public_stun'])
  const issuedAt = timestamp(payload.issued_at), expiresAt = timestamp(payload.expires_at)
  const serial = counter(payload.serial)
  if (payload.format !== 1 || payload.network !== network || payload.generation !== generation || issuedAt < notBefore || expiresAt <= issuedAt || expiresAt - issuedAt > 90 * DAY_MS) throw new TypeError('Invalid network payload')
  const aliases = new Set<string>(), identities = new Set<string>(), stunURLs = new Set<string>()
  const stun = (value: unknown): string => {
    if (typeof value !== 'string') throw new TypeError('Invalid STUN endpoint')
    const raw = value, canonical = canonicalSTUN(raw)
    if (raw !== canonical || stunURLs.has(raw)) throw new TypeError('Duplicate or noncanonical STUN endpoint')
    stunURLs.add(raw)
    return raw
  }
  const nodes: NetworkNode[] = []
  for (const value of array(payload.nodes, 256)) {
    const node = object(value, ['alias', 'relay', 'stun', 'roles', 'zone', 'region', 'provider', 'operator', 'capacity_mbps', 'status'])
    if (typeof node.relay !== 'string') throw new TypeError('Invalid relay endpoint')
    const alias = text(node.alias), relay = node.relay
    const identity = (await canonicalRelayEndpoint(relay)).relayIdentityEndpoint
    const roles = array(node.roles, 2).map(text), stuns = array(node.stun, 4).map(stun)
    const capacityMbps = counter(node.capacity_mbps)
    if (!validNodeAlias(alias) || aliases.has(alias) || identities.has(identity) || !relay.startsWith('https://') || /[?#]/.test(relay) ||
      !['cn', 'global'].includes(String(node.zone)) || !['active', 'canary', 'draining'].includes(String(node.status)) || capacityMbps > 0xffffffff ||
      roles.length === 0 || new Set(roles).size !== roles.length || roles.some(role => role !== 'stun' && role !== 'relay') || roles.includes('stun') !== (stuns.length > 0)) throw new TypeError('Invalid network node')
    aliases.add(alias); identities.add(identity)
    nodes.push(Object.freeze({ alias, relay, identity, roles: Object.freeze(roles), stun: Object.freeze(stuns), capacityMbps,
      zone: node.zone as NetworkNode['zone'], status: node.status as NetworkNode['status'], region: text(node.region), provider: text(node.provider), operator: text(node.operator) }))
  }
  const publicSTUN = array(payload.public_stun, 128).map(value => {
    const endpoint = object(value, ['url', 'region', 'provider', 'failure_domain'])
    return Object.freeze({ url: stun(endpoint.url), region: text(endpoint.region), provider: text(endpoint.provider), failureDomain: text(endpoint.failure_domain) })
  })
  return Object.freeze({ cursor: Object.freeze({ generation, serial, digest: encodeBase64Url(await sha256(payloadBytes)) }), nodes: Object.freeze(nodes),
    publicSTUN: Object.freeze(publicSTUN), notBefore: Math.max(notBefore, issuedAt), trustedUntil: Math.min(certificateExpiry, expiresAt + DIRECTORY_GRACE_MS), envelope: envelopeText })
}

export function usableDirectory(snapshot: NetworkSnapshot | undefined, now: number): snapshot is NetworkSnapshot {
  return snapshot !== undefined && now + CLOCK_SKEW_MS >= snapshot.notBefore && now < snapshot.trustedUntil
}

export function rejectRollback(previous: DirectoryCursor | undefined, next: DirectoryCursor): void {
  if (previous && (next.generation < previous.generation || next.generation === previous.generation &&
    (next.serial < previous.serial || next.serial === previous.serial && next.digest !== previous.digest))) throw new Error('Network directory rollback or equivocation')
}

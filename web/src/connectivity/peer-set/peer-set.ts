import { encodeBase64Url } from '../../crypto/bytes'
import type { V2PeerPathIdentity } from '../../session/v2-identities'
import { PeerSlotPool } from '../../session/peer-slots/slots'
import { V2PeerRecoverySupervisor, type V2PeerRecoverySupervisorOptions } from './path'

/** One session owner indexes independent paths; a path never cancels another path's wave. */
export class V2PeerSet {
  readonly #paths = new Map<string, V2PeerRecoverySupervisor>()
  readonly #retiring = new Set<Promise<void>>()
  readonly #slots = new Map<string, PeerSlotPool>()

  path(identity: V2PeerPathIdentity): V2PeerRecoverySupervisor | undefined {
    return this.#paths.get(encodeBase64Url(identity.copyBytes()))
  }

  add(options: Omit<V2PeerRecoverySupervisorOptions, 'slot'>): V2PeerRecoverySupervisor {
    const key = encodeBase64Url(options.peerPathId.copyBytes())
    const existing = this.#paths.get(key)
    if (existing !== undefined) return existing
    const session = encodeBase64Url(options.protocolSessionId.copyBytes())
    let pool = this.#slots.get(session)
    if (pool === undefined) {
      pool = new PeerSlotPool()
      this.#slots.set(session, pool)
    }
    const slot = pool.acquire()
    let path: V2PeerRecoverySupervisor
    try {
      path = new V2PeerRecoverySupervisor({ ...options, slot })
    } catch (error) {
      slot.close()
      throw error
    }
    this.#paths.set(key, path)
    return path
  }

  async close(): Promise<void> {
    await Promise.allSettled([...this.#paths.values()].map((path) => path.close()))
    this.#paths.clear()
    await Promise.allSettled([...this.#retiring])
  }

  async remove(identity: V2PeerPathIdentity): Promise<void> {
    const key = encodeBase64Url(identity.copyBytes())
    const path = this.#paths.get(key)
    if (path === undefined) return
    this.#paths.delete(key)
    const task = path.close()
    this.#retiring.add(task)
    try { await task } finally { this.#retiring.delete(task) }
  }
}

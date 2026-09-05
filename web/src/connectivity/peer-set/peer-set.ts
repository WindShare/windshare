import { encodeBase64Url } from '../../crypto/bytes'
import type { V2PeerPathIdentity } from '../../session/v2-identities'
import { V2PeerRecoverySupervisor, type V2PeerRecoverySupervisorOptions } from './path'

const MAXIMUM_PEER_PATHS = 4

/** One session owner indexes independent paths; a path never cancels another path's wave. */
export class V2PeerSet {
  readonly #paths = new Map<string, V2PeerRecoverySupervisor>()

  path(identity: V2PeerPathIdentity): V2PeerRecoverySupervisor | undefined {
    return this.#paths.get(encodeBase64Url(identity.copyBytes()))
  }

  add(options: V2PeerRecoverySupervisorOptions): V2PeerRecoverySupervisor {
    const key = encodeBase64Url(options.peerPathId.copyBytes())
    const existing = this.#paths.get(key)
    if (existing !== undefined) return existing
    if (this.#paths.size === MAXIMUM_PEER_PATHS) throw new RangeError('Peer path capacity reached')
    const path = new V2PeerRecoverySupervisor(options)
    this.#paths.set(key, path)
    return path
  }

  async close(): Promise<void> {
    await Promise.allSettled([...this.#paths.values()].map((path) => path.close()))
    this.#paths.clear()
  }
}

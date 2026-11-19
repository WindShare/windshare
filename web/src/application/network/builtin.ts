import release from '../../../../deploy/network/release.json'
import { NetworkDirectory } from '../../network/directory'
import { directoryICEEndpointPool } from '../../connectivity/ice-policy/defaults'
import type { ICEEndpointPool } from '../../connectivity/ice-policy/endpoints'
import type { DirectoryObservation } from '../../network/directory'
import type { Suite02CapabilityLink } from '../../crypto/suite02-link'
import { resolveLinkRelays } from './resolve'
import { RelayCandidates } from '../../receiver/relay-candidates'
import { OfficialRelayAuthority } from './authority'
import type { AuthorityConfig, AuthorityObservation } from '../../network/authority/feed'

export const BUILTIN_LINK_HOST = release.link_host
export const BUILTIN_BACKUP_ENTRY = release.backup_entry
export class BuiltinNetwork {
  #directory: NetworkDirectory | undefined
  readonly #authority: OfficialRelayAuthority | undefined
  readonly #reportListeners = new Set<() => void>()
  #reportURL: string | undefined
  readonly #observe: ((event: DirectoryObservation) => void) | undefined
  constructor(observe?: (event: DirectoryObservation) => void, authorityObserve?: (event: AuthorityObservation) => void) {
    this.#observe = observe
    const authority = (release as typeof release & { authority?: AuthorityConfig | null }).authority
    if (authority) this.#authority = new OfficialRelayAuthority(authority, authorityObserve)
  }
  getReportURL = (): string | undefined => this.#reportURL
  observeReport = (listener: () => void): (() => void) => {
    this.#reportListeners.add(listener)
    return () => { this.#reportListeners.delete(listener) }
  }
  clearReport(): void { this.#reportURL = undefined; this.#reportListeners.forEach(listener => listener()) }
  #source(): NetworkDirectory {
    this.#directory ??= new NetworkDirectory({ network: release.network, roots: release.roots,
      mirrors: release.mirrors, ...(release.snapshot === null ? {} : { embedded: JSON.stringify(release.snapshot) }),
      ...(this.#observe === undefined ? {} : { observe: this.#observe }),
      ...(typeof location === 'undefined' ? {} : { origin: location.origin }) })
    return this.#directory
  }
  icePool = (): ICEEndpointPool => directoryICEEndpointPool(release.roots.length === 0 ? undefined : this.#source().snapshot)
  resolve(capability: Suite02CapabilityLink): RelayCandidates {
    const source = capability.nodeAliases.length > 0 || this.#authority ? this.#source() : undefined
    const candidates = resolveLinkRelays(capability, source)
    this.#reportURL = this.#authority?.reportURL(capability.shareId)
    this.#reportListeners.forEach(listener => listener())
    return this.#authority && source ? new RelayCandidates(candidates.bases, candidates.all(), this.#authority.admission(capability.shareId, source)) : candidates
  }
  close(): void { this.#directory?.close(); this.#authority?.close(); this.#reportListeners.clear() }
}

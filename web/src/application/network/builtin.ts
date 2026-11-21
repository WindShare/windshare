import release from '../../../../deploy/network/release.json'
import { NetworkDirectory, type DirectoryObservation, type NetworkDirectoryOptions } from '../../network/directory'
import { directoryICEEndpointPool } from '../../connectivity/ice-policy/defaults'
import type { ICEEndpointPool } from '../../connectivity/ice-policy/endpoints'
import type { NetworkRoot } from '../../network/node-directory'
import type { Suite02CapabilityLink } from '../../crypto/suite02-link'
import { resolveLinkRelays } from './resolve'
import type { RelayCandidates } from '../../receiver/relay-selection/candidates'
import { OfficialRelayAuthority } from './authority'
import type { AuthorityConfig, AuthorityObservation } from '../../network/authority/feed'

export interface BuiltinNetworkRelease {
  readonly network: string
  readonly roots: readonly NetworkRoot[]
  readonly mirrors: readonly string[]
  readonly snapshot: unknown
  readonly authority?: AuthorityConfig | null
}

export interface BuiltinNetworkDependencies {
  readonly release?: BuiltinNetworkRelease
  readonly directory?: Pick<NetworkDirectoryOptions, 'cache' | 'fetch' | 'now' | 'origin'>
}

export class BuiltinNetwork {
  readonly #release: BuiltinNetworkRelease
  readonly #directoryOptions: BuiltinNetworkDependencies['directory']
  #directory: NetworkDirectory | undefined
  readonly #authority: OfficialRelayAuthority | undefined
  readonly #reportListeners = new Set<() => void>()
  #reportURL: string | undefined
  #stopReport: (() => void) | undefined
  readonly #observe: ((event: DirectoryObservation) => void) | undefined
  constructor(
    observe?: (event: DirectoryObservation) => void,
    authorityObserve?: (event: AuthorityObservation) => void,
    dependencies: BuiltinNetworkDependencies = {},
  ) {
    this.#observe = observe
    this.#release = dependencies.release ?? (release as BuiltinNetworkRelease)
    this.#directoryOptions = dependencies.directory
    if (this.#release.authority) this.#authority = new OfficialRelayAuthority(this.#release.authority, authorityObserve)
  }
  getReportURL = (): string | undefined => this.#reportURL
  observeReport = (listener: () => void): (() => void) => {
    this.#reportListeners.add(listener)
    return () => { this.#reportListeners.delete(listener) }
  }
  clearReport(): void {
    this.#stopReport?.()
    this.#stopReport = undefined
    this.#setReportURL(undefined)
  }
  #setReportURL(next: string | undefined): void {
    if (this.#reportURL === next) return
    this.#reportURL = next
    this.#reportListeners.forEach(listener => listener())
  }
  #source(): NetworkDirectory {
    const origin = this.#directoryOptions?.origin ?? (typeof location === 'undefined' ? undefined : location.origin)
    this.#directory ??= new NetworkDirectory({
      network: this.#release.network,
      roots: this.#release.roots,
      // Native release-asset fallbacks (e.g. GitHub Releases) lack CORS headers
      // and cannot be fetched by browsers; wire only live CORS-enabled mirrors.
      mirrors: this.#release.mirrors,
      ...(this.#release.snapshot === null || this.#release.snapshot === undefined ? {} : { embedded: JSON.stringify(this.#release.snapshot) }),
      ...(this.#observe === undefined ? {} : { observe: this.#observe }),
      ...(origin === undefined ? {} : { origin }),
      ...(this.#directoryOptions?.cache === undefined ? {} : { cache: this.#directoryOptions.cache }),
      ...(this.#directoryOptions?.fetch === undefined ? {} : { fetch: this.#directoryOptions.fetch }),
      ...(this.#directoryOptions?.now === undefined ? {} : { now: this.#directoryOptions.now }),
    })
    return this.#directory
  }
  icePool = (): ICEEndpointPool => directoryICEEndpointPool(this.#release.roots.length === 0 ? undefined : this.#source().snapshot)
  resolve(capability: Suite02CapabilityLink): RelayCandidates {
    const source = capability.nodeAliases.length > 0 || this.#authority ? this.#source() : undefined
    const candidates = resolveLinkRelays(capability, source, source && this.#authority?.admission(capability.shareId, source))
    this.#stopReport?.()
    this.#stopReport = undefined
    if (this.#authority && source) {
      let matched = false
      const stop = this.#authority.watchReport(capability.shareId, candidates, source, url => {
        matched = true
        this.#stopReport = undefined
        this.#setReportURL(url)
      })
      if (!matched) {
        this.#stopReport = stop
        this.#setReportURL(undefined)
      }
      return candidates
    }
    this.#setReportURL(undefined)
    return candidates
  }
  close(): void {
    this.#stopReport?.()
    this.#stopReport = undefined
    this.#reportURL = undefined
    this.#directory?.close()
    this.#authority?.close()
    this.#reportListeners.clear()
  }
}

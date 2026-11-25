import release from '../../../../deploy/network/release.json'
import { NetworkDirectory, type DirectoryObservation, type NetworkDirectoryOptions } from '../../network/directory'
import { directoryICEEndpointPool } from '../../connectivity/ice-policy/defaults'
import type { ICEEndpointPool } from '../../connectivity/ice-policy/endpoints'
import type { NetworkRoot } from '../../network/node-directory'
import type { Suite02CapabilityLink } from '../../crypto/suite02-link'
import { resolveLinkRelays } from './resolve'
import type { RelayCandidates } from '../../receiver/relay-selection/candidates'
import { OfficialShareReporter, type ShareReport } from './report'

export interface BuiltinNetworkRelease {
  readonly network: string
  readonly roots: readonly NetworkRoot[]
  readonly mirrors: readonly string[]
  readonly snapshot: unknown
  readonly authority?: { readonly url: string } | null
}

export interface BuiltinNetworkDependencies {
  readonly release?: BuiltinNetworkRelease
  readonly directory?: Pick<NetworkDirectoryOptions, 'cache' | 'fetch' | 'now' | 'origin'>
}

export interface ResolvedLinkRelays {
  readonly candidates: RelayCandidates
  readonly report: ShareReport | undefined
}

export class BuiltinNetwork {
  readonly #release: BuiltinNetworkRelease
  readonly #directoryOptions: BuiltinNetworkDependencies['directory']
  #directory: NetworkDirectory | undefined
  readonly #reporter: OfficialShareReporter | undefined
  readonly #observe: ((event: DirectoryObservation) => void) | undefined
  constructor(
    observe?: (event: DirectoryObservation) => void,
    dependencies: BuiltinNetworkDependencies = {},
  ) {
    this.#observe = observe
    this.#release = dependencies.release ?? (release as BuiltinNetworkRelease)
    this.#directoryOptions = dependencies.directory
    if (this.#release.authority) this.#reporter = new OfficialShareReporter(this.#release.authority.url)
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
  resolve(capability: Suite02CapabilityLink): ResolvedLinkRelays {
    const source = capability.nodeAliases.length > 0 || this.#reporter ? this.#source() : undefined
    const candidates = resolveLinkRelays(capability, source)
    const report = this.#reporter && source
      ? this.#reporter.track(capability.shareId, candidates, source)
      : undefined
    return Object.freeze({ candidates, report })
  }
  close(): void {
    this.#directory?.close()
  }
}

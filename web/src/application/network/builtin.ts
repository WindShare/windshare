import release from '../../../../deploy/network/release.json'
import { NetworkDirectory } from '../../network/directory'
import { directoryICEEndpointPool } from '../../connectivity/ice-policy/defaults'
import type { ICEEndpointPool } from '../../connectivity/ice-policy/endpoints'
import type { DirectoryObservation } from '../../network/directory'
import type { Suite02CapabilityLink } from '../../crypto/suite02-link'
import { resolveLinkRelays } from './resolve'
import type { RelayCandidates } from '../../receiver/relay-candidates'

export const BUILTIN_LINK_HOST = release.link_host
export const BUILTIN_BACKUP_ENTRY = release.backup_entry
export class BuiltinNetwork {
  #directory: NetworkDirectory | undefined
  readonly #observe: ((event: DirectoryObservation) => void) | undefined
  constructor(observe?: (event: DirectoryObservation) => void) { this.#observe = observe }
  #source(): NetworkDirectory {
    this.#directory ??= new NetworkDirectory({ network: release.network, roots: release.roots,
      mirrors: release.mirrors, ...(release.snapshot === null ? {} : { embedded: JSON.stringify(release.snapshot) }),
      ...(this.#observe === undefined ? {} : { observe: this.#observe }),
      ...(typeof location === 'undefined' ? {} : { origin: location.origin }) })
    return this.#directory
  }
  icePool = (): ICEEndpointPool => directoryICEEndpointPool(release.roots.length === 0 ? undefined : this.#source().snapshot)
  resolve(capability: Suite02CapabilityLink): RelayCandidates {
    return resolveLinkRelays(capability, capability.nodeAliases.length === 0 ? undefined : this.#source())
  }
  close(): void { this.#directory?.close() }
}

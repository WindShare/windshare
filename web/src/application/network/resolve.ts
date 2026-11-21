import type { Suite02CapabilityLink } from '../../crypto/suite02-link'
import type { NetworkDirectory } from '../../network/directory'
import { RelayCandidates } from '../../receiver/relay-selection/candidates'
import { canonicalRelayIdentity } from '../../network/relay-endpoint'

import type { RelayAdmission } from '../../receiver/relay-admission'

export function resolveLinkRelays(capability: Suite02CapabilityLink, directory?: NetworkDirectory, admission?: RelayAdmission): RelayCandidates {
  if (capability.nodeAliases.length === 0) return new RelayCandidates(uniqueEndpoints(capability.relays), undefined, admission)
  if (!directory) {
    if (capability.relays.length) return new RelayCandidates(uniqueEndpoints(capability.relays), undefined, admission)
    throw new Error('Network directory is unavailable')
  }
  const source = directory
  const resolve = () => {
    const nodes = source.snapshot?.nodes ?? []
    return capability.nodeAliases.flatMap(alias => { const node = nodes.find(node => node.alias === alias); return node ? [node.relay] : [] })
  }
  const initial = uniqueEndpoints([...capability.relays, ...resolve()])
  return new RelayCandidates(initial, async emit => {
    const publish = () => emit(uniqueEndpoints([...initial, ...resolve()]))
    const stop = source.observeSnapshot(publish)
    try {
      await source.ready
      publish()
      const missing = capability.nodeAliases.filter(alias => !source.snapshot?.nodes.some(node => node.alias === alias))
      if (missing.length) await source.refresh(missing)
      publish()
    } finally { stop() }
  }, admission)
}

function uniqueEndpoints(endpoints: readonly string[]): string[] {
  const seen = new Set<string>()
  return endpoints.filter(endpoint => {
    const identity = canonicalRelayIdentity(endpoint)
    if (seen.has(identity)) return false
    seen.add(identity)
    return true
  })
}

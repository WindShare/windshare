import type { Suite02CapabilityLink } from '../../crypto/suite02-link'
import type { NetworkDirectory } from '../../network/directory'
import type { NetworkSnapshot } from '../../network/contracts/node-directory'
import { RelayCandidates } from '../../receiver/relay-selection/candidates'
import { canonicalRelayIdentity } from '../../network/contracts/relay-endpoint'

export function resolveLinkRelays(capability: Suite02CapabilityLink, directory?: NetworkDirectory): RelayCandidates {
  if (capability.nodeAliases.length === 0) return new RelayCandidates(uniqueEndpoints(capability.relays))
  if (!directory) {
    if (capability.relays.length) return new RelayCandidates(uniqueEndpoints(capability.relays))
    throw new Error('Network directory is unavailable')
  }
  const source = directory
  const resolve = () => {
    const nodes = source.snapshot?.nodes ?? []
    return capability.nodeAliases.flatMap(alias => { const node = nodes.find(node => node.alias === alias); return node ? [node.relay] : [] })
  }
  const missingAliases = () => capability.nodeAliases.filter(alias => !source.snapshot?.nodes.some(node => node.alias === alias))
  const initial = uniqueEndpoints([...capability.relays, ...resolve()])
  let checkedSnapshot: NetworkSnapshot | undefined
  return new RelayCandidates(initial, async (emit, options) => {
    const publish = () => emit(uniqueEndpoints([...initial, ...resolve()]))
    const stop = source.observeSnapshot(publish)
    try {
      await source.ready
      publish()
      const missing = missingAliases()
      if (missing.length === 0) return 'complete'
      // Avoid hammering mirrors on automatic retries when the current verified
      // snapshot already came back from a refresh; explicit user retries still probe.
      if (!options.explicit && checkedSnapshot !== undefined && source.snapshot === checkedSnapshot) {
        return 'incomplete'
      }
      await source.refresh(missing, options)
      publish()
      if (missingAliases().length === 0) return 'complete'
      checkedSnapshot = source.snapshot
      return 'incomplete'
    } finally { stop() }
  })
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

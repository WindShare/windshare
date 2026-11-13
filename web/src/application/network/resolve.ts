import type { Suite02CapabilityLink } from '../../crypto/suite02-link'
import type { NetworkDirectory } from '../../network/directory'
import { RelayCandidates } from '../../receiver/relay-candidates'
import { canonicalRelayIdentity } from '../../network/relay-endpoint'

export function resolveLinkRelays(capability: Suite02CapabilityLink, directory?: NetworkDirectory): RelayCandidates {
  if (capability.nodeAliases.length === 0) return new RelayCandidates(uniqueEndpoints(capability.relays))
  if (!directory) throw new Error('Network directory is unavailable')
  const source = directory
  const resolve = () => {
    const nodes = source.snapshot?.nodes ?? []
    return capability.nodeAliases.flatMap(alias => { const node = nodes.find(node => node.alias === alias); return node ? [node.relay] : [] })
  }
  const initial = uniqueEndpoints([...capability.relays, ...resolve()])
  const expansion = (async () => {
    await source.ready
    if (resolve().length < capability.nodeAliases.length) await source.refresh()
    return uniqueEndpoints([...initial, ...resolve()])
  })()
  return new RelayCandidates(initial, expansion)
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

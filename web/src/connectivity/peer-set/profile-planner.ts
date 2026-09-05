import { defaultICEEndpointPool } from '../ice-policy/defaults'
import { selectAttemptProfile, ICEEndpointPool, type AttemptICEProfile } from '../ice-policy/endpoints'
import { ICEFactStore } from '../ice-policy/facts'
import type { PeerProviderFact } from './provider-facts'
import { PeerNetworkGeneration } from './network-generation'

export class PeerProfilePlanner {
  readonly #pool: ICEEndpointPool
  readonly #network: PeerNetworkGeneration
  #wave = 0
  #usedEndpointIDs: string[] = []
  #usedFailureDomains: string[] = []
  #selected: AttemptICEProfile | undefined
  #rotationConsidered = false
  readonly #facts: ICEFactStore

  constructor(pool?: ICEEndpointPool, network = new PeerNetworkGeneration()) {
    this.#pool = pool ?? defaultICEEndpointPool()
    this.#network = network
    this.#facts = new ICEFactStore(network.id)
  }

  networkChanged(now: number): void {
    this.#network.changed(now)
    this.#facts.setGeneration(this.#network.id)
  }

  select(wave: number, sequence: number, nowMs: number): AttemptICEProfile {
    if (this.#wave !== wave) {
      this.#wave = wave
      this.#usedEndpointIDs = []
      this.#usedFailureDomains = []
      this.#selected = undefined
      this.#rotationConsidered = false
    }
    if (this.#selected !== undefined && this.#rotationConsidered) {
      return this.#reuse(wave, sequence, nowMs)
    }
    this.#rotationConsidered = this.#selected !== undefined
    const profile = selectAttemptProfile(this.#pool, {
      networkGenerationID: this.#network.id, waveID: String(wave),
      sequence, nowMs, usedEndpointIDs: this.#usedEndpointIDs,
      usedFailureDomains: this.#usedFailureDomains, ipv4: true, ipv6: true,
    }, this.#facts.snapshot())
    this.#usedEndpointIDs.push(...profile.endpointIDs)
    this.#usedFailureDomains.push(...profile.failureDomains)
    if (this.#selected === undefined || profile.endpointIDs.length > 0) this.#selected = profile
    return this.#reuse(wave, sequence, nowMs)
  }

  #reuse(wave: number, sequence: number, nowMs: number): AttemptICEProfile {
    // Footprint limits govern discovery of alternatives, never permission to reuse
    // the chosen profile. Only attributable failures can cool an endpoint down.
    const pool = new ICEEndpointPool(this.#pool.endpoints.filter((endpoint) =>
      this.#selected!.endpointIDs.includes(endpoint.id)))
    return selectAttemptProfile(pool, { networkGenerationID: this.#network.id,
      waveID: String(wave), sequence, nowMs, usedEndpointIDs: [], ipv4: true, ipv6: true },
    this.#facts.snapshot())
  }

  observe(profile: AttemptICEProfile, fact: PeerProviderFact, startedAt: number, nowMs: number): void {
    if (fact.kind === 'ice-error') {
      const index = profile.urls.indexOf(fact.endpoint)
      const endpointID = profile.endpointIDs[index]
      if (endpointID !== undefined) this.#facts.recordEndpointFailure(profile, endpointID, nowMs)
    } else if (fact.kind === 'candidate' && fact.candidateType === 'srflx' && fact.disposition === 'accepted') {
      this.#facts.recordProfile(profile, true, Math.max(0, nowMs - startedAt))
    }
  }
}

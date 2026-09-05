import { ENDPOINT_FAILURE_COOLDOWN_MS, MAXIMUM_CATALOG_ENDPOINTS, type AttemptICEProfile, type GenerationFacts } from './endpoints'

const MAXIMUM_PROFILE_FACTS = 64
type EndpointFact = GenerationFacts['endpoints'][number]
type ProfileFact = NonNullable<GenerationFacts['profiles']>[number]

export class ICEFactStore {
 private generation: string
 private readonly endpoints = new Map<string, EndpointFact>()
 private readonly profiles = new Map<string, ProfileFact>()
 constructor(networkGenerationID: string) { this.generation = networkGenerationID }
 setGeneration(networkGenerationID: string): void {
  if (this.generation === networkGenerationID) return
  this.generation = networkGenerationID
  this.endpoints.clear()
  this.profiles.clear()
 }
 recordEndpointFailure(profile: AttemptICEProfile, endpointID: string, nowMs: number): boolean {
  if (profile.networkGenerationID !== this.generation || !profile.endpointIDs.includes(endpointID)) return false
  if (!this.endpoints.has(endpointID) && this.endpoints.size >= MAXIMUM_CATALOG_ENDPOINTS) return false
  this.endpoints.set(endpointID, { endpointID, failedUntilMs: nowMs + ENDPOINT_FAILURE_COOLDOWN_MS })
  return true
 }
 recordProfile(profile: AttemptICEProfile, serverReflexiveProduced: boolean, firstCandidateDelayMs: number): void {
  if (profile.networkGenerationID !== this.generation || firstCandidateDelayMs < 0) return
  if (!this.profiles.has(profile.id) && this.profiles.size >= MAXIMUM_PROFILE_FACTS) {
   const oldest = this.profiles.keys().next().value
   if (oldest !== undefined) this.profiles.delete(oldest)
  }
  this.profiles.set(profile.id, { profileID: profile.id, serverReflexiveProduced, firstCandidateDelayMs })
 }
 snapshot(): GenerationFacts {
  return { networkGenerationID: this.generation, endpoints: [...this.endpoints.values()].map(fact => ({ ...fact })),
   profiles: [...this.profiles.values()].map(fact => ({ ...fact })) }
 }
}

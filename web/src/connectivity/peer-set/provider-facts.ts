export type PeerProviderFact =
  | { readonly kind: 'state'; readonly phase: 'signaling' | 'ice-check' | 'dtls-datachannel'; readonly state: string; readonly elapsedMs: number }
  | { readonly kind: 'candidate'; readonly candidateType: string; readonly protocol: string; readonly family: string; readonly interfaceClass: 'unknown'; readonly endpoint: 'unknown'; readonly disposition: string }
  | { readonly kind: 'selected-pair'; readonly route: 'direct' | 'turn'; readonly localType: string; readonly remoteType: string; readonly protocol: string; readonly family: string; readonly rttMs: number | null; readonly ageMs: number; readonly switchReason: 'initial' | 'pair-changed' | 'sample' }
  | { readonly kind: 'ice-error'; readonly code: number; readonly endpoint: string }
  | { readonly kind: 'profile'; readonly networkGenerationID: string; readonly profileID: string; readonly side: 'receiver'; readonly endpointIDs: string }
  | { readonly kind: 'observer-loss'; readonly count: number }

export function candidateFact(candidate: string, disposition: string): PeerProviderFact {
  const fields = candidate.replace(/^a=/, '').trim().split(/\s+/)
  const address = fields[4] ?? ''
  return Object.freeze({ kind: 'candidate', candidateType: fields[7] ?? 'unknown',
    protocol: fields[2]?.toLowerCase() ?? 'unknown',
    family: addressFamily(address),
    interfaceClass: 'unknown', endpoint: 'unknown', disposition })
}

/** Stats contain credentials and arbitrary browser fields; only this closed projection leaves the adapter. */
export function selectedPairFact(stats: RTCStatsReport, previousID: string | undefined,
  selectedAt: number, now: number): { readonly id: string; readonly fact: Extract<PeerProviderFact, { kind: 'selected-pair' }> } | undefined {
  let pair: Record<string, unknown> | undefined
  stats.forEach((report: Record<string, unknown>) => {
    if (report.type === 'transport' && typeof report.selectedCandidatePairId === 'string') {
      pair = stats.get(report.selectedCandidatePairId) as Record<string, unknown> | undefined
    }
  })
  if (pair === undefined) stats.forEach((report: Record<string, unknown>) => {
    if (report.type === 'candidate-pair' && report.selected === true && report.state === 'succeeded') pair = report
  })
  if (pair === undefined || typeof pair.id !== 'string') return undefined
  const local = stats.get(String(pair.localCandidateId)) as Record<string, unknown> | undefined
  const remote = stats.get(String(pair.remoteCandidateId)) as Record<string, unknown> | undefined
  if (local === undefined || remote === undefined) return undefined
  const localType = knownCandidateType(local.candidateType)
  const remoteType = knownCandidateType(remote.candidateType)
  if (localType === 'unknown' || remoteType === 'unknown') return undefined
  const address = typeof local.address === 'string' ? local.address : ''
  const id = pair.id
  return { id, fact: Object.freeze({ kind: 'selected-pair',
    route: localType === 'relay' || remoteType === 'relay' ? 'turn' : 'direct',
    localType, remoteType, protocol: local.protocol === 'udp' || local.protocol === 'tcp' ? local.protocol : 'unknown',
    family: addressFamily(address),
    rttMs: pairRTT(pair.currentRoundTripTime),
    ageMs: id === previousID ? Math.max(0, now - selectedAt) : 0,
    switchReason: switchReason(previousID, id) }) }
}
function knownCandidateType(value: unknown): string {
  return typeof value === 'string' && ['host', 'srflx', 'prflx', 'relay'].includes(value) ? value : 'unknown'
}

function addressFamily(address: string): string {
  if (address.includes(':')) return 'ipv6'
  return /^\d+\.\d+\.\d+\.\d+$/.test(address) ? 'ipv4' : 'unknown'
}
function switchReason(previous: string | undefined, current: string): 'initial' | 'sample' | 'pair-changed' {
  if (previous === undefined) return 'initial'
  return current === previous ? 'sample' : 'pair-changed'
}
function pairRTT(value: unknown): number | null {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value * 1000 : null
}

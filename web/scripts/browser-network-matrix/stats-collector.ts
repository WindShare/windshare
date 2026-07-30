import { parseNetworkCandidatePath, type NetworkCandidatePath } from './candidate.ts'
import { networkMatrixError } from './contract-support.ts'

interface NetworkStatsRecord {
  readonly id: string
  readonly type: string
  readonly [field: string]: unknown
}

export interface NetworkMatrixStatsSource {
  getStats(): Promise<RTCStatsReport>
}

/**
 * A missing selected pair is an observation, not an exception: it is the
 * expected terminal evidence for the restricted-UDP profile. Ambiguous or
 * internally inconsistent stats still fail closed because they cannot support
 * an auditable candidate-path claim.
 */
export async function collectNetworkCandidatePath(
  source: NetworkMatrixStatsSource,
): Promise<NetworkCandidatePath> {
  const report = await source.getStats()
  const records: unknown[] = []
  report.forEach((record) => records.push(record))
  return networkCandidatePathFromStats(records)
}

export function networkCandidatePathFromStats(
  values: readonly unknown[],
): NetworkCandidatePath {
  const records = values.map(statsRecord)
  const byId = new Map<string, NetworkStatsRecord>()
  for (const record of records) {
    if (byId.has(record.id)) networkMatrixError(`getStats repeats record ID ${record.id}`)
    byId.set(record.id, record)
  }

  const pairId = selectedCandidatePairId(records)
  if (pairId === null) {
    return parseNetworkCandidatePath({
      selectedPair: 'absent',
      localCandidateType: null,
      localAddress: null,
      localPort: null,
      remoteCandidateType: null,
      remoteAddress: null,
      remotePort: null,
      protocol: null,
    })
  }

  const pair = byId.get(pairId)
  if (pair?.type !== 'candidate-pair') {
    networkMatrixError(`selected candidate-pair stats ${pairId} are absent`)
  }
  const localId = requiredReference(pair, 'localCandidateId')
  const remoteId = requiredReference(pair, 'remoteCandidateId')
  const local = byId.get(localId)
  const remote = byId.get(remoteId)
  if (local?.type !== 'local-candidate' || remote?.type !== 'remote-candidate') {
    networkMatrixError('selected candidate pair does not reference local and remote candidate stats')
  }

  const localEndpoint = browserLocalEndpoint(local)
  return parseNetworkCandidatePath({
    selectedPair: 'present',
    localCandidateType: local.candidateType,
    localAddress: localEndpoint.address,
    localPort: localEndpoint.port,
    remoteCandidateType: remote.candidateType,
    remoteAddress: candidateAddress(remote),
    remotePort: remote.port,
    protocol: selectedPairProtocol(pair, local, remote),
  })
}

function browserLocalEndpoint(candidate: NetworkStatsRecord): {
  readonly address: unknown
  readonly port: unknown
} {
  const address = candidateAddress(candidate)
  // Address and port are independently optional in browser stats. Keeping an
  // mDNS name proves privacy masking without pretending it is an observed IP.
  return Object.freeze({ address: address ?? null, port: candidate.port ?? null })
}

function candidateAddress(candidate: NetworkStatsRecord): unknown {
  return candidate.address ?? candidate.ip
}

function selectedCandidatePairId(records: readonly NetworkStatsRecord[]): string | null {
  const transportReferences = uniqueStrings(records
    .filter(({ type }) => type === 'transport')
    .map((record) => record.selectedCandidatePairId))
  if (transportReferences.length !== 0) {
    return exactlyOne(transportReferences, 'transport-selected candidate pair')
  }

  const explicitlySelected = records
    .filter((record) => record.type === 'candidate-pair' && record.selected === true)
    .map(({ id }) => id)
  if (explicitlySelected.length !== 0) {
    return exactlyOne(explicitlySelected, 'explicitly selected candidate pair')
  }

  // WebKit versions that omit `selected` still expose public nominated/state
  // fields. Uniqueness is required so the fallback cannot silently choose.
  const nominated = records
    .filter((record) =>
      record.type === 'candidate-pair' && record.nominated === true && record.state === 'succeeded')
    .map(({ id }) => id)
  if (nominated.length === 0) return null
  return exactlyOne(nominated, 'nominated succeeded candidate pair')
}

function selectedPairProtocol(
  pair: NetworkStatsRecord,
  local: NetworkStatsRecord,
  remote: NetworkStatsRecord,
): string {
  const protocols = uniqueStrings([pair.protocol, local.protocol, remote.protocol]
    .map((value) => typeof value === 'string' ? value.toLowerCase() : value))
  return exactlyOne(protocols, 'selected-pair protocol')
}

function uniqueStrings(values: readonly unknown[]): string[] {
  return [...new Set(values.filter((value): value is string => value !== '' && typeof value === 'string'))]
}

function exactlyOne(values: readonly string[], label: string): string {
  if (values.length !== 1) {
    networkMatrixError(`getStats exposes ${values.length} ${label} records; expected exactly one`)
  }
  return values[0] as string
}

function statsRecord(value: unknown): NetworkStatsRecord {
  if (
    typeof value !== 'object' || value === null ||
    typeof (value as { id?: unknown }).id !== 'string' ||
    (value as { id: string }).id === '' ||
    typeof (value as { type?: unknown }).type !== 'string' ||
    (value as { type: string }).type === ''
  ) networkMatrixError('getStats returned a record without non-empty string id and type')
  return value as NetworkStatsRecord
}

function requiredReference(record: NetworkStatsRecord, field: string): string {
  const reference = record[field]
  if (typeof reference !== 'string' || reference === '') {
    networkMatrixError(`${record.type} stats ${record.id} lack ${field}`)
  }
  return reference
}

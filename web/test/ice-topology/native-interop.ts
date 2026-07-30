import {
  parseBrowserSelectedPair,
  parsePionSelectedPair,
  type BrowserIceCandidateEvidence,
  type BrowserSelectedPairEvidence,
  type PionSelectedPairEvidence,
} from '../../scripts/browser-evidence/attempt-evidence'
import type {
  NativeInteropEvidence,
  NativeInteropFailureCode,
} from '../../scripts/browser-evidence/result'
import {
  contractError,
  requireCanonicalIdentity,
  requireExactKeys,
  requireRecord,
  requireSha256,
} from '../../scripts/browser-evidence/contract/json'
import {
  parseTestIceTopology,
  parseTestIceTopologyResolution,
  selectedPairAllowedByTopology,
  verifyTestIceTopologyLock,
  type VerifiedTestIceTopologyLock,
} from '../../scripts/browser-evidence/test-ice-topology'

const MAXIMUM_NETWORK_PORT = 65_535
const NATIVE_INTEROP_FAILURE_MESSAGE_MAXIMUM_LENGTH = 512

export interface SerializedTestIceTopologyLock {
  readonly profile: unknown
  readonly resolution: unknown
  readonly profileSha256: string
  readonly resolutionSha256: string
}

export interface NativeInteropFailureDetails {
  readonly failureCode: NativeInteropFailureCode
  readonly failureMessage: string
}

export interface ClassifiedNativeInteropFailureEvidence extends NativeInteropEvidence {
  readonly failureCode: NativeInteropFailureCode
  readonly failureMessage: string
}

interface StatsRecord {
  readonly id: string
  readonly type: string
  readonly [field: string]: unknown
}

export async function verifySerializedTestIceTopologyLock(
  value: unknown,
): Promise<VerifiedTestIceTopologyLock> {
  const serialized = requireRecord(value, 'serialized test ICE topology lock')
  requireExactKeys(
    serialized,
    ['profile', 'resolution', 'profileSha256', 'resolutionSha256'],
    [],
    'serialized test ICE topology lock',
  )
  const profileSha256 = requireSha256(serialized.profileSha256, 'topology profile SHA-256')
  const resolutionSha256 = requireSha256(
    serialized.resolutionSha256,
    'topology resolution SHA-256',
  )
  const profile = parseTestIceTopology(serialized.profile)
  const resolution = parseTestIceTopologyResolution(
    serialized.resolution,
    profile,
    profileSha256,
  )
  return verifyTestIceTopologyLock(profile, resolution, profileSha256, resolutionSha256)
}

export async function captureBrowserSelectedPair(
  peer: RTCPeerConnection,
): Promise<BrowserSelectedPairEvidence> {
  const report = await peer.getStats()
  const records: StatsRecord[] = []
  report.forEach((value) => {
    records.push(statsRecord(value))
  })
  return selectedPairFromStats(records)
}

export function selectedPairFromStats(values: readonly unknown[]): BrowserSelectedPairEvidence {
  const records = values.map(statsRecord)
  const byID = new Map(records.map((record) => [record.id, record]))
  const pairID = selectedCandidatePairID(records)
  const pair = byID.get(pairID)
  if (pair?.type !== 'candidate-pair') {
    throw new Error(`selected candidate-pair stats ${pairID} are absent`)
  }
  const localID = requiredStatsReference(pair, 'localCandidateId')
  const remoteID = requiredStatsReference(pair, 'remoteCandidateId')
  const local = byID.get(localID)
  const remote = byID.get(remoteID)
  if (local?.type !== 'local-candidate' || remote?.type !== 'remote-candidate') {
    throw new Error('selected candidate pair does not reference local and remote candidate stats')
  }
  return parseBrowserSelectedPair({
    candidatePairId: pairID,
    local: browserCandidateFromStats(local),
    remote: browserCandidateFromStats(remote),
  })
}

export function validateNativeInteropProof(
  browserAttemptID: unknown,
  browserPairValue: unknown,
  pionAttemptID: unknown,
  pionPairValue: unknown,
  topologyLock: VerifiedTestIceTopologyLock,
): NativeInteropEvidence {
  const browserAttempt = requireCanonicalIdentity(browserAttemptID, 'browser native attempt ID')
  const pionAttempt = requireCanonicalIdentity(pionAttemptID, 'Pion native attempt ID')
  if (browserAttempt !== pionAttempt) {
    contractError('native browser and Pion evidence identify different attempts')
  }
  const browserPair = parseBrowserSelectedPair(browserPairValue)
  const pionPair = parsePionSelectedPair(pionPairValue)
  if (
    !selectedPairAllowedByTopology(
      browserPair,
      topologyLock.profile,
      topologyLock.resolution,
    ) ||
    !selectedPairAllowedByTopology(pionPair, topologyLock.profile, topologyLock.resolution) ||
    !selectedPairsCorrelate(browserPair, pionPair)
  ) {
    contractError('native interop lacks a correlated resolution-bound direct selected pair')
  }
  return Object.freeze({
    browser: Object.freeze({ attemptId: browserAttempt, selectedPair: browserPair }),
    pion: Object.freeze({ attemptId: pionAttempt, selectedPair: pionPair }),
  })
}

export function buildNativeInteropFailureEvidence(
  browserAttemptID: unknown,
  browserPairValue: unknown | null,
  pionAttemptID: unknown,
  pionPairValue: unknown | null,
  cause: unknown,
  pionErrors: readonly string[] = [],
): ClassifiedNativeInteropFailureEvidence {
  const browserAttempt = requireCanonicalIdentity(browserAttemptID, 'browser native attempt ID')
  const pionAttempt = requireCanonicalIdentity(pionAttemptID, 'Pion native attempt ID')
  if (browserAttempt !== pionAttempt) {
    contractError('failed native browser and Pion evidence identify different attempts')
  }
  const failure = classifyNativeInteropFailure(cause, pionErrors)
  return Object.freeze({
    browser: Object.freeze({
      attemptId: browserAttempt,
      selectedPair: browserPairValue === null ? null : parseBrowserSelectedPair(browserPairValue),
    }),
    pion: Object.freeze({
      attemptId: pionAttempt,
      selectedPair: pionPairValue === null ? null : parsePionSelectedPair(pionPairValue),
    }),
    failureCode: failure.failureCode,
    failureMessage: failure.failureMessage,
  })
}

/**
 * Pion's in-process error is more authoritative than the browser's resulting
 * channel closure. Preserving both keeps the terminal useful while classifying
 * the stage that actually failed rather than its downstream symptom.
 */
export function classifyNativeInteropFailure(
  cause: unknown,
  pionErrors: readonly string[] = [],
): NativeInteropFailureDetails {
  const browserFailure = diagnosticText(cause)
  const authoritativePionFailures = pionErrors
    .map((message) => message.trim())
    .filter((message) => message !== '')
  const failureMessage = (authoritativePionFailures.length === 0
    ? browserFailure
    : [
        ...authoritativePionFailures.map((message) => `Pion: ${message}`),
        `Browser: ${browserFailure}`,
      ].join('; ')
  ).slice(0, NATIVE_INTEROP_FAILURE_MESSAGE_MAXIMUM_LENGTH)
  const authoritativeText = authoritativePionFailures.join(' ')
  return Object.freeze({
    failureCode: authoritativeText === ''
      ? failureCodeFrom(cause, browserFailure)
      : failureCodeFrom(authoritativeText, authoritativeText),
    failureMessage,
  })
}

function selectedCandidatePairID(records: readonly StatsRecord[]): string {
  const transportIDs = uniqueReferences(records, 'transport', 'selectedCandidatePairId')
  if (transportIDs.length > 0) return exactlyOne(transportIDs, 'transport-selected candidate pair')

  const explicitlySelected = records
    .filter((record) => record.type === 'candidate-pair' && record.selected === true)
    .map(({ id }) => id)
  if (explicitlySelected.length > 0) {
    return exactlyOne(explicitlySelected, 'explicitly selected candidate pair')
  }

  // Some standards-compliant stats reports omit `selected`; a unique nominated,
  // succeeded pair is still public evidence, while ambiguity fails closed.
  const nominated = records
    .filter((record) =>
      record.type === 'candidate-pair' && record.nominated === true && record.state === 'succeeded')
    .map(({ id }) => id)
  return exactlyOne(nominated, 'nominated succeeded candidate pair')
}

function uniqueReferences(
  records: readonly StatsRecord[],
  type: string,
  field: string,
): string[] {
  return [...new Set(records
    .filter((record) => record.type === type && typeof record[field] === 'string')
    .map((record) => record[field] as string))]
}

function exactlyOne(values: readonly string[], label: string): string {
  if (values.length !== 1 || values[0] === '') {
    throw new Error(`getStats exposed ${values.length} ${label} records; expected exactly one`)
  }
  return values[0] as string
}

function statsRecord(value: unknown): StatsRecord {
  if (
    typeof value !== 'object' || value === null ||
    typeof (value as { id?: unknown }).id !== 'string' ||
    typeof (value as { type?: unknown }).type !== 'string'
  ) {
    throw new Error('getStats returned a record without string id and type')
  }
  return value as StatsRecord
}

function requiredStatsReference(record: StatsRecord, field: string): string {
  const value = record[field]
  if (typeof value !== 'string' || value === '') {
    throw new Error(`${record.type} stats ${record.id} lack ${field}`)
  }
  return value
}

function browserCandidateFromStats(record: StatsRecord): BrowserIceCandidateEvidence {
  const candidateType = record.candidateType
  const protocol = record.protocol
  if (typeof candidateType !== 'string' || typeof protocol !== 'string') {
    throw new Error(`${record.type} stats ${record.id} lack candidate type or protocol`)
  }
  const address = optionalStatsAddress(record)
  const port = optionalStatsPort(record)
  return {
    candidateId: record.id,
    candidateType: candidateType as BrowserIceCandidateEvidence['candidateType'],
    protocol: protocol.toLowerCase() as BrowserIceCandidateEvidence['protocol'],
    ...(address === undefined ? {} : { address }),
    ...(port === undefined ? {} : { port }),
  }
}

function optionalStatsAddress(record: StatsRecord): string | undefined {
  if (record.address === undefined || record.address === '') return undefined
  if (typeof record.address !== 'string') {
    throw new Error(`${record.type} stats ${record.id} expose an invalid address`)
  }
  return record.address
}

function optionalStatsPort(record: StatsRecord): number | undefined {
  if (record.port === undefined || record.port === 0) return undefined
  if (
    !Number.isSafeInteger(record.port) ||
    (record.port as number) < 1 ||
    (record.port as number) > MAXIMUM_NETWORK_PORT
  ) {
    throw new Error(`${record.type} stats ${record.id} expose an invalid port`)
  }
  return record.port as number
}

function selectedPairsCorrelate(
  browser: BrowserSelectedPairEvidence,
  pion: PionSelectedPairEvidence,
): boolean {
  return browserEndpointMatchesPion(browser.local, pion.remote) &&
    browserEndpointMatchesPion(browser.remote, pion.local)
}

function browserEndpointMatchesPion(
  browser: BrowserSelectedPairEvidence['local'],
  pion: PionSelectedPairEvidence['local'],
): boolean {
  return (browser.address === undefined || !isIPLiteral(browser.address) || browser.address === pion.address) &&
    (browser.port === undefined || browser.port === pion.port)
}

function isIPLiteral(address: string): boolean {
  return address.includes(':') || /^(?:0|[1-9]\d{0,2})(?:\.(?:0|[1-9]\d{0,2})){3}$/u.test(address)
}

function failureCodeFrom(cause: unknown, message: string): NativeInteropFailureCode {
  const normalized = message.toLowerCase()
  const deadlineException = cause instanceof DOMException && cause.name === 'TimeoutError'
  if (normalized.includes('candidate') || normalized.includes('selected pair')) return 'selected-pair'
  if (normalized.includes('protocol')) return 'protocol'
  if (
    deadlineException ||
    normalized.includes('timed out') || normalized.includes('deadline') || normalized.includes('exceeded')
  ) {
    return 'interop-deadline'
  }
  if (normalized.includes('datachannel') || normalized.includes('channel')) return 'datachannel'
  if (
    normalized.includes('offer') || normalized.includes('answer') ||
    normalized.includes('negotiat') || normalized.includes('ice gathering') ||
    normalized.includes('peer connection')
  ) {
    return 'negotiation'
  }
  return 'unexpected'
}

function diagnosticText(error: unknown): string {
  const message = error instanceof Error ? error.message : String(error)
  return (message.trim() || 'unexpected native interop failure')
    .slice(0, NATIVE_INTEROP_FAILURE_MESSAGE_MAXIMUM_LENGTH)
}

import type {
  BrowserSelectedPairEvidence,
  PionSelectedPairEvidence,
} from './attempt-evidence.ts'
import {
  contractError,
  freezeRecord,
  requireArray,
  requireEnum,
  requireExactKeys,
  requireLiteral,
  requireRecord,
  requireSafeInteger,
  requireSha256,
  requireString,
} from './contract/json.ts'
import { parseCanonicalJsonText } from './contract/strict-json.ts'
import type { IceCandidateType, IceProtocol } from './vocabulary.ts'

export const TEST_ICE_TOPOLOGY_PROFILE_SCHEMA_VERSION = 1 as const
export const TEST_ICE_TOPOLOGY_RESOLUTION_SCHEMA_VERSION = 1 as const
export const TEST_ICE_TOPOLOGY_MAXIMUM_FILE_BYTES = 16_384 as const
export const PR_TEST_ICE_TOPOLOGY_ID = 'pr-same-host-kernel-route-ipv4' as const
export const TEST_ICE_SOURCE_SELECTOR_ALGORITHM = 'udp-connect-source-consensus-v1' as const
export const TEST_ICE_PROBE_DESTINATIONS = Object.freeze([
  Object.freeze({ address: '192.0.2.1' as const, port: 9 as const }),
  Object.freeze({ address: '198.51.100.1' as const, port: 9 as const }),
  Object.freeze({ address: '203.0.113.1' as const, port: 9 as const }),
] as const)
export const TEST_ICE_ADDRESS_FAMILIES = Object.freeze(['ipv4'] as const)
export const TEST_ICE_TRANSPORT_POLICIES = Object.freeze(['all'] as const)
export const TEST_ICE_SELECTED_PAIR_TYPES = Object.freeze(['host', 'prflx'] as const)
export const TEST_ICE_PROTOCOLS = Object.freeze(['udp'] as const)

export interface TestIceTopology {
  readonly topologyProfileSchemaVersion: typeof TEST_ICE_TOPOLOGY_PROFILE_SCHEMA_VERSION
  readonly topologyId: typeof PR_TEST_ICE_TOPOLOGY_ID
  readonly sourceSelector: {
    readonly algorithm: typeof TEST_ICE_SOURCE_SELECTOR_ALGORITHM
    readonly probeDestinations: typeof TEST_ICE_PROBE_DESTINATIONS
  }
  readonly addressFamily: (typeof TEST_ICE_ADDRESS_FAMILIES)[number]
  readonly rtcConfiguration: {
    readonly iceServers: readonly []
    readonly iceTransportPolicy: (typeof TEST_ICE_TRANSPORT_POLICIES)[number]
  }
  readonly candidatePolicy: {
    readonly allowedSelectedPairTypes: readonly ['host', 'prflx']
    readonly allowedProtocols: readonly ['udp']
  }
}

export interface TestIceTopologyResolution {
  readonly topologyResolutionSchemaVersion: typeof TEST_ICE_TOPOLOGY_RESOLUTION_SCHEMA_VERSION
  readonly topologyId: typeof PR_TEST_ICE_TOPOLOGY_ID
  readonly topologyProfileSha256: string
  readonly selectorAlgorithm: typeof TEST_ICE_SOURCE_SELECTOR_ALGORITHM
  readonly addressFamily: (typeof TEST_ICE_ADDRESS_FAMILIES)[number]
  readonly probeResults: readonly TestIceProbeResult[]
  readonly interface: TestIceResolvedInterface
}

export interface TestIceProbeResult {
  readonly destinationAddress: string
  readonly destinationPort: number
  readonly sourceAddress: string
}

export interface TestIceResolvedInterface {
  readonly index: number
  readonly name: string
  readonly selectedAddress: string
  readonly eligibleAddresses: readonly TestIceEligibleAddress[]
}

export interface TestIceEligibleAddress {
  readonly address: string
  readonly prefixLength: number
}

export interface VerifiedTestIceTopologyLock {
  readonly profile: TestIceTopology
  readonly resolution: TestIceTopologyResolution
  readonly profileSha256: string
  readonly resolutionSha256: string
}

const VERIFIED_TEST_ICE_TOPOLOGY_LOCKS = new WeakSet<object>()

export function parseTestIceTopology(value: unknown): TestIceTopology {
  const profile = requireRecord(value, 'test ICE topology')
  requireExactKeys(
    profile,
    [
      'topologyProfileSchemaVersion',
      'topologyId',
      'sourceSelector',
      'addressFamily',
      'rtcConfiguration',
      'candidatePolicy',
    ],
    [],
    'test ICE topology',
  )
  const selector = requireRecord(profile.sourceSelector, 'test ICE source selector')
  requireExactKeys(selector, ['algorithm', 'probeDestinations'], [], 'test ICE source selector')
  requireExactProbeDestinations(selector.probeDestinations)
  const rtc = requireRecord(profile.rtcConfiguration, 'test ICE RTC configuration')
  requireExactKeys(rtc, ['iceServers', 'iceTransportPolicy'], [], 'test ICE RTC configuration')
  const iceServers = requireArray(rtc.iceServers, 'test ICE servers')
  if (iceServers.length !== 0) contractError('PR test topology cannot use STUN or TURN servers')
  const policy = requireRecord(profile.candidatePolicy, 'test ICE candidate policy')
  requireExactKeys(
    policy,
    ['allowedSelectedPairTypes', 'allowedProtocols'],
    [],
    'test ICE candidate policy',
  )
  requireExactVocabulary(
    policy.allowedSelectedPairTypes,
    TEST_ICE_SELECTED_PAIR_TYPES,
    'allowed selected-pair types',
  )
  requireExactVocabulary(policy.allowedProtocols, TEST_ICE_PROTOCOLS, 'allowed ICE protocols')
  return freezeRecord({
    topologyProfileSchemaVersion: requireLiteral(
      profile.topologyProfileSchemaVersion,
      TEST_ICE_TOPOLOGY_PROFILE_SCHEMA_VERSION,
      'test ICE topology profile schema version',
    ),
    topologyId: requireLiteral(profile.topologyId, PR_TEST_ICE_TOPOLOGY_ID, 'test ICE topology ID'),
    sourceSelector: freezeRecord({
      algorithm: requireLiteral(
        selector.algorithm,
        TEST_ICE_SOURCE_SELECTOR_ALGORITHM,
        'test ICE source selector algorithm',
      ),
      probeDestinations: TEST_ICE_PROBE_DESTINATIONS,
    }),
    addressFamily: requireEnum(
      profile.addressFamily,
      TEST_ICE_ADDRESS_FAMILIES,
      'test ICE address family',
    ),
    rtcConfiguration: freezeRecord({
      iceServers: Object.freeze([]) as readonly [],
      iceTransportPolicy: requireEnum(
        rtc.iceTransportPolicy,
        TEST_ICE_TRANSPORT_POLICIES,
        'test ICE transport policy',
      ),
    }),
    candidatePolicy: freezeRecord({
      allowedSelectedPairTypes: TEST_ICE_SELECTED_PAIR_TYPES,
      allowedProtocols: TEST_ICE_PROTOCOLS,
    }),
  })
}

export function parseTestIceTopologyResolution(
  value: unknown,
  profile: TestIceTopology,
  expectedProfileSha256: string,
): TestIceTopologyResolution {
  const parsedProfile = parseTestIceTopology(profile)
  const profileSha256 = requireSha256(expectedProfileSha256, 'expected topology profile SHA-256')
  const resolution = requireRecord(value, 'test ICE topology resolution')
  requireExactKeys(
    resolution,
    [
      'topologyResolutionSchemaVersion',
      'topologyId',
      'topologyProfileSha256',
      'selectorAlgorithm',
      'addressFamily',
      'probeResults',
      'interface',
    ],
    [],
    'test ICE topology resolution',
  )
  const probeResults = parseProbeResults(resolution.probeResults, parsedProfile)
  const resolvedInterface = parseResolvedInterface(resolution.interface)
  if (probeResults.some((probe) => probe.sourceAddress !== resolvedInterface.selectedAddress)) {
    contractError('test ICE route probes do not unanimously select the resolved interface address')
  }
  if (!resolvedInterface.eligibleAddresses.some(
    (candidate) => candidate.address === resolvedInterface.selectedAddress,
  )) {
    contractError('test ICE selected address is absent from the frozen eligible address inventory')
  }
  return freezeRecord({
    topologyResolutionSchemaVersion: requireLiteral(
      resolution.topologyResolutionSchemaVersion,
      TEST_ICE_TOPOLOGY_RESOLUTION_SCHEMA_VERSION,
      'test ICE topology resolution schema version',
    ),
    topologyId: requireLiteral(
      resolution.topologyId,
      parsedProfile.topologyId,
      'test ICE topology resolution ID',
    ),
    topologyProfileSha256: requireLiteral(
      resolution.topologyProfileSha256,
      profileSha256,
      'test ICE topology resolution profile SHA-256',
    ),
    selectorAlgorithm: requireLiteral(
      resolution.selectorAlgorithm,
      parsedProfile.sourceSelector.algorithm,
      'test ICE topology resolution selector algorithm',
    ),
    addressFamily: requireLiteral(
      resolution.addressFamily,
      parsedProfile.addressFamily,
      'test ICE topology resolution address family',
    ),
    probeResults,
    interface: resolvedInterface,
  })
}

export function parseTestIceTopologyJson(encoded: string): TestIceTopology {
  const profile = parseTestIceTopology(parseCanonicalJsonText(encoded, 'test ICE topology'))
  if (encoded !== canonicalTestIceTopologyJson(profile)) {
    contractError('test ICE topology bytes must equal the exact canonical profile encoding')
  }
  return profile
}

export function parseTestIceTopologyResolutionJson(
  encoded: string,
  profile: TestIceTopology,
  expectedProfileSha256: string,
): TestIceTopologyResolution {
  const resolution = parseTestIceTopologyResolution(
    parseCanonicalJsonText(encoded, 'test ICE topology resolution'),
    profile,
    expectedProfileSha256,
  )
  if (encoded !== canonicalTestIceTopologyResolutionJson(resolution, profile, expectedProfileSha256)) {
    contractError('test ICE topology resolution bytes must equal the exact canonical encoding')
  }
  return resolution
}

export function canonicalTestIceTopologyJson(profile: TestIceTopology): string {
  return JSON.stringify(parseTestIceTopology(profile))
}

export function canonicalTestIceTopologyResolutionJson(
  resolution: TestIceTopologyResolution,
  profile: TestIceTopology,
  expectedProfileSha256: string,
): string {
  return JSON.stringify(parseTestIceTopologyResolution(resolution, profile, expectedProfileSha256))
}

export async function testIceTopologySha256(profile: TestIceTopology): Promise<string> {
  return sha256(canonicalTestIceTopologyJson(profile))
}

export async function testIceTopologyResolutionSha256(
  resolution: TestIceTopologyResolution,
  profile: TestIceTopology,
  expectedProfileSha256: string,
): Promise<string> {
  return sha256(canonicalTestIceTopologyResolutionJson(resolution, profile, expectedProfileSha256))
}

export async function verifyTestIceTopologyLock(
  profile: TestIceTopology,
  resolution: TestIceTopologyResolution,
  expectedProfileSha256: string,
  expectedResolutionSha256: string,
): Promise<VerifiedTestIceTopologyLock> {
  const parsedProfile = parseTestIceTopology(profile)
  const profileSha256 = requireSha256(expectedProfileSha256, 'expected topology profile SHA-256')
  if (await testIceTopologySha256(parsedProfile) !== profileSha256) {
    contractError('expected topology profile SHA-256 does not match the canonical profile')
  }
  const parsedResolution = parseTestIceTopologyResolution(resolution, parsedProfile, profileSha256)
  const resolutionSha256 = requireSha256(
    expectedResolutionSha256,
    'expected topology resolution SHA-256',
  )
  if (
    await testIceTopologyResolutionSha256(parsedResolution, parsedProfile, profileSha256) !==
    resolutionSha256
  ) {
    contractError('expected topology resolution SHA-256 does not match the canonical resolution')
  }
  const lock = freezeRecord({
    profile: parsedProfile,
    resolution: parsedResolution,
    profileSha256,
    resolutionSha256,
  })
  VERIFIED_TEST_ICE_TOPOLOGY_LOCKS.add(lock)
  return lock
}

export function readVerifiedTestIceTopologyLock(
  value: VerifiedTestIceTopologyLock,
): VerifiedTestIceTopologyLock {
  if (typeof value !== 'object' || value === null || !VERIFIED_TEST_ICE_TOPOLOGY_LOCKS.has(value)) {
    contractError('browser evidence requires a canonically verified test ICE topology lock')
  }
  return value
}

export function browserRtcConfiguration(profile: TestIceTopology): RTCConfiguration {
  const parsed = parseTestIceTopology(profile)
  return { iceServers: [], iceTransportPolicy: parsed.rtcConfiguration.iceTransportPolicy }
}

export function selectedPairAllowedByTopology(
  pair: BrowserSelectedPairEvidence | PionSelectedPairEvidence,
  profile: TestIceTopology,
  resolution: TestIceTopologyResolution,
): boolean {
  const parsedProfile = parseTestIceTopology(profile)
  const parsedResolution = parseTestIceTopologyResolution(
    resolution,
    parsedProfile,
    resolution.topologyProfileSha256,
  )
  const candidatesAllowed = [pair.local, pair.remote].every((candidate) =>
    parsedProfile.candidatePolicy.allowedSelectedPairTypes.includes(
      candidate.candidateType as 'host' | 'prflx',
    ) && parsedProfile.candidatePolicy.allowedProtocols.includes(candidate.protocol as 'udp'))
  if (!candidatesAllowed) return false
  const localHasFamily = Object.hasOwn(pair.local, 'addressFamily')
  const remoteHasFamily = Object.hasOwn(pair.remote, 'addressFamily')
  if (!localHasFamily && !remoteHasFamily) return true
  if (!localHasFamily || !remoteHasFamily) return false
  const pion = pair as PionSelectedPairEvidence
  const eligible = new Set(parsedResolution.interface.eligibleAddresses.map(({ address }) => address))
  return pion.local.addressFamily === parsedProfile.addressFamily &&
    pion.remote.addressFamily === parsedProfile.addressFamily &&
    pion.local.address === parsedResolution.interface.selectedAddress &&
    eligible.has(pion.remote.address)
}

function parseProbeResults(value: unknown, profile: TestIceTopology): readonly TestIceProbeResult[] {
  const results = requireArray(value, 'test ICE probe results')
  if (results.length !== profile.sourceSelector.probeDestinations.length) {
    contractError('test ICE resolution must record every frozen route probe exactly once')
  }
  return Object.freeze(results.map((value, index) => {
    const result = requireRecord(value, `test ICE probe result ${index}`)
    requireExactKeys(
      result,
      ['destinationAddress', 'destinationPort', 'sourceAddress'],
      [],
      `test ICE probe result ${index}`,
    )
    const expected = profile.sourceSelector.probeDestinations[index]
    if (expected === undefined) contractError('test ICE probe result exceeds the frozen probe registry')
    return freezeRecord({
      destinationAddress: requireLiteral(
        result.destinationAddress,
        expected.address,
        `test ICE probe result ${index} destination`,
      ),
      destinationPort: requireLiteral(
        result.destinationPort,
        expected.port,
        `test ICE probe result ${index} port`,
      ),
      sourceAddress: requireOperationalIPv4(
        result.sourceAddress,
        `test ICE probe result ${index} source address`,
      ),
    })
  }))
}

function parseResolvedInterface(value: unknown): TestIceResolvedInterface {
  const resolved = requireRecord(value, 'test ICE resolved interface')
  requireExactKeys(
    resolved,
    ['index', 'name', 'selectedAddress', 'eligibleAddresses'],
    [],
    'test ICE resolved interface',
  )
  const eligibleAddresses = requireArray(
    resolved.eligibleAddresses,
    'test ICE eligible addresses',
  ).map((value, index) => {
    const candidate = requireRecord(value, `test ICE eligible address ${index}`)
    requireExactKeys(candidate, ['address', 'prefixLength'], [], `test ICE eligible address ${index}`)
    return freezeRecord({
      address: requireOperationalIPv4(candidate.address, `test ICE eligible address ${index}`),
      prefixLength: requireSafeInteger(
        candidate.prefixLength,
        1,
        32,
        `test ICE eligible address ${index} prefix length`,
      ),
    })
  })
  if (new Set(eligibleAddresses.map(({ address }) => address)).size !== eligibleAddresses.length) {
    contractError('test ICE eligible address inventory contains duplicates')
  }
  const ordered = [...eligibleAddresses].sort((left, right) =>
    ipv4Number(left.address) - ipv4Number(right.address) || left.prefixLength - right.prefixLength)
  if (eligibleAddresses.some((candidate, index) => candidate !== ordered[index])) {
    contractError('test ICE eligible address inventory must use canonical numeric ordering')
  }
  return freezeRecord({
    index: requireSafeInteger(resolved.index, 1, 0xffff_ffff, 'test ICE interface index'),
    name: requireString(resolved.name, 'test ICE interface name', 255),
    selectedAddress: requireOperationalIPv4(
      resolved.selectedAddress,
      'test ICE selected interface address',
    ),
    eligibleAddresses: Object.freeze(eligibleAddresses),
  })
}

function requireExactProbeDestinations(value: unknown): void {
  const probes = requireArray(value, 'test ICE source selector probes')
  if (probes.length !== TEST_ICE_PROBE_DESTINATIONS.length) {
    contractError('test ICE source selector must use the frozen probe registry')
  }
  probes.forEach((value, index) => {
    const probe = requireRecord(value, `test ICE source selector probe ${index}`)
    requireExactKeys(probe, ['address', 'port'], [], `test ICE source selector probe ${index}`)
    const expected = TEST_ICE_PROBE_DESTINATIONS[index]
    if (
      expected === undefined || probe.address !== expected.address || probe.port !== expected.port
    ) {
      contractError('test ICE source selector probes must equal the frozen ordered registry')
    }
  })
}

function requireExactVocabulary<T extends IceCandidateType | IceProtocol>(
  value: unknown,
  expected: readonly T[],
  label: string,
): void {
  const items = requireArray(value, label)
  if (items.length !== expected.length || items.some((item, index) => item !== expected[index])) {
    contractError(`${label} must equal the frozen ${expected.join(',')} policy`)
  }
}

function requireOperationalIPv4(value: unknown, label: string): string {
  const address = requireString(value, label, 15)
  if (!isOperationalIPv4Unicast(address)) {
    contractError(`${label} must be operational non-loopback IPv4 unicast`)
  }
  return address
}

export function isOperationalIPv4Unicast(address: string): boolean {
  const parts = address.split('.')
  if (
    parts.length !== 4 ||
    parts.some((part) => !/^(0|[1-9]\d{0,2})$/u.test(part) || Number(part) > 255)
  ) return false
  const octets = parts.map(Number)
  const first = octets[0]
  const second = octets[1]
  return first !== undefined && second !== undefined && first !== 0 && first !== 127 &&
    first < 224 && !(first === 169 && second === 254)
}

function ipv4Number(address: string): number {
  return address.split('.').reduce((result, octet) => result * 256 + Number(octet), 0)
}

async function sha256(value: string): Promise<string> {
  const bytes = new TextEncoder().encode(value)
  const digest = new Uint8Array(await globalThis.crypto.subtle.digest('SHA-256', bytes))
  return [...digest].map((item) => item.toString(16).padStart(2, '0')).join('')
}

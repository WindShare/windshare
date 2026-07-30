import { readFile } from 'node:fs/promises'

import { describe, expect, it } from 'vitest'

import { parsePionSelectedPair } from '../../scripts/browser-evidence/attempt-evidence.ts'
import {
  browserRtcConfiguration,
  canonicalTestIceTopologyJson,
  canonicalTestIceTopologyResolutionJson,
  isOperationalIPv4Unicast,
  parseTestIceTopology,
  parseTestIceTopologyJson,
  parseTestIceTopologyResolution,
  parseTestIceTopologyResolutionJson,
  selectedPairAllowedByTopology,
  testIceTopologyResolutionSha256,
  testIceTopologySha256,
} from '../../scripts/browser-evidence/test-ice-topology.ts'
import { BROWSER_EVIDENCE_VOCABULARY } from '../../scripts/browser-evidence/vocabulary.ts'
import { browserPair, pionPair } from './fixtures.ts'

const SHARED_PROFILE_SHA256 = '7d1082df592602db632e83b538b6d758fb71c66971b46e685f992b2c7a76c7ae'
const SHARED_RESOLUTION_SHA256 = '2fac2dd7a746ea4a853081553db529d5a996c75fc81be53d54d843fc5cd64cf6'

describe('shared browser evidence vocabulary', () => {
  it('exactly matches the ordered language-neutral emitter registry', async () => {
    const registry = JSON.parse(await readFile(
      new URL('../../../testdata/browser-evidence/v1/vocabulary.json', import.meta.url),
      'utf8',
    )) as Record<string, unknown>
    expect(registry).toStrictEqual(BROWSER_EVIDENCE_VOCABULARY)
  })
})

describe('TestIceTopology profile and resolution contracts', () => {
  it('round-trips and hashes the separately versioned shared locks', async () => {
    const profileEncoded = await sharedFixture('pr-same-host-kernel-route-ipv4.json')
    const resolutionEncoded = await sharedFixture('pr-same-host-kernel-route-ipv4-resolution.json')
    const profile = parseTestIceTopologyJson(profileEncoded)
    const resolution = parseTestIceTopologyResolutionJson(
      resolutionEncoded,
      profile,
      SHARED_PROFILE_SHA256,
    )
    expect(parseTestIceTopology(JSON.parse(canonicalTestIceTopologyJson(profile)))).toEqual(profile)
    expect(parseTestIceTopologyResolution(
      JSON.parse(canonicalTestIceTopologyResolutionJson(
        resolution,
        profile,
        SHARED_PROFILE_SHA256,
      )),
      profile,
      SHARED_PROFILE_SHA256,
    )).toEqual(resolution)
    expect(browserRtcConfiguration(profile)).toEqual({ iceServers: [], iceTransportPolicy: 'all' })
    expect(await testIceTopologySha256(profile)).toBe(SHARED_PROFILE_SHA256)
    expect(await testIceTopologyResolutionSha256(
      resolution,
      profile,
      SHARED_PROFILE_SHA256,
    )).toBe(SHARED_RESOLUTION_SHA256)
  })

  it('accepts only the checked-in exact canonical bytes used by the frozen digests', async () => {
    const encoded = await sharedFixture('pr-same-host-kernel-route-ipv4.json')
    const resolutionEncoded = await sharedFixture('pr-same-host-kernel-route-ipv4-resolution.json')
    const profile = parseTestIceTopologyJson(encoded)
    expect(await testIceTopologySha256(profile)).toBe(SHARED_PROFILE_SHA256)
    expect(encoded).toBe(canonicalTestIceTopologyJson(profile))
    const resolution = parseTestIceTopologyResolutionJson(
      resolutionEncoded,
      profile,
      SHARED_PROFILE_SHA256,
    )
    expect(resolutionEncoded).toBe(canonicalTestIceTopologyResolutionJson(
      resolution,
      profile,
      SHARED_PROFILE_SHA256,
    ))
    expect(() => parseTestIceTopologyJson(`${encoded}\n`)).toThrow(/exact canonical profile encoding/u)
    expect(() => parseTestIceTopologyJson(` ${encoded}`)).toThrow(/exact canonical profile encoding/u)
    expect(() => parseTestIceTopologyResolutionJson(
      `${resolutionEncoded}\n`,
      profile,
      SHARED_PROFILE_SHA256,
    )).toThrow(/exact canonical encoding/u)
  })

  it('rejects the shared duplicate, integer, and Unicode adversarial corpus', async () => {
    const profile = JSON.stringify(validProfile())
    const registry = JSON.parse(await sharedFixture('strict-json-invalid-cases.json')) as {
      topologyContractCaseRegistrySchemaVersion: number
      cases: { name: string; needle: string; replacement: string }[]
    }
    expect(registry.topologyContractCaseRegistrySchemaVersion).toBe(1)
    for (const contractCase of registry.cases) {
      expect(
        () => parseTestIceTopologyJson(profile.replace(contractCase.needle, contractCase.replacement)),
        contractCase.name,
      ).toThrow()
    }
  })

  it('rejects unknown fields, servers, reordered probes, and broadened candidate policy', () => {
    const profile = validProfile()
    expect(() => parseTestIceTopology({ ...profile, machineAddress: '192.0.2.1' })).toThrow(/unknown field/u)
    expect(() => parseTestIceTopology({
      ...profile,
      sourceSelector: {
        ...profile.sourceSelector,
        probeDestinations: [...profile.sourceSelector.probeDestinations].reverse(),
      },
    })).toThrow(/ordered registry/u)
    expect(() => parseTestIceTopology({
      ...profile,
      rtcConfiguration: { iceServers: [{ urls: 'stun:example.invalid' }], iceTransportPolicy: 'all' },
    })).toThrow(/cannot use STUN or TURN/u)
    expect(() => parseTestIceTopology({
      ...profile,
      candidatePolicy: {
        allowedSelectedPairTypes: ['host', 'prflx', 'relay'],
        allowedProtocols: ['udp'],
      },
    })).toThrow(/frozen/u)
  })

  it('requires unanimous probes, canonical inventory, and profile binding', () => {
    const profile = parseTestIceTopology(validProfile())
    const resolution = validResolution()
    expect(() => parseTestIceTopologyResolution({
      ...resolution,
      probeResults: resolution.probeResults.map((result, index) =>
        index === 1 ? { ...result, sourceAddress: '192.0.2.11' } : result),
    }, profile, SHARED_PROFILE_SHA256)).toThrow(/unanimously/u)
    expect(() => parseTestIceTopologyResolution({
      ...resolution,
      topologyProfileSha256: 'f'.repeat(64),
    }, profile, SHARED_PROFILE_SHA256)).toThrow(/profile SHA/u)
    expect(() => parseTestIceTopologyResolution({
      ...resolution,
      interface: {
        ...resolution.interface,
        eligibleAddresses: [
          { address: '192.0.2.11', prefixLength: 24 },
          { address: '192.0.2.10', prefixLength: 24 },
        ],
      },
    }, profile, SHARED_PROFILE_SHA256)).toThrow(/canonical numeric ordering/u)
  })

  it('binds direct pairs to the resolved interface and ignores inherited family pollution', () => {
    const profile = parseTestIceTopology(validProfile())
    const resolution = parseTestIceTopologyResolution(
      validResolution(),
      profile,
      SHARED_PROFILE_SHA256,
    )
    expect(selectedPairAllowedByTopology(browserPair(), profile, resolution)).toBe(true)
    expect(selectedPairAllowedByTopology(pionPair(), profile, resolution)).toBe(true)
    expect(selectedPairAllowedByTopology({
      ...pionPair(),
      local: { ...pionPair().local, address: '192.0.2.11' },
    }, profile, resolution)).toBe(false)
    expect(selectedPairAllowedByTopology({
      ...browserPair(),
      remote: { ...browserPair().remote, candidateType: 'relay' },
    }, profile, resolution)).toBe(false)

    const pollutedLocal = Object.assign(Object.create({ addressFamily: 'ipv6' }), browserPair().local)
    expect(selectedPairAllowedByTopology({
      ...browserPair(),
      local: pollutedLocal,
    }, profile, resolution)).toBe(true)
    expect(selectedPairAllowedByTopology({
      ...browserPair(),
      local: { ...browserPair().local, addressFamily: 'ipv4' },
    } as never, profile, resolution)).toBe(false)
    expect(() => parsePionSelectedPair({
      ...pionPair(),
      local: { ...pionPair().local, address: '127.0.0.1' },
      remote: { ...pionPair().remote, address: '127.0.0.1' },
    })).toThrow(/non-loopback/u)
    expect(() => parsePionSelectedPair({
      ...pionPair(),
      remote: {
        ...pionPair().remote,
        address: pionPair().local.address,
        port: pionPair().local.port,
      },
    })).toThrow(/distinct local and remote/u)
  })

  it('uses one operational IPv4 boundary in both contract languages', () => {
    for (const rejected of [
      ipv4(0, 1, 2, 3),
      ipv4(127, 0, 0, 1),
      ipv4(169, 254, 1, 1),
      ipv4(224, 0, 0, 1),
      ipv4(240, 0, 0, 1),
      ipv4(255, 255, 255, 255),
    ]) {
      expect(isOperationalIPv4Unicast(rejected), rejected).toBe(false)
    }
    for (const accepted of [ipv4(10, 0, 0, 1), '192.0.2.10', ipv4(223, 255, 255, 254)]) {
      expect(isOperationalIPv4Unicast(accepted), accepted).toBe(true)
    }
  })
})

function validProfile() {
  return {
    topologyProfileSchemaVersion: 1,
    topologyId: 'pr-same-host-kernel-route-ipv4',
    sourceSelector: {
      algorithm: 'udp-connect-source-consensus-v1',
      probeDestinations: [
        { address: '192.0.2.1', port: 9 },
        { address: '198.51.100.1', port: 9 },
        { address: '203.0.113.1', port: 9 },
      ],
    },
    addressFamily: 'ipv4',
    rtcConfiguration: { iceServers: [], iceTransportPolicy: 'all' },
    candidatePolicy: {
      allowedSelectedPairTypes: ['host', 'prflx'],
      allowedProtocols: ['udp'],
    },
  }
}

function validResolution() {
  return {
    topologyResolutionSchemaVersion: 1,
    topologyId: 'pr-same-host-kernel-route-ipv4',
    topologyProfileSha256: SHARED_PROFILE_SHA256,
    selectorAlgorithm: 'udp-connect-source-consensus-v1',
    addressFamily: 'ipv4',
    probeResults: [
      { destinationAddress: '192.0.2.1', destinationPort: 9, sourceAddress: '192.0.2.10' },
      { destinationAddress: '198.51.100.1', destinationPort: 9, sourceAddress: '192.0.2.10' },
      { destinationAddress: '203.0.113.1', destinationPort: 9, sourceAddress: '192.0.2.10' },
    ],
    interface: {
      index: 7,
      name: 'test-uplink0',
      selectedAddress: '192.0.2.10',
      eligibleAddresses: [{ address: '192.0.2.10', prefixLength: 24 }],
    },
  }
}

async function sharedFixture(name: string): Promise<string> {
  return readFile(new URL(`../../../testdata/test-ice-topology/${name}`, import.meta.url), 'utf8')
}

function ipv4(...octets: readonly number[]): string {
  return octets.join('.')
}

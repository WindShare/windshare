import { isIP } from 'node:net'

import {
  parseNetworkCandidatePath,
  type NetworkCandidatePath,
} from './candidate.ts'
import {
  networkMatrixError,
  requireCanonicalUtcTimestamp,
  requireEnum,
  requireExactKeys,
  requireLiteral,
  requireRecord,
  requireRunId,
  requireSha256,
} from './contract-support.ts'
import {
  NETWORK_MATRIX_ADDRESS_FAMILIES,
  NETWORK_MATRIX_CANDIDATE_TYPES,
  NETWORK_MATRIX_PION_AUTHORITIES,
  NETWORK_MATRIX_PROTOCOLS,
  type NetworkMatrixAddressFamily,
  type NetworkMatrixCandidateType,
  type NetworkMatrixPionAuthority,
  type NetworkMatrixProfileId,
  type NetworkMatrixProtocol,
} from './vocabulary.ts'
import {
  EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
  parseSignedExternalFixtureAttestation,
  type SignedExternalFixtureAttestation,
} from './linux-topology/external-fixture-attestation.ts'
import {
  networkMatrixAttemptAuthoritySha256,
  parseNetworkMatrixAttemptAuthority,
  sameNetworkMatrixAttemptAuthority,
  type NetworkMatrixAttemptAuthority,
} from './sample-authority.ts'
import {
  parseSignedExternalFixtureTerminalReceipt,
  type SignedExternalFixtureTerminalReceipt,
} from './linux-topology/external-fixture-terminal-receipt.ts'

export type {
  NetworkMatrixAddressFamily,
  NetworkMatrixPionAuthority,
} from './vocabulary.ts'

const ATTEMPT_ID_PATTERN = /^[A-Za-z0-9_-]{16,128}$/u
const EXTERNAL_PION_AUTHORITY: NetworkMatrixPionAuthority = 'external-remote'

const ESTABLISHED_PION_PROFILE_POLICIES = Object.freeze({
  'scheduled-public-stun': Object.freeze({
    local: Object.freeze(['host', 'srflx', 'prflx'] as const),
    remote: Object.freeze(['host', 'srflx', 'prflx'] as const),
    protocols: Object.freeze(['udp'] as const),
  }),
  'scheduled-coturn': Object.freeze({
    local: Object.freeze(['host', 'srflx', 'prflx'] as const),
    remote: Object.freeze(['relay'] as const),
    protocols: Object.freeze(['udp', 'tcp'] as const),
  }),
  'manual-real-nat': Object.freeze({
    local: Object.freeze(['host', 'srflx', 'prflx'] as const),
    remote: Object.freeze(['host', 'srflx', 'prflx'] as const),
    protocols: Object.freeze(['udp'] as const),
  }),
})

export type NetworkMatrixBrowserSelectedPair = NetworkCandidatePath

export interface PresentNetworkMatrixPionSelectedPair {
  readonly selectedPair: 'present'
  readonly localCandidateType: NetworkMatrixCandidateType
  readonly localAddressFamily: NetworkMatrixAddressFamily
  readonly remoteCandidateType: NetworkMatrixCandidateType
  readonly remoteAddressFamily: NetworkMatrixAddressFamily
  readonly protocol: NetworkMatrixProtocol
  readonly localAddress: string
  readonly localPort: number
  readonly remoteAddress: string
  readonly remotePort: number
}

export interface AbsentNetworkMatrixPionSelectedPair {
  readonly selectedPair: 'absent'
  readonly localCandidateType: null
  readonly localAddressFamily: null
  readonly remoteCandidateType: null
  readonly remoteAddressFamily: null
  readonly protocol: null
  readonly localAddress: null
  readonly localPort: null
  readonly remoteAddress: null
  readonly remotePort: null
}

export type NetworkMatrixPionSelectedPair =
  | PresentNetworkMatrixPionSelectedPair
  | AbsentNetworkMatrixPionSelectedPair

export interface NetworkMatrixChallengeProof {
  readonly bindingSha256: string
  readonly challenge: string
  readonly pionChallengeObserved: true
  readonly browserEchoObserved: true
}

export interface NetworkMatrixExternalFixtureAttemptBinding {
  readonly runId: string
  readonly authorityInstanceId: string
  readonly remoteServiceInstanceId: string
  readonly attestationSha256: string
  readonly attestationPublicKeySpki: string
  readonly signedAttestation: SignedExternalFixtureAttestation
  readonly networkBindingSha256: string
  readonly remotePeerBindingSha256: string
  readonly controllerPublicIp: string
  readonly attestationExpiresAt: string
  readonly remotePeerPublicIp: string
  readonly remotePeerUdpPortMin: number
  readonly remotePeerUdpPortMax: number
}

/** Joins independent endpoint facts so browser-local getStats cannot authenticate a topology alone. */
export interface NetworkMatrixAttemptEvidence {
  readonly attemptAuthority: NetworkMatrixAttemptAuthority
  readonly pionAuthority: NetworkMatrixPionAuthority
  readonly externalFixture: NetworkMatrixExternalFixtureAttemptBinding
  readonly browserSelectedPair: NetworkMatrixBrowserSelectedPair
  readonly pionSelectedPair: NetworkMatrixPionSelectedPair
  readonly challenge: NetworkMatrixChallengeProof | null
  readonly terminalReceipt: SignedExternalFixtureTerminalReceipt
}

export function parseNetworkMatrixAttemptEvidence(
  value: unknown,
  profileId: NetworkMatrixProfileId,
): NetworkMatrixAttemptEvidence {
  const record = requireRecord(value, 'network matrix attempt evidence')
  requireExactKeys(record, [
    'attemptAuthority',
    'pionAuthority',
    'externalFixture',
    'browserSelectedPair',
    'pionSelectedPair',
    'challenge',
    'terminalReceipt',
  ], 'network matrix attempt evidence')
  const evidence = Object.freeze({
    attemptAuthority: parseNetworkMatrixAttemptAuthority(record.attemptAuthority),
    pionAuthority: requireEnum(
      record.pionAuthority,
      NETWORK_MATRIX_PION_AUTHORITIES,
      'network matrix Pion authority',
    ),
    externalFixture: parseExternalFixtureAttemptBinding(record.externalFixture),
    browserSelectedPair: parseNetworkCandidatePath(record.browserSelectedPair),
    pionSelectedPair: parsePionSelectedPair(record.pionSelectedPair),
    challenge: record.challenge === null ? null : parseChallengeProof(record.challenge),
    terminalReceipt: parseSignedExternalFixtureTerminalReceipt(record.terminalReceipt),
  })
  validateProfileAttempt(evidence, profileId)
  return evidence
}

export function networkMatrixPionSelectedPairFromTerminalReceipt(
  value: unknown | null,
): NetworkMatrixPionSelectedPair {
  if (value === null) return parsePionSelectedPair({
    selectedPair: 'absent',
    localCandidateType: null,
    localAddressFamily: null,
    remoteCandidateType: null,
    remoteAddressFamily: null,
    protocol: null,
    localAddress: null,
    localPort: null,
    remoteAddress: null,
    remotePort: null,
  })
  const pair = requireRecord(value, 'signed terminal Pion selected pair')
  requireExactKeys(pair, ['local', 'remote'], 'signed terminal Pion selected pair')
  const local = parseTerminalCandidate(pair.local, 'local')
  const remote = parseTerminalCandidate(pair.remote, 'remote')
  if (local.protocol !== remote.protocol) {
    networkMatrixError('signed terminal Pion candidates use different protocols')
  }
  return parsePionSelectedPair({
    selectedPair: 'present',
    localCandidateType: local.candidateType,
    localAddressFamily: local.addressFamily,
    remoteCandidateType: remote.candidateType,
    remoteAddressFamily: remote.addressFamily,
    protocol: local.protocol,
    localAddress: local.address,
    localPort: local.port,
    remoteAddress: remote.address,
    remotePort: remote.port,
  })
}

function parseTerminalCandidate(
  value: unknown,
  side: 'local' | 'remote',
): {
  readonly candidateType: NetworkMatrixCandidateType
  readonly addressFamily: NetworkMatrixAddressFamily
  readonly protocol: NetworkMatrixProtocol
  readonly address: string
  readonly port: number
} {
  const candidate = requireRecord(value, `signed terminal Pion ${side} candidate`)
  requireExactKeys(candidate, [
    'candidateType', 'protocol', 'address', 'port', 'addressFamily',
  ], `signed terminal Pion ${side} candidate`)
  const addressFamily = requireEnum(
    candidate.addressFamily,
    NETWORK_MATRIX_ADDRESS_FAMILIES,
    `signed terminal Pion ${side} candidate address family`,
  )
  const endpoint = requireCandidateEndpoint(
    candidate.address,
    candidate.port,
    addressFamily,
    `signed terminal Pion ${side}`,
  )
  return Object.freeze({
    candidateType: requireEnum(
      candidate.candidateType,
      NETWORK_MATRIX_CANDIDATE_TYPES,
      `signed terminal Pion ${side} candidate type`,
    ),
    addressFamily,
    protocol: requireEnum(
      candidate.protocol,
      NETWORK_MATRIX_PROTOCOLS,
      `signed terminal Pion ${side} candidate protocol`,
    ),
    address: endpoint.address,
    port: endpoint.port,
  })
}

function parseExternalFixtureAttemptBinding(
  value: unknown,
): NetworkMatrixExternalFixtureAttemptBinding {
  const binding = requireRecord(value, 'network matrix external fixture attempt binding')
  requireExactKeys(binding, [
    'runId', 'authorityInstanceId', 'remoteServiceInstanceId', 'attestationSha256',
    'attestationPublicKeySpki', 'signedAttestation',
    'networkBindingSha256', 'remotePeerBindingSha256', 'controllerPublicIp',
    'attestationExpiresAt', 'remotePeerPublicIp', 'remotePeerUdpPortMin',
    'remotePeerUdpPortMax',
  ], 'network matrix external fixture attempt binding')
  const controllerPublicIp = binding.controllerPublicIp
  const attestationExpiresAt = requireCanonicalUtcTimestamp(
    binding.attestationExpiresAt,
    'network matrix external fixture attestation expiry',
  )
  const remotePeerPublicIp = binding.remotePeerPublicIp
  const remotePeerUdpPortMin = binding.remotePeerUdpPortMin
  const remotePeerUdpPortMax = binding.remotePeerUdpPortMax
  if (
    typeof binding.attestationPublicKeySpki !== 'string' ||
    !/^[A-Za-z0-9_-]{32,256}$/u.test(binding.attestationPublicKeySpki) ||
    Buffer.from(binding.attestationPublicKeySpki, 'base64url').toString('base64url') !==
      binding.attestationPublicKeySpki ||
    typeof remotePeerPublicIp !== 'string' || isIP(remotePeerPublicIp) !== 4 ||
    !Number.isSafeInteger(remotePeerUdpPortMin) ||
    !Number.isSafeInteger(remotePeerUdpPortMax) ||
    (remotePeerUdpPortMin as number) < 1 ||
    (remotePeerUdpPortMax as number) > 65_535 ||
    (remotePeerUdpPortMin as number) > (remotePeerUdpPortMax as number)
  ) networkMatrixError('network matrix external fixture remote endpoint is invalid')
  if (
    typeof controllerPublicIp !== 'string' ||
    isIP(controllerPublicIp) !== 4
  ) networkMatrixError('network matrix external fixture attempt endpoint is invalid')
  return Object.freeze({
    runId: requireRunId(binding.runId, 'network matrix external fixture run ID'),
    authorityInstanceId: requireRunId(
      binding.authorityInstanceId,
      'network matrix external fixture authority instance ID',
    ),
    remoteServiceInstanceId: requireRunId(
      binding.remoteServiceInstanceId,
      'network matrix external fixture remote service instance ID',
    ),
    attestationSha256: requireSha256(
      binding.attestationSha256,
      'network matrix external fixture live attestation digest',
    ),
    attestationPublicKeySpki: binding.attestationPublicKeySpki,
    signedAttestation: parseSignedExternalFixtureAttestation(
      binding.signedAttestation,
      EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
    ),
    networkBindingSha256: requireSha256(
      binding.networkBindingSha256,
      'network matrix external fixture network binding digest',
    ),
    remotePeerBindingSha256: requireSha256(
      binding.remotePeerBindingSha256,
      'network matrix external fixture remote peer binding digest',
    ),
    controllerPublicIp,
    attestationExpiresAt,
    remotePeerPublicIp,
    remotePeerUdpPortMin: remotePeerUdpPortMin as number,
    remotePeerUdpPortMax: remotePeerUdpPortMax as number,
  })
}

function parsePionSelectedPair(value: unknown): NetworkMatrixPionSelectedPair {
  const record = requireRecord(value, 'network matrix Pion selected pair')
  requireExactKeys(record, [
    'selectedPair',
    'localCandidateType',
    'localAddressFamily',
    'remoteCandidateType',
    'remoteAddressFamily',
    'protocol',
    'localAddress',
    'localPort',
    'remoteAddress',
    'remotePort',
  ], 'network matrix Pion selected pair')
  if (record.selectedPair === 'absent') {
    if (
      record.localCandidateType !== null || record.localAddressFamily !== null ||
      record.remoteCandidateType !== null || record.remoteAddressFamily !== null ||
      record.protocol !== null || record.localAddress !== null || record.localPort !== null ||
      record.remoteAddress !== null || record.remotePort !== null
    ) networkMatrixError('an absent Pion selected pair cannot carry candidate observations')
    return Object.freeze({
      selectedPair: 'absent',
      localCandidateType: null,
      localAddressFamily: null,
      remoteCandidateType: null,
      remoteAddressFamily: null,
      protocol: null,
      localAddress: null,
      localPort: null,
      remoteAddress: null,
      remotePort: null,
    })
  }
  requireLiteral(record.selectedPair, 'present', 'network matrix Pion selected-pair observation')
  const localAddress = requireCandidateEndpoint(
    record.localAddress,
    record.localPort,
    record.localAddressFamily,
    'Pion local',
  )
  const remoteAddress = requireCandidateEndpoint(
    record.remoteAddress,
    record.remotePort,
    record.remoteAddressFamily,
    'Pion remote',
  )
  return Object.freeze({
    selectedPair: 'present',
    localCandidateType: requireEnum(
      record.localCandidateType,
      NETWORK_MATRIX_CANDIDATE_TYPES,
      'network matrix Pion local candidate type',
    ),
    localAddressFamily: requireEnum(
      record.localAddressFamily,
      NETWORK_MATRIX_ADDRESS_FAMILIES,
      'network matrix Pion local address family',
    ),
    remoteCandidateType: requireEnum(
      record.remoteCandidateType,
      NETWORK_MATRIX_CANDIDATE_TYPES,
      'network matrix Pion remote candidate type',
    ),
    remoteAddressFamily: requireEnum(
      record.remoteAddressFamily,
      NETWORK_MATRIX_ADDRESS_FAMILIES,
      'network matrix Pion remote address family',
    ),
    protocol: requireEnum(
      record.protocol,
      NETWORK_MATRIX_PROTOCOLS,
      'network matrix Pion candidate protocol',
    ),
    localAddress: localAddress.address,
    localPort: localAddress.port,
    remoteAddress: remoteAddress.address,
    remotePort: remoteAddress.port,
  })
}

function parseChallengeProof(value: unknown): NetworkMatrixChallengeProof {
  const record = requireRecord(value, 'network matrix attempt challenge')
  requireExactKeys(record, [
    'bindingSha256',
    'challenge',
    'pionChallengeObserved',
    'browserEchoObserved',
  ], 'network matrix attempt challenge')
  return Object.freeze({
    bindingSha256: requireSha256(
      record.bindingSha256,
      'network matrix attempt challenge binding digest',
    ),
    challenge: requireAttemptId(record.challenge),
    pionChallengeObserved: requireLiteral(
      record.pionChallengeObserved,
      true,
      'network matrix Pion challenge observation',
    ),
    browserEchoObserved: requireLiteral(
      record.browserEchoObserved,
      true,
      'network matrix browser challenge echo observation',
    ),
  })
}

function validateProfileAttempt(
  evidence: NetworkMatrixAttemptEvidence,
  profileId: NetworkMatrixProfileId,
): void {
  const expectedAuthority = EXTERNAL_PION_AUTHORITY
  if (evidence.pionAuthority !== expectedAuthority) {
    networkMatrixError(`network matrix Pion authority differs from profile ${profileId}`)
  }
  if (
    evidence.attemptAuthority.requestAuthority.controlAuthority.sampleAuthority.profileId !== profileId ||
    !sameNetworkMatrixAttemptAuthority(
      evidence.attemptAuthority,
      evidence.terminalReceipt.receipt.attemptAuthority,
    )
  ) networkMatrixError('network matrix attempt authority differs from its signed terminal receipt')
  if (evidence.browserSelectedPair.selectedPair !== evidence.pionSelectedPair.selectedPair) {
    networkMatrixError('browser and Pion selected-pair presence differ for one attempt')
  }

  if (profileId === 'scheduled-restricted-udp') {
    if (evidence.browserSelectedPair.selectedPair !== 'absent' || evidence.challenge !== null) {
      networkMatrixError('restricted UDP evidence must carry two absent pairs and no challenge')
    }
    return
  }

  if (
    evidence.browserSelectedPair.selectedPair !== 'present' ||
    evidence.pionSelectedPair.selectedPair !== 'present' ||
    evidence.challenge === null
  ) {
    networkMatrixError('established connectivity evidence needs two present pairs and a challenge')
  }
  if (
    evidence.browserSelectedPair.protocol !== evidence.pionSelectedPair.protocol ||
    evidence.browserSelectedPair.remoteAddress !== evidence.pionSelectedPair.localAddress ||
    evidence.browserSelectedPair.remotePort !== evidence.pionSelectedPair.localPort ||
    evidence.browserSelectedPair.localAddress !== null &&
      isIP(evidence.browserSelectedPair.localAddress) !== 0 &&
      evidence.browserSelectedPair.localAddress !== evidence.pionSelectedPair.remoteAddress ||
    evidence.browserSelectedPair.localPort !== null &&
      evidence.browserSelectedPair.localPort !== evidence.pionSelectedPair.remotePort
  ) {
    networkMatrixError('browser and Pion selected pairs do not describe the same attempt')
  }
  if (
    evidence.pionSelectedPair.localAddress !== evidence.externalFixture.remotePeerPublicIp ||
    evidence.pionSelectedPair.localPort < evidence.externalFixture.remotePeerUdpPortMin ||
    evidence.pionSelectedPair.localPort > evidence.externalFixture.remotePeerUdpPortMax
  ) {
    networkMatrixError('Pion selected pair differs from its signed remote endpoint authority')
  }
  const pionPolicy = ESTABLISHED_PION_PROFILE_POLICIES[profileId]
  if (
    !includesVocabulary(evidence.pionSelectedPair.localCandidateType, pionPolicy.local) ||
    !includesVocabulary(evidence.pionSelectedPair.remoteCandidateType, pionPolicy.remote) ||
    !includesVocabulary(evidence.pionSelectedPair.protocol, pionPolicy.protocols)
  ) networkMatrixError(`Pion selected pair contradicts external fixture profile ${profileId}`)
  if (
    evidence.challenge.challenge !== evidence.attemptAuthority.challenge ||
    evidence.challenge.bindingSha256 !==
      networkMatrixAttemptAuthoritySha256(evidence.attemptAuthority)
  ) {
    networkMatrixError('network matrix challenge does not bind the exact signed attempt authority')
  }
}

function requireAttemptId(value: unknown): string {
  if (typeof value !== 'string' || !ATTEMPT_ID_PATTERN.test(value)) {
    networkMatrixError('network matrix attempt ID must be 16-128 ASCII authority characters')
  }
  return value
}

function requireCandidateEndpoint(
  address: unknown,
  port: unknown,
  addressFamily: unknown,
  label: string,
): { readonly address: string; readonly port: number } {
  const expectedAddressFamily = requireEnum(
    addressFamily,
    NETWORK_MATRIX_ADDRESS_FAMILIES,
    `network matrix ${label} address family`,
  )
  if (
    typeof address !== 'string' ||
    isIP(address) !== (expectedAddressFamily === 'ipv4' ? 4 : 6) ||
    !Number.isSafeInteger(port) || (port as number) < 1 || (port as number) > 65_535
  ) networkMatrixError(`network matrix ${label} endpoint is invalid`)
  return Object.freeze({ address, port: port as number })
}

function includesVocabulary(value: string, entries: readonly string[]): boolean {
  return entries.includes(value)
}

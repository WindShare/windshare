import {
  parseNetworkCandidatePath,
  type NetworkCandidatePath,
} from '../../candidate.ts'
import { parseNetworkMatrixAttemptAuthority } from '../../sample-authority.ts'
import { networkCandidatePathFromStats } from '../../stats-collector.ts'
import {
  isGlobalUnicastIpv4,
  parseSignedExternalFixtureAttestation,
} from '../external-fixture-attestation.ts'
import { parseSignedExternalFixtureTerminalReceipt } from '../external-fixture-terminal-receipt.ts'
import { REMOTE_PION_PROTOCOL_VERSION } from '../remote-pion.ts'
import {
  CONTAINED_BROWSER_SAMPLE_OUTPUT_SCHEMA,
  type ContainedBrowserSampleOutput,
} from './contracts.ts'
import {
  SHA256_PATTERN,
  exactRecord,
  invalidOutput,
  isCanonicalTimestamp,
  requireBrowser,
  requireCanonicalId,
  requireOpaqueId,
  requireProcessInstanceId,
} from './contract-validation.ts'

const MAXIMUM_STATS_RECORDS = 65_536
const MAXIMUM_UDP_PORT = 65_535
const SPKI_BASE64URL_PATTERN = /^[A-Za-z0-9_-]{32,256}$/u

export function parseContainedBrowserSampleOutput(value: unknown): ContainedBrowserSampleOutput {
  const output = exactRecord(value, [
    'schemaVersion', 'processInstanceId', 'browser', 'protocolResult', 'browserSelectedPair',
  ], invalidOutput)
  if (output.schemaVersion !== CONTAINED_BROWSER_SAMPLE_OUTPUT_SCHEMA) invalidOutput()
  const browser = requireBrowser(output.browser)
  const protocol = exactRecord(output.protocolResult, [
    'runId', 'authorityInstanceId', 'attestationSha256', 'remoteServiceInstanceId',
    'attestationPublicKeySpki', 'signedAttestation',
    'networkBindingSha256', 'remotePeerBindingSha256', 'controllerPublicIp',
    'attestationExpiresAt', 'remotePeerPublicIp', 'remotePeerUdpPortMin',
    'remotePeerUdpPortMax', 'attemptAuthority', 'state', 'selectedPair',
    'challengeBindingSha256', 'challenge', 'failureCode', 'challengeEchoed',
    'terminalReceipt',
  ], invalidOutput)
  const runId = requireCanonicalId(protocol.runId, invalidOutput)
  const authorityInstanceId = requireCanonicalId(protocol.authorityInstanceId, invalidOutput)
  const remoteServiceInstanceId = requireCanonicalId(
    protocol.remoteServiceInstanceId,
    invalidOutput,
  )
  const attemptAuthority = parseNetworkMatrixAttemptAuthority(protocol.attemptAuthority)
  const processInstanceId = requireProcessInstanceId(output.processInstanceId)
  const challenge = requireOpaqueId(protocol.challenge)
  const signedAttestation = parseSignedExternalFixtureAttestation(
    protocol.signedAttestation,
    REMOTE_PION_PROTOCOL_VERSION,
  )
  const terminalReceipt = parseSignedExternalFixtureTerminalReceipt(protocol.terminalReceipt)
  if (
    typeof protocol.attestationSha256 !== 'string' ||
    !SHA256_PATTERN.test(protocol.attestationSha256) ||
    typeof protocol.attestationPublicKeySpki !== 'string' ||
    !SPKI_BASE64URL_PATTERN.test(protocol.attestationPublicKeySpki) ||
    Buffer.from(protocol.attestationPublicKeySpki, 'base64url').toString('base64url') !==
      protocol.attestationPublicKeySpki ||
    typeof protocol.networkBindingSha256 !== 'string' ||
    !SHA256_PATTERN.test(protocol.networkBindingSha256) ||
    typeof protocol.remotePeerBindingSha256 !== 'string' ||
    !SHA256_PATTERN.test(protocol.remotePeerBindingSha256) ||
    typeof protocol.controllerPublicIp !== 'string' ||
    !isGlobalUnicastIpv4(protocol.controllerPublicIp) ||
    typeof protocol.attestationExpiresAt !== 'string' ||
    !isCanonicalTimestamp(protocol.attestationExpiresAt) ||
    typeof protocol.remotePeerPublicIp !== 'string' ||
    !isGlobalUnicastIpv4(protocol.remotePeerPublicIp) ||
    !Number.isSafeInteger(protocol.remotePeerUdpPortMin) ||
    !Number.isSafeInteger(protocol.remotePeerUdpPortMax) ||
    (protocol.remotePeerUdpPortMin as number) < 1 ||
    (protocol.remotePeerUdpPortMax as number) > MAXIMUM_UDP_PORT ||
    (protocol.remotePeerUdpPortMin as number) > (protocol.remotePeerUdpPortMax as number) ||
    protocol.state !== 'established' && protocol.state !== 'failed' ||
    typeof protocol.challengeBindingSha256 !== 'string' ||
    !SHA256_PATTERN.test(protocol.challengeBindingSha256) ||
    protocol.failureCode !== null && typeof protocol.failureCode !== 'string' ||
    typeof protocol.challengeEchoed !== 'boolean'
  ) invalidOutput()
  const sampleAuthority = attemptAuthority.requestAuthority.controlAuthority.sampleAuthority
  const fixtureBinding = attemptAuthority.requestAuthority.fixtureBinding
  if (
    sampleAuthority.runId !== runId || sampleAuthority.browser !== browser ||
    sampleAuthority.processInstanceId !== processInstanceId ||
    fixtureBinding.authorityInstanceId !== authorityInstanceId ||
    fixtureBinding.attestationSha256 !== protocol.attestationSha256 ||
    fixtureBinding.remoteServiceInstanceId !== remoteServiceInstanceId ||
    fixtureBinding.networkBindingSha256 !== protocol.networkBindingSha256 ||
    fixtureBinding.remotePeerBindingSha256 !== protocol.remotePeerBindingSha256 ||
    attemptAuthority.challenge !== challenge
  ) invalidOutput()
  return Object.freeze({
    schemaVersion: CONTAINED_BROWSER_SAMPLE_OUTPUT_SCHEMA,
    processInstanceId,
    browser,
    protocolResult: Object.freeze({
      runId,
      authorityInstanceId,
      attestationSha256: protocol.attestationSha256,
      attestationPublicKeySpki: protocol.attestationPublicKeySpki,
      signedAttestation,
      remoteServiceInstanceId,
      networkBindingSha256: protocol.networkBindingSha256,
      remotePeerBindingSha256: protocol.remotePeerBindingSha256,
      controllerPublicIp: protocol.controllerPublicIp,
      attestationExpiresAt: protocol.attestationExpiresAt,
      remotePeerPublicIp: protocol.remotePeerPublicIp,
      remotePeerUdpPortMin: protocol.remotePeerUdpPortMin as number,
      remotePeerUdpPortMax: protocol.remotePeerUdpPortMax as number,
      attemptAuthority,
      state: protocol.state,
      selectedPair: protocol.selectedPair ?? null,
      challengeBindingSha256: protocol.challengeBindingSha256,
      challenge,
      failureCode: protocol.failureCode,
      challengeEchoed: protocol.challengeEchoed,
      terminalReceipt,
    }),
    browserSelectedPair: parseNetworkCandidatePath(output.browserSelectedPair),
  })
}

export function containedBrowserCandidatePath(value: unknown): NetworkCandidatePath {
  try {
    return networkCandidatePathFromStats(parsePublicRtcStats(value))
  } catch {
    // getStats is browser-controlled input. Its values must never be reflected
    // through the child error channel, even when a vendor field is malicious.
    throw new Error('contained browser candidate path projection is invalid')
  }
}

function parsePublicRtcStats(value: unknown): readonly unknown[] {
  if (!Array.isArray(value) || value.length > MAXIMUM_STATS_RECORDS) {
    throw new Error('contained browser public getStats projection is invalid')
  }
  return Object.freeze(value.map((record) => {
    const base = exactRecord(record, publicStatsKeys(record), invalidOutput)
    if (typeof base.id !== 'string' || base.id === '' || typeof base.type !== 'string') {
      invalidOutput()
    }
    return Object.freeze({ ...base })
  }))
}

function publicStatsKeys(value: unknown): readonly string[] {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) invalidOutput()
  const type = (value as { readonly type?: unknown }).type
  if (type === 'transport') return ['id', 'type', 'selectedCandidatePairId']
  if (type === 'candidate-pair') {
    return [
      'id', 'type', 'localCandidateId', 'remoteCandidateId', 'selected', 'nominated',
      'state', 'protocol',
    ]
  }
  if (type === 'local-candidate' || type === 'remote-candidate') {
    return ['id', 'type', 'candidateType', 'address', 'ip', 'port', 'protocol']
  }
  invalidOutput()
}

import { randomBytes } from 'node:crypto'

import {
  parseNetworkCandidatePath,
  type NetworkCandidatePath,
} from '../candidate.ts'
import type { NetworkMatrixBrowser, NetworkMatrixProfileId } from '../vocabulary.ts'
import { networkCandidatePathFromStats } from '../stats-collector.ts'
import {
  parseNetworkMatrixAttemptAuthority,
  parseNetworkMatrixControlAuthority,
  sameNetworkMatrixControlAuthority,
  type NetworkMatrixAttemptAuthority,
} from '../sample-authority.ts'
import type {
  ExternalFixtureProbeResult,
  NetworkMatrixRtcConfiguration,
} from '../runtime-authority.ts'
import {
  REMOTE_PION_ATTESTATION_LEASE_MS,
  REMOTE_PION_MAXIMUM_ATTEMPT_LEASE_MS,
  REMOTE_PION_PROTOCOL_VERSION,
  RemotePionControlClient,
  type RemotePionControlLeaseBinding,
  type RemotePionAttemptLease,
  type RemotePionAttemptResult,
} from './remote-pion.ts'
import {
  isGlobalUnicastIpv4,
  parseSignedExternalFixtureAttestation,
  type SignedExternalFixtureAttestation,
  type ManualOperatorTopologyIdentity,
} from './external-fixture-attestation.ts'
import {
  parseSignedExternalFixtureTerminalReceipt,
  type SignedExternalFixtureTerminalReceipt,
} from './external-fixture-terminal-receipt.ts'

export const CONTAINED_BROWSER_SAMPLE_SECRET_SCHEMA =
  'windshare.browser-network-matrix.contained-browser-secret/v3' as const
export const CONTAINED_BROWSER_SAMPLE_OUTPUT_SCHEMA =
  'windshare.browser-network-matrix.contained-browser-output/v3' as const
const MAXIMUM_SECRET_BYTES = 1_048_576
const SECRET_METADATA_LENGTH_BYTES = 4
const MINIMUM_CONTROL_CREDENTIAL_BYTES = 32
const MAXIMUM_CONTROL_CREDENTIAL_BYTES = 512
const MAXIMUM_STATS_RECORDS = 65_536
const SHA256_PATTERN = /^[a-f0-9]{64}$/u
const OPAQUE_ID_PATTERN = /^[A-Za-z0-9_-]{16,128}$/u
const PROCESS_INSTANCE_PATTERN = /^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/u
const BROWSERS = Object.freeze(['chromium', 'firefox', 'webkit'] as const)

export interface ContainedBrowserSampleSecret {
  readonly schemaVersion: typeof CONTAINED_BROWSER_SAMPLE_SECRET_SCHEMA
  readonly expectedConnectivity: 'established' | 'blocked'
  readonly control: {
    readonly controllerOrigin: string
    readonly controlLease: RemotePionControlLeaseBinding
    readonly tlsCertificateAuthority: string
    readonly tlsCertificateSha256: string
    readonly attestationPublicKey: string
    readonly credential: Uint8Array
    readonly manualOperatorIdentity: ManualOperatorTopologyIdentity | null
  }
  readonly attemptLeaseMs: number
  readonly resultPollIntervalMs: number
  readonly resultDeadlineMs: number
  readonly challengeDeadlineMs: number
  readonly cleanupDeadlineMs: number
}

export interface ContainedBrowserProtocolResult {
  readonly runId: string
  readonly authorityInstanceId: string
  readonly attestationSha256: string
  readonly attestationPublicKeySpki: string
  readonly signedAttestation: SignedExternalFixtureAttestation
  readonly remoteServiceInstanceId: string
  readonly networkBindingSha256: string
  readonly remotePeerBindingSha256: string
  readonly controllerPublicIp: string
  readonly attestationExpiresAt: string
  readonly remotePeerPublicIp: string
  readonly remotePeerUdpPortMin: number
  readonly remotePeerUdpPortMax: number
  readonly attemptAuthority: NetworkMatrixAttemptAuthority
  readonly state: 'established' | 'failed'
  readonly selectedPair: unknown | null
  readonly challengeBindingSha256: string
  readonly challenge: string
  readonly failureCode: string | null
  readonly challengeEchoed: boolean
  readonly terminalReceipt: SignedExternalFixtureTerminalReceipt
}

export interface ContainedBrowserSampleOutput {
  readonly schemaVersion: typeof CONTAINED_BROWSER_SAMPLE_OUTPUT_SCHEMA
  readonly processInstanceId: string
  readonly browser: NetworkMatrixBrowser
  readonly protocolResult: ContainedBrowserProtocolResult
  readonly browserSelectedPair: NetworkCandidatePath
}

export interface ContainedBrowserSession {
  readonly engine: string
  createOffer(configuration: NetworkMatrixRtcConfiguration): Promise<string>
  acceptAnswer(answer: string): Promise<void>
  exchangeChallenge(frame: string, deadlineMs: number): Promise<string>
  getStats(): Promise<readonly unknown[]>
  close(): Promise<void>
}

export interface ContainedBrowserPionControl {
  probeFixture(signal: AbortSignal): Promise<ExternalFixtureProbeResult>
  createAttempt(
    requestId: string,
    leaseMillis: number,
    signal: AbortSignal,
  ): Promise<RemotePionAttemptLease>
  offer(attemptId: string, sdp: string, signal: AbortSignal): Promise<string>
  result(attemptId: string, signal: AbortSignal): Promise<RemotePionAttemptResult>
  deleteAttempt(attemptId: string, signal: AbortSignal): Promise<void>
}

export interface ContainedBrowserSampleDependencies {
  readonly launch: (browser: NetworkMatrixBrowser) => Promise<ContainedBrowserSession>
  readonly control: (secret: ContainedBrowserSampleSecret) => ContainedBrowserPionControl
  readonly requestId: () => string
  readonly delay: (milliseconds: number, signal: AbortSignal) => Promise<void>
  readonly now: () => number
}

export interface RunContainedBrowserSampleOptions {
  readonly browser: NetworkMatrixBrowser
  readonly secret: ContainedBrowserSampleSecret
  readonly signal: AbortSignal
  readonly dependencies?: ContainedBrowserSampleDependencies
}

export interface ContainedBrowserSampleSecretFrame {
  readonly bytes: Uint8Array
  readonly credentialOffset: number
  readonly credentialByteLength: number
}

export async function loadContainedBrowserSampleSecret(
  input: AsyncIterable<Uint8Array | string>,
): Promise<ContainedBrowserSampleSecret> {
  const chunks: Buffer[] = []
  let observedBytes = 0
  try {
    for await (const chunk of input) {
      const bytes = typeof chunk === 'string' ? Buffer.from(chunk, 'utf8') : Buffer.from(chunk)
      observedBytes += bytes.byteLength
      if (observedBytes > MAXIMUM_SECRET_BYTES) invalidSecret()
      chunks.push(bytes)
    }
  } finally {
    const destroy = (input as { readonly destroy?: () => void }).destroy
    destroy?.call(input)
  }
  if (observedBytes === 0) invalidSecret()
  const bytes = Buffer.concat(chunks, observedBytes)
  try {
    return parseContainedBrowserSampleSecretFrame(bytes)
  } catch {
    invalidSecret()
  } finally {
    bytes.fill(0)
    for (const chunk of chunks) chunk.fill(0)
  }
  return invalidSecret()
}

/**
 * The parent serializes only non-secret metadata as JSON. Credential ownership
 * remains an erasable byte range appended to the anonymous stdin frame.
 */
export function encodeContainedBrowserSampleSecretFrame(
  value: unknown,
  credential: Uint8Array,
): ContainedBrowserSampleSecretFrame {
  if (!isControlCredentialBytes(credential)) invalidSecret()
  const secret = parseContainedBrowserSampleSecret(withCredential(value, credential))
  const metadata = secretFrameMetadata(secret)
  const metadataBytes = Buffer.from(`${JSON.stringify(metadata)}\n`, 'utf8')
  if (
    metadataBytes.byteLength === 0 ||
    metadataBytes.byteLength + credential.byteLength + SECRET_METADATA_LENGTH_BYTES >
      MAXIMUM_SECRET_BYTES
  ) invalidSecret()
  const credentialOffset = SECRET_METADATA_LENGTH_BYTES + metadataBytes.byteLength
  const frame = Buffer.alloc(credentialOffset + credential.byteLength)
  frame.writeUInt32BE(metadataBytes.byteLength, 0)
  metadataBytes.copy(frame, SECRET_METADATA_LENGTH_BYTES)
  frame.set(credential, credentialOffset)
  return Object.freeze({
    bytes: frame,
    credentialOffset,
    credentialByteLength: credential.byteLength,
  })
}

function parseContainedBrowserSampleSecretFrame(bytes: Buffer): ContainedBrowserSampleSecret {
  if (bytes.byteLength <= SECRET_METADATA_LENGTH_BYTES) invalidSecret()
  const metadataByteLength = bytes.readUInt32BE(0)
  const credentialOffset = SECRET_METADATA_LENGTH_BYTES + metadataByteLength
  if (
    metadataByteLength === 0 || credentialOffset >= bytes.byteLength ||
    credentialOffset > MAXIMUM_SECRET_BYTES
  ) invalidSecret()
  const encodedMetadata = new TextDecoder('utf-8', { fatal: true }).decode(
    bytes.subarray(SECRET_METADATA_LENGTH_BYTES, credentialOffset),
  )
  if (
    !encodedMetadata.endsWith('\n') || encodedMetadata.includes('\r') ||
    encodedMetadata.slice(0, -1).includes('\n')
  ) invalidSecret()
  let metadata: unknown
  try {
    metadata = JSON.parse(encodedMetadata)
  } catch {
    invalidSecret()
  }
  if (encodedMetadata !== `${JSON.stringify(metadata)}\n`) invalidSecret()
  const rawCredential = bytes.subarray(credentialOffset)
  const credential = Buffer.from(rawCredential)
  try {
    const secret = secretFromFrameMetadata(metadata, credential)
    return parseContainedBrowserSampleSecret(secret)
  } catch {
    credential.fill(0)
    invalidSecret()
  }
}

function withCredential(value: unknown, credential: Uint8Array): unknown {
  const secret = exactRecord(value, [
    'schemaVersion', 'expectedConnectivity', 'control', 'attemptLeaseMs',
    'resultPollIntervalMs', 'resultDeadlineMs', 'challengeDeadlineMs', 'cleanupDeadlineMs',
  ], 'contained browser secret frame')
  const control = exactRecord(secret.control, [
    'controllerOrigin', 'controlLease',
    'tlsCertificateAuthority', 'tlsCertificateSha256', 'attestationPublicKey',
    'manualOperatorIdentity',
  ], 'contained browser secret frame control')
  return {
    schemaVersion: secret.schemaVersion,
    expectedConnectivity: secret.expectedConnectivity,
    control: {
      controllerOrigin: control.controllerOrigin,
      controlLease: control.controlLease,
      tlsCertificateAuthority: control.tlsCertificateAuthority,
      tlsCertificateSha256: control.tlsCertificateSha256,
      attestationPublicKey: control.attestationPublicKey,
      credential,
      manualOperatorIdentity: control.manualOperatorIdentity,
    },
    attemptLeaseMs: secret.attemptLeaseMs,
    resultPollIntervalMs: secret.resultPollIntervalMs,
    resultDeadlineMs: secret.resultDeadlineMs,
    challengeDeadlineMs: secret.challengeDeadlineMs,
    cleanupDeadlineMs: secret.cleanupDeadlineMs,
  }
}

function secretFrameMetadata(secret: ContainedBrowserSampleSecret): unknown {
  return {
    schemaVersion: secret.schemaVersion,
    expectedConnectivity: secret.expectedConnectivity,
    control: {
      controllerOrigin: secret.control.controllerOrigin,
      controlLease: secret.control.controlLease,
      tlsCertificateAuthority: secret.control.tlsCertificateAuthority,
      tlsCertificateSha256: secret.control.tlsCertificateSha256,
      attestationPublicKey: secret.control.attestationPublicKey,
      credentialByteLength: secret.control.credential.byteLength,
      manualOperatorIdentity: secret.control.manualOperatorIdentity,
    },
    attemptLeaseMs: secret.attemptLeaseMs,
    resultPollIntervalMs: secret.resultPollIntervalMs,
    resultDeadlineMs: secret.resultDeadlineMs,
    challengeDeadlineMs: secret.challengeDeadlineMs,
    cleanupDeadlineMs: secret.cleanupDeadlineMs,
  }
}

function secretFromFrameMetadata(metadataValue: unknown, credential: Uint8Array): unknown {
  const metadata = exactRecord(metadataValue, [
    'schemaVersion', 'expectedConnectivity', 'control', 'attemptLeaseMs',
    'resultPollIntervalMs', 'resultDeadlineMs', 'challengeDeadlineMs', 'cleanupDeadlineMs',
  ], 'contained browser secret frame')
  const control = exactRecord(metadata.control, [
    'controllerOrigin', 'controlLease',
    'tlsCertificateAuthority', 'tlsCertificateSha256', 'attestationPublicKey',
    'credentialByteLength', 'manualOperatorIdentity',
  ], 'contained browser secret frame control')
  if (
    control.credentialByteLength !== credential.byteLength
  ) invalidSecret()
  return withCredential({
    schemaVersion: metadata.schemaVersion,
    expectedConnectivity: metadata.expectedConnectivity,
    control: {
      controllerOrigin: control.controllerOrigin,
      controlLease: control.controlLease,
      tlsCertificateAuthority: control.tlsCertificateAuthority,
      tlsCertificateSha256: control.tlsCertificateSha256,
      attestationPublicKey: control.attestationPublicKey,
      manualOperatorIdentity: control.manualOperatorIdentity,
    },
    attemptLeaseMs: metadata.attemptLeaseMs,
    resultPollIntervalMs: metadata.resultPollIntervalMs,
    resultDeadlineMs: metadata.resultDeadlineMs,
    challengeDeadlineMs: metadata.challengeDeadlineMs,
    cleanupDeadlineMs: metadata.cleanupDeadlineMs,
  }, credential)
}

export function parseContainedBrowserSampleSecret(value: unknown): ContainedBrowserSampleSecret {
  const secret = exactRecord(value, [
    'schemaVersion', 'expectedConnectivity', 'control', 'attemptLeaseMs',
    'resultPollIntervalMs', 'resultDeadlineMs', 'challengeDeadlineMs', 'cleanupDeadlineMs',
  ], 'contained browser secret authority')
  if (secret.schemaVersion !== CONTAINED_BROWSER_SAMPLE_SECRET_SCHEMA) invalidSecret()
  const control = exactRecord(secret.control, [
    'controllerOrigin', 'controlLease',
    'tlsCertificateAuthority', 'tlsCertificateSha256', 'attestationPublicKey',
    'credential', 'manualOperatorIdentity',
  ], 'contained browser control authority')
  const controllerOrigin = requireOriginOnlyHttps(control.controllerOrigin)
  const controlLease = parseControlLease(control.controlLease)
  const sampleAuthority = controlLease.controlAuthority.sampleAuthority
  const expectedConnectivity = sampleAuthority.profileId === 'scheduled-restricted-udp'
    ? 'blocked'
    : 'established'
  if (
    secret.expectedConnectivity !== expectedConnectivity ||
    typeof control.tlsCertificateAuthority !== 'string' ||
    control.tlsCertificateAuthority.length === 0 ||
    control.tlsCertificateAuthority.length > MAXIMUM_SECRET_BYTES ||
    typeof control.tlsCertificateSha256 !== 'string' ||
    !SHA256_PATTERN.test(control.tlsCertificateSha256) ||
    typeof control.attestationPublicKey !== 'string' ||
    control.attestationPublicKey.length === 0 ||
    control.attestationPublicKey.length > MAXIMUM_SECRET_BYTES ||
    !isControlCredentialBytes(control.credential)
  ) invalidSecret()
  const manualOperatorIdentity = parseManualOperatorIdentity(
    control.manualOperatorIdentity,
    sampleAuthority.profileId,
  )
  const attemptLeaseMs = boundedMilliseconds(secret.attemptLeaseMs)
  const resultPollIntervalMs = boundedMilliseconds(secret.resultPollIntervalMs)
  const resultDeadlineMs = boundedMilliseconds(secret.resultDeadlineMs)
  const challengeDeadlineMs = boundedMilliseconds(secret.challengeDeadlineMs)
  const cleanupDeadlineMs = boundedMilliseconds(secret.cleanupDeadlineMs)
  if (
    resultPollIntervalMs >= resultDeadlineMs || resultDeadlineMs > attemptLeaseMs ||
    challengeDeadlineMs > attemptLeaseMs ||
    attemptLeaseMs > REMOTE_PION_MAXIMUM_ATTEMPT_LEASE_MS ||
    attemptLeaseMs >= REMOTE_PION_ATTESTATION_LEASE_MS
  ) invalidSecret()
  return Object.freeze({
    schemaVersion: CONTAINED_BROWSER_SAMPLE_SECRET_SCHEMA,
    expectedConnectivity,
    control: Object.freeze({
      controllerOrigin,
      controlLease,
      tlsCertificateAuthority: control.tlsCertificateAuthority as string,
      tlsCertificateSha256: control.tlsCertificateSha256 as string,
      attestationPublicKey: control.attestationPublicKey as string,
      credential: control.credential,
      manualOperatorIdentity,
    }),
    attemptLeaseMs,
    resultPollIntervalMs,
    resultDeadlineMs,
    challengeDeadlineMs,
    cleanupDeadlineMs,
  })
}

export async function runContainedBrowserSample(
  options: RunContainedBrowserSampleOptions,
): Promise<ContainedBrowserSampleOutput> {
  const browser = requireBrowser(options.browser)
  const secret = parseContainedBrowserSampleSecret(options.secret)
  const sampleAuthority = secret.control.controlLease.controlAuthority.sampleAuthority
  if (sampleAuthority.browser !== browser) {
    throw new Error('contained browser engine differs from its sample authority')
  }
  const dependencies = options.dependencies ?? DEFAULT_DEPENDENCIES
  const control = dependencies.control(secret)
  let session: ContainedBrowserSession | undefined
  let attemptId: string | undefined
  let primaryFailure: unknown
  let output: ContainedBrowserSampleOutput | undefined
  try {
    requireActive(options.signal)
    const probe = requireExactFixtureProbe(
      secret,
      await control.probeFixture(options.signal),
      dependencies.now(),
    )
    session = await dependencies.launch(browser)
    if (session.engine !== browser) throw new Error('contained browser launcher returned the wrong engine')
    const requestId = requireOpaqueId(dependencies.requestId())
    const lease = await control.createAttempt(
      requestId,
      secret.attemptLeaseMs,
      options.signal,
    )
    requireLeaseBinding(probe, lease, secret.control.controlLease.controlAuthority)
    const issuedAttemptId = requireOpaqueId(lease.attemptAuthority.attemptId)
    if (issuedAttemptId === requestId) throw new Error('remote Pion attempt ID was not independently issued')
    // Once a distinct server authority is known, every later validation branch
    // must retain it so finally can reap the exact attempt.
    attemptId = issuedAttemptId
    const frame = challengeFrame(lease)
    const offer = await session.createOffer(probe.rtcConfiguration)
    const answer = await control.offer(attemptId, offer, options.signal)
    await session.acceptAnswer(answer)
    const echo = session.exchangeChallenge(frame, secret.challengeDeadlineMs).then(
      (value) => Object.freeze({ outcome: 'received' as const, value }),
      () => Object.freeze({ outcome: 'absent' as const, value: null }),
    )
    const terminal = await pollTerminalResult(
      control,
      attemptId,
      secret,
      options.signal,
      dependencies.delay,
    )
    requireAttemptBinding(probe, terminal)
    if (terminal.terminalReceipt === null) {
      throw new Error('contained browser terminal result lacks a signed receipt')
    }
    const echoOutcome = terminal.state === 'established' ? await echo : undefined
    const challengeEchoed = echoOutcome?.outcome === 'received' && echoOutcome.value === frame
    requireExpectedTerminal(secret.expectedConnectivity, terminal, challengeEchoed)
    const browserSelectedPair = containedBrowserCandidatePath(await session.getStats())
    output = Object.freeze({
      schemaVersion: CONTAINED_BROWSER_SAMPLE_OUTPUT_SCHEMA,
      processInstanceId: requireProcessInstanceId(sampleAuthority.processInstanceId),
      browser,
      protocolResult: Object.freeze({
        runId: probe.runId,
        authorityInstanceId: probe.authorityInstanceId,
        attestationSha256: terminal.attemptAuthority.requestAuthority.fixtureBinding.attestationSha256,
        attestationPublicKeySpki: probe.attestationPublicKeySpki,
        signedAttestation: probe.signedAttestation,
        remoteServiceInstanceId:
          terminal.attemptAuthority.requestAuthority.fixtureBinding.remoteServiceInstanceId,
        networkBindingSha256:
          terminal.attemptAuthority.requestAuthority.fixtureBinding.networkBindingSha256,
        remotePeerBindingSha256:
          terminal.attemptAuthority.requestAuthority.fixtureBinding.remotePeerBindingSha256,
        controllerPublicIp: probe.controllerPublicIp,
        attestationExpiresAt: probe.leaseExpiresAt,
        remotePeerPublicIp: probe.signedAttestation.attestation.fixture.remotePeerPublicIp,
        remotePeerUdpPortMin: probe.signedAttestation.attestation.fixture.remotePeerUdpPortMin,
        remotePeerUdpPortMax: probe.signedAttestation.attestation.fixture.remotePeerUdpPortMax,
        attemptAuthority: terminal.attemptAuthority,
        state: terminal.state,
        selectedPair: terminal.selectedPair,
        challengeBindingSha256: terminal.challengeBindingSha256,
        challenge: lease.attemptAuthority.challenge,
        failureCode: terminal.failureCode,
        challengeEchoed,
        terminalReceipt: terminal.terminalReceipt,
      }),
      browserSelectedPair,
    })
  } catch (cause) {
    primaryFailure = cause
  }
  const cleanupFailures = await cleanupSample(control, attemptId, session, secret.cleanupDeadlineMs)
  if (cleanupFailures.length !== 0) {
    throw new AggregateError(
      [...(primaryFailure === undefined ? [] : [primaryFailure]), ...cleanupFailures],
      'contained browser sample cleanup did not prove terminal reaping',
    )
  }
  if (primaryFailure !== undefined) throw primaryFailure
  if (output === undefined) throw new Error('contained browser sample did not publish terminal output')
  return output
}

export function parseContainedBrowserSampleOutput(value: unknown): ContainedBrowserSampleOutput {
  const output = exactRecord(value, [
    'schemaVersion', 'processInstanceId', 'browser', 'protocolResult', 'browserSelectedPair',
  ], 'contained browser sample output', invalidOutput)
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
  ], 'contained browser protocol result', invalidOutput)
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
    !/^[A-Za-z0-9_-]{32,256}$/u.test(protocol.attestationPublicKeySpki) ||
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
    (protocol.remotePeerUdpPortMax as number) > 65_535 ||
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
    const base = exactRecord(
      record,
      publicStatsKeys(record),
      'contained browser public getStats record',
      invalidOutput,
    )
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

export function challengeFrame(
  input: RemotePionAttemptLease,
): string {
  return JSON.stringify({
    protocolVersion: REMOTE_PION_PROTOCOL_VERSION,
    attemptAuthority: input.attemptAuthority,
  })
}

async function pollTerminalResult(
  control: ContainedBrowserPionControl,
  attemptId: string,
  secret: ContainedBrowserSampleSecret,
  signal: AbortSignal,
  delay: ContainedBrowserSampleDependencies['delay'],
): Promise<RemotePionAttemptResult & { readonly state: 'established' | 'failed' }> {
  const deadline = performance.now() + secret.resultDeadlineMs
  for (;;) {
    requireActive(signal)
    const result = await control.result(attemptId, signal)
    if (result.state === 'established' || result.state === 'failed') {
      return Object.freeze({ ...result, state: result.state })
    }
    if (performance.now() >= deadline) throw new Error('remote Pion attempt did not become terminal')
    await delay(secret.resultPollIntervalMs, signal)
  }
}

function requireExpectedTerminal(
  expectation: ContainedBrowserSampleSecret['expectedConnectivity'],
  result: RemotePionAttemptResult & { readonly state: 'established' | 'failed' },
  challengeEchoed: boolean,
): void {
  if (
    expectation === 'established' &&
    (result.state !== 'established' || result.selectedPair === null || !challengeEchoed)
  ) throw new Error('contained browser application path was not established')
  if (
    expectation === 'blocked' &&
    (result.state !== 'failed' || result.selectedPair !== null || challengeEchoed)
  ) throw new Error('contained browser unexpectedly established connectivity')
}

async function cleanupSample(
  control: ContainedBrowserPionControl,
  attemptId: string | undefined,
  session: ContainedBrowserSession | undefined,
  deadlineMs: number,
): Promise<readonly unknown[]> {
  const failures: unknown[] = []
  if (attemptId !== undefined) {
    const controller = new AbortController()
    const timeout = setTimeout(() => controller.abort(), deadlineMs)
    try {
      await control.deleteAttempt(attemptId, controller.signal)
    } catch {
      failures.push(new Error('remote Pion attempt cleanup failed'))
    } finally {
      clearTimeout(timeout)
    }
  }
  if (session !== undefined) {
    try {
      await session.close()
    } catch {
      failures.push(new Error('contained browser process cleanup failed'))
    }
  }
  return failures
}

function exactRecord(
  value: unknown,
  keys: readonly string[],
  _label: string,
  invalid: () => never = invalidSecret,
): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) invalid()
  const record = value as Record<string, unknown>
  const actual = Object.keys(record)
  const expected = [...keys]
  if (actual.length !== expected.length || actual.some((key, index) => key !== expected[index])) {
    invalid()
  }
  return record
}

function requireOriginOnlyHttps(value: unknown): string {
  if (typeof value !== 'string') invalidSecret()
  let endpoint: URL
  try {
    endpoint = new URL(value)
  } catch {
    invalidSecret()
  }
  if (
    endpoint.protocol !== 'https:' || endpoint.username !== '' || endpoint.password !== '' ||
    endpoint.pathname !== '/' || endpoint.search !== '' || endpoint.hash !== '' ||
    value !== `${endpoint.origin}/`
  ) invalidSecret()
  return value
}

function boundedMilliseconds(value: unknown): number {
  if (!Number.isSafeInteger(value) || (value as number) < 1 || (value as number) > 300_000) {
    invalidSecret()
  }
  return value as number
}

function requireBrowser(value: unknown): NetworkMatrixBrowser {
  if (typeof value !== 'string' || !BROWSERS.includes(value as NetworkMatrixBrowser)) invalidOutput()
  return value as NetworkMatrixBrowser
}

function isControlCredentialBytes(value: unknown): value is Uint8Array {
  if (
    !(value instanceof Uint8Array) ||
    value.byteLength < MINIMUM_CONTROL_CREDENTIAL_BYTES ||
    value.byteLength > MAXIMUM_CONTROL_CREDENTIAL_BYTES
  ) return false
  for (const byte of value) {
    const alphaNumeric = byte >= 0x30 && byte <= 0x39 ||
      byte >= 0x41 && byte <= 0x5a || byte >= 0x61 && byte <= 0x7a
    if (!alphaNumeric && byte !== 0x2d && byte !== 0x5f) return false
  }
  return true
}

function requireExactFixtureProbe(
  secret: ContainedBrowserSampleSecret,
  result: ExternalFixtureProbeResult,
  now: number,
): Extract<ExternalFixtureProbeResult, { outcome: 'satisfied' }> {
  const sampleAuthority = secret.control.controlLease.controlAuthority.sampleAuthority
  if (
    result.outcome !== 'satisfied' || !PROCESS_INSTANCE_PATTERN.test(result.probeId) ||
    result.runId !== sampleAuthority.runId || result.profileId !== sampleAuthority.profileId ||
    result.authorityInstanceId !== secret.control.controlLease.authorityInstanceId ||
    result.attestationSha256 !== secret.control.controlLease.attestationSha256 ||
    Date.parse(result.leaseExpiresAt) <= now + secret.attemptLeaseMs ||
    Date.parse(secret.control.controlLease.expiresAt) <= now + secret.attemptLeaseMs ||
    Date.parse(secret.control.controlLease.expiresAt) > Date.parse(result.leaseExpiresAt)
  ) throw new Error('contained browser external fixture probe was not exact')
  return result
}

function requireLeaseBinding(
  probe: Extract<ExternalFixtureProbeResult, { outcome: 'satisfied' }>,
  lease: RemotePionAttemptLease,
  expectedControlAuthority: RemotePionControlLeaseBinding['controlAuthority'],
): void {
  const requestAuthority = lease.attemptAuthority.requestAuthority
  if (
    !sameNetworkMatrixControlAuthority(
      requestAuthority.controlAuthority,
      expectedControlAuthority,
    ) ||
    requestAuthority.controlAuthority.sampleAuthority.runId !== probe.runId ||
    requestAuthority.fixtureBinding.attestationSha256 !== probe.attestationSha256 ||
    requestAuthority.fixtureBinding.remoteServiceInstanceId !== probe.remoteServiceInstanceId ||
    requestAuthority.fixtureBinding.networkBindingSha256 !== probe.networkBindingSha256 ||
    requestAuthority.fixtureBinding.remotePeerBindingSha256 !== probe.remotePeerBindingSha256
  ) throw new Error('remote Pion attempt lease crossed its signed fixture authority')
}

function requireAttemptBinding(
  probe: Extract<ExternalFixtureProbeResult, { outcome: 'satisfied' }>,
  result: RemotePionAttemptResult,
): void {
  const requestAuthority = result.attemptAuthority.requestAuthority
  if (
    requestAuthority.controlAuthority.sampleAuthority.runId !== probe.runId ||
    requestAuthority.fixtureBinding.attestationSha256 !== probe.attestationSha256 ||
    requestAuthority.fixtureBinding.remoteServiceInstanceId !== probe.remoteServiceInstanceId ||
    requestAuthority.fixtureBinding.networkBindingSha256 !== probe.networkBindingSha256 ||
    requestAuthority.fixtureBinding.remotePeerBindingSha256 !== probe.remotePeerBindingSha256
  ) throw new Error('remote Pion result crossed its signed fixture authority')
}

function requireOpaqueId(value: unknown): string {
  if (typeof value !== 'string' || !OPAQUE_ID_PATTERN.test(value)) invalidOutput()
  return value
}

function requireProcessInstanceId(value: unknown): string {
  if (typeof value !== 'string' || !PROCESS_INSTANCE_PATTERN.test(value)) invalidOutput()
  return value
}

function requireCanonicalId(value: unknown, invalid: () => never = invalidSecret): string {
  if (typeof value !== 'string' || !PROCESS_INSTANCE_PATTERN.test(value)) invalid()
  return value
}

function parseManualOperatorIdentity(
  value: unknown,
  profileId: NetworkMatrixProfileId,
): ManualOperatorTopologyIdentity | null {
  if (profileId !== 'manual-real-nat') {
    if (value !== null) invalidSecret()
    return null
  }
  const identity = exactRecord(
    value,
    ['senderHostId', 'senderNetworkBoundaryId'],
    'manual operator topology identity',
  )
  return Object.freeze({
    senderHostId: requireCanonicalId(identity.senderHostId),
    senderNetworkBoundaryId: requireCanonicalId(identity.senderNetworkBoundaryId),
  })
}

function parseControlLease(
  value: unknown,
): RemotePionControlLeaseBinding {
  const lease = exactRecord(value, [
    'controlAuthority', 'probeNonce', 'authorityInstanceId', 'attestationSha256',
    'issuedAt', 'expiresAt', 'maxAttempts',
  ], 'contained browser control credential lease')
  const controlAuthority = parseNetworkMatrixControlAuthority(lease.controlAuthority)
  if (
    typeof lease.probeNonce !== 'string' || !OPAQUE_ID_PATTERN.test(lease.probeNonce) ||
    typeof lease.authorityInstanceId !== 'string' ||
    !PROCESS_INSTANCE_PATTERN.test(lease.authorityInstanceId) ||
    typeof lease.attestationSha256 !== 'string' ||
    !SHA256_PATTERN.test(lease.attestationSha256) || lease.maxAttempts !== 1
  ) invalidSecret()
  const issuedAt = canonicalTimestamp(lease.issuedAt)
  const expiresAt = canonicalTimestamp(lease.expiresAt)
  if (expiresAt <= issuedAt || expiresAt - issuedAt > REMOTE_PION_ATTESTATION_LEASE_MS) {
    invalidSecret()
  }
  return Object.freeze({
    controlAuthority,
    probeNonce: lease.probeNonce,
    authorityInstanceId: lease.authorityInstanceId,
    attestationSha256: lease.attestationSha256,
    issuedAt: lease.issuedAt as string,
    expiresAt: lease.expiresAt as string,
    maxAttempts: 1,
  })
}

function canonicalTimestamp(value: unknown): number {
  if (
    typeof value !== 'string' ||
    !/^\d{4}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01])T(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d\.\d{3}Z$/u.test(value)
  ) invalidSecret()
  const timestamp = Date.parse(value)
  if (Number.isNaN(timestamp) || new Date(timestamp).toISOString() !== value) invalidSecret()
  return timestamp
}

function isCanonicalTimestamp(value: string): boolean {
  if (!/^\d{4}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01])T(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d\.\d{3}Z$/u.test(value)) {
    return false
  }
  const timestamp = Date.parse(value)
  return !Number.isNaN(timestamp) && new Date(timestamp).toISOString() === value
}

function requireActive(signal: AbortSignal): void {
  if (signal.aborted) throw new Error('contained browser sample was terminated')
}

function invalidSecret(): never {
  throw new Error('contained browser secret authority is invalid')
}

function invalidOutput(): never {
  throw new Error('contained browser sample output is invalid')
}

const DEFAULT_DEPENDENCIES: ContainedBrowserSampleDependencies = Object.freeze({
  launch: launchPlaywrightBrowser,
  control: externalFixtureControl,
  requestId: () => randomBytes(24).toString('base64url'),
  delay: abortableDelay,
  now: Date.now,
})

function externalFixtureControl(secret: ContainedBrowserSampleSecret): ContainedBrowserPionControl {
  const client = new RemotePionControlClient({
    controllerOrigin: secret.control.controllerOrigin,
    tlsCertificateAuthority: secret.control.tlsCertificateAuthority,
    tlsCertificateSha256: secret.control.tlsCertificateSha256,
    attestationPublicKey: secret.control.attestationPublicKey,
    controlCredential: secret.control.credential,
    controlLease: secret.control.controlLease,
    ...(secret.control.manualOperatorIdentity === null
      ? {}
      : { manualOperatorIdentity: secret.control.manualOperatorIdentity }),
  })
  return Object.freeze({
    probeFixture: async (signal: AbortSignal): Promise<ExternalFixtureProbeResult> => {
      const operation = client.probe({
        sampleAuthority: secret.control.controlLease.controlAuthority.sampleAuthority,
        signal,
      })
      try {
        return await operation.result
      } catch (primaryFailure) {
        try {
          await operation.forceTerminateAndWait('sample-execute')
        } catch (cleanupFailure) {
          throw new AggregateError(
            [primaryFailure, cleanupFailure],
            'contained browser external fixture probe cleanup failed',
            { cause: cleanupFailure },
          )
        }
        throw primaryFailure
      }
    },
    createAttempt: (
      requestId: string,
      leaseMillis: number,
      signal: AbortSignal,
    ) => client.createAttempt(requestId, leaseMillis, signal),
    offer: (attemptId: string, sdp: string, signal: AbortSignal) =>
      client.offer(attemptId, sdp, signal),
    result: (attemptId: string, signal: AbortSignal) => client.result(attemptId, signal),
    deleteAttempt: (attemptId: string, signal: AbortSignal) =>
      client.deleteAttempt(attemptId, signal),
  })
}

async function abortableDelay(milliseconds: number, signal: AbortSignal): Promise<void> {
  await new Promise<void>((resolve, reject) => {
    if (signal.aborted) {
      reject(new Error('contained browser sample was terminated'))
      return
    }
    const timeout = setTimeout(done, milliseconds)
    signal.addEventListener('abort', aborted, { once: true })
    function done(): void {
      signal.removeEventListener('abort', aborted)
      resolve()
    }
    function aborted(): void {
      clearTimeout(timeout)
      reject(new Error('contained browser sample was terminated'))
    }
  })
}

async function launchPlaywrightBrowser(
  requested: NetworkMatrixBrowser,
): Promise<ContainedBrowserSession> {
  const playwright = await import('@playwright/test')
  const browserType = {
    chromium: playwright.chromium,
    firefox: playwright.firefox,
    webkit: playwright.webkit,
  }[requested]
  const browser = await browserType.launch({ headless: true })
  const engine = browser.browserType().name()
  const page = await browser.newPage()
  return Object.freeze({
    engine,
    createOffer: (configuration: NetworkMatrixRtcConfiguration) =>
      page.evaluate(createOfferInPage, configuration),
    acceptAnswer: (answer: string) => page.evaluate(acceptAnswerInPage, answer),
    exchangeChallenge: (frame: string, deadlineMs: number) => page.evaluate(
      exchangeChallengeInPage,
      { expected: frame, timeoutMs: deadlineMs },
    ),
    getStats: () => page.evaluate(getStatsInPage),
    close: () => browser.close(),
  })
}

async function createOfferInPage(rtc: NetworkMatrixRtcConfiguration): Promise<string> {
  const peer = new RTCPeerConnection({
    iceServers: rtc.iceServers.map((server) => ({
      urls: [...server.urls],
      ...(server.username === null ? {} : { username: server.username }),
      ...(server.credential === null ? {} : { credential: server.credential }),
    })),
    iceTransportPolicy: rtc.iceTransportPolicy,
  })
  const channel = peer.createDataChannel('windshare-network-matrix')
  ;(globalThis as typeof globalThis & {
    __windshareNetworkMatrix?: { peer: RTCPeerConnection; channel: RTCDataChannel }
  }).__windshareNetworkMatrix = { peer, channel }
  const offer = await peer.createOffer()
  await peer.setLocalDescription(offer)
  if (peer.iceGatheringState !== 'complete') {
    await new Promise<void>((resolve) => {
      peer.addEventListener('icegatheringstatechange', () => {
        if (peer.iceGatheringState === 'complete') resolve()
      })
    })
  }
  const local = peer.localDescription
  if (local === null || local.sdp === '') throw new Error('browser offer is unavailable')
  return local.sdp
}

async function acceptAnswerInPage(sdp: string): Promise<void> {
  const state = (globalThis as typeof globalThis & {
    __windshareNetworkMatrix?: { peer: RTCPeerConnection }
  }).__windshareNetworkMatrix
  if (state === undefined) throw new Error('browser peer is unavailable')
  await state.peer.setRemoteDescription({ type: 'answer', sdp })
}

function exchangeChallengeInPage(input: {
  readonly expected: string
  readonly timeoutMs: number
}): Promise<string> {
  return new Promise<string>((resolve, reject) => {
    const state = (globalThis as typeof globalThis & {
      __windshareNetworkMatrix?: { channel: RTCDataChannel }
    }).__windshareNetworkMatrix
    if (state === undefined) {
      reject(new Error('browser data channel is unavailable'))
      return
    }
    const timeout = setTimeout(
      () => reject(new Error('browser challenge deadline exceeded')),
      input.timeoutMs,
    )
    const receive = (event: MessageEvent<unknown>): void => {
      clearTimeout(timeout)
      if (event.data !== input.expected) {
        reject(new Error('browser challenge echo was not exact'))
        return
      }
      resolve(event.data)
    }
    state.channel.addEventListener('message', receive, { once: true })
    const send = (): void => state.channel.send(input.expected)
    if (state.channel.readyState === 'open') send()
    else state.channel.addEventListener('open', send, { once: true })
  })
}

async function getStatsInPage(): Promise<unknown[]> {
  const state = (globalThis as typeof globalThis & {
    __windshareNetworkMatrix?: { peer: RTCPeerConnection }
  }).__windshareNetworkMatrix
  if (state === undefined) throw new Error('browser peer is unavailable')
  const records: unknown[] = []
  const report = await state.peer.getStats()
  report.forEach((record) => {
    assertNoSensitiveStatsFields(record)
    const projected = projectPublicStatsRecord(record)
    if (projected !== null) records.push(projected)
  })
  return records

  function assertNoSensitiveStatsFields(value: unknown): void {
    const pending: unknown[] = [value]
    const seen = new WeakSet<object>()
    while (pending.length !== 0) {
      const current = pending.pop()
      if (typeof current !== 'object' || current === null || seen.has(current)) continue
      seen.add(current)
      for (const [key, nested] of Object.entries(current)) {
        const normalized = key.replace(/[^a-z]/giu, '').toLowerCase()
        if (
          normalized.includes('authorization') || normalized.includes('credential') ||
          normalized.includes('password') || normalized.includes('secret') ||
          normalized.includes('token')
        ) throw new Error('browser stats contained a prohibited private field')
        pending.push(nested)
      }
    }
  }

  function projectPublicStatsRecord(value: RTCStats): Record<string, unknown> | null {
    const record = value as RTCStats & Record<string, unknown>
    const nullable = (field: string): unknown => record[field] ?? null
    if (record.type === 'transport') {
      return {
        id: record.id,
        type: record.type,
        selectedCandidatePairId: nullable('selectedCandidatePairId'),
      }
    }
    if (record.type === 'candidate-pair') {
      return {
        id: record.id,
        type: record.type,
        localCandidateId: nullable('localCandidateId'),
        remoteCandidateId: nullable('remoteCandidateId'),
        selected: nullable('selected'),
        nominated: nullable('nominated'),
        state: nullable('state'),
        protocol: nullable('protocol'),
      }
    }
    if (record.type === 'local-candidate' || record.type === 'remote-candidate') {
      return {
        id: record.id,
        type: record.type,
        candidateType: nullable('candidateType'),
        address: nullable('address'),
        ip: nullable('ip'),
        port: nullable('port'),
        protocol: nullable('protocol'),
      }
    }
    return null
  }
}

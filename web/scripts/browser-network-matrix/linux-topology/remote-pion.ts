import { createHash, createPublicKey, timingSafeEqual } from 'node:crypto'
import { request as httpsRequest, type RequestOptions } from 'node:https'
import { networkInterfaces } from 'node:os'
import { checkServerIdentity, type TLSSocket } from 'node:tls'

import type {
  ExternalFixtureProbeResult,
  NetworkMatrixRtcConfiguration,
  SampleExternalFixtureProbe,
} from '../runtime-authority.ts'
import type { NetworkMatrixOwnedOperation } from '../owned-operation.ts'
import type { NetworkMatrixProfileId } from '../vocabulary.ts'
import {
  NETWORK_MATRIX_ATTEMPT_REQUEST_AUTHORITY_SCHEMA,
  networkMatrixAttemptAuthoritySha256,
  parseNetworkMatrixAttemptAuthority,
  parseNetworkMatrixControlAuthority,
  parseNetworkMatrixFixtureAuthorityBinding,
  parseNetworkMatrixSampleAuthority,
  sameNetworkMatrixAttemptAuthority,
  sameNetworkMatrixControlAuthority,
  sameNetworkMatrixSampleAuthority,
  type NetworkMatrixAttemptAuthority,
  type NetworkMatrixAttemptRequestAuthority,
  type NetworkMatrixControlAuthority,
  type NetworkMatrixFixtureAuthorityBinding,
  type NetworkMatrixSampleAuthority,
} from '../sample-authority.ts'
import {
  ExternalFixtureAttestationError,
  EXTERNAL_FIXTURE_CONTROL_PROTOCOL,
  isGlobalUnicastIpv4,
  parseExternalFixtureTurnCredential,
  parseSignedExternalFixtureAttestation,
  verifyExternalFixtureAttestation,
  type ManualOperatorTopologyIdentity,
  type VerifiedExternalFixtureAuthority,
} from './external-fixture-attestation.ts'
import { externalFixtureRtcConfiguration } from './external-fixture-rtc.ts'
import {
  authenticateSignedExternalFixtureTerminalReceipt,
  EXTERNAL_FIXTURE_MAXIMUM_ATTEMPT_LEASE_MS,
  parseSignedExternalFixtureTerminalReceipt,
  type SignedExternalFixtureTerminalReceipt,
} from './external-fixture-terminal-receipt.ts'

export const REMOTE_PION_PROTOCOL_VERSION = EXTERNAL_FIXTURE_CONTROL_PROTOCOL
export const REMOTE_PION_REQUEST_DEADLINE_MS = 15_000
export const REMOTE_PION_ATTESTATION_LEASE_MS = 120_000
export const REMOTE_PION_MAXIMUM_ATTEMPT_LEASE_MS = EXTERNAL_FIXTURE_MAXIMUM_ATTEMPT_LEASE_MS
const MAXIMUM_RESPONSE_BYTES = 1_048_576
const SHA256_PATTERN = /^[a-f0-9]{64}$/u
const OPAQUE_ID_PATTERN = /^[A-Za-z0-9_-]{16,128}$/u
const CANONICAL_ID_PATTERN = /^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/u
const MINIMUM_CONTROL_CREDENTIAL_BYTES = 32
const MAXIMUM_CONTROL_CREDENTIAL_BYTES = 512
const CONTROL_LEASE_ID_HEADER = 'X-WindShare-Control-Lease-ID'
const CANONICAL_UTC_TIMESTAMP_PATTERN =
  /^\d{4}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01])T(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d\.\d{3}Z$/u
const CREATE_RECOVERY_STATUS_CODES = Object.freeze([200, 201] as const)
const REMOTE_PION_MAXIMUM_CLOCK_SKEW_MS = 30_000

export interface RemotePionControlOptions {
  readonly controllerOrigin: string
  readonly tlsCertificateAuthority: string | Buffer
  readonly tlsCertificateSha256: string
  readonly attestationPublicKey: string | Buffer
  /** The caller retains the only alias and erases these bytes when the lease retires. */
  readonly controlCredential: Uint8Array
  readonly controlLease: RemotePionControlLeaseBinding
  readonly manualOperatorIdentity?: ManualOperatorTopologyIdentity
  readonly request?: RemotePionRequest
  readonly now?: () => number
  readonly localInterfaceAddresses?: () => readonly string[]
}

export interface RemotePionControlLeaseBinding {
  readonly controlAuthority: NetworkMatrixControlAuthority
  readonly probeNonce: string
  readonly authorityInstanceId: string
  readonly attestationSha256: string
  readonly issuedAt: string
  readonly expiresAt: string
  readonly maxAttempts: 1
}

export interface RemotePionHttpRequest {
  readonly endpoint: URL
  readonly path: string
  readonly body: string | null
  readonly method: 'GET' | 'POST' | 'DELETE'
  readonly controlCredential: Uint8Array
  readonly controlLeaseId: string
  readonly tlsServerName: string
  readonly tlsCertificateAuthority: string | Buffer
  readonly tlsCertificateSha256: string
  readonly signal: AbortSignal
}

export interface RemotePionHttpResponse {
  readonly statusCode: number
  readonly body: string
  readonly observedRemoteAddress: string
  readonly observedTlsCertificateSha256: string
}

export type RemotePionRequest = (
  request: RemotePionHttpRequest,
) => Promise<RemotePionHttpResponse>

export interface RemotePionAuthorityBinding {
  readonly controlAuthority: NetworkMatrixControlAuthority
  readonly fixtureBinding: NetworkMatrixFixtureAuthorityBinding
}

export interface RemotePionAttemptLease {
  readonly attemptAuthority: NetworkMatrixAttemptAuthority
  readonly leaseIssuedAt: string
  readonly leaseExpiresAt: string
  readonly leaseMillis: number
}

export interface RemotePionAttemptResult {
  readonly attemptAuthority: NetworkMatrixAttemptAuthority
  readonly state: 'pending' | 'established' | 'failed'
  readonly selectedPair: unknown | null
  readonly challengeBindingSha256: string
  readonly failureCode: string | null
  readonly terminalReceipt: SignedExternalFixtureTerminalReceipt | null
}

interface LiveAuthorityState extends RemotePionAuthorityBinding {
  readonly profileId: NetworkMatrixProfileId
  readonly authorityInstanceId: string
  readonly controllerPublicIp: string
  readonly issuedAt: string
  readonly expiresAt: string
  readonly verified: VerifiedExternalFixtureAuthority
  readonly rtcConfiguration: NetworkMatrixRtcConfiguration | null
  readonly attestationPublicKeySpki: string
  readonly signedAttestation: ReturnType<typeof parseSignedExternalFixtureAttestation>
}

interface AttemptAuthorityState extends RemotePionAuthorityBinding {
  readonly profileId: NetworkMatrixProfileId
  readonly attemptAuthority: NetworkMatrixAttemptAuthority
  readonly leaseIssuedAt: string
  readonly leaseExpiresAt: string
  readonly leaseMillis: number
  readonly authorityIssuedAt: string
  readonly authorityExpiresAt: string
}

interface RemotePionCallResult {
  readonly value: Record<string, unknown>
  readonly observedRemoteAddress: string
  readonly observedTlsCertificateSha256: string
}

export class RemotePionContainmentError extends Error {
  constructor() {
    super('remote Pion lease cleanup did not prove terminal reaping')
    this.name = 'RemotePionContainmentError'
  }
}

export class RemotePionTransportUnavailableError extends Error {
  constructor() {
    super('remote Pion control transport is unreachable')
    this.name = 'RemotePionTransportUnavailableError'
  }
}

export class RemotePionAuthorityExpiredError extends Error {
  constructor() {
    super('remote Pion signed authority or attempt lease expired')
    this.name = 'RemotePionAuthorityExpiredError'
  }
}

export class RemotePionProtocolError extends Error {
  readonly failureCode: 'proof-invalid' | 'authority-binding-mismatch'

  constructor(
    failureCode: 'proof-invalid' | 'authority-binding-mismatch',
    message: string,
  ) {
    super(message)
    this.name = 'RemotePionProtocolError'
    this.failureCode = failureCode
  }
}

class RemotePionUnexpectedStatusError extends Error {
  readonly statusCode: number

  constructor(statusCode: number) {
    super('remote Pion control response has an unexpected status')
    this.name = 'RemotePionUnexpectedStatusError'
    this.statusCode = statusCode
  }
}

class RemotePionOperationAbortedError extends Error {
  constructor() {
    super('remote Pion control operation was aborted')
    this.name = 'RemotePionOperationAbortedError'
  }
}

export class RemotePionControlClient implements SampleExternalFixtureProbe {
  readonly #endpoint: URL
  readonly #tlsServerName: string
  readonly #ca: string | Buffer
  readonly #certificateSha256: string
  readonly #attestationPublicKey: string | Buffer
  readonly #attestationPublicKeySpki: string
  readonly #credential: Uint8Array
  readonly #controlLease: RemotePionControlLeaseBinding
  readonly #deadlineMs: number
  readonly #attestationLeaseMs: number
  readonly #manualOperatorIdentity: ManualOperatorTopologyIdentity | undefined
  readonly #request: RemotePionRequest
  readonly #now: () => number
  readonly #localInterfaceAddresses: () => readonly string[]
  readonly #liveAuthorityByRun = new Map<string, LiveAuthorityState>()
  readonly #authorityByAttempt = new Map<string, AttemptAuthorityState>()
  readonly #probeTailByRun = new Map<string, Promise<void>>()
  #attemptIssued = false

  constructor(options: RemotePionControlOptions) {
    const endpoint = new URL(options.controllerOrigin)
    if (
      endpoint.protocol !== 'https:' || endpoint.username !== '' || endpoint.password !== '' ||
      endpoint.search !== '' || endpoint.hash !== '' || endpoint.pathname !== '/' ||
      options.controllerOrigin !== `${endpoint.origin}/` || endpoint.hostname.includes(':')
    ) throw new Error('remote Pion control endpoint must be a canonical HTTPS origin')
    if (
      !SHA256_PATTERN.test(options.tlsCertificateSha256) ||
      !isControlCredentialBytes(options.controlCredential) ||
      !validControlLeaseBinding(options.controlLease)
    ) throw new Error('remote Pion control trust authority is incomplete')
    this.#endpoint = endpoint
    this.#tlsServerName = endpoint.hostname
    this.#ca = options.tlsCertificateAuthority
    this.#certificateSha256 = options.tlsCertificateSha256
    let attestationPublicKey: ReturnType<typeof createPublicKey>
    try {
      attestationPublicKey = createPublicKey(options.attestationPublicKey)
    } catch {
      throw new Error('remote Pion attestation public key is invalid')
    }
    if (attestationPublicKey.asymmetricKeyType !== 'ed25519') {
      throw new Error('remote Pion attestation public key must be Ed25519')
    }
    this.#attestationPublicKey = options.attestationPublicKey
    this.#attestationPublicKeySpki = Buffer.from(attestationPublicKey.export({
      type: 'spki',
      format: 'der',
    })).toString('base64url')
    this.#credential = options.controlCredential
    this.#controlLease = Object.freeze({
      ...options.controlLease,
      controlAuthority: parseNetworkMatrixControlAuthority(options.controlLease.controlAuthority),
    })
    this.#deadlineMs = REMOTE_PION_REQUEST_DEADLINE_MS
    this.#attestationLeaseMs = REMOTE_PION_ATTESTATION_LEASE_MS
    this.#manualOperatorIdentity = options.manualOperatorIdentity
    this.#request = options.request ?? executeRemotePionRequest
    this.#now = options.now ?? Date.now
    this.#localInterfaceAddresses = options.localInterfaceAddresses ?? systemInterfaceAddresses
  }

  probe(input: {
    readonly sampleAuthority: NetworkMatrixSampleAuthority
    readonly signal: AbortSignal
  }): NetworkMatrixOwnedOperation<ExternalFixtureProbeResult> {
    const controller = new AbortController()
    const abort = (): void => controller.abort()
    input.signal.addEventListener('abort', abort, { once: true })
    if (input.signal.aborted) controller.abort()
    const result = this.#serializeProbe(input, controller.signal).finally(() => {
      input.signal.removeEventListener('abort', abort)
    })
    return Object.freeze({
      result,
      forceTerminateAndWait: async (): Promise<void> => {
        controller.abort()
        await result.catch(() => undefined)
      },
    })
  }

  async #serializeProbe(
    input: { readonly sampleAuthority: NetworkMatrixSampleAuthority },
    signal: AbortSignal,
  ): Promise<ExternalFixtureProbeResult> {
    const runId = input.sampleAuthority.runId
    const predecessor = this.#probeTailByRun.get(runId) ?? Promise.resolve()
    let release: () => void = () => {}
    const held = new Promise<void>((resolve) => { release = resolve })
    const tail = predecessor.catch(() => undefined).then(() => held)
    this.#probeTailByRun.set(runId, tail)
    await predecessor.catch(() => undefined)
    try {
      if (signal.aborted) throw new RemotePionOperationAbortedError()
      return await this.#probe(input, signal)
    } finally {
      release()
      if (this.#probeTailByRun.get(runId) === tail) {
        this.#probeTailByRun.delete(runId)
      }
    }
  }

  async createAttempt(
    requestId: string,
    leaseMillis: number,
    signal: AbortSignal,
  ): Promise<RemotePionAttemptLease> {
    if (
      this.#attemptIssued ||
      !OPAQUE_ID_PATTERN.test(requestId) ||
      !Number.isSafeInteger(leaseMillis) || leaseMillis < 1 ||
      leaseMillis > REMOTE_PION_MAXIMUM_ATTEMPT_LEASE_MS
    ) throw new Error('remote Pion attempt request is invalid')
    this.#attemptIssued = true
    const authority = this.#requireLiveAuthority(
      this.#controlLease.controlAuthority.sampleAuthority.runId,
    )
    let lease: RemotePionAttemptLease
    try {
      lease = await this.#requestAttempt(authority, requestId, leaseMillis, signal, 201)
    } catch (cause) {
      if (cause instanceof RemotePionAuthorityExpiredError) throw cause
      await this.#recoverAndReapAmbiguousCreate(authority, requestId, leaseMillis)
      throw new Error('remote Pion attempt creation did not publish a bound response', { cause })
    }
    const attemptId = lease.attemptAuthority.attemptId
    if (this.#authorityByAttempt.has(attemptId)) {
      try {
        await this.#deleteAttemptAuthority(lease, new AbortController().signal)
      } catch {
        throw new RemotePionContainmentError()
      }
      this.#authorityByAttempt.delete(attemptId)
      throw new RemotePionProtocolError(
        'authority-binding-mismatch',
        'remote Pion attempt response repeats a live attempt authority',
      )
    }
    this.#authorityByAttempt.set(attemptId, Object.freeze({
      ...authorityBindingFromAttempt(lease.attemptAuthority),
      profileId: authority.profileId,
      attemptAuthority: lease.attemptAuthority,
      leaseIssuedAt: lease.leaseIssuedAt,
      leaseExpiresAt: lease.leaseExpiresAt,
      leaseMillis: lease.leaseMillis,
      authorityIssuedAt: authority.issuedAt,
      authorityExpiresAt: authority.expiresAt,
    }))
    return lease
  }

  async #requestAttempt(
    authority: LiveAuthorityState,
    requestId: string,
    leaseMillis: number,
    signal: AbortSignal,
    expectedStatus: number | readonly number[],
  ): Promise<RemotePionAttemptLease> {
    const requestedAt = this.#now()
    if (requestedAt + leaseMillis > Date.parse(authority.expiresAt)) {
      throw new RemotePionAuthorityExpiredError()
    }
    const requestAuthority: NetworkMatrixAttemptRequestAuthority = Object.freeze({
      schemaVersion: NETWORK_MATRIX_ATTEMPT_REQUEST_AUTHORITY_SCHEMA,
      controlAuthority: authority.controlAuthority,
      requestId,
      fixtureBinding: authority.fixtureBinding,
    })
    const call = await this.#post('/v2/attempts', {
      protocolVersion: REMOTE_PION_PROTOCOL_VERSION,
      requestAuthority,
      leaseMillis,
    }, signal, expectedStatus, authority)
    const value = call.value
    requireExactKeys(value, [
      'protocolVersion', 'attemptAuthority', 'leaseIssuedAt', 'leaseExpiresAt', 'leaseMillis',
    ])
    const attemptAuthority = parseAttemptAuthority(value.attemptAuthority)
    const leaseIssuedAt = stringField(value, 'leaseIssuedAt')
    const leaseExpiresAt = stringField(value, 'leaseExpiresAt')
    const actualLeaseMillis = value.leaseMillis
    const leaseIssued = canonicalUtcTimestamp(leaseIssuedAt)
    const leaseExpires = canonicalUtcTimestamp(leaseExpiresAt)
    const observedAt = this.#now()
    if (
      value.protocolVersion !== REMOTE_PION_PROTOCOL_VERSION ||
      JSON.stringify(attemptAuthority.requestAuthority) !== JSON.stringify(requestAuthority) ||
      !Number.isSafeInteger(actualLeaseMillis) ||
      (actualLeaseMillis as number) < 1 || (actualLeaseMillis as number) > leaseMillis ||
      leaseExpires - leaseIssued !== actualLeaseMillis || leaseExpires <= observedAt ||
      leaseIssued < Date.parse(authority.issuedAt) ||
      leaseIssued > observedAt + REMOTE_PION_MAXIMUM_CLOCK_SKEW_MS ||
      leaseIssued < requestedAt - REMOTE_PION_MAXIMUM_CLOCK_SKEW_MS ||
      leaseExpires > Date.parse(authority.expiresAt)
    ) throw protocolInvalid('remote Pion attempt response is not bound to its request')
    return Object.freeze({
      attemptAuthority,
      leaseIssuedAt,
      leaseExpiresAt,
      leaseMillis: actualLeaseMillis as number,
    })
  }

  async #recoverAndReapAmbiguousCreate(
    authority: LiveAuthorityState,
    requestId: string,
    leaseMillis: number,
  ): Promise<void> {
    try {
      const lease = await this.#requestAttempt(
        authority,
        requestId,
        leaseMillis,
        new AbortController().signal,
        CREATE_RECOVERY_STATUS_CODES,
      )
      await this.#deleteAttemptAuthority(lease, new AbortController().signal)
      this.#authorityByAttempt.delete(lease.attemptAuthority.attemptId)
    } catch {
      throw new RemotePionContainmentError()
    }
  }

  async offer(attemptId: string, sdp: string, signal: AbortSignal): Promise<string> {
    const authority = this.#requireAttemptAuthority(attemptId, false)
    if (typeof sdp !== 'string' || sdp === '' || sdp.includes('\0')) {
      throw new Error('remote Pion SDP offer is invalid')
    }
    const call = await this.#post(`/v2/attempts/${attemptId}/offer`, {
      protocolVersion: REMOTE_PION_PROTOCOL_VERSION,
      attemptAuthority: authority.attemptAuthority,
      type: 'offer',
      sdp,
    }, signal, 200, authorityBindingFromAttempt(authority.attemptAuthority))
    const value = call.value
    requireExactKeys(value, [
      'protocolVersion', 'attemptAuthority', 'type', 'sdp',
    ])
    const responseAuthority = parseAttemptAuthority(value.attemptAuthority)
    if (
      value.protocolVersion !== REMOTE_PION_PROTOCOL_VERSION ||
      !sameNetworkMatrixAttemptAuthority(responseAuthority, authority.attemptAuthority) ||
      value.type !== 'answer'
    ) throw protocolInvalid('remote Pion answer is not bound to its attempt')
    const answer = stringField(value, 'sdp')
    if (answer === '' || answer.includes('\0')) throw protocolInvalid('remote Pion answer is invalid')
    return answer
  }

  async result(attemptId: string, signal: AbortSignal): Promise<RemotePionAttemptResult> {
    const authority = this.#requireAttemptAuthority(attemptId, false)
    const call = await this.#call(
      `/v2/attempts/${attemptId}`,
      null,
      signal,
      200,
      'GET',
      authorityBindingFromAttempt(authority.attemptAuthority),
    )
    return parseRemotePionAttemptResult(call.value, authority, this.#attestationPublicKey)
  }

  async deleteAttempt(attemptId: string, signal: AbortSignal): Promise<void> {
    const authority = this.#requireAttemptAuthority(attemptId, true)
    try {
      await this.#deleteAttemptAuthority(authority, signal)
    } catch {
      throw new RemotePionContainmentError()
    }
    this.#authorityByAttempt.delete(attemptId)
    this.#retireExpiredLiveAuthority(authority.controlAuthority.sampleAuthority.runId)
  }

  async #deleteAttemptAuthority(
    authority: { readonly attemptAuthority: NetworkMatrixAttemptAuthority },
    signal: AbortSignal,
  ): Promise<void> {
    const attemptId = authority.attemptAuthority.attemptId
    const call = await this.#call(
      `/v2/attempts/${attemptId}`,
      null,
      signal,
      200,
      'DELETE',
      authorityBindingFromAttempt(authority.attemptAuthority),
    )
    const value = call.value
    requireExactKeys(value, [
      'protocolVersion', 'attemptAuthority', 'terminal',
    ])
    const responseAuthority = parseAttemptAuthority(value.attemptAuthority)
    if (
      value.protocolVersion !== REMOTE_PION_PROTOCOL_VERSION ||
      !sameNetworkMatrixAttemptAuthority(responseAuthority, authority.attemptAuthority) ||
      value.terminal !== 'reaped'
    ) throw new RemotePionContainmentError()
  }

  async #probe(
    input: {
      readonly sampleAuthority: NetworkMatrixSampleAuthority
    },
    signal: AbortSignal,
  ): Promise<ExternalFixtureProbeResult> {
    let sampleAuthority: NetworkMatrixSampleAuthority
    try {
      sampleAuthority = parseNetworkMatrixSampleAuthority(input.sampleAuthority)
    } catch {
      return Object.freeze({ outcome: 'invalid', failureCode: 'proof-invalid' })
    }
    const runId = sampleAuthority.runId
    const profileId = sampleAuthority.profileId
    try {
      if (
        !sameNetworkMatrixSampleAuthority(
          sampleAuthority,
          this.#controlLease.controlAuthority.sampleAuthority,
        ) ||
        Date.parse(this.#controlLease.expiresAt) <= this.#now()
      ) throw new RemotePionProtocolError(
        'authority-binding-mismatch',
        'remote Pion probe crossed its control credential scope',
      )
      const reusable = await this.#prepareRunForProbe({ runId, profileId })
      if (reusable !== null) return fixtureProbeResult(reusable)
      const nonce = this.#controlLease.probeNonce
      if (!OPAQUE_ID_PATTERN.test(nonce)) {
        return Object.freeze({ outcome: 'failed', failureCode: 'authority-probe-failed' })
      }
      const call = await this.#post('/v2/authority-probe', {
        protocolVersion: REMOTE_PION_PROTOCOL_VERSION,
        controlAuthority: this.#controlLease.controlAuthority,
        nonce,
        requestedLeaseMillis: this.#attestationLeaseMs,
      }, signal, 200)
      const signed = parseSignedExternalFixtureAttestation(call.value, REMOTE_PION_PROTOCOL_VERSION)
      const observedRemoteAddress = requireObservedControllerAddress(
        call,
        this.#certificateSha256,
        this.#localInterfaceAddresses(),
      )
      const verified = verifyExternalFixtureAttestation(signed, {
        protocolVersion: REMOTE_PION_PROTOCOL_VERSION,
        profileId,
        runId,
        nonce,
        requestedLeaseMillis: this.#attestationLeaseMs,
        controllerOrigin: this.#endpoint.toString(),
        tlsCertificateSha256: this.#certificateSha256,
        observedControllerIp: observedRemoteAddress,
        observedTlsCertificateSha256: call.observedTlsCertificateSha256,
        attestationPublicKey: this.#attestationPublicKey,
        ...(this.#manualOperatorIdentity === undefined
          ? {}
          : { manualOperatorIdentity: this.#manualOperatorIdentity }),
        now: this.#now,
      })
      const fixture = verified.attestation.fixture
      if (
        fixture.authorityInstanceId !== this.#controlLease.authorityInstanceId ||
        verified.attestationSha256 !== this.#controlLease.attestationSha256 ||
        Date.parse(this.#controlLease.expiresAt) > Date.parse(verified.attestation.expiresAt)
      ) throw new RemotePionProtocolError(
        'authority-binding-mismatch',
        'remote Pion attestation differs from its ephemeral control credential lease',
      )
      const binding: RemotePionAuthorityBinding = Object.freeze({
        controlAuthority: this.#controlLease.controlAuthority,
        fixtureBinding: Object.freeze({
          attestationSha256: verified.attestationSha256,
          authorityInstanceId: fixture.authorityInstanceId,
          remoteServiceInstanceId: fixture.remoteServiceInstanceId,
          networkBindingSha256: verified.networkBindingSha256,
          remotePeerBindingSha256: verified.remotePeerBindingSha256,
        }),
      })
      const state: LiveAuthorityState = Object.freeze({
        ...binding,
        profileId,
        authorityInstanceId: fixture.authorityInstanceId,
        controllerPublicIp: observedRemoteAddress,
        issuedAt: verified.attestation.issuedAt,
        expiresAt: verified.attestation.expiresAt,
        verified,
        rtcConfiguration: null,
        attestationPublicKeySpki: this.#attestationPublicKeySpki,
        signedAttestation: signed,
      })
      this.#liveAuthorityByRun.set(runId, state)
      let rtcConfiguration: NetworkMatrixRtcConfiguration
      try {
        const turnCredential = fixture.networkSemantics.kind === 'coturn-relay'
          ? await this.#turnCredential(binding, verified, signal)
          : null
        rtcConfiguration = externalFixtureRtcConfiguration(
          fixture,
          turnCredential,
          this.#now,
        )
        if (verified.activeStunEndpoint !== null) {
          await this.#activeStunProbe(binding, verified, nonce, signal)
        }
      } catch (cause) {
        if (this.#liveAuthorityByRun.get(runId) === state) {
          this.#liveAuthorityByRun.delete(runId)
        }
        throw cause
      }
      if (this.#liveAuthorityByRun.get(runId) !== state) {
        throw new RemotePionContainmentError()
      }
      const ready = Object.freeze({ ...state, rtcConfiguration })
      this.#liveAuthorityByRun.set(runId, ready)
      return fixtureProbeResult(ready)
    } catch (cause) {
      if (cause instanceof RemotePionContainmentError) throw cause
      return classifyProbeFailure(cause, signal)
    }
  }

  async #turnCredential(
    binding: RemotePionAuthorityBinding,
    verified: VerifiedExternalFixtureAuthority,
    signal: AbortSignal,
  ) {
    const call = await this.#post('/v2/turn-credential', {
      protocolVersion: REMOTE_PION_PROTOCOL_VERSION,
      ...attemptBinding(binding),
    }, signal, 200, binding)
    return parseExternalFixtureTurnCredential(call.value, {
      protocolVersion: REMOTE_PION_PROTOCOL_VERSION,
      ...attemptBinding(binding),
      fixture: verified.attestation.fixture,
      now: this.#now,
    })
  }

  async #activeStunProbe(
    binding: RemotePionAuthorityBinding,
    verified: VerifiedExternalFixtureAuthority,
    nonce: string,
    signal: AbortSignal,
  ): Promise<void> {
    const call = await this.#post('/v2/stun-probe', {
      protocolVersion: REMOTE_PION_PROTOCOL_VERSION,
      ...attemptBinding(binding),
      nonce,
      stunUri: verified.activeStunEndpoint,
    }, signal, 200, binding)
    const value = call.value
    requireExactKeys(value, [
      'protocolVersion', 'controlAuthority', 'fixtureBinding', 'nonce',
      'serverReflexiveObserved',
    ])
    requireResponseBinding(value, binding)
    if (
      value.protocolVersion !== REMOTE_PION_PROTOCOL_VERSION || value.nonce !== nonce ||
      value.serverReflexiveObserved !== true
    ) throw new RemotePionProtocolError(
      'authority-binding-mismatch',
      'remote Pion STUN proof is not bound to the live fixture',
    )
  }

  async #prepareRunForProbe(input: {
    readonly runId: string
    readonly profileId: NetworkMatrixProfileId
  }): Promise<LiveAuthorityState | null> {
    const current = this.#liveAuthorityByRun.get(input.runId)
    if (
      current !== undefined && current.profileId === input.profileId &&
      current.rtcConfiguration !== null && Date.parse(current.expiresAt) > this.#now()
    ) return current

    const attempts = [...this.#authorityByAttempt.entries()]
      .filter(([, authority]) =>
        authority.controlAuthority.sampleAuthority.runId === input.runId)
    for (const [attemptId, authority] of attempts) {
      try {
        await this.#deleteAttemptAuthority(
          authority,
          new AbortController().signal,
        )
      } catch {
        throw new RemotePionContainmentError()
      }
      this.#authorityByAttempt.delete(attemptId)
    }
    this.#liveAuthorityByRun.delete(input.runId)
    return null
  }

  #requireLiveAuthority(runId: string): LiveAuthorityState {
    const authority = this.#liveAuthorityByRun.get(runId)
    if (authority === undefined) throw new Error('remote Pion run has no verified live authority')
    if (Date.parse(authority.expiresAt) <= this.#now()) {
      this.#retireExpiredLiveAuthority(runId)
      throw new RemotePionAuthorityExpiredError()
    }
    if (authority.rtcConfiguration === null) throw new Error('remote Pion run authority is not ready')
    return authority
  }

  #requireAttemptAuthority(attemptId: string, permitExpired: boolean): AttemptAuthorityState {
    requireAttemptId(attemptId)
    const authority = this.#authorityByAttempt.get(attemptId)
    if (authority === undefined) throw new Error('remote Pion attempt has no issued authority')
    if (
      !permitExpired &&
      (Date.parse(authority.leaseExpiresAt) <= this.#now() ||
        Date.parse(authority.authorityExpiresAt) <= this.#now())
    ) throw new RemotePionAuthorityExpiredError()
    return authority
  }

  #retireExpiredLiveAuthority(runId: string): void {
    const hasAttempts = [...this.#authorityByAttempt.values()]
      .some((authority) => authority.controlAuthority.sampleAuthority.runId === runId)
    if (!hasAttempts) this.#liveAuthorityByRun.delete(runId)
  }

  #post(
    path: string,
    body: Readonly<Record<string, unknown>>,
    signal: AbortSignal,
    expectedStatus: number | readonly number[],
    authority?: RemotePionAuthorityBinding,
  ): Promise<RemotePionCallResult> {
    return this.#call(path, JSON.stringify(body), signal, expectedStatus, 'POST', authority)
  }

  async #call(
    path: string,
    body: string | null,
    outerSignal: AbortSignal,
    expectedStatus: number | readonly number[],
    method: 'GET' | 'POST' | 'DELETE',
    authority?: RemotePionAuthorityBinding,
  ): Promise<RemotePionCallResult> {
    if (!isControlCredentialBytes(this.#credential)) {
      throw protocolInvalid('remote Pion control credential authority is invalid')
    }
    const controller = new AbortController()
    let timedOut = false
    const abort = (): void => controller.abort()
    outerSignal.addEventListener('abort', abort, { once: true })
    if (outerSignal.aborted) controller.abort()
    const timeout = setTimeout(() => {
      timedOut = true
      controller.abort()
    }, this.#deadlineMs)
    try {
      let response: RemotePionHttpResponse
      try {
        response = await this.#request({
          endpoint: this.#endpoint,
          path,
          body,
          method,
          controlCredential: this.#credential,
          controlLeaseId: this.#controlLease.controlAuthority.controlLeaseId,
          tlsServerName: this.#tlsServerName,
          tlsCertificateAuthority: this.#ca,
          tlsCertificateSha256: this.#certificateSha256,
          signal: controller.signal,
        })
      } catch (cause) {
        if (outerSignal.aborted) throw new RemotePionOperationAbortedError()
        if (timedOut) throw new RemotePionTransportUnavailableError()
        throw cause
      }
      if (outerSignal.aborted) throw new RemotePionOperationAbortedError()
      if (timedOut || controller.signal.aborted) throw new RemotePionTransportUnavailableError()
      requireRemotePionStatus(response.statusCode, expectedStatus, authority !== undefined)
      const observedRemoteAddress = requireObservedControllerAddress(
        response,
        this.#certificateSha256,
        this.#localInterfaceAddresses(),
      )
      requireRemotePionControllerAuthority(
        this.#liveAuthorityByRun,
        authority,
        observedRemoteAddress,
      )
      return Object.freeze({
        value: parseCanonicalObject(response.body),
        observedRemoteAddress,
        observedTlsCertificateSha256: response.observedTlsCertificateSha256,
      })
    } finally {
      clearTimeout(timeout)
      outerSignal.removeEventListener('abort', abort)
    }
  }
}

function parseRemotePionAttemptResult(
  value: Record<string, unknown>,
  authority: AttemptAuthorityState,
  attestationPublicKey: string | Buffer,
): RemotePionAttemptResult {
  requireExactKeys(value, [
    'protocolVersion', 'attemptAuthority', 'state',
    'selectedPair', 'challengeBindingSha256', 'failureCode', 'terminalReceipt',
  ])
  const responseAuthority = parseAttemptAuthority(value.attemptAuthority)
  if (value.protocolVersion !== REMOTE_PION_PROTOCOL_VERSION) {
    throw protocolInvalid('remote Pion result is not bound to its attempt')
  }
  if (!sameNetworkMatrixAttemptAuthority(responseAuthority, authority.attemptAuthority)) {
    throw new RemotePionProtocolError(
      'authority-binding-mismatch',
      'remote Pion result crossed its issued attempt authority',
    )
  }
  const state = parseRemotePionResultState(value.state)
  const failureCode = parseRemotePionFailureCode(value.failureCode)
  const challengeBindingSha256 = stringField(value, 'challengeBindingSha256')
  if (
    !SHA256_PATTERN.test(challengeBindingSha256) ||
    challengeBindingSha256 !== remotePionChallengeBindingSha256(authority.attemptAuthority)
  ) throw protocolInvalid('remote Pion result is not bound to its application challenge')
  const terminalReceipt = value.terminalReceipt === null
    ? null
    : parseSignedExternalFixtureTerminalReceipt(value.terminalReceipt)
  const selectedPair = value.selectedPair ?? null
  requireValidRemotePionTerminalReceipt({
    authority,
    attestationPublicKey,
    state,
    selectedPair,
    challengeBindingSha256,
    failureCode,
    terminalReceipt,
  })
  return Object.freeze({
    attemptAuthority: authority.attemptAuthority,
    state,
    selectedPair,
    challengeBindingSha256,
    failureCode,
    terminalReceipt,
  })
}

function parseRemotePionResultState(value: unknown): RemotePionAttemptResult['state'] {
  if (value !== 'pending' && value !== 'established' && value !== 'failed') {
    throw protocolInvalid('remote Pion result state is invalid')
  }
  return value
}

function parseRemotePionFailureCode(value: unknown): string | null {
  if (value !== null && typeof value !== 'string') {
    throw protocolInvalid('remote Pion result failure is invalid')
  }
  return value
}

function requireValidRemotePionTerminalReceipt(input: {
  readonly authority: AttemptAuthorityState
  readonly attestationPublicKey: string | Buffer
  readonly state: RemotePionAttemptResult['state']
  readonly selectedPair: unknown | null
  readonly challengeBindingSha256: string
  readonly failureCode: string | null
  readonly terminalReceipt: SignedExternalFixtureTerminalReceipt | null
}): void {
  if (input.state === 'pending') {
    if (input.terminalReceipt !== null) {
      throw protocolInvalid('pending remote Pion result cannot carry a terminal receipt')
    }
    return
  }
  if (input.terminalReceipt === null) {
    throw protocolInvalid('terminal remote Pion result lacks a signed receipt')
  }
  const receipt = authenticateSignedExternalFixtureTerminalReceipt(
    input.terminalReceipt,
    input.attestationPublicKey,
  )
  const terminalAt = canonicalUtcTimestamp(receipt.terminalAt)
  const attemptIssuedAt = canonicalUtcTimestamp(receipt.attemptLeaseIssuedAt)
  const attemptExpiresAt = canonicalUtcTimestamp(receipt.attemptLeaseExpiresAt)
  if (
    !sameNetworkMatrixAttemptAuthority(
      receipt.attemptAuthority,
      input.authority.attemptAuthority,
    ) || receipt.state !== input.state ||
    receipt.challengeBindingSha256 !== input.challengeBindingSha256 ||
    receipt.failureCode !== input.failureCode ||
    JSON.stringify(receipt.selectedPair) !== JSON.stringify(input.selectedPair) ||
    receipt.attemptLeaseIssuedAt !== input.authority.leaseIssuedAt ||
    receipt.attemptLeaseExpiresAt !== input.authority.leaseExpiresAt ||
    receipt.attemptLeaseMillis !== input.authority.leaseMillis ||
    attemptExpiresAt - attemptIssuedAt !== receipt.attemptLeaseMillis ||
    terminalAt < attemptIssuedAt || terminalAt >= attemptExpiresAt ||
    terminalAt < Date.parse(input.authority.authorityIssuedAt) ||
    terminalAt >= Date.parse(input.authority.authorityExpiresAt)
  ) throw protocolInvalid('remote Pion terminal receipt crossed its signed attempt authority')
}

function requireRemotePionStatus(
  statusCode: number,
  expectedStatus: number | readonly number[],
  authorityBound: boolean,
): void {
  const accepted = Array.isArray(expectedStatus)
    ? expectedStatus.includes(statusCode)
    : statusCode === expectedStatus
  if (accepted) return
  if (authorityBound && statusCode === 410) throw new RemotePionAuthorityExpiredError()
  throw new RemotePionUnexpectedStatusError(statusCode)
}

function requireRemotePionControllerAuthority(
  authorities: ReadonlyMap<string, LiveAuthorityState>,
  authority: RemotePionAuthorityBinding | undefined,
  observedRemoteAddress: string,
): void {
  if (authority === undefined) return
  const runId = authority.controlAuthority.sampleAuthority.runId
  const live = authorities.get(runId)
  if (
    live === undefined || live.controllerPublicIp !== observedRemoteAddress ||
    !sameAuthorityBinding(live, authority)
  ) throw new RemotePionProtocolError(
    'authority-binding-mismatch',
    'remote Pion request crossed its attested controller authority',
  )
}

function fixtureProbeResult(authority: LiveAuthorityState): Extract<
  ExternalFixtureProbeResult,
  { readonly outcome: 'satisfied' }
> {
  if (authority.rtcConfiguration === null) {
    throw new Error('remote Pion run authority is not ready')
  }
  return Object.freeze({
    outcome: 'satisfied',
    probeId: `probe-${authority.fixtureBinding.attestationSha256}`,
    runId: authority.controlAuthority.sampleAuthority.runId,
    profileId: authority.profileId,
    authorityInstanceId: authority.authorityInstanceId,
    remoteServiceInstanceId: authority.fixtureBinding.remoteServiceInstanceId,
    attestationSha256: authority.fixtureBinding.attestationSha256,
    attestationPublicKeySpki: authority.attestationPublicKeySpki,
    signedAttestation: authority.signedAttestation,
    networkBindingSha256: authority.fixtureBinding.networkBindingSha256,
    remotePeerBindingSha256: authority.fixtureBinding.remotePeerBindingSha256,
    controllerPublicIp: authority.controllerPublicIp,
    leaseExpiresAt: authority.expiresAt,
    rtcConfiguration: authority.rtcConfiguration,
  })
}

export function remotePionChallengeBindingSha256(
  authority: NetworkMatrixAttemptAuthority,
): string {
  return networkMatrixAttemptAuthoritySha256(parseAttemptAuthority(authority))
}

export const executeRemotePionRequest: RemotePionRequest = async (
  request,
): Promise<RemotePionHttpResponse> => new Promise((resolve, reject) => {
  if (
    !isControlCredentialBytes(request.controlCredential) ||
    !OPAQUE_ID_PATTERN.test(request.controlLeaseId)
  ) {
    reject(protocolInvalid('remote Pion control credential authority is invalid'))
    return
  }
  // Node's HTTP API requires a string header. Materializing it at this final
  // transport boundary keeps the erasable authority as bytes everywhere above it.
  const authorization = `Bearer ${new TextDecoder('ascii', { fatal: true })
    .decode(request.controlCredential)}`
  const options: RequestOptions = {
    protocol: 'https:',
    hostname: request.endpoint.hostname,
    port: request.endpoint.port,
    path: request.path,
    method: request.method,
    servername: request.tlsServerName,
    ca: request.tlsCertificateAuthority,
    minVersion: 'TLSv1.3',
    agent: false,
    headers: {
      Authorization: authorization,
      [CONTROL_LEASE_ID_HEADER]: request.controlLeaseId,
      Accept: 'application/json',
      ...(request.body === null
        ? {}
        : { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(request.body) }),
    },
    checkServerIdentity: (hostname, certificate) => {
      const hostnameFailure = checkServerIdentity(hostname, certificate)
      if (hostnameFailure !== undefined) {
        return new RemotePionProtocolError(
          'authority-binding-mismatch',
          'remote Pion TLS hostname identity mismatch',
        )
      }
      const expected = Buffer.from(request.tlsCertificateSha256, 'hex')
      const raw = certificate.raw === undefined
        ? Buffer.alloc(0)
        : Buffer.from(certificate.raw).subarray(0)
      const digest = createHash('sha256').update(raw).digest()
      return digest.length === expected.length && timingSafeEqual(digest, expected)
        ? undefined
        : new RemotePionProtocolError(
            'authority-binding-mismatch',
            'remote Pion TLS certificate pin mismatch',
          )
    },
    signal: request.signal,
  }
  const child = httpsRequest(options, (response) => {
    const chunks: Buffer[] = []
    let bytes = 0
    response.on('data', (chunk: Buffer) => {
      bytes += chunk.byteLength
      if (bytes > MAXIMUM_RESPONSE_BYTES) {
        child.destroy(protocolInvalid('remote Pion response exceeded its authority'))
        return
      }
      chunks.push(chunk)
    })
    response.once('end', () => {
      const socket = response.socket as TLSSocket
      const certificate = socket.getPeerCertificate()
      const raw = certificate.raw === undefined ? Buffer.alloc(0) : Buffer.from(certificate.raw)
      resolve(Object.freeze({
        statusCode: response.statusCode ?? 0,
        body: Buffer.concat(chunks).toString('utf8'),
        observedRemoteAddress: socket.remoteAddress ?? '',
        observedTlsCertificateSha256: createHash('sha256').update(raw).digest('hex'),
      }))
    })
  })
  child.once('error', (cause: Error & { readonly code?: string }) => {
    if (cause instanceof RemotePionProtocolError) {
      reject(cause)
      return
    }
    if (isTlsIdentityFailure(cause.code)) {
      reject(new RemotePionProtocolError(
        'authority-binding-mismatch',
        'remote Pion TLS identity could not be authenticated',
      ))
      return
    }
    reject(new RemotePionTransportUnavailableError())
  })
  if (request.body !== null) child.write(request.body)
  child.end()
})

function classifyProbeFailure(
  cause: unknown,
  signal: AbortSignal,
): ExternalFixtureProbeResult {
  if (signal.aborted || cause instanceof RemotePionOperationAbortedError) {
    return Object.freeze({ outcome: 'failed', failureCode: 'authority-probe-failed' })
  }
  if (cause instanceof ExternalFixtureAttestationError) {
    return cause.failureCode === 'authority-attestation-expired'
      ? Object.freeze({ outcome: 'unavailable', failureCode: cause.failureCode })
      : Object.freeze({ outcome: 'invalid', failureCode: cause.failureCode })
  }
  if (cause instanceof RemotePionProtocolError) {
    return Object.freeze({ outcome: 'invalid', failureCode: cause.failureCode })
  }
  if (cause instanceof RemotePionUnexpectedStatusError) {
    if (cause.statusCode === 410) {
      return Object.freeze({
        outcome: 'unavailable',
        failureCode: 'authority-attestation-expired',
      })
    }
    if (cause.statusCode === 428) {
      return Object.freeze({
        outcome: 'unavailable',
        failureCode: 'authority-key-rotation-required',
      })
    }
    if (cause.statusCode === 409) {
      return Object.freeze({ outcome: 'invalid', failureCode: 'authority-binding-mismatch' })
    }
    return Object.freeze({ outcome: 'failed', failureCode: 'authority-probe-failed' })
  }
  return cause instanceof RemotePionTransportUnavailableError
    ? Object.freeze({ outcome: 'unavailable', failureCode: 'authority-unreachable' })
    : Object.freeze({ outcome: 'failed', failureCode: 'authority-probe-failed' })
}

function attemptBinding(value: RemotePionAuthorityBinding): RemotePionAuthorityBinding {
  return {
    controlAuthority: value.controlAuthority,
    fixtureBinding: value.fixtureBinding,
  }
}

function authorityBindingFromAttempt(
  authority: NetworkMatrixAttemptAuthority,
): RemotePionAuthorityBinding {
  return Object.freeze({
    controlAuthority: authority.requestAuthority.controlAuthority,
    fixtureBinding: authority.requestAuthority.fixtureBinding,
  })
}

function parseAttemptAuthority(value: unknown): NetworkMatrixAttemptAuthority {
  try {
    return parseNetworkMatrixAttemptAuthority(value)
  } catch {
    throw protocolInvalid('remote Pion attempt authority is invalid')
  }
}

function parseControlAuthority(value: unknown): NetworkMatrixControlAuthority {
  try {
    return parseNetworkMatrixControlAuthority(value)
  } catch {
    throw protocolInvalid('remote Pion control authority is invalid')
  }
}

function parseFixtureBinding(value: unknown): NetworkMatrixFixtureAuthorityBinding {
  try {
    return parseNetworkMatrixFixtureAuthorityBinding(value)
  } catch {
    throw protocolInvalid('remote Pion fixture authority binding is invalid')
  }
}

function sameAuthorityBinding(
  left: RemotePionAuthorityBinding,
  right: RemotePionAuthorityBinding,
): boolean {
  return sameNetworkMatrixControlAuthority(left.controlAuthority, right.controlAuthority) &&
    JSON.stringify(left.fixtureBinding) === JSON.stringify(right.fixtureBinding)
}

function requireResponseBinding(
  value: Record<string, unknown>,
  expected: RemotePionAuthorityBinding,
): void {
  if (
    !sameNetworkMatrixControlAuthority(
      parseControlAuthority(value.controlAuthority),
      expected.controlAuthority,
    ) ||
    JSON.stringify(parseFixtureBinding(value.fixtureBinding)) !==
      JSON.stringify(expected.fixtureBinding)
  ) throw new RemotePionProtocolError(
    'authority-binding-mismatch',
    'remote Pion response differs from its signed authority binding',
  )
}

function requireObservedControllerAddress(
  response: Pick<
    RemotePionHttpResponse,
    'observedRemoteAddress' | 'observedTlsCertificateSha256'
  >,
  expectedCertificateSha256: string,
  localAddresses: readonly string[],
): string {
  const observed = normalizeObservedIpv4(response.observedRemoteAddress)
  if (
    observed === null || !isGlobalUnicastIpv4(observed) ||
    response.observedTlsCertificateSha256 !== expectedCertificateSha256 ||
    localAddresses.map(normalizeObservedIpv4).some((address) => address === observed)
  ) throw new RemotePionProtocolError(
    'authority-binding-mismatch',
    'remote Pion socket is not an independently hosted pinned controller',
  )
  return observed
}

function normalizeObservedIpv4(value: string): string | null {
  const candidate = value.toLowerCase().startsWith('::ffff:') ? value.slice(7) : value
  return isGlobalUnicastIpv4(candidate) || /^\d{1,3}(?:\.\d{1,3}){3}$/u.test(candidate)
    ? candidate
    : null
}

function systemInterfaceAddresses(): readonly string[] {
  return Object.freeze(Object.values(networkInterfaces()).flatMap((entries) =>
    (entries ?? []).map((entry) => entry.address)))
}

function parseCanonicalObject(encoded: string): Record<string, unknown> {
  let value: unknown
  try {
    value = JSON.parse(encoded)
  } catch {
    throw protocolInvalid('remote Pion response is not canonical JSON')
  }
  if (
    typeof value !== 'object' || value === null || Array.isArray(value) ||
    encoded !== `${JSON.stringify(value)}\n`
  ) throw protocolInvalid('remote Pion response is not a canonical protocol object')
  return value as Record<string, unknown>
}

function stringField(value: Record<string, unknown>, name: string): string {
  const field = value[name]
  if (typeof field !== 'string' || field.includes('\0')) {
    throw protocolInvalid('remote Pion response contains an invalid string field')
  }
  return field
}

function canonicalUtcTimestamp(value: string): number {
  if (!CANONICAL_UTC_TIMESTAMP_PATTERN.test(value)) {
    throw protocolInvalid('remote Pion lease expiry is not canonical UTC')
  }
  const timestamp = Date.parse(value)
  if (Number.isNaN(timestamp) || new Date(timestamp).toISOString() !== value) {
    throw protocolInvalid('remote Pion lease expiry is not a real UTC instant')
  }
  return timestamp
}

function requireExactKeys(value: Record<string, unknown>, expectedKeys: readonly string[]): void {
  const actual = Object.keys(value)
  if (
    actual.length !== expectedKeys.length ||
    actual.some((key, index) => key !== expectedKeys[index])
  ) throw protocolInvalid('remote Pion response contains fields outside its protocol authority')
}

function requireAttemptId(value: string): void {
  if (!OPAQUE_ID_PATTERN.test(value)) throw new Error('remote Pion attempt ID is invalid')
}

function protocolInvalid(message: string): RemotePionProtocolError {
  return new RemotePionProtocolError('proof-invalid', message)
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

function validControlLeaseBinding(value: RemotePionControlLeaseBinding): boolean {
  if (
    typeof value !== 'object' || value === null ||
    !OPAQUE_ID_PATTERN.test(value.probeNonce) ||
    !CANONICAL_ID_PATTERN.test(value.authorityInstanceId) ||
    !SHA256_PATTERN.test(value.attestationSha256) || value.maxAttempts !== 1
  ) return false
  try {
    parseNetworkMatrixControlAuthority(value.controlAuthority)
    const issuedAt = canonicalUtcTimestamp(value.issuedAt)
    const expiresAt = canonicalUtcTimestamp(value.expiresAt)
    return expiresAt > issuedAt && expiresAt - issuedAt <= REMOTE_PION_ATTESTATION_LEASE_MS
  } catch {
    return false
  }
}

function isTlsIdentityFailure(code: string | undefined): boolean {
  return code !== undefined && (
    code.startsWith('CERT_') || code.startsWith('ERR_TLS_CERT_') ||
    code.includes('SELF_SIGNED_CERT') || code.includes('UNABLE_TO_VERIFY') ||
    code.includes('UNABLE_TO_GET_ISSUER')
  )
}

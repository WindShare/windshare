import { createPublicKey } from 'node:crypto'

import type {
  ExternalFixtureProbeResult,
  NetworkMatrixRtcConfiguration,
  SampleExternalFixtureProbe,
} from '../runtime-authority.ts'
import type { NetworkMatrixOwnedOperation } from '../owned-operation.ts'
import type { NetworkMatrixProfileId } from '../vocabulary.ts'
import {
  NETWORK_MATRIX_ATTEMPT_REQUEST_AUTHORITY_SCHEMA,
  parseNetworkMatrixControlAuthority,
  parseNetworkMatrixSampleAuthority,
  sameNetworkMatrixAttemptAuthority,
  sameNetworkMatrixSampleAuthority,
  type NetworkMatrixAttemptRequestAuthority,
  type NetworkMatrixSampleAuthority,
} from '../sample-authority.ts'
import {
  parseExternalFixtureTurnCredential,
  type VerifiedExternalFixtureAuthority,
} from './external-fixture-attestation.ts'
import { externalFixtureRtcConfiguration } from './external-fixture-rtc.ts'
import { authenticateRemotePionAuthority } from './remote-pion/attestation.ts'
import {
  REMOTE_PION_ATTESTATION_LEASE_MS,
  REMOTE_PION_MAXIMUM_ATTEMPT_LEASE_MS,
  REMOTE_PION_PROTOCOL_VERSION,
  type LiveRemotePionAuthority,
  type RemotePionAttemptAuthorityState,
  type RemotePionAttemptLease,
  type RemotePionAttemptResult,
  type RemotePionAuthorityBinding,
  type RemotePionCallResult,
  type RemotePionControlLeaseBinding,
  type RemotePionControlOptions,
} from './remote-pion/contracts.ts'
import { RemotePionControlChannel } from './remote-pion/control-channel.ts'
import {
  RemotePionAuthorityExpiredError,
  RemotePionContainmentError,
  RemotePionOperationAbortedError,
  RemotePionProtocolError,
} from './remote-pion/errors.ts'
import {
  attemptBinding,
  authorityBindingFromAttempt,
  canonicalUtcTimestamp,
  classifyProbeFailure,
  fixtureProbeResult,
  isControlCredentialBytes,
  parseAttemptAuthority,
  parseRemotePionAttemptResult,
  protocolInvalid,
  requireAttemptId,
  requireExactKeys,
  requireRemotePionControllerAuthority,
  requireResponseBinding,
  stringField,
  validControlLeaseBinding,
  validOpaqueId,
} from './remote-pion/protocol.ts'

export {
  REMOTE_PION_ATTESTATION_LEASE_MS,
  REMOTE_PION_MAXIMUM_ATTEMPT_LEASE_MS,
  REMOTE_PION_PROTOCOL_VERSION,
  REMOTE_PION_REQUEST_DEADLINE_MS,
} from './remote-pion/contracts.ts'
export type {
  RemotePionAttemptLease,
  RemotePionAttemptResult,
  RemotePionAuthorityBinding,
  RemotePionControlLeaseBinding,
  RemotePionControlOptions,
  RemotePionHttpRequest,
  RemotePionHttpResponse,
  RemotePionRequest,
} from './remote-pion/contracts.ts'
export {
  RemotePionAuthorityExpiredError,
  RemotePionContainmentError,
  RemotePionProtocolError,
  RemotePionTransportUnavailableError,
} from './remote-pion/errors.ts'
export { executeRemotePionRequest } from './remote-pion/http-transport.ts'
export { remotePionChallengeBindingSha256 } from './remote-pion/protocol.ts'

const SHA256_PATTERN = /^[a-f0-9]{64}$/u
const CREATE_RECOVERY_STATUS_CODES = Object.freeze([200, 201] as const)
const REMOTE_PION_MAXIMUM_CLOCK_SKEW_MS = 30_000

export class RemotePionControlClient implements SampleExternalFixtureProbe {
  readonly #endpoint: URL
  readonly #certificateSha256: string
  readonly #attestationPublicKey: string | Buffer
  readonly #attestationPublicKeySpki: string
  readonly #controlLease: RemotePionControlLeaseBinding
  readonly #manualOperatorIdentity: RemotePionControlOptions['manualOperatorIdentity']
  readonly #now: () => number
  readonly #channel: RemotePionControlChannel
  readonly #liveAuthorityByRun = new Map<string, LiveRemotePionAuthority>()
  readonly #authorityByAttempt = new Map<string, RemotePionAttemptAuthorityState>()
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
    this.#controlLease = Object.freeze({
      ...options.controlLease,
      controlAuthority: parseNetworkMatrixControlAuthority(options.controlLease.controlAuthority),
    })
    this.#manualOperatorIdentity = options.manualOperatorIdentity
    this.#now = options.now ?? Date.now
    this.#channel = new RemotePionControlChannel({
      endpoint,
      tlsCertificateAuthority: options.tlsCertificateAuthority,
      tlsCertificateSha256: options.tlsCertificateSha256,
      controlCredential: options.controlCredential,
      controlLeaseId: this.#controlLease.controlAuthority.controlLeaseId,
      ...(options.request === undefined ? {} : { request: options.request }),
      ...(options.localInterfaceAddresses === undefined
        ? {}
        : { localInterfaceAddresses: options.localInterfaceAddresses }),
    })
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
      !validOpaqueId(requestId) ||
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
    authority: LiveRemotePionAuthority,
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
    authority: LiveRemotePionAuthority,
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
    authority: { readonly attemptAuthority: RemotePionAttemptAuthorityState['attemptAuthority'] },
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
    input: { readonly sampleAuthority: NetworkMatrixSampleAuthority },
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
      if (!validOpaqueId(nonce)) {
        return Object.freeze({ outcome: 'failed', failureCode: 'authority-probe-failed' })
      }
      const call = await this.#post('/v2/authority-probe', {
        protocolVersion: REMOTE_PION_PROTOCOL_VERSION,
        controlAuthority: this.#controlLease.controlAuthority,
        nonce,
        requestedLeaseMillis: REMOTE_PION_ATTESTATION_LEASE_MS,
      }, signal, 200)
      const state = authenticateRemotePionAuthority({
        call,
        controlLease: this.#controlLease,
        profileId,
        nonce,
        requestedLeaseMillis: REMOTE_PION_ATTESTATION_LEASE_MS,
        controllerOrigin: this.#endpoint.toString(),
        tlsCertificateSha256: this.#certificateSha256,
        attestationPublicKey: this.#attestationPublicKey,
        attestationPublicKeySpki: this.#attestationPublicKeySpki,
        manualOperatorIdentity: this.#manualOperatorIdentity,
        now: this.#now,
      })
      this.#liveAuthorityByRun.set(runId, state)
      let rtcConfiguration: NetworkMatrixRtcConfiguration
      try {
        const fixture = state.verified.attestation.fixture
        const turnCredential = fixture.networkSemantics.kind === 'coturn-relay'
          ? await this.#turnCredential(state, state.verified, signal)
          : null
        rtcConfiguration = externalFixtureRtcConfiguration(
          fixture,
          turnCredential,
          this.#now,
        )
        if (state.verified.activeStunEndpoint !== null) {
          await this.#activeStunProbe(state, state.verified, nonce, signal)
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
  }): Promise<LiveRemotePionAuthority | null> {
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
        await this.#deleteAttemptAuthority(authority, new AbortController().signal)
      } catch {
        throw new RemotePionContainmentError()
      }
      this.#authorityByAttempt.delete(attemptId)
    }
    this.#liveAuthorityByRun.delete(input.runId)
    return null
  }

  #requireLiveAuthority(runId: string): LiveRemotePionAuthority {
    const authority = this.#liveAuthorityByRun.get(runId)
    if (authority === undefined) throw new Error('remote Pion run has no verified live authority')
    if (Date.parse(authority.expiresAt) <= this.#now()) {
      this.#retireExpiredLiveAuthority(runId)
      throw new RemotePionAuthorityExpiredError()
    }
    if (authority.rtcConfiguration === null) throw new Error('remote Pion run authority is not ready')
    return authority
  }

  #requireAttemptAuthority(
    attemptId: string,
    permitExpired: boolean,
  ): RemotePionAttemptAuthorityState {
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

  async #post(
    path: string,
    body: Readonly<Record<string, unknown>>,
    signal: AbortSignal,
    expectedStatus: number | readonly number[],
    authority?: RemotePionAuthorityBinding,
  ): Promise<RemotePionCallResult> {
    const call = await this.#channel.post(
      path,
      body,
      signal,
      expectedStatus,
      authority !== undefined,
    )
    requireRemotePionControllerAuthority(
      this.#liveAuthorityByRun,
      authority,
      call.observedRemoteAddress,
    )
    return call
  }

  async #call(
    path: string,
    body: string | null,
    signal: AbortSignal,
    expectedStatus: number | readonly number[],
    method: 'GET' | 'POST' | 'DELETE',
    authority?: RemotePionAuthorityBinding,
  ): Promise<RemotePionCallResult> {
    const call = await this.#channel.call(
      path,
      body,
      signal,
      expectedStatus,
      method,
      authority !== undefined,
    )
    requireRemotePionControllerAuthority(
      this.#liveAuthorityByRun,
      authority,
      call.observedRemoteAddress,
    )
    return call
  }
}

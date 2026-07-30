import {
  NETWORK_MATRIX_FAILED_FAILURE_CODES,
  NETWORK_MATRIX_INVALID_FAILURE_CODES,
  NETWORK_MATRIX_UNAVAILABLE_FAILURE_CODES,
  NETWORK_RUNTIME_ATTESTATION_SCHEMA,
  type NetworkMatrixProfileId,
} from './vocabulary.ts'
import {
  parseNetworkRuntimeAttestation,
  type ExternalFixtureTrustProof,
  type NetworkRuntimeAttestation,
  type NetworkRuntimeAttestationContext,
  type NetworkRuntimeProof,
} from './attestation.ts'
import type {
  LoadedNetworkMatrixRegistry,
  NetworkMatrixProfileReference,
} from './manifest.ts'
import type { NetworkTopologyProfile } from './profile.ts'
import {
  completedOwnedOperation,
  NetworkMatrixOwnershipCleanupError,
  type NetworkMatrixOwnedOperation,
  type NetworkMatrixOperationClass,
} from './owned-operation.ts'
import { validNetworkMatrixIceUri } from './ice-uri.ts'
import type { SignedExternalFixtureAttestation } from './linux-topology/external-fixture-attestation.ts'
import { externalFixturePublicConfigurationSha256 } from './external-fixture-proof.ts'
import type { NetworkMatrixSampleAuthority } from './sample-authority.ts'

const MAXIMUM_ICE_CREDENTIAL_BYTES = 512

type UnavailableFailureCode = (typeof NETWORK_MATRIX_UNAVAILABLE_FAILURE_CODES)[number]
type InvalidFailureCode = (typeof NETWORK_MATRIX_INVALID_FAILURE_CODES)[number]
type FailedFailureCode = (typeof NETWORK_MATRIX_FAILED_FAILURE_CODES)[number]

export interface NetworkMatrixCoturnInput {
  readonly urls: readonly string[]
  readonly username: string
  readonly credential: string
}

export interface NetworkMatrixExternalFixtureInput {
  readonly profileId: NetworkMatrixProfileId
}

export type NetworkMatrixExternalFixtureInputs = Readonly<
  Partial<Record<NetworkMatrixProfileId, NetworkMatrixExternalFixtureInput>>
>

export interface NetworkMatrixRuntimeInputs {
  readonly externalFixtures: NetworkMatrixExternalFixtureInputs
}

export interface NetworkMatrixIceServer {
  readonly urls: readonly string[]
  readonly username: string | null
  readonly credential: string | null
}

export interface NetworkMatrixRtcConfiguration {
  readonly iceServers: readonly NetworkMatrixIceServer[]
  readonly iceTransportPolicy: 'all' | 'relay'
}

export interface NetworkMatrixExecutionAuthority {
  readonly profileId: NetworkMatrixProfileId
  readonly runtimeKind: 'external-fixture'
}

export interface NetworkMatrixAuthorityPreparationContext {
  readonly registry: LoadedNetworkMatrixRegistry
  readonly reference: NetworkMatrixProfileReference
  readonly profile: NetworkTopologyProfile
  readonly runId: string
  readonly signal: AbortSignal
}

export interface PreparedNetworkMatrixAuthority {
  readonly attestation: NetworkRuntimeAttestation
  readonly execution: NetworkMatrixExecutionAuthority | null
  close(): NetworkMatrixOwnedOperation<void>
  /** Independent, idempotent fallback retained even if close() cannot start. */
  forceTerminateAndWait(reason: NetworkMatrixOperationClass): Promise<void>
}

export interface NetworkMatrixAuthorityResolver {
  prepare(
    context: NetworkMatrixAuthorityPreparationContext,
  ): NetworkMatrixOwnedOperation<PreparedNetworkMatrixAuthority>
}

export type ExternalFixtureProbeResult =
  | {
      readonly outcome: 'satisfied'
      readonly probeId: string
      readonly runId: string
      readonly profileId: NetworkMatrixProfileId
      readonly authorityInstanceId: string
      readonly remoteServiceInstanceId: string
      readonly attestationSha256: string
      readonly attestationPublicKeySpki: string
      readonly signedAttestation: SignedExternalFixtureAttestation
      readonly networkBindingSha256: string
      readonly remotePeerBindingSha256: string
      readonly controllerPublicIp: string
      readonly leaseExpiresAt: string
      readonly rtcConfiguration: NetworkMatrixRtcConfiguration
    }
  | {
      readonly outcome: 'unavailable'
      readonly failureCode: Extract<
        UnavailableFailureCode,
        | 'authority-attestation-expired'
        | 'authority-key-rotation-required'
        | 'authority-unreachable'
      >
    }
  | {
      readonly outcome: 'invalid'
      readonly failureCode: Extract<InvalidFailureCode, 'proof-invalid' | 'authority-binding-mismatch'>
    }
  | {
      readonly outcome: 'failed'
      readonly failureCode: Extract<FailedFailureCode, 'authority-probe-failed' | 'runtime-check-failed'>
    }

/** A live probe is sample-owned because its credential is bound to this exact authority. */
export interface SampleExternalFixtureProbe {
  probe(input: {
    readonly sampleAuthority: NetworkMatrixSampleAuthority
    readonly signal: AbortSignal
  }): NetworkMatrixOwnedOperation<ExternalFixtureProbeResult>
}

export type ExternalFixtureTrustInspectionResult =
  | {
      readonly outcome: 'satisfied'
      readonly profileId: NetworkMatrixProfileId
      readonly trust: ExternalFixtureTrustProof
    }
  | {
      readonly outcome: 'unavailable'
      readonly failureCode: Extract<UnavailableFailureCode, 'authority-not-provisioned'>
    }
  | {
      readonly outcome: 'invalid'
      readonly failureCode: Extract<InvalidFailureCode, 'proof-invalid'>
    }
  | {
      readonly outcome: 'failed'
      readonly failureCode: Extract<FailedFailureCode, 'runtime-check-failed'>
    }

export interface ExternalFixtureTrustInspector {
  inspect(input: {
    readonly profileId: NetworkMatrixProfileId
    readonly expectedAttestationPublicKeySha256: string
    readonly signal: AbortSignal
  }): NetworkMatrixOwnedOperation<ExternalFixtureTrustInspectionResult>
}

interface AuthorityOperationState {
  active: NetworkMatrixOwnedOperation<unknown> | null
  prepared: PreparedNetworkMatrixAuthority | null
}

export interface InjectedNetworkMatrixAuthorityResolverOptions {
  readonly inputs: NetworkMatrixRuntimeInputs
  readonly externalFixtureTrust: ExternalFixtureTrustInspector
}

export class InjectedNetworkMatrixAuthorityResolver implements NetworkMatrixAuthorityResolver {
  readonly #inputs: NetworkMatrixRuntimeInputs
  readonly #externalFixtureTrust: ExternalFixtureTrustInspector

  constructor(options: InjectedNetworkMatrixAuthorityResolverOptions) {
    this.#inputs = options.inputs
    this.#externalFixtureTrust = options.externalFixtureTrust
  }

  prepare(
    context: NetworkMatrixAuthorityPreparationContext,
  ): NetworkMatrixOwnedOperation<PreparedNetworkMatrixAuthority> {
    const state: AuthorityOperationState = { active: null, prepared: null }
    const result = this.#prepare(context, state).then((prepared) => {
      state.prepared = prepared
      return prepared
    })
    return Object.freeze({
      result,
      forceTerminateAndWait: (reason: NetworkMatrixOperationClass) =>
        terminateAuthorityOperation(state, reason),
    })
  }

  async #prepare(
    context: NetworkMatrixAuthorityPreparationContext,
    state: AuthorityOperationState,
  ): Promise<PreparedNetworkMatrixAuthority> {
    const input = this.#inputs.externalFixtures[context.profile.profileId]
    if (input === undefined) return unavailable(context, 'authority-not-provisioned')
    if (!validExternalFixtureInput(input, context.profile.profileId)) {
      return invalid(context, 'proof-invalid')
    }
    let inspection: ExternalFixtureTrustInspectionResult
    try {
      inspection = await settleDependencyOperation(state, this.#externalFixtureTrust.inspect({
        profileId: context.profile.profileId,
        expectedAttestationPublicKeySha256:
          context.profile.authority.attestationPublicKeySha256,
        signal: context.signal,
      }))
    } catch (cause) {
      if (cause instanceof NetworkMatrixOwnershipCleanupError) throw cause
      return failed(context, 'runtime-check-failed')
    }
    if (inspection.outcome !== 'satisfied') return prerequisiteFailure(context, inspection)
    if (inspection.profileId !== context.profile.profileId) return invalid(context, 'proof-invalid')
    try {
      return satisfied(
        context,
        {
          proofKind: 'external-fixture-trust',
          externalFixtureTrust: inspection.trust,
        },
        {
          profileId: context.profile.profileId,
          runtimeKind: 'external-fixture',
        },
        noClose,
        noForceClose,
      )
    } catch {
      return invalid(context, 'proof-invalid')
    }
  }
}

export function failedNetworkMatrixAuthorityPreparation(
  context: NetworkMatrixAuthorityPreparationContext,
  failureCode: FailedFailureCode = 'runtime-check-failed',
): PreparedNetworkMatrixAuthority {
  return failed(context, failureCode)
}

export function externalFixtureConfigurationSha256(
  profileId: NetworkMatrixProfileId,
  attestationSha256: string,
  rtc: NetworkMatrixRtcConfiguration,
): string {
  if (!validSha256(attestationSha256)) {
    throw new Error('external fixture live attestation digest is invalid')
  }
  const publicSemantics = freezeRtcConfiguration(rtc)
  return externalFixturePublicConfigurationSha256(
    profileId,
    attestationSha256,
    publicSemantics.iceTransportPolicy,
    publicSemantics.iceServers.map((server) => server.urls),
  )
}

function satisfied(
  context: NetworkMatrixAuthorityPreparationContext,
  proof: NetworkRuntimeProof,
  execution: NetworkMatrixExecutionAuthority,
  close: () => NetworkMatrixOwnedOperation<void>,
  forceTerminateAndWait: (reason: NetworkMatrixOperationClass) => Promise<void>,
): PreparedNetworkMatrixAuthority {
  return Object.freeze({
    attestation: attestation(context, 'satisfied', proof, null),
    execution: Object.freeze(execution),
    close,
    forceTerminateAndWait,
  })
}

function unavailable(
  context: NetworkMatrixAuthorityPreparationContext,
  failureCode: UnavailableFailureCode,
): PreparedNetworkMatrixAuthority {
  return prerequisiteFailure(context, { outcome: 'unavailable', failureCode })
}

function invalid(
  context: NetworkMatrixAuthorityPreparationContext,
  failureCode: InvalidFailureCode,
): PreparedNetworkMatrixAuthority {
  return prerequisiteFailure(context, { outcome: 'invalid', failureCode })
}

function failed(
  context: NetworkMatrixAuthorityPreparationContext,
  failureCode: FailedFailureCode,
): PreparedNetworkMatrixAuthority {
  return prerequisiteFailure(context, { outcome: 'failed', failureCode })
}

function prerequisiteFailure(
  context: NetworkMatrixAuthorityPreparationContext,
  failure: {
    readonly outcome: 'unavailable' | 'invalid' | 'failed'
    readonly failureCode: string
  },
): PreparedNetworkMatrixAuthority {
  return Object.freeze({
    attestation: attestation(context, failure.outcome, null, {
      failureKind: failure.outcome,
      failureCode: failure.failureCode,
    }),
    execution: null,
    close: noClose,
    forceTerminateAndWait: noForceClose,
  })
}

function attestation(
  context: NetworkMatrixAuthorityPreparationContext,
  prerequisiteOutcome: 'satisfied' | 'unavailable' | 'invalid' | 'failed',
  proof: NetworkRuntimeProof | null,
  failure: { readonly failureKind: string; readonly failureCode: string } | null,
): NetworkRuntimeAttestation {
  return parseNetworkRuntimeAttestation({
    schemaVersion: NETWORK_RUNTIME_ATTESTATION_SCHEMA,
    runId: context.runId,
    manifestSha256: context.registry.manifestSha256,
    profileId: context.reference.profileId,
    profileSha256: context.reference.profileSha256,
    authorityId: context.reference.authorityId,
    authorityKind: context.reference.authorityKind,
    prerequisiteOutcome,
    proof,
    failure,
  }, attestationContext(context))
}

function attestationContext(
  context: NetworkMatrixAuthorityPreparationContext,
): NetworkRuntimeAttestationContext {
  return Object.freeze({
    manifest: context.registry.manifest,
    manifestSha256: context.registry.manifestSha256,
    runId: context.runId,
  })
}

export function networkMatrixRtcConfiguration(
  iceServers: readonly NetworkMatrixIceServer[],
  iceTransportPolicy: NetworkMatrixRtcConfiguration['iceTransportPolicy'],
): NetworkMatrixRtcConfiguration {
  return freezeRtcConfiguration({ iceServers, iceTransportPolicy })
}

function freezeRtcConfiguration(rtc: NetworkMatrixRtcConfiguration): NetworkMatrixRtcConfiguration {
  return Object.freeze({
    iceServers: Object.freeze(rtc.iceServers.map((server) => Object.freeze({
      urls: Object.freeze([...server.urls]),
      username: server.username,
      credential: server.credential,
    }))),
    iceTransportPolicy: rtc.iceTransportPolicy,
  })
}

function validExternalFixtureInput(
  input: NetworkMatrixExternalFixtureInput,
  expectedProfileId: NetworkMatrixProfileId,
): boolean {
  return typeof input === 'object' && input !== null && !Array.isArray(input) &&
    Object.keys(input).length === 1 && input.profileId === expectedProfileId
}

export function coturnRtcConfiguration(value: NetworkMatrixCoturnInput): NetworkMatrixRtcConfiguration {
  if (
    !Array.isArray(value.urls) || value.urls.length === 0 || value.urls.length > 16 ||
    new Set(value.urls).size !== value.urls.length ||
    !value.urls.every((url) => validNetworkMatrixIceUri(url, ['turn:', 'turns:'])) ||
    !validCredential(value.username) || !validCredential(value.credential)
  ) throw new Error('network matrix Coturn configuration is invalid')
  return networkMatrixRtcConfiguration([{
    urls: value.urls,
    username: value.username,
    credential: value.credential,
  }], 'relay')
}

export function externalStunRtcConfiguration(endpoint: string): NetworkMatrixRtcConfiguration {
  if (!validNetworkMatrixIceUri(endpoint, ['stun:'])) {
    throw new Error('network matrix STUN configuration is invalid')
  }
  return networkMatrixRtcConfiguration([{
    urls: [endpoint],
    username: null,
    credential: null,
  }], 'all')
}

export function restrictedUdpRtcConfiguration(): NetworkMatrixRtcConfiguration {
  return networkMatrixRtcConfiguration([], 'all')
}

function validCredential(value: unknown): value is string {
  return typeof value === 'string' && value !== '' &&
    Buffer.byteLength(value, 'utf8') <= MAXIMUM_ICE_CREDENTIAL_BYTES
}

export { validNetworkMatrixIceUri } from './ice-uri.ts'

function validSha256(value: unknown): value is string {
  return typeof value === 'string' && /^[0-9a-f]{64}$/u.test(value)
}

function noClose(): NetworkMatrixOwnedOperation<void> {
  return completedOwnedOperation(undefined)
}

function noForceClose(): Promise<void> {
  return Promise.resolve()
}

async function settleDependencyOperation<T>(
  state: AuthorityOperationState,
  operation: NetworkMatrixOwnedOperation<T>,
): Promise<T> {
  state.active = operation
  try {
    try {
      return await operation.result
    } catch (primaryFailure) {
      try {
        await operation.forceTerminateAndWait('authority-prepare')
      } catch (cleanupFailure) {
        throw new NetworkMatrixOwnershipCleanupError(
          'authority-prepare',
          primaryFailure,
          cleanupFailure,
        )
      }
      throw primaryFailure
    }
  } finally {
    if (state.active === operation) state.active = null
  }
}

async function terminateAuthorityOperation(
  state: AuthorityOperationState,
  reason: NetworkMatrixOperationClass,
): Promise<void> {
  if (state.active !== null) {
    await state.active.forceTerminateAndWait(reason)
    return
  }
  await state.prepared?.forceTerminateAndWait(reason)
}

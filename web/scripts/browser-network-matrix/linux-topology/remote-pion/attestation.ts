import type { NetworkMatrixProfileId } from '../../vocabulary.ts'
import {
  parseSignedExternalFixtureAttestation,
  verifyExternalFixtureAttestation,
} from '../external-fixture-attestation.ts'
import {
  REMOTE_PION_PROTOCOL_VERSION,
  type LiveRemotePionAuthority,
  type RemotePionAuthorityBinding,
  type RemotePionCallResult,
  type RemotePionControlLeaseBinding,
} from './contracts.ts'
import { RemotePionProtocolError } from './errors.ts'

export function authenticateRemotePionAuthority(input: {
  readonly call: RemotePionCallResult
  readonly controlLease: RemotePionControlLeaseBinding
  readonly profileId: NetworkMatrixProfileId
  readonly nonce: string
  readonly requestedLeaseMillis: number
  readonly controllerOrigin: string
  readonly tlsCertificateSha256: string
  readonly attestationPublicKey: string | Buffer
  readonly attestationPublicKeySpki: string
  readonly now: () => number
}): LiveRemotePionAuthority {
  const signed = parseSignedExternalFixtureAttestation(
    input.call.value,
    REMOTE_PION_PROTOCOL_VERSION,
  )
  const verified = verifyExternalFixtureAttestation(signed, {
    protocolVersion: REMOTE_PION_PROTOCOL_VERSION,
    profileId: input.profileId,
    runId: input.controlLease.controlAuthority.sampleAuthority.runId,
    nonce: input.nonce,
    requestedLeaseMillis: input.requestedLeaseMillis,
    controllerOrigin: input.controllerOrigin,
    tlsCertificateSha256: input.tlsCertificateSha256,
    observedControllerIp: input.call.observedRemoteAddress,
    observedTlsCertificateSha256: input.call.observedTlsCertificateSha256,
    attestationPublicKey: input.attestationPublicKey,
    now: input.now,
  })
  const fixture = verified.attestation.fixture
  if (
    fixture.authorityInstanceId !== input.controlLease.authorityInstanceId ||
    verified.attestationSha256 !== input.controlLease.attestationSha256 ||
    Date.parse(input.controlLease.expiresAt) > Date.parse(verified.attestation.expiresAt)
  ) throw new RemotePionProtocolError(
    'authority-binding-mismatch',
    'remote Pion attestation differs from its ephemeral control credential lease',
  )
  const binding: RemotePionAuthorityBinding = Object.freeze({
    controlAuthority: input.controlLease.controlAuthority,
    fixtureBinding: Object.freeze({
      attestationSha256: verified.attestationSha256,
      authorityInstanceId: fixture.authorityInstanceId,
      remoteServiceInstanceId: fixture.remoteServiceInstanceId,
      networkBindingSha256: verified.networkBindingSha256,
      remotePeerBindingSha256: verified.remotePeerBindingSha256,
    }),
  })
  return Object.freeze({
    ...binding,
    profileId: input.profileId,
    authorityInstanceId: fixture.authorityInstanceId,
    controllerPublicIp: input.call.observedRemoteAddress,
    issuedAt: verified.attestation.issuedAt,
    expiresAt: verified.attestation.expiresAt,
    verified,
    rtcConfiguration: null,
    attestationPublicKeySpki: input.attestationPublicKeySpki,
    signedAttestation: signed,
  })
}

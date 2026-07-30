import {
  coturnRtcConfiguration,
  externalStunRtcConfiguration,
  restrictedUdpRtcConfiguration,
  type NetworkMatrixRtcConfiguration,
} from '../runtime-authority.ts'
import {
  ExternalFixtureAttestationError,
  type ExternalFixtureDeclaration,
  type ExternalFixtureTurnCredential,
} from './external-fixture-attestation.ts'

export function externalFixtureRtcConfiguration(
  fixture: ExternalFixtureDeclaration,
  turnCredential: ExternalFixtureTurnCredential | null,
  now: () => number = Date.now,
): NetworkMatrixRtcConfiguration {
  const semantics = fixture.networkSemantics
  switch (semantics.kind) {
    case 'public-stun':
    case 'operator-real-nat':
      if (turnCredential !== null) invalidBinding()
      return externalStunRtcConfiguration(semantics.stunEndpoint)
    case 'restricted-udp':
      if (turnCredential !== null) invalidBinding()
      return restrictedUdpRtcConfiguration()
    case 'coturn-relay':
      if (
        turnCredential === null ||
        turnCredential.credentialId !== semantics.turnCredentialId ||
        turnCredential.expiresAt !== semantics.turnCredentialExpiresAt ||
        turnCredential.username !== semantics.turnUsername ||
        Date.parse(turnCredential.expiresAt) <= now()
      ) expiredAttestation()
      return coturnRtcConfiguration({
        urls: semantics.turnUrls,
        username: turnCredential.username,
        credential: turnCredential.credential,
      })
  }
}

function invalidBinding(): never {
  throw new ExternalFixtureAttestationError('authority-binding-mismatch')
}

function expiredAttestation(): never {
  throw new ExternalFixtureAttestationError('authority-attestation-expired')
}
